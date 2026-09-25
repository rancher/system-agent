//go:build e2e

package remoteplan_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/k8splan"
	"github.com/rancher/system-agent/test/framework"
)

// Node paths used by the cancellation specs. Each spec gets its own path because AfterEach deletes
// the plan Secret but does not clean the agent container's filesystem. Without isolated paths, a
// "this file never appeared" assertion could pass or fail based on artifacts left by an earlier spec.
const (
	cancelRunningGate    = "/tmp/e2e-cancel-running-gate"
	cancelRunningStepTwo = "/tmp/e2e-cancel-running-step-two.txt"
	cancelPendingRan     = "/tmp/e2e-cancel-pending-ran.txt"
	cancelTerminalRan    = "/tmp/e2e-cancel-terminal-periodic-ran.txt"
	cancelTreeGate       = "/tmp/e2e-cancel-tree-gate"
	cancelTreeChildLog   = "/tmp/e2e-cancel-tree-child.log"
	cancelSucceededRan   = "/tmp/e2e-cancel-succeeded-periodic-ran.txt"
)

var _ = Describe("Remote Plan - Cancellation", Label(framework.ShortTestLabel), func() {
	It("should cancel an in-flight plan and never run the instructions that follow", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)
		releaseGateOnCleanup(cancelRunningGate)

		By("Creating a plan whose first instruction blocks long enough to be caught mid-flight")
		plan := framework.NewPlan().
			WithInstruction("long-running", "/bin/sh",
				[]string{"-c", blockingScript(cancelRunningGate)}, true).
			WithInstruction("should-not-run", "/bin/sh",
				[]string{"-c", "touch " + cancelRunningStepTwo}, true).
			Build()

		Expect(framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey: []byte(planapi.PlanStatePending),
			})).To(Succeed())

		By("Waiting for plan-state to become in-progress")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateInProgress },
			framework.WaitTimeout, time.Second)

		By("Setting " + planapi.PlanCanceledAnnotation + " while the first instruction is still running")
		Expect(framework.SetSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanCanceledAnnotation, "true")).To(Succeed())

		By("Waiting for plan-state to become canceled")
		// Cancel is prompt: the in-flight instruction's context is canceled rather than being
		// allowed to finish, so this must not take anything like the instruction's own cap.
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateCanceled },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying the instruction after the canceled one never runs")
		Consistently(func() bool { return nodeFileExists(ctx, podName, cancelRunningStepTwo) },
			20*time.Second, 4*time.Second).Should(BeFalse(),
			"a canceled plan must start nothing further")

		By("Verifying plan-progress reports partial execution rather than a suspension")
		progress := framework.GetPlanProgress(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(progress).NotTo(BeNil(), "a cancellation must leave a plan-progress report behind")
		Expect(progress).NotTo(HaveKey("paused"),
			"a cancellation is a report, not a suspension: Paused is omitempty and must be left zero, "+
				"since only a suspended checkpoint may grant a resume")
		completed := progressIntOrZero(progress, "completedInstructions")
		total := progressIntOrZero(progress, "totalInstructions")
		Expect(completed).To(BeNumerically("<", total),
			"the plan was stopped mid-flight, so fewer instructions completed than the plan contains")

		By("Verifying applied-checksum was not written for a plan that never finished")
		// Writing it would tell Rancher the node is in sync with a plan that was abandoned partway.
		Expect(framework.GetAppliedChecksum(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)).To(BeEmpty())
	})

	It("should cancel a pending plan without executing anything at all", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		By("Creating a pending plan that already carries " + planapi.PlanCanceledAnnotation)
		// No gate and no cleanup are needed here: the annotation is present before the agent's
		// first reconcile, so recordInterruptAtEntry returns before Apply is ever called and no
		// instruction can be left running.
		plan := framework.NewPlan().
			WithInstruction("should-not-run", "/bin/sh",
				[]string{"-c", "touch " + cancelPendingRan}, true).
			Build()

		Expect(framework.CreatePlanSecretWithAnnotations(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStatePending)},
			map[string]string{planapi.PlanCanceledAnnotation: "true"})).To(Succeed())

		By("Waiting for plan-state to become canceled")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateCanceled },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying the plan's only instruction never runs, across a full re-enqueue cycle")
		// An interrupted plan is re-enqueued on the probe period (5s by default), so this window spans
		// many re-enqueues of the canceled plan. This is the only cancellation coverage for reconciles
		// that follow the one that recorded the cancellation, which is where a missing terminal-state
		// guard would surface.
		Consistently(func() bool { return nodeFileExists(ctx, podName, cancelPendingRan) },
			70*time.Second, 5*time.Second).Should(BeFalse(),
			"a plan canceled before it started must have no side effects on the node whatsoever, "+
				"including on the re-enqueues that follow")

		By("Verifying the checkpoint records that nothing was executed")
		progress := framework.GetPlanProgress(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(progress).NotTo(BeNil())
		Expect(progressIntOrZero(progress, "completedInstructions")).To(BeEquivalentTo(0))
		Expect(progressIntOrZero(progress, "totalInstructions")).To(BeEquivalentTo(1))
		Expect(progress).NotTo(HaveKey("paused"))

		By("Verifying applied-checksum was not written")
		Expect(framework.GetAppliedChecksum(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)).To(BeEmpty())
	})

	It("should stay monitoring-only after cancel annotation is removed from a canceled plan", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)
		const cancelReport = `{"checksum":"canceled-report","completedInstructions":1,"totalInstructions":2}`

		By("Creating a canceled terminal plan with periodic instructions")
		plan := framework.NewPlan().
			WithPeriodicInstruction("should-not-run-after-cancel", "/bin/sh",
				[]string{"-c", "touch " + cancelTerminalRan}, 5).
			Build()

		Expect(framework.CreatePlanSecretWithAnnotations(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey:       []byte(planapi.PlanStateCanceled),
				planapi.PlanCheckpointKey:  []byte(cancelReport),
				k8splan.AppliedChecksumKey: []byte(""),
				k8splan.ProbeStatusesKey:   []byte("{}"),
			},
			map[string]string{planapi.PlanCanceledAnnotation: "true"})).To(Succeed())

		By("Removing the cancel annotation")
		Expect(framework.RemoveSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanCanceledAnnotation)).To(Succeed())
		rvAfterRemove := framework.GetSecretResourceVersion(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)

		By("Verifying no plan instructions run after cancellation is reported")
		Consistently(func() bool { return nodeFileExists(ctx, podName, cancelTerminalRan) },
			20*time.Second, 4*time.Second).Should(BeFalse(),
			"clearing the cancel annotation must not re-enable execution on a terminal canceled plan")

		By("Verifying the cancellation report and checksum remain untouched")
		progress := framework.GetPlanProgress(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(progress).NotTo(BeNil())
		Expect(progress["checksum"]).To(Equal("canceled-report"))
		Expect(progressIntOrZero(progress, "completedInstructions")).To(BeEquivalentTo(1))
		Expect(progressIntOrZero(progress, "totalInstructions")).To(BeEquivalentTo(2))
		Expect(progress).NotTo(HaveKey("paused"))
		Expect(framework.GetAppliedChecksum(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)).To(BeEmpty())

		By("Verifying the reconcile does not rewrite lifecycle data after un-cancel")
		Consistently(func() string {
			return framework.GetSecretResourceVersion(ctx, cl,
				framework.E2ENamespace, framework.PlanSecretName)
		}, 12*time.Second, 3*time.Second).Should(Equal(rvAfterRemove),
			"monitoring-only reconciles should not rewrite Secret data in this steady state")
	})

	It("should cancel a succeeded plan that is still running periodic instructions, permanently", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		By("Creating a succeeded plan, with the cancel annotation already set, that has periodic instructions")
		// plan-state:succeeded is set directly, as plan_state_test.go's "should not re-apply..."
		// spec does: what matters here is that a succeeded plan keeps running periodic
		// instructions on every reconcile, unlike every other terminal state. The annotation is
		// present at creation so the very first reconcile is the one that must record the
		// cancellation, exactly as the "cancel a pending plan" spec above does for pending.
		plan := framework.NewPlan().
			WithPeriodicInstruction("should-not-run", "/bin/sh",
				[]string{"-c", "touch " + cancelSucceededRan}, 5).
			Build()

		Expect(framework.CreatePlanSecretWithAnnotations(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey:       []byte(planapi.PlanStateSucceeded),
				k8splan.AppliedChecksumKey: []byte("pre-existing-checksum"),
				k8splan.ProbeStatusesKey:   []byte("{}"),
			},
			map[string]string{planapi.PlanCanceledAnnotation: "true"})).To(Succeed())

		By("Waiting for plan-state to become canceled")
		// Before the fix, a succeeded plan-state was (wrongly) treated the same as an already
		// terminal, inert one: the cancellation was never recorded and plan-state stayed
		// succeeded, so periodic instructions kept running as if nothing had happened.
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateCanceled },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying the periodic instruction never ran")
		Consistently(func() bool { return nodeFileExists(ctx, podName, cancelSucceededRan) },
			20*time.Second, 4*time.Second).Should(BeFalse(),
			"cancelling a succeeded plan must stop its periodic instructions, not merely leave them be")

		By("Verifying plan-progress reports a cancellation, not a suspension")
		progress := framework.GetPlanProgress(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(progress).NotTo(BeNil())
		Expect(progress).NotTo(HaveKey("paused"),
			"a cancellation is a report, not a suspension: only a suspended checkpoint may grant a resume")

		By("Removing the cancel annotation")
		Expect(framework.RemoveSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanCanceledAnnotation)).To(Succeed())

		By("Verifying the periodic instruction still never runs: cancel is terminal")
		// This is the failure mode the fix closes: without it, removing the annotation left
		// plan-state exactly where it started (succeeded), so periodic execution simply resumed
		// as if the cancellation had never been requested.
		Consistently(func() bool { return nodeFileExists(ctx, podName, cancelSucceededRan) },
			20*time.Second, 4*time.Second).Should(BeFalse(),
			"clearing the cancel annotation must not resume periodic execution on a canceled plan")
		Expect(currentPlanState(ctx)).To(Equal(planapi.PlanStateCanceled),
			"cancel is terminal: only new pending plan content from the orchestrator may move the plan again")
	})

	It("should kill the instruction's whole process tree, not merely its shell", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)
		releaseGateOnCleanup(cancelTreeGate)

		By("Creating a plan whose instruction backgrounds a child that keeps writing")
		// The backgrounded loop is the process-tree case this test is meant to cover. Plan instructions
		// almost always run a run.sh that shells out to an installer or package manager, so signaling
		// only the direct child could leave the actual work running on a node whose operator believes
		// they stopped it.
		//
		// Redirect the child's stdout to /dev/null so it does not inherit the agent's pipes. execute()
		// waits for eg.Wait() before cmd.Wait(), and eg.Wait() does not return until both pipes reach EOF.
		// A child that keeps those pipes open would therefore block Apply and the agent's single worker for
		// the child's entire lifetime instead of the parent's. That is a separate failure mode from the
		// one under test; the watchdog's pipe-closing behavior is covered by unit tests in
		// pkg/applyinator.
		//
		// The child also watches the gate, so cleanup can release the entire tree. Its 300-iteration cap
		// is a backstop against a stray writer and far exceeds the ~90s of assertion windows below, so it
		// cannot mask a cancellation that failed to reach the child.
		child := fmt.Sprintf("i=0; while [ ! -e %s ] && [ $i -lt 300 ]; do echo tick >> %s; sleep 1; i=$((i+1)); done",
			cancelTreeGate, cancelTreeChildLog)
		script := fmt.Sprintf("(%s) >/dev/null 2>&1 & %s", child, blockingScript(cancelTreeGate))
		plan := framework.NewPlan().
			WithInstruction("spawns-a-child", "/bin/sh", []string{"-c", script}, true).
			Build()

		Expect(framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey: []byte(planapi.PlanStatePending),
			})).To(Succeed())

		By("Waiting until the child has actually written, so the assertion below cannot pass vacuously")
		Eventually(func() int { return nodeFileLineCount(ctx, podName, cancelTreeChildLog) },
			framework.WaitTimeout, 2*time.Second).Should(BeNumerically(">=", 2),
			"the backgrounded child should be looping before the cancel is issued")

		By("Setting " + planapi.PlanCanceledAnnotation)
		Expect(framework.SetSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanCanceledAnnotation, "true")).To(Succeed())

		By("Waiting for plan-state to become canceled")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateCanceled },
			framework.WaitTimeout, 2*time.Second)

		By("Waiting for the child to stop writing")
		// The process group is asked to stop with SIGTERM and only killed outright ten seconds
		// later, so allow for that escalation rather than asserting on the instant plan-state
		// lands. lastCount starts at -1 so the very first sample can never be mistaken for a
		// stable one.
		lastCount := -1
		Eventually(func() bool {
			current := nodeFileLineCount(ctx, podName, cancelTreeChildLog)
			stopped := current == lastCount
			lastCount = current
			return stopped
		}, 60*time.Second, 5*time.Second).Should(BeTrue(),
			"the grandchild is still appending well after the cancel: the signal did not reach the process group")

		By("Verifying the child stays dead")
		Consistently(func() int { return nodeFileLineCount(ctx, podName, cancelTreeChildLog) },
			20*time.Second, 4*time.Second).Should(Equal(lastCount),
			"the file grew again, so a descendant of the canceled instruction is still alive")
	})

	It("should cancel a periodic instruction while it is genuinely in-flight and terminate it immediately", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)
		const (
			periodicCancelStarted = "/tmp/e2e-cancel-periodic-inflight-started"
			periodicCancelGate    = "/tmp/e2e-cancel-periodic-inflight-gate"
			periodicCancelMarker  = "/tmp/e2e-cancel-periodic-inflight-marker.txt"
		)
		releaseGateOnCleanup(periodicCancelGate)

		By("Creating a succeeded plan with a gated periodic instruction")
		plan := framework.NewPlan().
			WithPeriodicInstruction("periodic-cancel-inflight", "/bin/sh",
				[]string{"-c", fmt.Sprintf("touch %s; %s; echo completed >> %s",
					periodicCancelStarted, blockingScript(periodicCancelGate), periodicCancelMarker)},
				300).
			Build()

		By("Creating the plan Secret already at plan-state:succeeded")
		Expect(framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey:       []byte(planapi.PlanStateSucceeded),
				k8splan.AppliedChecksumKey: []byte("pre-existing-checksum"),
				k8splan.ProbeStatusesKey:   []byte("{}"),
			})).To(Succeed())

		By("Waiting for the periodic instruction to start and block on its gate")
		Eventually(func() bool { return nodeFileExists(ctx, podName, periodicCancelStarted) },
			framework.WaitTimeout, time.Second).Should(BeTrue(),
			"the periodic instruction should have started and be waiting on the gate")

		By("Capturing the periodic output recorded before this reconcile, if any")
		// Apply() is a single blocking call: nothing is written to applied-periodic-output until
		// it returns, and it cannot return while the instruction is genuinely blocked on its gate.
		// This is the first run of this periodic instruction, so the key legitimately does not
		// exist yet; a blocking wait for it to appear would time out by design. Read whatever is
		// on the Secret right now instead of waiting for a write that has not happened.
		outputBefore := framework.GetSecretData(ctx, cl, framework.E2ENamespace, framework.PlanSecretName)[k8splan.AppliedPeriodicOutputKey]
		outputMapBefore, err := framework.DecodePeriodicOutput(outputBefore)
		Expect(err).NotTo(HaveOccurred())
		runTimeBefore := outputMapBefore["periodic-cancel-inflight"].LastSuccessfulRunTime

		By("Setting " + planapi.PlanCanceledAnnotation + " while the periodic instruction is still running")
		Expect(framework.SetSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanCanceledAnnotation, "true")).To(Succeed())

		By("Waiting for plan-state to become canceled")
		// Cancel is prompt: the instruction is signaled immediately, not allowed to finish.
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateCanceled },
			framework.WaitTimeout, 2*time.Second)

		By("Opening the gate so the process can be reaped")
		execInAgent(ctx, podName, "touch "+periodicCancelGate)

		By("Verifying the periodic instruction was terminated and did not complete")
		// A single point-in-time check right after opening the gate could pass on a completion
		// that merely arrives late. Consistently over a window that comfortably exceeds the
		// termination grace period turns that false negative into a real assertion.
		Consistently(func() bool { return nodeFileExists(ctx, podName, periodicCancelMarker) },
			15*time.Second, 2*time.Second).Should(BeFalse(),
			"the canceled periodic instruction must not have written its completion marker; "+
				"if it did, the cancel was not prompt")

		By("Verifying LastSuccessfulRunTime was NOT updated (instruction did not complete)")
		outputAfterCancel := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			k8splan.AppliedPeriodicOutputKey, 5*time.Second, time.Second)
		outputMapAfterCancel, err := framework.DecodePeriodicOutput(outputAfterCancel)
		Expect(err).NotTo(HaveOccurred())
		// The output entry might not exist if this was the first run, or it should match the old time
		currentRunTime := outputMapAfterCancel["periodic-cancel-inflight"].LastSuccessfulRunTime
		if runTimeBefore != "" {
			Expect(currentRunTime).To(Equal(runTimeBefore),
				"the canceled periodic instruction did not complete, so its LastSuccessfulRunTime must not change")
		} else {
			Expect(currentRunTime).To(BeEmpty(),
				"the canceled periodic instruction never completed, so LastSuccessfulRunTime must remain unset")
		}

		By("Verifying applied-checksum was not rewritten by the cancellation")
		// handleCancellation never touches AppliedChecksumKey at all: it neither clears it nor
		// records a new one. The Secret was created with a pre-existing value, standing in for
		// whatever the last successful apply actually recorded, so the correct assertion is that
		// cancellation leaves it untouched, not that it ends up empty.
		Expect(framework.GetAppliedChecksum(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)).To(Equal("pre-existing-checksum"),
			"a canceled plan must not overwrite applied-checksum with a new value, even if periodic instructions ran")
	})

	It("should cancel outright when canceled=true is valid even though paused carries an invalid value", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)
		const precedenceRan = "/tmp/e2e-cancel-precedence-invalid-pause-ran.txt"

		By("Creating a pending plan with a valid cancel and an invalid pause annotation together")
		// A valid canceled=true must win outright, even when the pause value alongside it is
		// invalid: the agent must not treat the invalid pause as a configuration error that blocks
		// the reconcile once a valid cancellation is present.
		plan := framework.NewPlan().
			WithInstruction("should-not-run", "/bin/sh",
				[]string{"-c", "touch " + precedenceRan}, true).
			Build()

		Expect(framework.CreatePlanSecretWithAnnotations(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStatePending)},
			map[string]string{
				planapi.PlanCanceledAnnotation: "true",
				planapi.PlanPausedAnnotation:   "True", // invalid: capitalised
			})).To(Succeed())

		By("Waiting for plan-state to become canceled, not paused and not stuck pending")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateCanceled },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying the plan never ran")
		Expect(nodeFileExists(ctx, podName, precedenceRan)).To(BeFalse())
	})

	It("should resume only when the orchestrator supplies new plan content, never merely by clearing the annotation", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)
		const (
			oldPlanRan = "/tmp/e2e-cancel-then-new-plan-old-ran.txt"
			newPlanRan = "/tmp/e2e-cancel-then-new-plan-new-ran.txt"
		)

		By("Creating a pending plan that already carries " + planapi.PlanCanceledAnnotation)
		oldPlan := framework.NewPlan().
			WithInstruction("should-not-run", "/bin/sh",
				[]string{"-c", "touch " + oldPlanRan}, true).
			Build()

		Expect(framework.CreatePlanSecretWithAnnotations(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, oldPlan,
			map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStatePending)},
			map[string]string{planapi.PlanCanceledAnnotation: "true"})).To(Succeed())

		By("Waiting for plan-state to become canceled")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateCanceled },
			framework.WaitTimeout, 2*time.Second)

		By("Removing the cancel annotation alone: cancel is terminal, this must not resume anything")
		Expect(framework.RemoveSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanCanceledAnnotation)).To(Succeed())
		Consistently(func() planapi.PlanState { return currentPlanState(ctx) },
			15*time.Second, 3*time.Second).Should(Equal(planapi.PlanStateCanceled),
			"clearing the annotation alone must not move a canceled plan")

		By("Delivering new plan content at plan-state:pending, the orchestrator's documented recovery path")
		newPlan := framework.NewPlan().
			WithInstruction("new-plan-runs", "/bin/sh",
				[]string{"-c", "touch " + newPlanRan}, true).
			Build()
		Expect(framework.UpdateSecretData(ctx, cl, framework.E2ENamespace, framework.PlanSecretName, map[string][]byte{
			k8splan.PlanKey:      newPlan,
			planapi.PlanStateKey: []byte(planapi.PlanStatePending),
		})).To(Succeed())

		By("Waiting for the new plan to actually execute")
		Eventually(func() bool { return nodeFileExists(ctx, podName, newPlanRan) },
			framework.WaitTimeout, time.Second).Should(BeTrue(),
			"new plan content at plan-state:pending is the only thing that moves a canceled plan again")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateSucceeded },
			framework.WaitTimeout, 2*time.Second)
	})
})

func progressIntOrZero(progress map[string]any, key string) float64 {
	value, ok := progress[key]
	if !ok || value == nil {
		return 0
	}

	number, ok := value.(float64)
	Expect(ok).To(BeTrue(), "plan-progress %q must decode as a JSON number", key)
	return number
}
