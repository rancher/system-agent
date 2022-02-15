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
	"sort"
	"strconv"
	"strings"
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
	PeriodSeconds int `json:"periodSeconds,omitempty"` // default 600, i.e. 10 minutes
}

type PeriodicInstructionOutput struct {
	Name                  string `json:"name"`
	Stdout                []byte `json:"stdout"`                // Stdout is a byte array of the gzip+base64 stdout output
	Stderr                []byte `json:"stderr"`                // Stderr is a byte array of the gzip+base64 stderr output
	ExitCode              int    `json:"exitCode"`              // ExitCode is an int representing the exit code of the last run instruction
	LastSuccessfulRunTime string `json:"lastSuccessfulRunTime"` // LastSuccessfulRunTime is a time.UnixDate formatted string of the last successful time (exit code 0) the instruction was run
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
const planRetentionPolicyCount = 64

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

type ApplyOutput struct {
	OneTimeOutput          []byte
	OneTimeApplySucceeded  bool
	PeriodicOutput         []byte
	PeriodicApplySucceeded bool
}

type ApplyInput struct {
	CalculatedPlan         CalculatedPlan
	RunOneTimeInstructions bool
	ReconcileFiles         bool
	ExistingOneTimeOutput  []byte
	ExistingPeriodicOutput []byte
}

// Apply accepts a context, calculated plan, a bool to indicate whether to run the onetime instructions, the existing onetimeinstruction output, and an input byte slice which is a base64+gzip json-marshalled map of PeriodicInstructionOutput
// entries where the key is the PeriodicInstructionOutput.Name. It outputs a revised versions of the existing outputs, and if specified, runs the one time instructions. Notably, ApplyOutput.OneTimeApplySucceeded will be false if ApplyInput.RunOneTimeInstructions is false
func (a *Applyinator) Apply(ctx context.Context, input ApplyInput) (ApplyOutput, error) {
	logrus.Infof("[Applyinator] Applying plan with checksum %s", input.CalculatedPlan.Checksum)
	logrus.Tracef("[Applyinator] Applying plan - attempting to get lock")
	output := ApplyOutput{
		OneTimeOutput:  input.ExistingOneTimeOutput,
		PeriodicOutput: input.ExistingPeriodicOutput,
	}
	a.mu.Lock()
	logrus.Tracef("[Applyinator] Applying plan - lock achieved")
	defer a.mu.Unlock()
	now := time.Now()
	nowUnixTimeString := now.Format(time.UnixDate)
	nowString := now.Format(applyinatorDateCodeLayout)
	appliedPlanFile := nowString + appliedPlanFileSuffix
	executionDir := filepath.Join(a.workDir, nowString)
	logrus.Tracef("[Applyinator] Applying calculated node plan contents %v", input.CalculatedPlan.Checksum)
	logrus.Tracef("[Applyinator] Using %s as execution directory", executionDir)
	if a.appliedPlanDir != "" {
		logrus.Debugf("[Applyinator] Writing applied calculated plan contents to historical plan directory %s", a.appliedPlanDir)
		if err := os.MkdirAll(a.appliedPlanDir, 0700); err != nil {
			return output, err
		}
		anpString, err := json.Marshal(input.CalculatedPlan)
		if err != nil {
			return output, err
		}
		if err := writeContentToFile(filepath.Join(a.appliedPlanDir, appliedPlanFile), os.Getuid(), os.Getgid(), 0600, anpString); err != nil {
			return output, err
		}
		if err := a.appliedPlanRetentionPolicy(planRetentionPolicyCount); err != nil {
			logrus.Errorf("error while applying plan retention policy: %v", err)
		}
	}

	if input.ReconcileFiles {
		for _, file := range input.CalculatedPlan.Plan.Files {
			if file.Directory {
				logrus.Debugf("[Applyinator] Creating directory %s", file.Path)
				if err := createDirectory(file); err != nil {
					return output, err
				}
			} else {
				logrus.Debugf("[Applyinator] Writing file %s", file.Path)
				if err := writeBase64ContentToFile(file); err != nil {
					return output, err
				}
			}
		}
	}

	if !a.preserveWorkDir {
		logrus.Debugf("[Applyinator] Cleaning working directory before applying %s", a.workDir)
		if err := os.RemoveAll(a.workDir); err != nil {
			return output, err
		}
	}
	if input.RunOneTimeInstructions {
		executionOutputs := map[string][]byte{}
		if len(input.ExistingOneTimeOutput) > 0 {
			objectBuffer, err := generateByteBufferFromBytes(input.ExistingOneTimeOutput)
			if err != nil {
				return output, err
			}
			if err := json.Unmarshal(objectBuffer.Bytes(), &executionOutputs); err != nil {
				return output, err
			}
		}

		oneTimeApplySucceeded := true
		for index, instruction := range input.CalculatedPlan.Plan.OneTimeInstructions {
			logrus.Debugf("[Applyinator] Executing instruction %d for plan %s", index, input.CalculatedPlan.Checksum)
			executionInstructionDir := filepath.Join(executionDir, input.CalculatedPlan.Checksum+"_"+strconv.Itoa(index))
			prefix := input.CalculatedPlan.Checksum + "_" + strconv.Itoa(index)
			executeOutput, _, _, err := a.execute(ctx, prefix, executionInstructionDir, instruction.CommonInstruction, true)
			if err != nil {
				logrus.Errorf("error executing instruction %d: %v", index, err)
				oneTimeApplySucceeded = false
			}
			if instruction.Name == "" && instruction.SaveOutput {
				logrus.Errorf("instruction does not have a name set, cannot save output data")
			} else if instruction.SaveOutput {
				executionOutputs[instruction.Name] = executeOutput
			}
			// If we have failed to apply our one-time instructions, we need to break in order to stop subsequent instructions from executing.
			if !oneTimeApplySucceeded {
				break
			}
		}

		output.OneTimeApplySucceeded = oneTimeApplySucceeded

		marshalledExecutionOutputs, err := json.Marshal(executionOutputs)
		if err != nil {
			return output, err
		}

		oneTimeApplyOutput, err := gzipByteSlice(marshalledExecutionOutputs)
		if err != nil {
			return output, err
		}

		output.OneTimeOutput = oneTimeApplyOutput
	}

	periodicOutputs := map[string]PeriodicInstructionOutput{}
	if len(input.ExistingPeriodicOutput) > 0 {
		objectBuffer, err := generateByteBufferFromBytes(input.ExistingPeriodicOutput)
		if err != nil {
			return output, err
		}
		if err := json.Unmarshal(objectBuffer.Bytes(), &periodicOutputs); err != nil {
			return output, err
		}
	}

	periodicApplySucceeded := true
	for index, instruction := range input.CalculatedPlan.Plan.PeriodicInstructions {
		var previousRunTime string
		if po, ok := periodicOutputs[instruction.Name]; ok {
			logrus.Debugf("[Applyinator] Got periodic output and am now parsing last run time %s", po.LastSuccessfulRunTime)
			t, err := time.Parse(time.UnixDate, po.LastSuccessfulRunTime)
			if err != nil {
				logrus.Errorf("error encountered during parsing of last run time: %v", err)
			} else {
				previousRunTime = po.LastSuccessfulRunTime
				if instruction.PeriodSeconds == 0 {
					instruction.PeriodSeconds = 600 // set default period to 600 seconds
				}
				if now.Before(t.Add(time.Second * time.Duration(instruction.PeriodSeconds))) {
					logrus.Debugf("[Applyinator] Not running periodic instruction %s as period duration has not elapsed since last run", instruction.Name)
					continue
				}
			}
		}
		logrus.Debugf("[Applyinator] Executing periodic instruction %d for plan %s", index, input.CalculatedPlan.Checksum)
		executionInstructionDir := filepath.Join(executionDir, input.CalculatedPlan.Checksum+"_"+strconv.Itoa(index))
		prefix := input.CalculatedPlan.Checksum + "_" + strconv.Itoa(index)
		stdout, stderr, exitCode, err := a.execute(ctx, prefix, executionInstructionDir, instruction.CommonInstruction, false)
		if err != nil {
			periodicApplySucceeded = false
		}
		if instruction.Name == "" {
			logrus.Errorf("instruction does not have a name set, cannot save output data")
		} else {
			lrt := nowUnixTimeString
			if exitCode != 0 {
				lrt = previousRunTime
			}
			periodicOutputs[instruction.Name] = PeriodicInstructionOutput{
				Name:                  instruction.Name,
				Stdout:                stdout,
				Stderr:                stderr,
				ExitCode:              exitCode,
				LastSuccessfulRunTime: lrt,
			}
		}
		if !periodicApplySucceeded {
			break
		}
	}

	output.PeriodicApplySucceeded = periodicApplySucceeded

	marshalledExecutionOutputs, err := json.Marshal(periodicOutputs)
	if err != nil {
		return output, err
	}
	periodicApplyOutput, err := gzipByteSlice(marshalledExecutionOutputs)
	if err != nil {
		return output, err
	}

	output.PeriodicOutput = periodicApplyOutput
	return output, nil
}

func gzipByteSlice(input []byte) ([]byte, error) {
	var gzOutput bytes.Buffer

	gzWriter := gzip.NewWriter(&gzOutput)

	gzWriter.Write(input)
	if err := gzWriter.Close(); err != nil {
		return []byte{}, err
	}
	return gzOutput.Bytes(), nil
}

func generateByteBufferFromBytes(input []byte) (*bytes.Buffer, error) {
	buffer := bytes.NewBuffer(input)
	gzReader, err := gzip.NewReader(buffer)
	if err != nil {
		return nil, err
	}

	var objectBuffer bytes.Buffer
	_, err = io.Copy(&objectBuffer, gzReader)
	if err != nil {
		return nil, err
	}
	return &objectBuffer, nil
}

func (a *Applyinator) appliedPlanRetentionPolicy(retention int) error {
	var planFiles []os.DirEntry
	dirListedPlanFiles, err := os.ReadDir(a.appliedPlanDir)
	if err != nil {
		return err
	}

	for _, f := range dirListedPlanFiles {
		if strings.HasSuffix(f.Name(), appliedPlanFileSuffix) && !f.IsDir() {
			planFiles = append(planFiles, f)
		}
	}

	if len(planFiles) <= retention {
		return nil
	}

	sort.Slice(planFiles, func(i, j int) bool {
		return planFiles[i].Name() < planFiles[j].Name()
	})

	delCount := len(planFiles) - retention
	for _, df := range planFiles[:delCount] {
		historicalPlanFile := filepath.Join(a.appliedPlanDir, df.Name())
		logrus.Infof("[Applyinator] Removing historical applied plan (retention policy count: %d) %s", retention, historicalPlanFile)
		if err := os.Remove(historicalPlanFile); err != nil {
			return err
		}
	}
	return nil
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
		stdoutWriteLock *sync.Mutex
		stderrWriteLock *sync.Mutex
		stdoutBuffer    bytes.Buffer
		stderrBuffer    bytes.Buffer
	)

	if combinedOutput {
		stderrBuffer = stdoutBuffer
		stdoutWriteLock = &sync.Mutex{}
		stderrWriteLock = stdoutWriteLock
	} else {
		stdoutWriteLock = &sync.Mutex{}
		stderrWriteLock = &sync.Mutex{}
	}

	eg.Go(func() error {
		return streamLogs("["+prefix+":stdout]", &stdoutBuffer, stdout, stdoutWriteLock)
	})
	eg.Go(func() error {
		return streamLogs("["+prefix+":stderr]", &stderrBuffer, stderr, stderrWriteLock)
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
