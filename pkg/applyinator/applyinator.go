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

	"github.com/rancher/system-agent/pkg/prober"

	"github.com/rancher/system-agent/pkg/image"
	"github.com/sirupsen/logrus"
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
	Files        []File                  `json:"files,omitempty"`
	Instructions []Instruction           `json:"instructions,omitempty"`
	Probes       map[string]prober.Probe `json:"probes,omitempty"`
}

type Instruction struct {
	Name       string   `json:"name,omitempty"`
	SaveOutput bool     `json:"saveOutput,omitempty"`
	Image      string   `json:"image,omitempty"`
	Env        []string `json:"env,omitempty"`
	Args       []string `json:"args,omitempty"`
	Command    string   `json:"command,omitempty"`
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

func CalculatePlan(rawPlanByteData []byte) (CalculatedPlan, error) {
	var cp CalculatedPlan
	var plan Plan
	if err := json.Unmarshal(rawPlanByteData, &plan); err != nil {
		return cp, err
	}

	cp.Checksum = checksum(rawPlanByteData)
	cp.Plan = plan

	return cp, nil
}

func checksum(input []byte) string {
	h := sha256.New()
	h.Write(input)

	return fmt.Sprintf("%x", h.Sum(nil))
}

func (a *Applyinator) Apply(ctx context.Context, cp CalculatedPlan) ([]byte, error) {
	logrus.Infof("Applying plan with checksum %s", cp.Checksum)
	logrus.Tracef("Applying plan - attempting to get lock")
	a.mu.Lock()
	logrus.Tracef("Applying plan - lock achieved")
	defer a.mu.Unlock()
	now := time.Now().Format(applyinatorDateCodeLayout)
	executionDir := filepath.Join(a.workDir, now+appliedPlanFileSuffix)
	logrus.Tracef("Applying calculated node plan contents %v", cp)
	logrus.Tracef("Using %s as execution directory", executionDir)
	if a.appliedPlanDir != "" {
		logrus.Debugf("Writing applied calculated plan contents to historical plan directory %s", a.appliedPlanDir)
		if err := os.MkdirAll(filepath.Dir(a.appliedPlanDir), 0755); err != nil {
			return nil, err
		}
		anpString, err := json.Marshal(cp)
		if err != nil {
			return nil, err
		}
		if err := writeContentToFile(filepath.Join(a.appliedPlanDir, now), os.Getuid(), os.Getgid(), 0600, anpString); err != nil {
			return nil, err
		}
	}

	for _, file := range cp.Plan.Files {
		if file.Directory {
			logrus.Debugf("Creating directory %s", file.Path)
			if err := createDirectory(file); err != nil {
				return nil, err
			}
		} else {
			logrus.Debugf("Writing file %s", file.Path)
			if err := writeBase64ContentToFile(file); err != nil {
				return nil, err
			}
		}
	}

	if !a.preserveWorkDir {
		logrus.Debugf("Cleaning working directory before applying %s", a.workDir)
		if err := os.RemoveAll(a.workDir); err != nil {
			return nil, err
		}
	}

	executionOutputs := make(map[string][]byte)
	for index, instruction := range cp.Plan.Instructions {
		logrus.Debugf("Executing instruction %d for plan %s", index, cp.Checksum)
		executionInstructionDir := filepath.Join(executionDir, cp.Checksum+"_"+strconv.Itoa(index))
		output, err := a.execute(ctx, executionInstructionDir, instruction)
		if err != nil {
			return nil, fmt.Errorf("error executing instruction %d: %v", index, err)
		}
		if instruction.Name == "" && instruction.SaveOutput {
			logrus.Errorf("instruction does not have a name set, cannot save output data")
		} else {
			executionOutputs[instruction.Name] = output
		}
	}

	var gzOutput bytes.Buffer

	gzWriter := gzip.NewWriter(&gzOutput)

	marshalledExecutionOutputs, err := json.Marshal(executionOutputs)
	if err != nil {
		return nil, err
	}
	gzWriter.Write(marshalledExecutionOutputs)
	if err := gzWriter.Close(); err != nil {
		return nil, err
	}
	return gzOutput.Bytes(), nil
}

func (a *Applyinator) execute(ctx context.Context, executionDir string, instruction Instruction) ([]byte, error) {
	logrus.Infof("Extracting image %s to directory %s", instruction.Image, executionDir)
	err := a.imageUtil.Stage(executionDir, instruction.Image)
	if err != nil {
		logrus.Errorf("error while staging: %v", err)
		return nil, err
	}

	command := instruction.Command

	if command == "" {
		logrus.Debugf("Command was not specified, defaulting to %s%s", executionDir, defaultCommand)
		command = executionDir + defaultCommand
	}

	cmd := exec.CommandContext(ctx, command, instruction.Args...)
	logrus.Infof("Running command: %s %v", instruction.Command, instruction.Args)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, instruction.Env...)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", cattleAgentExecutionPwdEnvKey, executionDir))
	cmd.Env = append(cmd.Env, "PATH="+os.Getenv("PATH")+":"+executionDir)
	cmd.Dir = executionDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logrus.Errorf("error setting up stdout pipe: %v", err)
		return nil, err
	}
	defer stdout.Close()

	stderr, err := cmd.StderrPipe()
	if err != nil {
		logrus.Errorf("error setting up stderr pipe: %v", err)
		return nil, err
	}
	defer stderr.Close()

	var outputBuffer bytes.Buffer

	go streamLogs("[stdout]", &outputBuffer, stdout)
	go streamLogs("[stderr]", &outputBuffer, stderr)

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	if err := cmd.Wait(); err != nil {
		return outputBuffer.Bytes(), err
	}

	return outputBuffer.Bytes(), nil
}

func streamLogs(prefix string, outputBuffer *bytes.Buffer, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		logrus.Infof("%s: %s", prefix, scanner.Text())
		outputBuffer.Write(append(scanner.Bytes(), []byte("\n")...))
	}
}
