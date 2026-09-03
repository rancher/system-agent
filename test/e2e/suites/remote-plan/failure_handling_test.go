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

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/k8splan"
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
			k8splan.FailedChecksumKey, framework.WaitTimeout, 2*time.Second)

		By("Verifying failure keys are populated")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)

		failureCount, err := strconv.Atoi(string(data[k8splan.FailureCountKey]))
		Expect(err).NotTo(HaveOccurred())
		Expect(failureCount).To(BeNumerically(">=", 1))

		Expect(data).To(HaveKey(k8splan.FailedOutputKey))
		Expect(data[k8splan.FailedOutputKey]).ToNot(BeEmpty())

		By("Verifying failed-output content")
		failedDecoded, err := framework.DecodeOutput(data[k8splan.FailedOutputKey])
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
			k8splan.FailureCountKey, 2, 180*time.Second, 5*time.Second)
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
				k8splan.MaxFailuresKey: []byte("2"),
			})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for failure count to reach 2")
		framework.WaitForSecretFieldIntAtLeast(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			k8splan.FailureCountKey, 2, 180*time.Second, 5*time.Second)

		By("Recording the failure count snapshot")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		snapshotCount, err := strconv.Atoi(string(data[k8splan.FailureCountKey]))
		Expect(err).NotTo(HaveOccurred())

		By("Waiting and verifying failure count does not increase beyond threshold")
		time.Sleep(45 * time.Second)

		data = framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		currentCount, err := strconv.Atoi(string(data[k8splan.FailureCountKey]))
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
			k8splan.FailedChecksumKey, framework.WaitTimeout, 2*time.Second)

		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		failureCount, _ := strconv.Atoi(string(data[k8splan.FailureCountKey]))
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
			k8splan.AppliedChecksumKey,
			func(val []byte) bool { return string(val) == passingChecksum },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying failure keys are cleared/reset")
		data = framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(string(data[k8splan.FailureCountKey])).To(Equal("0"),
			"Failure count should be reset to 0")
		Expect(string(data[k8splan.FailedChecksumKey])).To(BeEmpty(),
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
			k8splan.AppliedChecksumKey, framework.WaitTimeout, 2*time.Second)

		By("Verifying success-count is at least 1")
		successCount := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			k8splan.SuccessCountKey, framework.WaitTimeout, 2*time.Second)
		count, err := strconv.Atoi(string(successCount))
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeNumerically(">=", 1))
	})

	It("should stay monitoring-only after cancel annotation is removed from a failed terminal plan", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)
		const (
			periodicMarker = "/tmp/e2e-failed-terminal-periodic-ran.txt"
			failedReport   = `{"checksum":"failed-report","completedInstructions":0,"totalInstructions":1}`
		)

		By("Creating a terminal failed plan with a periodic instruction and a cancellation hold")
		plan := framework.NewPlan().
			WithPeriodicInstruction("should-not-run-after-failed", "/bin/sh",
				[]string{"-c", "touch " + periodicMarker}, 5).
			Build()

		Expect(framework.CreatePlanSecretWithAnnotations(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey:      []byte(planapi.PlanStateFailed),
				planapi.PlanCheckpointKey: []byte(failedReport),
				k8splan.FailedChecksumKey: []byte("failed-checksum"),
				k8splan.FailureCountKey:   []byte("3"),
				k8splan.ProbeStatusesKey:  []byte("{}"),
			},
			map[string]string{planapi.PlanCanceledAnnotation: "true"})).To(Succeed())

		By("Removing the cancel annotation")
		Expect(framework.RemoveSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanCanceledAnnotation)).To(Succeed())
		rvAfterRemove := framework.GetSecretResourceVersion(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)

		By("Verifying terminal failed plans do not resume execution")
		Consistently(func() bool { return nodeFileExists(ctx, podName, periodicMarker) },
			20*time.Second, 4*time.Second).Should(BeFalse(),
			"removing cancellation from a failed terminal plan must keep reconciliation in monitoring-only mode")

		By("Verifying lifecycle keys remain unchanged")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(planapi.PlanState(data[planapi.PlanStateKey])).To(Equal(planapi.PlanStateFailed))
		Expect(string(data[k8splan.FailedChecksumKey])).To(Equal("failed-checksum"))
		Expect(string(data[k8splan.FailureCountKey])).To(Equal("3"))
		Expect(string(data[k8splan.AppliedChecksumKey])).To(BeEmpty())

		progress := framework.GetPlanProgress(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(progress).NotTo(BeNil())
		Expect(progress["checksum"]).To(Equal("failed-report"))
		Expect(progress["completedInstructions"]).To(BeEquivalentTo(0))
		Expect(progress["totalInstructions"]).To(BeEquivalentTo(1))

		By("Verifying monitoring-only reconciles do not rewrite secret data in steady state")
		Consistently(func() string {
			return framework.GetSecretResourceVersion(ctx, cl,
				framework.E2ENamespace, framework.PlanSecretName)
		}, 12*time.Second, 3*time.Second).Should(Equal(rvAfterRemove))
	})
})
