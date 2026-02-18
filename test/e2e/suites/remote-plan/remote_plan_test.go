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

package remoteplan_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rancher/system-agent/test/framework"
)

var _ = Describe("Remote Plan - File Operations", Label(framework.ShortTestLabel), func() {
	It("should write a single file via remote plan", func() {
		ctx := context.Background()

		By("Creating a plan that writes a single file")
		plan := framework.NewPlan().
			WithFile("/tmp/e2e-test-file.txt", "hello from e2e test", "0644").
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred(), "Failed to create plan secret")

		By("Waiting for the agent to apply the plan (applied-checksum appears)")
		expectedChecksum := fmt.Sprintf("%x", sha256.Sum256(plan))
		appliedChecksum := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 60*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(expectedChecksum),
			"Applied checksum should match the plan checksum")

		By("Verifying the file was created inside the agent pod")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		stdout, _, err := framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"cat", "/tmp/e2e-test-file.txt"})
		Expect(err).NotTo(HaveOccurred(), "Failed to read file from pod")
		Expect(stdout).To(Equal("hello from e2e test"),
			"File content should match what was specified in the plan")
	})
})

var _ = Describe("Remote Plan - Instruction Execution", Label(framework.ShortTestLabel), func() {
	It("should execute a one-time instruction and capture output", func() {
		ctx := context.Background()

		By("Creating a plan with a simple echo command")
		plan := framework.NewPlan().
			WithInstruction("echo-test", "/bin/sh",
				[]string{"-c", "echo 'instruction output'"}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-output to appear in the Secret")
		appliedOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-output", 60*time.Second, 2*time.Second)
		Expect(appliedOutput).ToNot(BeEmpty(), "applied-output should be populated")

		By("Decoding the gzip output")
		decoded, err := framework.DecodeOutput(appliedOutput)
		Expect(err).NotTo(HaveOccurred(), "Failed to decode applied-output")
		Expect(decoded).To(ContainSubstring("instruction output"),
			"Decoded output should contain the echo result")
	})
})
