package applyinator

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/rancher/system-agent/pkg/image"
	"github.com/rancher/system-agent/pkg/prober"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

type Applyinator struct {
	mu              *sync.Mutex
	workDir         string
	preserveWorkDir bool
	appliedPlanDir  string
	imageUtil       *image.Utility
}

// CalculatedPlan is passed into Applyinator and is a Plan with checksum calculated
type CalculatedPlan struct {
	Plan     Plan
	Checksum string
}

type Plan struct {
	Files                []File                  `json:"files,omitempty"`
	OneTimeInstructions  []OneTimeInstruction    `json:"instructions,omitempty"`
	Probes               map[string]prober.Probe `json:"probes,omitempty"`
	PeriodicInstructions []PeriodicInstruction   `json:"periodicInstructions,omitempty"`
}

type CommonInstruction struct {
	Name    string   `json:"name,omitempty"`
	Image   string   `json:"image,omitempty"`
	Env     []string `json:"env,omitempty"`
	Args    []string `json:"args,omitempty"`
	Command string   `json:"command,omitempty"`
}

type PeriodicInstruction struct {
	CommonInstruction
	PeriodSeconds int `json:"periodSeconds,omitempty"` // default 600, i.e. 5 minutes
}

type PeriodicInstructionOutput struct {
	Name        string `json:"name"`
	Stdout      []byte `json:"stdout"`      // Stdout is a byte array of the gzip+base64 stdout output
	Stderr      []byte `json:"stderr"`      // Stderr is a byte array of the gzip+base64 stderr output
	ExitCode    int    `json:"exitCode"`    // ExitCode is an int representing the exit code of the last run instruction
	LastRunTime string `json:"lastRunTime"` // LastRunTime is a time.UnixDate formatted string of the last time the instruction was run
}

type OneTimeInstruction struct {
	CommonInstruction
	SaveOutput bool `json:"saveOutput,omitempty"`
}

// Path would be `/etc/kubernetes/ssl/ca.pem`, Content is base64 encoded.
// If Directory is true, then we are creating a directory, not a file
type File struct {
	Content     string `json:"content,omitempty"`
	Directory   bool   `json:"directory,omitempty"`
	UID         int    `json:"uid,omitempty"`
	GID         int    `json:"gid,omitempty"`
	Path        string `json:"path,omitempty"`
	Permissions string `json:"permissions,omitempty"` // internally, the string will be converted to a uint32 to satisfy os.FileMode
}

const appliedPlanFileSuffix = "-applied.plan"
const applyinatorDateCodeLayout = "20060102-150405"
const defaultCommand = "/run.sh"
const cattleAgentExecutionPwdEnvKey = "CATTLE_AGENT_EXECUTION_PWD"

func NewApplyinator(workDir string, preserveWorkDir bool, appliedPlanDir string, imageUtil *image.Utility) *Applyinator {
	return &Applyinator{
		mu:              &sync.Mutex{},
		workDir:         workDir,
		preserveWorkDir: preserveWorkDir,
		appliedPlanDir:  appliedPlanDir,
		imageUtil:       imageUtil,
	}
}

func CalculatePlan(rawPlan []byte) (CalculatedPlan, error) {
	var cp CalculatedPlan
	var plan Plan
	if err := json.Unmarshal(rawPlan, &plan); err != nil {
		return cp, err
	}

	cp.Checksum = checksum(rawPlan)
	cp.Plan = plan

	return cp, nil
}

func checksum(input []byte) string {
	h := sha256.New()
	h.Write(input)

	return fmt.Sprintf("%x", h.Sum(nil))
}

// RunPeriodicInstructions accepts a context, calculated plan, and input byte slice which is a base64+gzip json-marshalled map of PeriodicInstructionOutput
// entries where the key is the PeriodicInstructionOutput.Name. It outputs a revised version of the input byte slice json-marshalled data

// Apply takes a bunch of paramters and returns
func (a *Applyinator) Apply(ctx context.Context, cp CalculatedPlan, runOneTimeInstructions bool, existingOneTimeOutput, existingPeriodicOutput []byte) (oneTimeApplySucceeded bool, oneTimeApplyOutput []byte, periodicApplySucceeded bool, periodicApplyOutput []byte, err error) {
	logrus.Infof("[Applyinator] Applying plan with checksum %s", cp.Checksum)
	logrus.Tracef("[Applyinator] Applying plan - attempting to get lock")
	a.mu.Lock()
	logrus.Tracef("[Applyinator] Applying plan - lock achieved")
	defer a.mu.Unlock()
	now := time.Now()
	nowUnixTimeString := now.Format(time.UnixDate)
	nowString := now.Format(applyinatorDateCodeLayout)
	appliedPlanFile := nowString + appliedPlanFileSuffix
	executionDir := filepath.Join(a.workDir, nowString)
	logrus.Tracef("[Applyinator] Applying calculated node plan contents %v", cp)
	logrus.Tracef("[Applyinator] Using %s as execution directory", executionDir)
	if a.appliedPlanDir != "" {
		logrus.Debugf("[Applyinator] Writing applied calculated plan contents to historical plan directory %s", a.appliedPlanDir)
		if err := os.MkdirAll(filepath.Dir(a.appliedPlanDir), 0700); err != nil {
			return false, existingOneTimeOutput, false, existingPeriodicOutput, err
		}
		anpString, err := json.Marshal(cp)
		if err != nil {
			return false, existingOneTimeOutput, false, existingPeriodicOutput, err
		}
		if err := writeContentToFile(filepath.Join(a.appliedPlanDir, appliedPlanFile), os.Getuid(), os.Getgid(), 0600, anpString); err != nil {
			return false, existingOneTimeOutput, false, existingPeriodicOutput, err
		}
	}

	for _, file := range cp.Plan.Files {
		if file.Directory {
			logrus.Debugf("[Applyinator] Creating directory %s", file.Path)
			if err := createDirectory(file); err != nil {
				return false, existingOneTimeOutput, false, existingPeriodicOutput, err
			}
		} else {
			logrus.Debugf("[Applyinator] Writing file %s", file.Path)
			if err := writeBase64ContentToFile(file); err != nil {
				return false, existingOneTimeOutput, false, existingPeriodicOutput, err
			}
		}
	}

	if !a.preserveWorkDir {
		logrus.Debugf("[Applyinator] Cleaning working directory before applying %s", a.workDir)
		if err := os.RemoveAll(a.workDir); err != nil {
			return false, existingOneTimeOutput, false, existingPeriodicOutput, err
		}
	}
	oneTimeApplySucceeded = true
	if runOneTimeInstructions {
		executionOutputs := map[string][]byte{}
		if len(existingPeriodicOutput) > 0 {
			buffer := bytes.NewBuffer(existingOneTimeOutput)
			gzReader, err := gzip.NewReader(buffer)
			if err != nil {
				return false, existingOneTimeOutput, false, existingPeriodicOutput, err
			}

			var objectBuffer bytes.Buffer
			_, err = io.Copy(&objectBuffer, gzReader)
			if err != nil {
				return false, existingOneTimeOutput, false, existingPeriodicOutput, err
			}

			if err := json.Unmarshal(objectBuffer.Bytes(), &executionOutputs); err != nil {
				return false, existingOneTimeOutput, false, existingPeriodicOutput, err
			}
		}

		for index, instruction := range cp.Plan.OneTimeInstructions {
			logrus.Debugf("[Applyinator] Executing instruction %d for plan %s", index, cp.Checksum)
			executionInstructionDir := filepath.Join(executionDir, cp.Checksum+"_"+strconv.Itoa(index))
			prefix := cp.Checksum + "_" + strconv.Itoa(index)
			output, _, _, err := a.execute(ctx, prefix, executionInstructionDir, instruction.CommonInstruction, true)
			if err != nil {
				logrus.Errorf("error executing instruction %d: %v", index, err)
				oneTimeApplySucceeded = false
			}
			if instruction.Name == "" && instruction.SaveOutput {
				logrus.Errorf("instruction does not have a name set, cannot save output data")
			} else if instruction.SaveOutput {
				executionOutputs[instruction.Name] = output
			}
			// If we have failed to apply our one-time instructions, we need to break in order to stop subsequent instructions from executing.
			if !oneTimeApplySucceeded {
				break
			}
		}
		var gzOutput bytes.Buffer

		gzWriter := gzip.NewWriter(&gzOutput)

		marshalledExecutionOutputs, err := json.Marshal(executionOutputs)
		if err != nil {
			return false, existingOneTimeOutput, false, existingPeriodicOutput, err
		}
		gzWriter.Write(marshalledExecutionOutputs)
		if err := gzWriter.Close(); err != nil {
			return false, existingOneTimeOutput, false, existingPeriodicOutput, err
		}
		oneTimeApplyOutput = gzOutput.Bytes()

	} else {
		// For posterity, if a one-time apply was not requested, the one-time apply success is technically false.
		oneTimeApplyOutput = existingOneTimeOutput
		oneTimeApplySucceeded = false
	}

	// ok, let's run the periodic instructions

	periodicOutputs := map[string]PeriodicInstructionOutput{}
	if len(existingPeriodicOutput) > 0 {
		buffer := bytes.NewBuffer(existingPeriodicOutput)
		gzReader, err := gzip.NewReader(buffer)
		if err != nil {
			return false, existingOneTimeOutput, false, existingPeriodicOutput, err
		}

		var objectBuffer bytes.Buffer
		_, err = io.Copy(&objectBuffer, gzReader)
		if err != nil {
			return false, existingOneTimeOutput, false, existingPeriodicOutput, err
		}

		if err := json.Unmarshal(objectBuffer.Bytes(), &periodicOutputs); err != nil {
			return false, existingOneTimeOutput, false, existingPeriodicOutput, err
		}
	}

	periodicApplySucceeded = true
	for index, instruction := range cp.Plan.PeriodicInstructions {
		if po, ok := periodicOutputs[instruction.Name]; ok {
			logrus.Debugf("[Applyinator] Got periodic output and am now parsing last run time %s", po.LastRunTime)
			t, err := time.Parse(time.UnixDate, po.LastRunTime)
			if err != nil {
				logrus.Errorf("error encountered during parsing of last run time: %v", err)
			} else {
				if now.Before(t.Add(time.Second * time.Duration(instruction.PeriodSeconds))) {
					logrus.Debugf("[Applyinator] Not running periodic instruction %s as period duration has not elapsed since last run", instruction.Name)
					continue
				}
			}
		}
		logrus.Debugf("[Applyinator] Executing periodic instruction %d for plan %s", index, cp.Checksum)
		executionInstructionDir := filepath.Join(executionDir, cp.Checksum+"_"+strconv.Itoa(index))
		prefix := cp.Checksum + "_" + strconv.Itoa(index)
		stdout, stderr, exitCode, err := a.execute(ctx, prefix, executionInstructionDir, instruction.CommonInstruction, false)
		if err != nil {
			periodicApplySucceeded = false
		}
		if instruction.Name == "" {
			logrus.Errorf("instruction does not have a name set, cannot save output data")
		} else {
			periodicOutputs[instruction.Name] = PeriodicInstructionOutput{
				Name:        instruction.Name,
				Stdout:      stdout,
				Stderr:      stderr,
				ExitCode:    exitCode,
				LastRunTime: nowUnixTimeString,
			}
		}
		if !periodicApplySucceeded {
			break
		}
	}

	var gzOutput bytes.Buffer

	gzWriter := gzip.NewWriter(&gzOutput)

	marshalledExecutionOutputs, err := json.Marshal(periodicOutputs)
	if err != nil {
		return false, existingOneTimeOutput, false, existingPeriodicOutput, err
	}
	gzWriter.Write(marshalledExecutionOutputs)
	if err := gzWriter.Close(); err != nil {
		return false, existingOneTimeOutput, false, existingPeriodicOutput, err
	}
	periodicApplyOutput = gzOutput.Bytes()
	return
}

func (a *Applyinator) execute(ctx context.Context, prefix, executionDir string, instruction CommonInstruction, combinedOutput bool) ([]byte, []byte, int, error) {
	if instruction.Image == "" {
		logrus.Infof("[Applyinator] No image provided, creating empty working directory %s", executionDir)
		if err := createDirectory(File{Directory: true, Path: executionDir}); err != nil {
			logrus.Errorf("error while creating empty working directory: %v", err)
			return nil, nil, -1, err
		}
	} else {
		logrus.Infof("[Applyinator] Extracting image %s to directory %s", instruction.Image, executionDir)
		if err := a.imageUtil.Stage(executionDir, instruction.Image); err != nil {
			logrus.Errorf("error while staging: %v", err)
			return nil, nil, -1, err
		}
	}

	command := instruction.Command

	if command == "" {
		logrus.Infof("[Applyinator] Command was not specified, defaulting to %s%s", executionDir, defaultCommand)
		command = executionDir + defaultCommand
	}

	cmd := exec.CommandContext(ctx, command, instruction.Args...)
	logrus.Infof("[Applyinator] Running command: %s %v", instruction.Command, instruction.Args)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, instruction.Env...)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", cattleAgentExecutionPwdEnvKey, executionDir))
	cmd.Env = append(cmd.Env, "PATH="+os.Getenv("PATH")+":"+executionDir)
	cmd.Dir = executionDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logrus.Errorf("error setting up stdout pipe: %v", err)
		return nil, nil, -1, err
	}
	defer stdout.Close()

	stderr, err := cmd.StderrPipe()
	if err != nil {
		logrus.Errorf("error setting up stderr pipe: %v", err)
		return nil, nil, -1, err
	}
	defer stderr.Close()

	var (
		eg              = errgroup.Group{}
		stdoutWriteLock sync.Mutex
		stderrWriteLock sync.Mutex
		stdoutBuffer    bytes.Buffer
		stderrBuffer    bytes.Buffer
	)

	if combinedOutput {
		stderrBuffer = stdoutBuffer
		stderrWriteLock = stdoutWriteLock
	}

	eg.Go(func() error {
		return streamLogs("["+prefix+":stdout]", &stdoutBuffer, stdout, &stdoutWriteLock)
	})
	eg.Go(func() error {
		return streamLogs("["+prefix+":stderr]", &stderrBuffer, stderr, &stderrWriteLock)
	})

	if err := cmd.Start(); err != nil {
		return nil, nil, -1, err
	}

	// Wait for I/O to complete before calling cmd.Wait() because cmd.Wait() will close the I/O pipes.
	_ = eg.Wait()
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	logrus.Infof("[Applyinator] Command %s %v finished with err: %v and exit code: %d", instruction.Command, instruction.Args, err, exitCode)
	return stdoutBuffer.Bytes(), stderrBuffer.Bytes(), exitCode, err
}

// streamLogs accepts a prefix, outputBuffer, reader, and buffer lock and will scan input from the reader and write it
// to the output buffer while also logging anything that comes from the reader with the prefix.
func streamLogs(prefix string, outputBuffer *bytes.Buffer, reader io.Reader, lock *sync.Mutex) error {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		logrus.Infof("%s: %s", prefix, scanner.Text())
		lock.Lock()
		outputBuffer.Write(append(scanner.Bytes(), []byte("\n")...))
		lock.Unlock()
	}
	return nil
}
