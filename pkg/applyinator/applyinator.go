package applyinator

import (
	"github.com/oats87/rancher-agent/pkg/image"
	"github.com/oats87/rancher-agent/pkg/types"
	"github.com/oats87/rancher-agent/pkg/util"
	"github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

type Applyinator struct {
	wd string
}

func NewApplyinator(workingDirectory string) *Applyinator {
	Applyinator := &Applyinator{
		wd: workingDirectory,
	}
	return Applyinator
}

func (a *Applyinator) Apply(plan types.NodePlan) error {
	logrus.Debugf("Applying plan %v", plan)

	checksum := util.ComputeChecksum(plan)
	for index, instruction := range plan.Instructions {
		directory := filepath.Join(a.wd, checksum+"_"+strconv.Itoa(index))
		logrus.Debugf("Extracting image %s to directory %s", instruction.Image, directory)
		err := image.Stage(directory, instruction.Image)
		if err != nil {
			logrus.Errorf("Error while staging: %v", err)
		}
		cmd := exec.Command(instruction.Command, instruction.Args...)
		logrus.Debugf("Running command: %s %v", instruction.Command, instruction.Args)
		cmd.Env = append(os.Environ(), instruction.Env...)
		cmd.Env = append(cmd.Env, "PATH="+os.Getenv("PATH")+":"+directory)
		output, err := cmd.CombinedOutput()
		if err != nil {
			logrus.Errorf("Error running command: %v", err)
			return err
		}
		logrus.Debugf("Output of command was %s", output)

	}
	return nil
}