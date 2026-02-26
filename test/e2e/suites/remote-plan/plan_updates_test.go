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

var _ = Describe("Remote Plan - Plan Updates", Label(framework.ShortTestLabel), func() {
	It("should re-apply when the plan is updated with new content", func() {
		ctx := context.Background()

		By("Creating plan A that writes file A")
		planA := framework.NewPlan().
			WithFile("/tmp/e2e-plan-a.txt", "plan A content", "0644").
			Build()

		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, planA)
		Expect(err).NotTo(HaveOccurred())

		checksumA := fmt.Sprintf("%x", sha256.Sum256(planA))

		By("Waiting for plan A to be applied")
		appliedChecksum := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(checksumA))

		By("Updating the secret to plan B that creates file B")
		planB := framework.NewPlan().
			WithFile("/tmp/e2e-plan-b.txt", "plan B content", "0644").
			Build()

		err = framework.UpdatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, planB)
		Expect(err).NotTo(HaveOccurred())

		checksumB := fmt.Sprintf("%x", sha256.Sum256(planB))

		By("Waiting for applied-checksum to match plan B")
		appliedChecksum = framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum",
			func(val []byte) bool { return string(val) == checksumB },
			120*time.Second, 2*time.Second)
		Expect(string(appliedChecksum)).To(Equal(checksumB))

		By("Verifying file B exists in the pod")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		stdout, _, err := framework.KubectlExec(ctx, kubeconfigPath,
			framework.E2ENamespace, podName, framework.AgentContainerName,
			[]string{"cat", "/tmp/e2e-plan-b.txt"})
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(Equal("plan B content"))
	})

	It("should skip re-application when the plan checksum hasn't changed", func() {
		ctx := context.Background()

		By("Creating a plan with an instruction")
		plan := framework.NewPlan().
			WithInstruction("skip-test", "/bin/sh",
				[]string{"-c", "echo 'ran'"}, true).
			Build()

		expectedChecksum := fmt.Sprintf("%x", sha256.Sum256(plan))

		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)

		By("Waiting for success-count to be populated")
		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"success-count", 120*time.Second, 2*time.Second)

		By("Recording the current success count")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		successCount1 := string(data["success-count"])

		By("Waiting through several re-enqueue cycles")
		time.Sleep(15 * time.Second)

		By("Verifying the checksum and success count are unchanged")
		data = framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(string(data["applied-checksum"])).To(Equal(expectedChecksum),
			"Applied checksum should remain the same")
		Expect(string(data["success-count"])).To(Equal(successCount1),
			"Success count should not increment when plan is already applied")
	})
})
