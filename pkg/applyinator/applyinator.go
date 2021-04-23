package applyinator

import (
	"bufio"
	"context"
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
	"github.com/rancher/system-agent/pkg/types"
	"github.com/sirupsen/logrus"
)

type Applyinator struct {
	mu              *sync.Mutex
	workDir         string
	dockerConfig    string
	preserveWorkDir bool
	appliedPlanDir  string
}

const appliedPlanFileSuffix = "-applied.plan"
const applyinatorDateCodeLayout = "20060102-150405"
const defaultCommand = "/run.sh"

func NewApplyinator(workDir string, preserveWorkDir bool, appliedPlanDir string, dockerConfig string) *Applyinator {
	return &Applyinator{
		mu:              &sync.Mutex{},
		workDir:         workDir,
		dockerConfig:    dockerConfig,
		preserveWorkDir: preserveWorkDir,
		appliedPlanDir:  appliedPlanDir,
	}
}

func (a *Applyinator) Apply(ctx context.Context, anp types.AgentNodePlan) error {
	logrus.Infof("Applying plan with checksum %s", anp.Checksum)
	logrus.Tracef("Applying plan - attempting to get lock")
	a.mu.Lock()
	logrus.Tracef("Applying plan - lock achieved")
	defer a.mu.Unlock()
	now := time.Now().Format(applyinatorDateCodeLayout)
	executionDir := filepath.Join(a.workDir, now+appliedPlanFileSuffix)
	logrus.Tracef("Applying plan contents %v", anp)
	logrus.Tracef("Using %s as execution directory", executionDir)
	if a.appliedPlanDir != "" {
		logrus.Debugf("Writing applied plan contents to historical plan directory %s", a.appliedPlanDir)
		if err := os.MkdirAll(filepath.Dir(a.appliedPlanDir), 0755); err != nil {
			return err
		}
		anpString, err := json.Marshal(anp)
		if err != nil {
			return err
		}
		if err := writeContentToFile(filepath.Join(a.appliedPlanDir, now), anpString); err != nil {
			return err
		}
	}

	for _, file := range anp.Plan.Files {
		path := filepath.Join(file.Path, file.Name)
		logrus.Debugf("Writing file %s to %s", file.Name, file.Path)
		if err := writeBase64ContentToFile(path, file.Content); err != nil {
			return err
		}
	}

	if !a.preserveWorkDir {
		logrus.Debugf("Cleaning working directory before applying %s", a.workDir)
		if err := os.RemoveAll(a.workDir); err != nil {
			return err
		}
	}

	for index, instruction := range anp.Plan.Instructions {
		logrus.Debugf("Executing instruction %d for plan %s", index, anp.Checksum)
		executionInstructionDir := filepath.Join(executionDir, anp.Checksum+"_"+strconv.Itoa(index))
		if err := a.execute(ctx, executionInstructionDir, instruction); err != nil {
			return fmt.Errorf("error executing instruction %d: %v", index, err)
		}
	}
	return nil
}

func (a *Applyinator) execute(ctx context.Context, executionDir string, instruction types.Instruction) error {

	logrus.Infof("Extracting image %s to directory %s", instruction.Image, executionDir)
	err := image.Stage(executionDir, instruction.Image, []byte(a.dockerConfig))
	if err != nil {
		logrus.Errorf("error while staging: %v", err)
		return err
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
	cmd.Env = append(cmd.Env, fmt.Sprintf("CATTLE_AGENT_EXECUTION_PWD=%s", executionDir))
	cmd.Env = append(cmd.Env, "PATH="+os.Getenv("PATH")+":"+executionDir)
	cmd.Dir = executionDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logrus.Errorf("Error running command: %v", err)
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		logrus.Errorf("Error running command: %v", err)
		return err
	}

	go streamLogs("[stdout]", stdout)
	go streamLogs("[stderr]", stderr)

	if err := cmd.Run(); err != nil {
		return err
	}

	logrus.Infof("Done running command: %s %v", command, instruction.Args)

	return nil
}

func streamLogs(prefix string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		logrus.Infof("%s: %s", prefix, scanner.Text())
	}

}
