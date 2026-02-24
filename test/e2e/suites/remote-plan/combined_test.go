//go:build e2e

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

var _ = Describe("Remote Plan - Files + Instructions Combined", Label(framework.ShortTestLabel), func() {
	It("should write files before executing instructions (script execution)", func() {
		ctx := context.Background()

		By("Creating a plan that writes a script file and then executes it")
		scriptContent := "#!/bin/sh\necho script-executed-successfully"
		plan := framework.NewPlan().
			WithFile("/tmp/e2e-script.sh", scriptContent, "0755").
			WithInstruction("run-script", "/tmp/e2e-script.sh", []string{}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-output")
		appliedOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-output", 120*time.Second, 2*time.Second)

		By("Verifying the script output is captured")
		decoded, err := framework.DecodeOutput(appliedOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(ContainSubstring("script-executed-successfully"))
	})

	It("should handle a complete plan lifecycle with files, instructions, and periodic instructions", func() {
		ctx := context.Background()

		By("Creating a plan with files, a one-time instruction, and a periodic instruction")
		plan := framework.NewPlan().
			WithFile("/tmp/e2e-lifecycle.txt", "lifecycle content", "0644").
			WithInstruction("lifecycle-cmd", "/bin/sh", []string{"-c", "echo lifecycle-instruction"}, true).
			WithPeriodicInstruction("lifecycle-periodic", "/bin/sh", []string{"-c", "echo periodic-running"}, 5).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying applied-checksum")
		expectedChecksum := fmt.Sprintf("%x", sha256.Sum256(plan))
		appliedChecksum := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(expectedChecksum))

		By("Verifying the file was written")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		stdout, _, err := framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"cat", "/tmp/e2e-lifecycle.txt"})
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(Equal("lifecycle content"))

		By("Verifying instruction output")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(data["applied-output"]).ToNot(BeEmpty())
		decoded, err := framework.DecodeOutput(data["applied-output"])
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(ContainSubstring("lifecycle-instruction"))

		By("Verifying periodic output is eventually populated")
		periodicOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-periodic-output", 120*time.Second, 5*time.Second)
		Expect(periodicOutput).ToNot(BeEmpty())

		By("Verifying periodic output contains expected content")
		periodicOutputMap, err := framework.DecodePeriodicOutput(periodicOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(periodicOutputMap).To(HaveKey("lifecycle-periodic"))
		Expect(periodicOutputMap["lifecycle-periodic"].ExitCode).To(Equal(0),
			"Periodic instruction should have succeeded")
	})
})
