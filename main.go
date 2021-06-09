package main

import (
	"context"
	"os"

	"github.com/rancher/system-agent/pkg/localplan"

	"github.com/mattn/go-colorable"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	"github.com/rancher/system-agent/pkg/image"
	"github.com/rancher/system-agent/pkg/k8splan"
	"github.com/rancher/system-agent/pkg/version"
	"github.com/rancher/wrangler/pkg/signals"
	"github.com/sirupsen/logrus"
)

const (
	cattleLogLevelEnv    = "CATTLE_LOGLEVEL"
	cattleAgentConfigEnv = "CATTLE_AGENT_CONFIG"
	defaultConfigFile    = "/etc/rancher/agent/config.yaml"
)

func main() {
	logrus.SetOutput(colorable.NewColorableStdout())

	if os.Getuid() != 0 {
		logrus.Fatalf("Must be run as root.")
	}

	rawLevel := os.Getenv(cattleLogLevelEnv)

	if rawLevel != "" {
		if lvl, err := logrus.ParseLevel(os.Getenv(cattleLogLevelEnv)); err != nil {
			logrus.Fatal(err)
		} else {
			logrus.SetLevel(lvl)
		}
	}

	run()
}

func run() {
	topContext := signals.SetupSignalHandler(context.Background())

	logrus.Infof("Rancher System Agent version %s is starting", version.FriendlyVersion())

	configFile := os.Getenv(cattleAgentConfigEnv)

	if configFile == "" {
		configFile = defaultConfigFile
	}

	var cf config.AgentConfig

	err := config.Parse(configFile, &cf)
	if err != nil {
		logrus.Fatalf("Unable to parse config file %v", err)
	}

	if !cf.LocalEnabled && !cf.RemoteEnabled {
		logrus.Fatalf("Local and remote were both not enabled. Exiting, as one must be enabled.")
	}

	logrus.Infof("Using directory %s for work", cf.WorkDir)

	imageUtil := image.NewUtility(cf.ImagesDir, cf.ImageCredentialProviderConfig, cf.ImageCredentialProviderBinDir, cf.AgentRegistriesFile)
	applyinator := applyinator.NewApplyinator(cf.WorkDir, cf.PreserveWorkDir, cf.AppliedPlanDir, imageUtil)

	if cf.RemoteEnabled {
		logrus.Infof("Starting remote watch of plans")

		var connInfo config.ConnectionInfo

		if err := config.Parse(cf.ConnectionInfoFile, &connInfo); err != nil {
			logrus.Fatalf("Unable to parse connection info file %v", err)
		}

		k8splan.Watch(topContext, *applyinator, connInfo)
	}

	if cf.LocalEnabled {
		logrus.Infof("Starting local watch of plans in %s", cf.LocalPlanDir)
		localplan.WatchFiles(topContext, *applyinator, cf.LocalPlanDir)
	}

	<-topContext.Done()
}
