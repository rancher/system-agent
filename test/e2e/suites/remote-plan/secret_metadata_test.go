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

var _ = Describe("Remote Plan - Secret Metadata", Label(framework.ShortTestLabel), func() {
	It("should set last-apply-time after plan application", func() {
		ctx := context.Background()

		By("Creating a plan with a simple instruction")
		plan := framework.NewPlan().
			WithInstruction("time-test", "/bin/sh",
				[]string{"-c", "echo 'time-check'"}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for last-apply-time")
		lastApplyTime := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"last-apply-time", 120*time.Second, 2*time.Second)
		Expect(lastApplyTime).ToNot(BeEmpty())

		By("Verifying last-apply-time is parseable as a timestamp")
		_, err = time.Parse(time.UnixDate, string(lastApplyTime))
		Expect(err).NotTo(HaveOccurred(),
			"last-apply-time should be in time.UnixDate format")
	})

	It("should accept custom probe-period-seconds in the secret", func() {
		ctx := context.Background()

		By("Creating a plan with a custom probe-period-seconds")
		plan := framework.NewPlan().
			WithInstruction("period-test", "/bin/sh",
				[]string{"-c", "echo 'period check'"}, true).
			Build()

		err := framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				"probe-period-seconds": []byte("3"),
			})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-checksum")
		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)

		By("Verifying the plan was applied successfully with the custom period")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(data).To(HaveKey("applied-checksum"))
		expectedChecksum := fmt.Sprintf("%x", sha256.Sum256(plan))
		Expect(string(data["applied-checksum"])).To(Equal(expectedChecksum),
			"applied-checksum should match the SHA256 of the plan")
		Expect(data).To(HaveKey("last-apply-time"),
			"last-apply-time should be set, confirming agent processed the plan")
	})
})
