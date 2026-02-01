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
	"github.com/rancher/wrangler/v3/pkg/signals"
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
			{
				Name:   "check",
				Usage:  "validate agent configuration and environment",
				Action: check,
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

func check(_ *cli.Context) error {
	logrus.Infof("Rancher System Agent version %s - Configuration Check", version.FriendlyVersion())

	configFile := os.Getenv(cattleAgentConfigEnv)
	if configFile == "" {
		configFile = defaultConfigFile
	}

	var hasErrors bool

	// Check 1: Config file exists
	logrus.Infof("Checking configuration file: %s", configFile)
	if _, err := os.Stat(configFile); err != nil {
		if os.IsNotExist(err) {
			logrus.Errorf("✗ Configuration file not found: %s", configFile)
			hasErrors = true
		} else {
			logrus.Errorf("✗ Error accessing configuration file: %v", err)
			hasErrors = true
		}
	} else {
		logrus.Infof("✓ Configuration file exists")
	}

	// Check 2: Parse config file
	var cf config.AgentConfig
	if err := config.Parse(configFile, &cf); err != nil {
		logrus.Errorf("✗ Failed to parse configuration file: %v", err)
		hasErrors = true
		// If we can't parse config, we can't continue with other checks
		if hasErrors {
			return fmt.Errorf("configuration validation failed")
		}
	}
	logrus.Infof("✓ Configuration file is valid")

	// Check 3: Verify local or remote is enabled
	if !cf.LocalEnabled && !cf.RemoteEnabled {
		logrus.Errorf("✗ Neither local nor remote watching is enabled")
		hasErrors = true
	} else {
		if cf.LocalEnabled {
			logrus.Infof("✓ Local plan watching is enabled")
		}
		if cf.RemoteEnabled {
			logrus.Infof("✓ Remote plan watching is enabled")
		}
	}

	// Check 4: Verify work directory
	if cf.WorkDir == "" {
		logrus.Warnf("⚠ Work directory not specified, will use default")
	} else {
		if _, err := os.Stat(cf.WorkDir); err != nil {
			if os.IsNotExist(err) {
				logrus.Warnf("⚠ Work directory does not exist: %s (will be created on startup)", cf.WorkDir)
			} else {
				logrus.Errorf("✗ Error accessing work directory: %v", err)
				hasErrors = true
			}
		} else {
			logrus.Infof("✓ Work directory exists: %s", cf.WorkDir)
		}
	}

	// Check 5: Verify applied plan directory
	if cf.AppliedPlanDir != "" {
		if _, err := os.Stat(cf.AppliedPlanDir); err != nil {
			if os.IsNotExist(err) {
				logrus.Warnf("⚠ Applied plan directory does not exist: %s (will be created on startup)", cf.AppliedPlanDir)
			} else {
				logrus.Errorf("✗ Error accessing applied plan directory: %v", err)
				hasErrors = true
			}
		} else {
			logrus.Infof("✓ Applied plan directory exists: %s", cf.AppliedPlanDir)
		}
	}

	// Check 6: If remote enabled, check connection info file
	if cf.RemoteEnabled {
		logrus.Infof("Checking remote configuration...")
		if cf.ConnectionInfoFile == "" {
			logrus.Errorf("✗ Remote watching enabled but connection info file not specified")
			hasErrors = true
		} else {
			logrus.Infof("Checking connection info file: %s", cf.ConnectionInfoFile)
			if _, err := os.Stat(cf.ConnectionInfoFile); err != nil {
				if os.IsNotExist(err) {
					logrus.Errorf("✗ Connection info file not found: %s", cf.ConnectionInfoFile)
					logrus.Errorf("  This file should be created by the system-agent install script")
					hasErrors = true
				} else {
					logrus.Errorf("✗ Error accessing connection info file: %v", err)
					hasErrors = true
				}
			} else {
				logrus.Infof("✓ Connection info file exists")

				// Try to parse the connection info file
				var connInfo config.ConnectionInfo
				if err := config.Parse(cf.ConnectionInfoFile, &connInfo); err != nil {
					logrus.Errorf("✗ Failed to parse connection info file: %v", err)
					logrus.Errorf("  The file may contain invalid JSON from a failed installation")
					hasErrors = true
				} else {
					logrus.Infof("✓ Connection info file is valid JSON")

					// Verify required fields
					if connInfo.KubeConfig == "" {
						logrus.Errorf("✗ Connection info missing kubeConfig field")
						hasErrors = true
					} else {
						logrus.Infof("✓ Connection info has kubeConfig")
					}

					if connInfo.Namespace == "" {
						logrus.Warnf("⚠ Connection info missing namespace field")
					} else {
						logrus.Infof("✓ Connection info has namespace: %s", connInfo.Namespace)
					}

					if connInfo.SecretName == "" {
						logrus.Warnf("⚠ Connection info missing secretName field")
					} else {
						logrus.Infof("✓ Connection info has secretName: %s", connInfo.SecretName)
					}
				}
			}
		}
	}

	// Check 7: If local enabled, check local plan directory
	if cf.LocalEnabled {
		logrus.Infof("Checking local configuration...")
		if cf.LocalPlanDir == "" {
			logrus.Errorf("✗ Local watching enabled but local plan directory not specified")
			hasErrors = true
		} else {
			if _, err := os.Stat(cf.LocalPlanDir); err != nil {
				if os.IsNotExist(err) {
					logrus.Warnf("⚠ Local plan directory does not exist: %s (will be created on startup)", cf.LocalPlanDir)
				} else {
					logrus.Errorf("✗ Error accessing local plan directory: %v", err)
					hasErrors = true
				}
			} else {
				logrus.Infof("✓ Local plan directory exists: %s", cf.LocalPlanDir)
			}
		}
	}

	// Summary
	logrus.Infof("")
	logrus.Infof("Configuration check complete")
	if hasErrors {
		logrus.Errorf("❌ Configuration validation failed - please fix the errors above")
		return fmt.Errorf("configuration validation failed")
	}

	logrus.Infof("✅ All configuration checks passed")
	return nil
}
