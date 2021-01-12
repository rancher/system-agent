package main

import (
	"context"
	"github.com/oats87/rancher-agent/pkg/applyinator"
	"github.com/oats87/rancher-agent/pkg/localplan"
	"github.com/rancher/wrangler/pkg/signals"
	"os"

	"github.com/mattn/go-colorable"
	"github.com/sirupsen/logrus"
)

var (
	Version    = "v0.0.0-dev"
	GitCommit  = "HEAD"
)

func main() {
	logrus.SetOutput(colorable.NewColorableStdout())
	if os.Getenv("CATTLE_DEBUG") == "true" || os.Getenv("RANCHER_DEBUG") == "true" {
		logrus.SetLevel(logrus.DebugLevel)
	}
	var err error
	err = run()

	if err != nil {
		logrus.Fatal(err)
	}

}

func run() error {
	topContext := signals.SetupSignalHandler(context.Background())

	logrus.Infof("Rancher System Agent version %s is starting", Version)

	var cwDir string
	var planDir string
	var err error

	cwDir, err = os.Getwd()
	if err != nil {
		logrus.Fatalf("Unable to get current working directory: %v", err)
	}

	planDir = os.Getenv("AGENT_PLANDIR" )

	if planDir == "" {
		planDir = cwDir + "/plans"
	}

	var workDir string
	workDir = os.Getenv("AGENT_WORKDIR")
	if workDir == "" {
		workDir = cwDir + "/work"
	}

	applyinator := applyinator.NewApplyinator(workDir)
	logrus.Debugf("Using directory %s for plans", planDir)
	logrus.Debugf("Using directory %s for work", workDir)

	localplan.WatchFiles(topContext, *applyinator, planDir)

	<-topContext.Done()
	return nil
}
