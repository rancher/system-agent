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

package framework

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/gomega"
)

// RunCommandInput represents the input parameters for running a shell command.
type RunCommandInput struct {
	// Command is the executable to run.
	Command string
	// Args are the arguments to pass to the command.
	Args []string
	// EnvironmentVariables is a map of env vars to set for the command.
	EnvironmentVariables map[string]string
}

// RunCommandResult represents the result of running a command.
type RunCommandResult struct {
	// ExitCode is the exit code of the command.
	ExitCode int
	// Stdout is the standard output of the command.
	Stdout []byte
	// Stderr is the standard error of the command.
	Stderr []byte
	// Error is the error that occurred, if any.
	Error error
}

// RunCommand executes a command and populates the result struct.
func RunCommand(ctx context.Context, input RunCommandInput, result *RunCommandResult) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for RunCommand")
	Expect(input.Command).ToNot(BeEmpty(), "Invalid argument. input.Command can't be empty")

	cmd := exec.CommandContext(ctx, input.Command, input.Args...)

	for name, val := range input.EnvironmentVariables {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", name, val))
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result.Error = err
	result.Stdout = stdout.Bytes()
	result.Stderr = stderr.Bytes()
	result.ExitCode = 0

	if exitError, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitError.ExitCode()
	}
}

// KubectlApply runs kubectl apply with the given kubeconfig and manifest path or stdin.
func KubectlApply(ctx context.Context, kubeconfigPath, manifestPath string) {
	args := []string{"apply"}
	if kubeconfigPath != "" {
		args = append(args, "--kubeconfig", kubeconfigPath)
	}
	args = append(args, "-f", manifestPath)

	result := &RunCommandResult{}
	RunCommand(ctx, RunCommandInput{
		Command: "kubectl",
		Args:    args,
	}, result)
	Expect(result.Error).NotTo(HaveOccurred(), "Failed to kubectl apply: %s", string(result.Stderr))
}

// KubectlApplyStdin runs kubectl apply with content from stdin.
func KubectlApplyStdin(ctx context.Context, kubeconfigPath string, manifest []byte) {
	args := []string{"apply"}
	if kubeconfigPath != "" {
		args = append(args, "--kubeconfig", kubeconfigPath)
	}
	args = append(args, "-f", "-")

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = bytes.NewReader(manifest)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	Expect(err).NotTo(HaveOccurred(), "Failed to kubectl apply from stdin: %s", stderr.String())
}

// KubectlWaitForPodsReady waits for pods with the given label selector to be ready.
func KubectlWaitForPodsReady(ctx context.Context, kubeconfigPath, namespace, labelSelector string, timeout time.Duration) {
	Eventually(func() bool {
		args := []string{"get", "pods",
			"-l", labelSelector,
			"-n", namespace,
			"-o", "name",
		}
		if kubeconfigPath != "" {
			args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
		}

		result := &RunCommandResult{}
		RunCommand(ctx, RunCommandInput{
			Command: "kubectl",
			Args:    args,
		}, result)

		return result.Error == nil && len(strings.TrimSpace(string(result.Stdout))) > 0
	}, timeout, 2*time.Second).Should(BeTrue(), "No pods found with label %s in namespace %s", labelSelector, namespace)

	args := []string{"wait", "--for=condition=Ready", "pod",
		"-l", labelSelector,
		"-n", namespace,
		"--timeout", fmt.Sprintf("%ds", int(timeout.Seconds())),
	}
	if kubeconfigPath != "" {
		args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
	}

	result := &RunCommandResult{}
	RunCommand(ctx, RunCommandInput{
		Command: "kubectl",
		Args:    args,
	}, result)
	Expect(result.Error).NotTo(HaveOccurred(), "Pods not ready in time: %s", string(result.Stderr))
}

// KubectlGetPodName returns the name of a pod matching the label selector.
func KubectlGetPodName(ctx context.Context, kubeconfigPath, namespace, labelSelector string) string {
	args := []string{"get", "pods",
		"-l", labelSelector,
		"-n", namespace,
		"-o", "jsonpath={.items[0].metadata.name}",
	}
	if kubeconfigPath != "" {
		args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
	}

	result := &RunCommandResult{}
	RunCommand(ctx, RunCommandInput{
		Command: "kubectl",
		Args:    args,
	}, result)
	Expect(result.Error).NotTo(HaveOccurred(), "Failed to get pod name: %s", string(result.Stderr))

	podName := strings.TrimSpace(string(result.Stdout))
	Expect(podName).ToNot(BeEmpty(), "No pod found matching label selector %s", labelSelector)
	return podName
}

// KubectlExec executes a command in a pod and returns stdout+stderr.
func KubectlExec(ctx context.Context, kubeconfigPath, namespace, podName, container string, command []string) (string, string, error) {
	args := []string{"exec", podName,
		"-n", namespace,
	}
	if kubeconfigPath != "" {
		args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
	}
	if container != "" {
		args = append(args, "-c", container)
	}
	args = append(args, "--")
	args = append(args, command...)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// KubectlGetLogs returns the logs from a pod.
func KubectlGetLogs(ctx context.Context, kubeconfigPath, namespace, podName string) string {
	args := []string{"logs", podName, "-n", namespace}
	if kubeconfigPath != "" {
		args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
	}

	result := &RunCommandResult{}
	RunCommand(ctx, RunCommandInput{
		Command: "kubectl",
		Args:    args,
	}, result)
	return string(result.Stdout)
}

// DeployHTTPTestServer deploys a simple busybox-based HTTP server pod and service
// in the given namespace. Waits for the pod to become ready.
func DeployHTTPTestServer(ctx context.Context, kubeconfigPath, namespace string, template []byte) {
	ApplyFromTemplate(ctx, ApplyFromTemplateInput{
		KubeconfigPath: kubeconfigPath,
		Template:       template,
		Variables: map[string]string{
			"NAMESPACE": namespace,
		},
	})
	KubectlWaitForPodsReady(ctx, kubeconfigPath, namespace, HTTPTestServerLabel, 60*time.Second)
}

// CleanupHTTPTestServer removes the HTTP test server pod and service.
func CleanupHTTPTestServer(ctx context.Context, kubeconfigPath, namespace string) {
	result := &RunCommandResult{}
	RunCommand(ctx, RunCommandInput{
		Command: "kubectl",
		Args:    []string{"--kubeconfig", kubeconfigPath, "delete", "pod,svc", HTTPTestServerName, "-n", namespace, "--ignore-not-found"},
	}, result)
}
