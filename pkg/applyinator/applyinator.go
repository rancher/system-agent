package applyinator

import (
	"bufio"
	"context"
	"fmt"
	"github.com/oats87/rancher-agent/pkg/image"
	"github.com/oats87/rancher-agent/pkg/types"
	"github.com/sirupsen/logrus"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
)

type Applyinator struct {
	mu               *sync.Mutex
	workingDirectory string
}

func NewApplyinator(workingDirectory string) *Applyinator {
	return &Applyinator{
		mu:               &sync.Mutex{},
		workingDirectory: workingDirectory,
	}
}

func (a *Applyinator) Apply(ctx context.Context, plan types.NodePlan) error {
	logrus.Debugf("Attempting to get lock")
	a.mu.Lock()
	logrus.Debugf("Lock achieved")
	defer a.mu.Unlock()
	logrus.Debugf("Applying plan %v", plan)

	for _, file := range plan.Files {
		path := filepath.Join(file.Path, file.Name)
		logrus.Debugf("Writing file %s to %s", file.Name, file.Path)
		if err := writeFile(path, file.Content); err != nil {
			return err
		}
	}

	checksum := plan.Checksum()
	for index, instruction := range plan.Instructions {
		directory := filepath.Join(a.workingDirectory, checksum+"_"+strconv.Itoa(index))
		if err := a.execute(ctx, directory, instruction); err != nil {
			return fmt.Errorf("error executing instruction: %v", err)
		}
	}
	return nil
}

func (a *Applyinator) execute(ctx context.Context, directory string, instruction types.Instruction) error {

	logrus.Infof("Extracting image %s to directory %s", instruction.Image, directory)
	err := image.Stage(directory, instruction.Image)
	if err != nil {
		logrus.Errorf("error while staging: %v", err)
	}
	cmd := exec.CommandContext(ctx, instruction.Command, instruction.Args...)
	logrus.Infof("Running command: %s %v", instruction.Command, instruction.Args)
	cmd.Env = append(os.Environ(), instruction.Env...)
	cmd.Env = append(cmd.Env, "PATH="+os.Getenv("PATH")+":"+directory)

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

	return nil
}

func streamLogs(prefix string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		logrus.Infof("%s: %s", prefix, scanner.Text())
	}

}
