//go:build e2e

package remoteplan_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rancher/system-agent/test/framework"
)

var _ = Describe("Remote Plan - Failure Handling", Label(framework.ShortTestLabel), func() {
	It("should populate failure keys on instruction failure", func() {
		ctx := context.Background()

		By("Creating a plan with a failing instruction")
		plan := framework.NewPlan().
			WithInstruction("will-fail", "/bin/sh",
				[]string{"-c", "exit 1"}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for failed-checksum")
		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"failed-checksum", 120*time.Second, 2*time.Second)

		By("Verifying failure keys are populated")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)

		failureCount, err := strconv.Atoi(string(data["failure-count"]))
		Expect(err).NotTo(HaveOccurred())
		Expect(failureCount).To(BeNumerically(">=", 1))

		Expect(data).To(HaveKey("failed-output"))
		Expect(data["failed-output"]).ToNot(BeEmpty())

		By("Verifying failed-output content")
		failedDecoded, err := framework.DecodeOutput(data["failed-output"])
		Expect(err).NotTo(HaveOccurred())
		Expect(failedDecoded).To(ContainSubstring("will-fail"),
			"Failed output should reference the failing instruction")
	})

	It("should increment failure count on retry", func() {
		ctx := context.Background()

		By("Creating a plan with a permanently failing instruction")
		plan := framework.NewPlan().
			WithInstruction("retry-fail", "/bin/sh",
				[]string{"-c", "exit 1"}, true).
			Build()

		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for failure count to reach at least 2 (requires cooldown cycle)")
		framework.WaitForSecretFieldIntAtLeast(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"failure-count", 2, 180*time.Second, 5*time.Second)
	})

	It("should stop retrying after max-failures threshold is reached", func() {
		ctx := context.Background()

		By("Creating a plan with a failing instruction and max-failures=2")
		plan := framework.NewPlan().
			WithInstruction("max-fail", "/bin/sh",
				[]string{"-c", "exit 1"}, true).
			Build()

		err := framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				"max-failures": []byte("2"),
			})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for failure count to reach 2")
		framework.WaitForSecretFieldIntAtLeast(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"failure-count", 2, 180*time.Second, 5*time.Second)

		By("Recording the failure count snapshot")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		snapshotCount, err := strconv.Atoi(string(data["failure-count"]))
		Expect(err).NotTo(HaveOccurred())

		By("Waiting and verifying failure count does not increase beyond threshold")
		time.Sleep(45 * time.Second)

		data = framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		currentCount, err := strconv.Atoi(string(data["failure-count"]))
		Expect(err).NotTo(HaveOccurred())
		Expect(currentCount).To(Equal(snapshotCount),
			"Failure count should not increase after reaching max-failures threshold")
	})

	It("should reset failure state when a successful plan replaces a failing one", func() {
		ctx := context.Background()

		By("Step 1: Applying a failing plan")
		failingPlan := framework.NewPlan().
			WithInstruction("initial-fail", "/bin/sh",
				[]string{"-c", "exit 1"}, true).
			Build()

		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, failingPlan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for a failure to be recorded")
		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"failed-checksum", 120*time.Second, 2*time.Second)

		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		failureCount, _ := strconv.Atoi(string(data["failure-count"]))
		Expect(failureCount).To(BeNumerically(">=", 1))

		By("Step 2: Updating to a passing plan")
		passingPlan := framework.NewPlan().
			WithInstruction("now-pass", "/bin/sh",
				[]string{"-c", "echo 'success'"}, true).
			Build()

		err = framework.UpdatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, passingPlan)
		Expect(err).NotTo(HaveOccurred())

		passingChecksum := fmt.Sprintf("%x", sha256.Sum256(passingPlan))

		By("Waiting for applied-checksum to match the passing plan")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum",
			func(val []byte) bool { return string(val) == passingChecksum },
			120*time.Second, 2*time.Second)

		By("Verifying failure keys are cleared/reset")
		data = framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(string(data["failure-count"])).To(Equal("0"),
			"Failure count should be reset to 0")
		Expect(string(data["failed-checksum"])).To(BeEmpty(),
			"Failed checksum should be cleared")
	})

	It("should track success count on successful application", func() {
		ctx := context.Background()

		By("Creating a plan with a successful instruction")
		plan := framework.NewPlan().
			WithInstruction("success-track", "/bin/sh",
				[]string{"-c", "echo 'ok'"}, true).
			Build()

		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-checksum")
		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)

		By("Verifying success-count is at least 1")
		successCount := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"success-count", 120*time.Second, 2*time.Second)
		count, err := strconv.Atoi(string(successCount))
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeNumerically(">=", 1))
	})
})
