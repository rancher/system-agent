//go:build e2e

package remoteplan_test

import (
	"context"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/k8splan"
	"github.com/rancher/system-agent/test/framework"
)

var _ = Describe("Plan States", Label(framework.ShortTestLabel), func() {
	It("should transition pending -> in-progress -> succeeded on successful apply", func() {
		ctx := context.Background()

		By("Creating a plan secret with plan-state:pending")
		plan := framework.NewPlan().
			WithInstruction("echo-ok", "/bin/sh",
				[]string{"-c", "echo 'success'"}, true).
			Build()

		err := framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey: []byte(planapi.PlanStatePending),
			})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for plan-state to become in-progress")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool {
				s := planapi.PlanState(val)
				return s == planapi.PlanStateInProgress || s == planapi.PlanStateSucceeded
			},
			framework.WaitTimeout, time.Second)

		By("Waiting for plan-state to become succeeded")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateSucceeded },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying plan-revision is incremented")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		revision, err := strconv.Atoi(string(data[planapi.PlanRevisionKey]))
		Expect(err).NotTo(HaveOccurred())
		Expect(revision).To(BeNumerically(">=", 1))
	})

	It("should transition pending -> in-progress -> failed on failing apply", func() {
		ctx := context.Background()

		By("Creating a plan secret with plan-state:pending and a failing instruction")
		plan := framework.NewPlan().
			WithInstruction("will-fail", "/bin/sh",
				[]string{"-c", "exit 1"}, true).
			Build()

		err := framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey: []byte(planapi.PlanStatePending),
			})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for plan-state to become failed")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateFailed },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying plan-revision is incremented")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		revision, err := strconv.Atoi(string(data[planapi.PlanRevisionKey]))
		Expect(err).NotTo(HaveOccurred())
		Expect(revision).To(BeNumerically(">=", 1))
	})

	It("should not re-apply when plan-state is succeeded and plan content unchanged", func() {
		ctx := context.Background()

		By("Creating a plan secret with plan-state:succeeded (simulating already-applied)")
		plan := framework.NewPlan().
			WithInstruction("echo-already-done", "/bin/sh",
				[]string{"-c", "echo 'done'"}, true).
			Build()

		err := framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey:       []byte(planapi.PlanStateSucceeded),
				k8splan.AppliedChecksumKey: []byte("some-checksum"),
			})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting a full probe cycle to confirm the state is not disturbed")
		time.Sleep(15 * time.Second)

		By("Verifying plan-state remains succeeded")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(planapi.PlanState(data[planapi.PlanStateKey])).To(Equal(planapi.PlanStateSucceeded))
	})

	It("should re-apply after orchestrator resets plan-state to pending", func() {
		ctx := context.Background()

		By("Creating a plan secret with plan-state:pending")
		planA := framework.NewPlan().
			WithFile("/tmp/e2e-plan-state-a.txt", "first apply", "0644").
			Build()

		err := framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, planA,
			map[string][]byte{
				planapi.PlanStateKey: []byte(planapi.PlanStatePending),
			})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for plan-state:succeeded")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateSucceeded },
			framework.WaitTimeout, 2*time.Second)

		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		revisionAfterFirst, err := strconv.Atoi(string(data[planapi.PlanRevisionKey]))
		Expect(err).NotTo(HaveOccurred())

		By("Simulating orchestrator delivering new plan content with plan-state:pending")
		planB := framework.NewPlan().
			WithFile("/tmp/e2e-plan-state-b.txt", "second apply", "0644").
			Build()

		secret, err := framework.GetSecret(ctx, cl, framework.E2ENamespace, framework.PlanSecretName)
		Expect(err).NotTo(HaveOccurred())
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data[k8splan.PlanKey] = planB
		secret.Data[planapi.PlanStateKey] = []byte(planapi.PlanStatePending)

		err = framework.UpdateSecretData(ctx, cl, framework.E2ENamespace, framework.PlanSecretName, secret.Data)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for plan-state:succeeded on the new plan")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateSucceeded },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying plan-revision incremented again")
		data = framework.GetSecretData(ctx, cl, framework.E2ENamespace, framework.PlanSecretName)
		revisionAfterSecond, err := strconv.Atoi(string(data[planapi.PlanRevisionKey]))
		Expect(err).NotTo(HaveOccurred())
		Expect(revisionAfterSecond).To(BeNumerically(">", revisionAfterFirst))
	})
})
