package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"sigs.k8s.io/yaml"
)

type AgentConfig struct {
	WorkDir                       string `json:"workDirectory,omitempty"`
	LocalEnabled                  bool   `json:"localEnabled,omitempty"`
	LocalPlanDir                  string `json:"localPlanDirectory,omitempty"`
	AppliedPlanDir                string `json:"appliedPlanDirectory,omitempty"`
	RemoteEnabled                 bool   `json:"remoteEnabled,omitempty"`
	ConnectionInfoFile            string `json:"connectionInfoFile,omitempty"`
	PreserveWorkDir               bool   `json:"preserveWorkDirectory,omitempty"`
	ImagesDir                     string `json:"imagesDirectory,omitempty"`
	AgentRegistriesFile           string `json:"agentRegistriesFile,omitempty"`
	ImageCredentialProviderConfig string `json:"imageCredentialProviderConfig,omitempty"`
	ImageCredentialProviderBinDir string `json:"imageCredentialProviderBinDirectory,omitempty"`
}

type ConnectionInfo struct {
	KubeConfig string `json:"kubeConfig"`
	Namespace  string `json:"namespace"`
	SecretName string `json:"secretName"`
}

func Parse(path string, result interface{}) error {
	if path == "" {
		return fmt.Errorf("empty file passed")
	}

	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("error gathering file information for file %s: %w", path, err)
	}

	if fi.Mode().Perm() != 0600 {
		return fmt.Errorf("file %s had permission %#o which was not expected 0600", path, fi.Mode().Perm())
	}

	if fi.Sys().(*syscall.Stat_t).Uid != 0 || fi.Sys().(*syscall.Stat_t).Gid != 0 {
		return fmt.Errorf("file %s had was not owned by root:root", path)
	}

	f, err := os.Open(path)

	if err != nil {
		return err
	}

	defer f.Close()

	file := filepath.Base(path)
	switch {
	case strings.Contains(file, ".json"):
		return json.NewDecoder(f).Decode(result)
	case strings.Contains(file, ".yaml"):
		b, err := ioutil.ReadAll(f)
		if err != nil {
			return err
		}
		return yaml.Unmarshal(b, result)
	default:
		return fmt.Errorf("file %s was not a JSON or YAML file", file)
	}
}
