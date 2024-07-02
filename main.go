package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-colorable"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	"github.com/rancher/system-agent/pkg/image"
	"github.com/rancher/system-agent/pkg/k8splan"
	"github.com/rancher/system-agent/pkg/localplan"
	"github.com/rancher/system-agent/pkg/version"
	"github.com/rancher/wrangler/pkg/signals"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

const (
	cattleLogLevelEnv          = "CATTLE_LOGLEVEL"
	cattleAgentConfigEnv       = "CATTLE_AGENT_CONFIG"
	cattleAgentStrictVerifyEnv = "CATTLE_AGENT_STRICT_VERIFY"
	defaultConfigFile          = "/etc/rancher/agent/config.yaml"
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

	app := &cli.App{
		Name:    "rancher-system-agent",
		Usage:   "Rancher System Agent runs a sentinel that reconciles desired plans with the node it is being run on",
		Version: version.FriendlyVersion(),
		Commands: []*cli.Command{
			{
				Name:   "sentinel",
				Usage:  "run the rancher-system-agent sentinel to watch plans",
				Action: run,
			},
		}}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatalf("Fatal error running: %v", err)
	}
}

func run(_ *cli.Context) error {
	topContext := signals.SetupSignalContext()

	logrus.Infof("Rancher System Agent version %s is starting", version.FriendlyVersion())

	configFile := os.Getenv(cattleAgentConfigEnv)

	if configFile == "" {
		configFile = defaultConfigFile
	}

	var cf config.AgentConfig

	err := config.Parse(configFile, &cf)
	if err != nil {
		return fmt.Errorf("unable to parse config file: %w", err)
	}

	if !cf.LocalEnabled && !cf.RemoteEnabled {
		return fmt.Errorf("local and/or remote watching must be enabled")
	}

	logrus.Infof("Using directory %s for work", cf.WorkDir)

	imageUtil := image.NewUtility(cf.ImagesDir, cf.ImageCredentialProviderConfig, cf.ImageCredentialProviderBinDir, cf.AgentRegistriesFile)
	applyinator := applyinator.NewApplyinator(cf.WorkDir, cf.PreserveWorkDir, cf.AppliedPlanDir, cf.InterlockDir, imageUtil)

	if cf.RemoteEnabled {
		logrus.Infof("Starting remote watch of plans")

		var connInfo config.ConnectionInfo

		if err := config.Parse(cf.ConnectionInfoFile, &connInfo); err != nil {
			return fmt.Errorf("unable to parse connection info file: %w", err)
		}

		var strictVerify bool // When strictVerify is set to true, the kubeconfig validator will not discard CA data if it is invalid
		if strings.ToLower(os.Getenv(cattleAgentStrictVerifyEnv)) == "true" {
			strictVerify = true
		}

		k8splan.Watch(topContext, *applyinator, connInfo, strictVerify)
	}

	if cf.LocalEnabled {
		logrus.Infof("Starting local watch of plans in %s", cf.LocalPlanDir)
		localplan.WatchFiles(topContext, *applyinator, cf.LocalPlanDir)
	}

	<-topContext.Done()
	return nil
}
