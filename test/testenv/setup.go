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

package testenv

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	capiframework "sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"

	"github.com/rancher/system-agent/test/e2e"
	"github.com/rancher/system-agent/test/framework"
)

// SetupTestClusterInput represents the input parameters for setting up the test cluster.
type SetupTestClusterInput struct {
	// E2EConfig holds the e2e test configuration.
	E2EConfig *e2e.E2EConfig

	// Scheme is the runtime scheme for the cluster proxy.
	Scheme *runtime.Scheme
}

// SetupTestClusterResult contains the results of test cluster setup.
type SetupTestClusterResult struct {
	// BootstrapClusterProvider manages provisioning of the bootstrap cluster.
	BootstrapClusterProvider bootstrap.ClusterProvider

	// BootstrapClusterProxy allows interaction with the bootstrap cluster.
	BootstrapClusterProxy capiframework.ClusterProxy

	// ClusterName is the name of the Kind cluster.
	ClusterName string

	// KubeconfigPath is the path to the kubeconfig file.
	KubeconfigPath string

	// ImageRef is the full image reference (name:tag).
	ImageRef string
}

// SetupTestCluster creates the complete test environment: creates a Kind cluster using
// cluster-api bootstrap with the pre-built image loaded, and deploys manifests.
// The system-agent image must be pre-built (e.g. via `make e2e-image`).
func SetupTestCluster(ctx context.Context, input SetupTestClusterInput) *SetupTestClusterResult {
	Expect(ctx).NotTo(BeNil(), "ctx is required for SetupTestCluster")
	Expect(input.E2EConfig).ToNot(BeNil(), "E2EConfig is required for SetupTestCluster")
	Expect(input.Scheme).ToNot(BeNil(), "Scheme is required for SetupTestCluster")

	config := input.E2EConfig
	imageRef := fmt.Sprintf("%s:%s", config.ImageName, config.ImageTag)
	result := &SetupTestClusterResult{
		ClusterName: config.ClusterName,
		ImageRef:    imageRef,
	}

	// Step 1: Create Kind cluster and load the pre-built image via CAPI bootstrap.
	if !config.UseExistingCluster {
		By("Creating Kind bootstrap cluster and loading images")
		clusterProvider := bootstrap.CreateKindBootstrapClusterAndLoadImages(ctx,
			bootstrap.CreateKindBootstrapClusterAndLoadImagesInput{
				Name:               config.ClusterName,
				RequiresDockerSock: true,
				Images: []clusterctl.ContainerImage{
					{
						Name:         imageRef,
						LoadBehavior: clusterctl.MustLoadImage,
					},
				},
			})
		Expect(clusterProvider).ToNot(BeNil(), "Failed to create bootstrap cluster")

		result.BootstrapClusterProvider = clusterProvider
		result.KubeconfigPath = clusterProvider.GetKubeconfigPath()
		Expect(result.KubeconfigPath).To(BeAnExistingFile(), "Failed to get kubeconfig for bootstrap cluster")
	}

	// Step 2: Create cluster proxy.
	By("Creating cluster proxy")
	proxy := capiframework.NewClusterProxy(config.ClusterName, result.KubeconfigPath, input.Scheme)
	Expect(proxy).ToNot(BeNil(), "Cluster proxy should not be nil")
	result.BootstrapClusterProxy = proxy

	// Step 3: Deploy namespace and RBAC from the unified setup manifest.
	By("Deploying system-agent namespace and RBAC")
	framework.KubectlApplyStdin(ctx, result.KubeconfigPath, e2e.SetupManifest)

	return result
}

// DeployRemoteAgent deploys system-agent in remote plan mode.
// It generates a kubeconfig from a ServiceAccount token, creates the connection-info
// configmap via template, and deploys the DaemonSet via template.
func DeployRemoteAgent(ctx context.Context, result *SetupTestClusterResult) {
	By("Generating ServiceAccount token for system-agent")
	kubeconfigForAgent := generateAgentKubeconfig(ctx, result.KubeconfigPath)

	// Escape the kubeconfig for JSON embedding in connection-info.
	connectionInfo := fmt.Sprintf(`{"kubeConfig": %q, "namespace": "%s", "secretName": "%s"}`,
		kubeconfigForAgent, framework.E2ENamespace, framework.PlanSecretName)

	By("Creating agent config for remote mode")
	framework.ApplyFromTemplate(ctx, framework.ApplyFromTemplateInput{
		KubeconfigPath: result.KubeconfigPath,
		Template:       e2e.AgentConfigTemplate,
		Variables: map[string]string{
			"NAMESPACE":       framework.E2ENamespace,
			"CONNECTION_INFO": connectionInfo,
		},
	})

	By("Deploying system-agent DaemonSet")
	framework.ApplyFromTemplate(ctx, framework.ApplyFromTemplateInput{
		KubeconfigPath: result.KubeconfigPath,
		Template:       e2e.AgentDaemonSetTemplate,
		Variables: map[string]string{
			"NAMESPACE":      framework.E2ENamespace,
			"IMAGE_REF":      result.ImageRef,
			"CONFIGMAP_NAME": "agent-config",
		},
	})

	By("Waiting for system-agent pods to be ready")
	framework.KubectlWaitForPodsReady(ctx, result.KubeconfigPath,
		framework.E2ENamespace, framework.AgentLabel, 120*time.Second)
}

// generateAgentKubeconfig creates a kubeconfig string from the ServiceAccount token.
// Uses the in-cluster Kubernetes service endpoint since the agent runs as a pod.
func generateAgentKubeconfig(ctx context.Context, kubeconfigPath string) string {
	// Create a token for the system-agent ServiceAccount.
	cmdResult := &framework.RunCommandResult{}
	framework.RunCommand(ctx, framework.RunCommandInput{
		Command: "kubectl",
		Args: []string{
			"--kubeconfig", kubeconfigPath,
			"create", "token", "system-agent",
			"-n", framework.E2ENamespace,
			"--duration", "24h",
		},
	}, cmdResult)
	Expect(cmdResult.Error).NotTo(HaveOccurred(), "Failed to create SA token: %s", string(cmdResult.Stderr))
	token := string(cmdResult.Stdout)

	// Get the CA data.
	cmdResult = &framework.RunCommandResult{}
	framework.RunCommand(ctx, framework.RunCommandInput{
		Command: "kubectl",
		Args: []string{
			"--kubeconfig", kubeconfigPath,
			"config", "view", "--minify", "--raw",
			"-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}",
		},
	}, cmdResult)
	Expect(cmdResult.Error).NotTo(HaveOccurred(), "Failed to get CA data")
	caData := string(cmdResult.Stdout)

	// Use the in-cluster Kubernetes service endpoint since the agent runs as a pod.
	apiServer := "https://kubernetes.default.svc.cluster.local:443"

	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: %s
    server: %s
  name: e2e
contexts:
- context:
    cluster: e2e
    user: system-agent
  name: e2e
current-context: e2e
users:
- name: system-agent
  user:
    token: %s
`, caData, apiServer, token)
}
