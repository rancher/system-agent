//go:build e2e

package remoteplan_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rancher/system-agent/test/framework"
)

var _ = Describe("Remote Plan - Periodic Instructions", Label(framework.ShortTestLabel), func() {
	It("should capture periodic instruction output", func() {
		ctx := context.Background()

		By("Creating a plan with a periodic instruction that echoes output")
		plan := framework.NewPlan().
			WithPeriodicInstruction("periodic-capture", "/bin/sh",
				[]string{"-c", "echo 'captured-output'"}, 5).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-periodic-output")
		periodicOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-periodic-output", 120*time.Second, 5*time.Second)

		By("Decoding and verifying the periodic output structure")
		outputMap, err := framework.DecodePeriodicOutput(periodicOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(outputMap).To(HaveKey("periodic-capture"))

		pio := outputMap["periodic-capture"]
		Expect(pio.ExitCode).To(Equal(0),
			"Periodic instruction should have succeeded")
		Expect(pio.LastSuccessfulRunTime).ToNot(BeEmpty(),
			"Last successful run time should be set")
		_, err = time.Parse(time.UnixDate, pio.LastSuccessfulRunTime)
		Expect(err).NotTo(HaveOccurred(),
			"Last successful run time should be in time.UnixDate format")
	})

	It("should track failures in periodic instruction output", func() {
		ctx := context.Background()

		By("Creating a plan with a failing periodic instruction")
		plan := framework.NewPlan().
			WithPeriodicInstruction("periodic-fail", "/bin/sh",
				[]string{"-c", "exit 1"}, 5).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-periodic-output")
		periodicOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-periodic-output", 120*time.Second, 5*time.Second)

		By("Verifying the periodic output shows failure tracking")
		outputMap, err := framework.DecodePeriodicOutput(periodicOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(outputMap).To(HaveKey("periodic-fail"))

		pio := outputMap["periodic-fail"]
		Expect(pio.ExitCode).ToNot(Equal(0),
			"Periodic instruction exit code should be non-zero")
		Expect(pio.Failures).To(BeNumerically(">=", 1),
			"Failure count should be at least 1")
		Expect(pio.LastFailedRunTime).ToNot(BeEmpty(),
			"Last failed run time should be set")
		_, err = time.Parse(time.UnixDate, pio.LastFailedRunTime)
		Expect(err).NotTo(HaveOccurred(),
			"Last failed run time should be in time.UnixDate format")
	})

	It("should capture stderr when saveStderrOutput is true", func() {
		ctx := context.Background()

		By("Creating a plan with a periodic instruction that writes to stderr")
		plan := framework.NewPlan().
			WithPeriodicInstructionSaveStderr("periodic-stderr", "/bin/sh",
				[]string{"-c", "echo 'stderr-data' >&2"}, 5, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-periodic-output")
		periodicOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-periodic-output", 120*time.Second, 5*time.Second)

		By("Verifying stderr was captured")
		outputMap, err := framework.DecodePeriodicOutput(periodicOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(outputMap).To(HaveKey("periodic-stderr"))

		pio := outputMap["periodic-stderr"]
		Expect(pio.Stderr).ToNot(BeEmpty(),
			"Stderr should be captured when saveStderrOutput is true")
		Expect(string(pio.Stderr)).To(ContainSubstring("stderr-data"),
			"Stderr should contain the expected output")
	})
})
