package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v2"
)

func TestValidate(t *testing.T) {
	// Create a temporary directory for test files
	tmpDir, err := os.MkdirTemp("", "system-agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tests := []struct {
		name          string
		setupFunc     func() (string, error)
		expectError   bool
		errorContains string
	}{
		{
			name: "valid config with remote enabled",
			setupFunc: func() (string, error) {
				configFile := filepath.Join(tmpDir, "valid-remote-config.yaml")
				configContent := `workDirectory: /tmp/test-work
remoteEnabled: true
localEnabled: false
connectionInfoFile: ` + filepath.Join(tmpDir, "connection-info.json") + `
`
				if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
					return "", err
				}

				// Create connection info file
				connInfoFile := filepath.Join(tmpDir, "connection-info.json")
				connInfoContent := `{"kubeConfig": "test-config", "namespace": "default", "secretName": "test-secret"}`
				if err := os.WriteFile(connInfoFile, []byte(connInfoContent), 0o600); err != nil {
					return "", err
				}

				return configFile, nil
			},
			expectError: false,
		},
		{
			name: "valid config with local enabled",
			setupFunc: func() (string, error) {
				configFile := filepath.Join(tmpDir, "valid-local-config.yaml")
				localPlanDir := filepath.Join(tmpDir, "plans")
				if err := os.MkdirAll(localPlanDir, 0o755); err != nil {
					return "", err
				}

				configContent := `workDirectory: /tmp/test-work
remoteEnabled: false
localEnabled: true
localPlanDirectory: ` + localPlanDir + `
`
				if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
					return "", err
				}

				return configFile, nil
			},
			expectError: false,
		},
		{
			name: "config file not found",
			setupFunc: func() (string, error) {
				return filepath.Join(tmpDir, "nonexistent.yaml"), nil
			},
			expectError:   true,
			errorContains: "configuration file not found",
		},
		{
			name: "invalid config - neither local nor remote enabled",
			setupFunc: func() (string, error) {
				configFile := filepath.Join(tmpDir, "invalid-config.yaml")
				configContent := `workDirectory: /tmp/test-work
remoteEnabled: false
localEnabled: false
`
				if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
					return "", err
				}

				return configFile, nil
			},
			expectError:   true,
			errorContains: "neither local nor remote watching is enabled",
		},
		{
			name: "remote enabled but connection info file missing",
			setupFunc: func() (string, error) {
				configFile := filepath.Join(tmpDir, "missing-conn-info.yaml")
				configContent := `workDirectory: /tmp/test-work
remoteEnabled: true
localEnabled: false
connectionInfoFile: ` + filepath.Join(tmpDir, "missing.json") + `
`
				if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
					return "", err
				}

				return configFile, nil
			},
			expectError:   true,
			errorContains: "connection info file not found",
		},
		{
			name: "connection info file with invalid JSON",
			setupFunc: func() (string, error) {
				configFile := filepath.Join(tmpDir, "invalid-json-config.yaml")
				configContent := `workDirectory: /tmp/test-work
remoteEnabled: true
localEnabled: false
connectionInfoFile: ` + filepath.Join(tmpDir, "invalid.json") + `
`
				if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
					return "", err
				}

				// Create invalid connection info file
				connInfoFile := filepath.Join(tmpDir, "invalid.json")
				connInfoContent := `Internal error occurred: failed calling webhook`
				if err := os.WriteFile(connInfoFile, []byte(connInfoContent), 0o600); err != nil {
					return "", err
				}

				return configFile, nil
			},
			expectError:   true,
			errorContains: "failed to parse connection info file",
		},
		{
			name: "connection info missing required kubeConfig field",
			setupFunc: func() (string, error) {
				configFile := filepath.Join(tmpDir, "missing-kubeconfig.yaml")
				configContent := `workDirectory: /tmp/test-work
remoteEnabled: true
localEnabled: false
connectionInfoFile: ` + filepath.Join(tmpDir, "no-kubeconfig.json") + `
`
				if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
					return "", err
				}

				// Create connection info file without kubeConfig
				connInfoFile := filepath.Join(tmpDir, "no-kubeconfig.json")
				connInfoContent := `{"namespace": "default", "secretName": "test-secret"}`
				if err := os.WriteFile(connInfoFile, []byte(connInfoContent), 0o600); err != nil {
					return "", err
				}

				return configFile, nil
			},
			expectError:   true,
			errorContains: "missing required kubeConfig field",
		},
		{
			name: "local enabled but local plan directory not specified",
			setupFunc: func() (string, error) {
				configFile := filepath.Join(tmpDir, "no-local-dir.yaml")
				configContent := `workDirectory: /tmp/test-work
remoteEnabled: false
localEnabled: true
`
				if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
					return "", err
				}

				return configFile, nil
			},
			expectError:   true,
			errorContains: "local plan directory not specified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configFile, err := tt.setupFunc()
			if err != nil {
				t.Fatalf("Setup failed: %v", err)
			}

			// Create a CLI context with the config file as an argument
			app := &cli.App{
				Commands: []*cli.Command{
					{
						Name:   "validate",
						Action: validate,
					},
				},
			}

			args := []string{"test", "validate", configFile}
			err = app.Run(args)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("Expected error containing %q, got: %v", tt.errorContains, err)
				}
			} else if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}
