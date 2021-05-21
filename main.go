package main

import (
	"context"
	"os"

	"github.com/mattn/go-colorable"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	"github.com/rancher/system-agent/pkg/image"
	"github.com/rancher/system-agent/pkg/k8splan"
	"github.com/rancher/system-agent/pkg/localplan"
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

	rawLevel := os.Getenv(cattleLogLevelEnv)

	if rawLevel != "" {
		if lvl, err := logrus.ParseLevel(os.Getenv(cattleLogLevelEnv)); err != nil {
			logrus.Fatal(err)
		} else {
			logrus.SetLevel(lvl)
		}
	}

	err := run()

	if err != nil {
		logrus.Fatal(err)
	}

}

func run() error {
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

	logrus.Infof("Using directory %s for work", cf.WorkDir)

	imageUtil := image.NewUtility(cf.ImagesDir, cf.ImageCredentialProviderConfig, cf.ImageCredentialProviderBinDir, cf.AgentRegistriesFile)

	applyinator := applyinator.NewApplyinator(cf.WorkDir, cf.PreserveWorkDir, cf.AppliedPlanDir, imageUtil)

	if cf.RemoteEnabled {
		logrus.Infof("Starting remote watch of plans")

		var connInfo config.ConnectionInfo

		if err := config.Parse(cf.ConnectionInfoFile, &connInfo); err != nil {
			logrus.Fatalf("Unable to parse connection info file %v", err)
		}

		if err := k8splan.Watch(topContext, *applyinator, connInfo); err != nil {
			return err
		}
	}

	logrus.Infof("Starting local watch of plans in %s", cf.LocalPlanDir)

	if err := localplan.WatchFiles(topContext, *applyinator, cf.LocalPlanDir); err != nil {
		return err
	}

	<-topContext.Done()
	return nil
}
