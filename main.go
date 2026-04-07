package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-colorable"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	"github.com/rancher/system-agent/pkg/image"
	"github.com/rancher/system-agent/pkg/k8splan"
	"github.com/rancher/system-agent/pkg/localplan"
	"github.com/rancher/system-agent/pkg/version"
	"github.com/rancher/wrangler/v3/pkg/signals"
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
			{
				Name:      "validate-config",
				Usage:     "validate agent configuration",
				Action:    validateConfig,
				ArgsUsage: "<config-file>",
			},
			{
				Name:      "validate-connection",
				Usage:     "validate Rancher connection information",
				Action:    validateConnection,
				ArgsUsage: "<connection-info-file>",
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatalf("Validation failed: %v", err)
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
			if os.IsNotExist(err) {
				return fmt.Errorf("connection info file not found at %s: this file should be created by the system-agent install script during registration with the Rancher server. Please verify the agent was installed correctly", cf.ConnectionInfoFile)
			}
			return fmt.Errorf("unable to parse connection info file %s: %w. The file may contain invalid JSON from a failed installation. Please verify it was written correctly during agent installation", cf.ConnectionInfoFile, err)
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

func validateConfig(c *cli.Context) error {
	logrus.Infof("Rancher System Agent version %s - Configuration Validation", version.FriendlyVersion())

	// Get config file from positional argument or use default
	configFile := c.Args().First()
	if configFile == "" {
		return fmt.Errorf("configuration file not specified. Please provide a configuration file as an argument or set the %s environment variable to point to the configuration file", cattleAgentConfigEnv)
	}

	return validateConfigurationFile(configFile)
}

func validateConfigurationFile(configFile string) error {
	logrus.Infof("Validating configuration file: %s", configFile)

	// Check config file exists
	if _, err := os.Stat(configFile); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("configuration file not found: %s", configFile)
		}
		return fmt.Errorf("error accessing configuration file: %w", err)
	}
	logrus.Infof("Configuration file exists")

	// Parse config file
	var cf config.AgentConfig
	if err := config.Parse(configFile, &cf); err != nil {
		return fmt.Errorf("invalid configuration file: %w", err)
	}
	logrus.Infof("Configuration file parsed successfully")

	// Verify local or remote is enabled
	if !cf.LocalEnabled && !cf.RemoteEnabled {
		return fmt.Errorf("neither local nor remote watching is enabled")
	}

	if cf.LocalEnabled {
		logrus.Infof("Local plan watching is enabled")
	}
	if cf.RemoteEnabled {
		logrus.Infof("Remote plan watching is enabled")
		if cf.ConnectionInfoFile == "" {
			return fmt.Errorf("remote watching enabled but connection info file not specified")
		}
	}

	// Validate local configuration if enabled
	if cf.LocalEnabled {
		if err := validateLocalConfig(cf); err != nil {
			return err
		}
	}

	logrus.Infof("Configuration validation successful")
	return nil
}

func validateConnection(c *cli.Context) error {
	logrus.Infof("Rancher System Agent version %s - Connection Info Validation", version.FriendlyVersion())

	connectionInfoFile := c.Args().First()
	if connectionInfoFile == "" {
		return fmt.Errorf("connection info file not specified")
	}

	if err := validateConnectionInfoFile(connectionInfoFile); err != nil {
		return err
	}

	logrus.Infof("Connection info validation successful")
	return nil
}

func validateConnectionInfoFile(connectionInfoFile string) error {
	logrus.Infof("Validating remote configuration")

	logrus.Infof("Checking connection info file: %s", connectionInfoFile)

	if _, err := os.Stat(connectionInfoFile); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("connection info file not found: %s. This file should be created by the system-agent install script", connectionInfoFile)
		}
		return fmt.Errorf("error accessing connection info file: %w", err)
	}
	logrus.Infof("Connection info file exists")

	// Parse the connection info file
	var connInfo config.ConnectionInfo
	if err := config.Parse(connectionInfoFile, &connInfo); err != nil {
		return fmt.Errorf("invalid connection info file: %w", err)
	}
	logrus.Infof("Connection info file is valid JSON")

	// Verify required fields
	if connInfo.KubeConfig == "" {
		return fmt.Errorf("connection info missing required kubeConfig field")
	}
	logrus.Infof("Connection info has required kubeConfig field")

	if connInfo.Namespace == "" {
		return fmt.Errorf("connection info missing required namespace field")
	} else {
		logrus.Infof("Connection info has namespace: %s", connInfo.Namespace)
	}

	if connInfo.SecretName == "" {
		return fmt.Errorf("connection info missing required secretName field")
	} else {
		logrus.Infof("Connection info has secretName: %s", connInfo.SecretName)
	}

	return nil
}

func validateLocalConfig(cf config.AgentConfig) error {
	logrus.Infof("Validating local configuration")

	if cf.LocalPlanDir == "" {
		return fmt.Errorf("local watching enabled but local plan directory not specified")
	}

	if _, err := os.Stat(cf.LocalPlanDir); err != nil {
		if os.IsNotExist(err) {
			logrus.Warnf("Local plan directory does not exist: %s (will be created on startup)", cf.LocalPlanDir)
		} else {
			return fmt.Errorf("error accessing local plan directory: %w", err)
		}
	} else {
		logrus.Infof("Local plan directory exists: %s", cf.LocalPlanDir)
	}

	return nil
}
