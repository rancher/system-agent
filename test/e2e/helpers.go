//go:build e2e

// Copyright © 2025 SUSE LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// E2EConfig holds configuration for e2e tests, loaded from environment variables.
type E2EConfig struct {
	// ImageTag is the Docker image tag for the system-agent image.
	ImageTag string

	// ImageName is the full image name (e.g., "rancher/system-agent").
	ImageName string

	// ClusterName is the name for the Kind cluster.
	ClusterName string

	// UseExistingCluster skips Kind cluster creation.
	UseExistingCluster bool

	// SkipCleanup skips resource cleanup after tests.
	SkipCleanup bool

	// ArtifactsFolder is the path for test artifacts.
	ArtifactsFolder string
}

// Setup is a shared data structure for parallel test setup
// between Ginkgo SynchronizedBeforeSuite processes.
type Setup struct {
	ClusterName    string `json:"clusterName"`
	KubeconfigPath string `json:"kubeconfigPath"`
}

// LoadE2EConfig creates an E2EConfig from environment variables.
func LoadE2EConfig() *E2EConfig {
	By("Loading e2e test configuration from environment variables")

	config := &E2EConfig{
		ImageTag:           getEnvOrDefault(ImageTagVar, "e2e-test"),
		ImageName:          getEnvOrDefault(ImageNameVar, "rancher/system-agent"),
		ClusterName:        getEnvOrDefault(ClusterNameVar, "system-agent-e2e"),
		UseExistingCluster: getBoolEnv(UseExistingClusterVar),
		SkipCleanup:        getBoolEnv(SkipResourceCleanupVar),
		ArtifactsFolder:    getEnvOrDefault(ArtifactsFolderVar, "_artifacts"),
	}

	Expect(config.ImageTag).ToNot(BeEmpty(), "image tag is required")
	Expect(config.ImageName).ToNot(BeEmpty(), "image name is required")
	Expect(config.ClusterName).ToNot(BeEmpty(), "cluster name is required")

	return config
}

// InitScheme returns a runtime.Scheme with the standard Kubernetes types registered.
func InitScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	return scheme
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// getBoolEnv returns true only when the environment variable is explicitly set to "true".
// Defaults to false for unset or any other value.
func getBoolEnv(key string) bool {
	return strings.EqualFold(os.Getenv(key), "true")
}
