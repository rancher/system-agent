//go:build e2e

package remoteplan_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rancher/system-agent/test/framework"
)

var _ = Describe("Remote Plan - Instruction Execution", Label(framework.ShortTestLabel), func() {
	It("should pass environment variables to instructions", func() {
		ctx := context.Background()

		By("Creating a plan with an instruction that uses a custom env var")
		plan := framework.NewPlan().
			WithInstructionEnv("env-test", "/bin/sh",
				[]string{"-c", "echo $MY_TEST_VAR"},
				[]string{"MY_TEST_VAR=hello-from-env"}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-output")
		appliedOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-output", 120*time.Second, 2*time.Second)

		By("Verifying the environment variable was available")
		decoded, err := framework.DecodeOutput(appliedOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(ContainSubstring("hello-from-env"))
	})

	It("should execute multiple sequential instructions in order", func() {
		ctx := context.Background()

		By("Creating a plan with two instructions")
		plan := framework.NewPlan().
			WithInstruction("step-1", "/bin/sh",
				[]string{"-c", "echo 'first'"}, true).
			WithInstruction("step-2", "/bin/sh",
				[]string{"-c", "echo 'second'"}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-output")
		appliedOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-output", 120*time.Second, 2*time.Second)

		By("Verifying all instruction outputs are captured")
		decoded, err := framework.DecodeOutput(appliedOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(ContainSubstring("first"))
		Expect(decoded).To(ContainSubstring("second"))
	})

	It("should stop executing instructions after a failure", func() {
		ctx := context.Background()

		By("Creating a plan where the first instruction fails")
		plan := framework.NewPlan().
			WithInstruction("fail-cmd", "/bin/sh",
				[]string{"-c", "exit 1"}, true).
			WithInstruction("should-not-run", "/bin/sh",
				[]string{"-c", "echo 'should-not-see-this'"}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for failed-checksum to appear")
		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"failed-checksum", 120*time.Second, 2*time.Second)

		By("Verifying the second instruction did NOT run")
		data := framework.GetSecretData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)

		failedOutput := data["failed-output"]
		Expect(failedOutput).ToNot(BeEmpty())
		outputMap, err := framework.GetOutputMap(failedOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(outputMap).ToNot(BeNil())

		_, hasSecondInstruction := outputMap["should-not-run"]
		Expect(hasSecondInstruction).To(BeFalse(),
			"Second instruction should not have run after first instruction failed")
	})

	It("should not save output when saveOutput is false", func() {
		ctx := context.Background()

		By("Creating a plan with one instruction with saveOutput false and one with true")
		plan := framework.NewPlan().
			WithInstruction("no-save", "/bin/sh",
				[]string{"-c", "echo 'not saved'"}, false).
			WithInstruction("with-save", "/bin/sh",
				[]string{"-c", "echo 'saved'"}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-output")
		appliedOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-output", 120*time.Second, 2*time.Second)

		By("Verifying the output map only contains the instruction with saveOutput=true")
		outputMap, err := framework.GetOutputMap(appliedOutput)
		Expect(err).NotTo(HaveOccurred())

		_, hasNoSave := outputMap["no-save"]
		Expect(hasNoSave).To(BeFalse(),
			"Instruction with saveOutput=false should not appear in output map")

		_, hasSave := outputMap["with-save"]
		Expect(hasSave).To(BeTrue(),
			"Instruction with saveOutput=true should appear in output map")
	})

	It("should inject CATTLE_AGENT_EXECUTION_PWD environment variable", func() {
		ctx := context.Background()

		By("Creating a plan that echoes CATTLE_AGENT_EXECUTION_PWD")
		plan := framework.NewPlan().
			WithInstruction("pwd-test", "/bin/sh",
				[]string{"-c", "echo $CATTLE_AGENT_EXECUTION_PWD"}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-output")
		appliedOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-output", 120*time.Second, 2*time.Second)

		By("Verifying the execution directory path is in the output")
		decoded, err := framework.DecodeOutput(appliedOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(ContainSubstring("/var/lib/rancher/agent/work/"),
			"CATTLE_AGENT_EXECUTION_PWD should contain the work directory path")
	})

	It("should inject CATTLE_AGENT_ATTEMPT_NUMBER environment variable", func() {
		ctx := context.Background()

		By("Creating a plan that echoes CATTLE_AGENT_ATTEMPT_NUMBER")
		plan := framework.NewPlan().
			WithInstruction("attempt-test", "/bin/sh",
				[]string{"-c", "echo $CATTLE_AGENT_ATTEMPT_NUMBER"}, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-output")
		appliedOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-output", 120*time.Second, 2*time.Second)

		By("Verifying the attempt number is 1 on first attempt")
		decoded, err := framework.DecodeOutput(appliedOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(ContainSubstring("1"),
			"CATTLE_AGENT_ATTEMPT_NUMBER should be 1 on first attempt")
	})
})
