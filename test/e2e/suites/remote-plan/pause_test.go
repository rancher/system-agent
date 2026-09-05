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
	"github.com/rancher/system-agent/test/e2e"
	"github.com/rancher/system-agent/test/framework"
)

// pauseInvalidRan is the marker for the invalid-value spec, which uses a single-instruction plan
// rather than the shared three-instruction fixture.
const pauseInvalidRan = "/tmp/e2e-pause-invalid-ran.txt"

// Node paths used by the periodic-pause spec. periodicFirstMarker records each successful run of
// the long-period instruction, so a duplicate line is direct evidence of an unwanted re-run.
const (
	periodicPauseStarted = "/tmp/e2e-pause-periodic-started"
	periodicPauseGate    = "/tmp/e2e-pause-periodic-gate"
	periodicFirstMarker  = "/tmp/e2e-pause-periodic-first-marker.txt"
	periodicSecondRan    = "/tmp/e2e-pause-periodic-second-ran.txt"
)

var _ = Describe("Remote Plan - Pause", Label(framework.ShortTestLabel), func() {
	It("should hold at an instruction boundary and resume without re-running what completed", func() {
		ctx := context.Background()
		paths := newPausePaths("e2e-pause-release")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		pauseAtFirstBoundary(ctx, podName, paths, pausePlan(paths).Build())

		By("Verifying the instruction after the boundary has not run")
		Consistently(func() bool { return nodeFileExists(ctx, podName, paths.stepTwo) },
			15*time.Second, 3*time.Second).Should(BeFalse(),
			"a held plan must not start the next instruction")

		By("Verifying plan-progress records the suspension")
		progress := framework.GetPlanProgress(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(progress).NotTo(BeNil(), "a pause must leave a resume checkpoint behind")
		Expect(progress["paused"]).To(Equal(true),
			"only a suspended checkpoint grants a resume, so the record must be marked as one")
		Expect(progress["completedInstructions"]).To(BeEquivalentTo(1))
		Expect(progress["totalInstructions"]).To(BeEquivalentTo(3))

		By("Removing " + planapi.PlanPausedAnnotation)
		Expect(framework.RemoveSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanPausedAnnotation)).To(Succeed())

		By("Waiting for plan-state to become succeeded")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateSucceeded },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying the instruction that had already completed was not re-executed")
		Expect(nodeFileContent(ctx, podName, paths.marker)).To(Equal("one\ntwo\nthree"),
			"each instruction must appear exactly once: a duplicate line means the resume re-ran completed work")
	})

	It("should keep updating probe statuses while the plan is held", func() {
		ctx := context.Background()
		paths := newPausePaths("e2e-pause-probe")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		By("Deploying the HTTP test server the probe targets")
		framework.DeployHTTPTestServer(ctx, kubeconfigPath, framework.E2ENamespace, e2e.HTTPTestServerManifestTemplate)
		DeferCleanup(func() {
			framework.CleanupHTTPTestServer(context.Background(), kubeconfigPath, framework.E2ENamespace)
		})

		probeURL := "http://" + framework.HTTPTestServerName + "." + framework.E2ENamespace + ".svc.cluster.local:8080/index.html"

		By("Creating the fixture with a probe against it")
		// failureThreshold is 1 so that a single probe run after the server disappears is enough
		// to flip the status. A held plan is only re-reconciled once a minute, so a threshold of
		// three would cost three minutes of waiting to observe the same thing.
		plan := pausePlan(paths).
			WithProbeCustomThresholds("held-probe", probeURL, true, 1, 1, 0, 5).
			Build()

		pauseAtFirstBoundary(ctx, podName, paths, plan)

		By("Verifying the probe reports healthy while the plan is held")
		Eventually(func() any { return probeStatus(ctx, "held-probe")["healthy"] },
			framework.WaitTimeout, 5*time.Second).Should(Equal(true),
			"the probe should have run and succeeded on the interrupt path")

		By("Removing the HTTP test server so the probe must start failing")
		framework.CleanupHTTPTestServer(ctx, kubeconfigPath, framework.E2ENamespace)

		By("Waiting for probe-statuses to record the failure")
		// An interrupt suppresses execution, never observation. Freezing probe statuses during a
		// hold would feed stale health data to Rancher's MachineHealthCheck on exactly the nodes
		// most likely to be unhealthy — a plan stopped mid-flight leaves the node partly changed.
		// The agent re-reconciles a held plan on the probe period, the same cadence it uses for an
		// executing plan, so allow for more than one cycle.
		//
		// All three assertions run against one read of the Secret. The non-nil check is what stops
		// this passing on a probe entry that vanished entirely, and healthy is asserted by its
		// absence because ProbeStatus marshals with omitempty throughout: an unhealthy probe drops
		// the key rather than writing false, so a rising failureCount is the only positive
		// evidence on the wire.
		Eventually(func(g Gomega) {
			status := probeStatus(ctx, "held-probe")
			g.Expect(status).NotTo(BeNil(), "the probe entry must still be recorded")
			g.Expect(status["failureCount"]).To(BeNumerically(">=", 1))
			g.Expect(status).NotTo(HaveKey("healthy"))
		}, 3*time.Minute, 5*time.Second).Should(Succeed(),
			"probe statuses must keep advancing while the plan is held")

		By("Verifying the plan is still held and still has not executed anything further")
		// This is the suite's post-re-enqueue coverage for pause, and it is why the Consistently
		// windows in the other pause specs are long enough as they stand: reaching this line took
		// several re-enqueue cycles, because a re-enqueue is the only thing that can record the
		// probe failure asserted above.
		Expect(currentPlanState(ctx)).To(Equal(planapi.PlanStatePaused))
		Expect(nodeFileExists(ctx, podName, paths.stepTwo)).To(BeFalse())
	})

	It("should not execute anything across an agent restart while the plan is held", func() {
		ctx := context.Background()
		paths := newPausePaths("e2e-pause-restart")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		pauseAtFirstBoundary(ctx, podName, paths, pausePlan(paths).Build())

		By("Recording the checkpoint before the restart")
		before := framework.GetPlanProgress(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(before).NotTo(BeNil())
		Expect(before["paused"]).To(Equal(true))
		Expect(before["completedInstructions"]).To(BeEquivalentTo(1))

		By("Restarting the agent while " + planapi.PlanPausedAnnotation + " is still set")
		deleteAgentPod(ctx, podName)
		framework.KubectlWaitForPodsReady(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel, framework.WaitTimeout)
		podName = framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		By("Waiting for the restarted agent to reconcile the held plan")
		// KubectlWaitForPodsReady returns once the container is running — the DaemonSet declares
		// no readinessProbe — which is not the same as the informer having delivered the Secret.
		// Everything below would otherwise pass on an agent that has not yet looked at the plan at
		// all, which is the most likely way for this spec to go quietly green while broken.
		//
		// This particular line is handlePause's write-once guard (pkg/k8splan/interrupt.go:284),
		// so it proves more than that a reconcile happened: the new agent read the Secret, saw the
		// hold, found its predecessor's checkpoint and declined to rewrite it. It is emitted at
		// debug, which the DaemonSet enables via CATTLE_LOGLEVEL=debug and main.go:35-43 honours.
		// The log is read from the new pod, whose output starts empty, so any occurrence is the
		// restarted agent's.
		Eventually(func() string {
			return framework.KubectlGetLogs(ctx, kubeconfigPath, framework.E2ENamespace, podName)
		}, framework.WaitTimeout, 5*time.Second).
			Should(ContainSubstring("suspension already recorded for checksum"),
				"the restarted agent must have reconciled the held plan and kept its predecessor's checkpoint")

		By("Verifying the restarted agent neither resumes the plan nor rewrites the checkpoint")
		// The checkpoint is keyed to the plan checksum and to nothing else — no per-process
		// identifier — which is what lets it outlive the agent that wrote it. This is the only
		// place that property is exercised against a real API server rather than a fake client,
		// and it is the failure mode the whole design exists to prevent: "pause works until the
		// agent restarts, and then the plan runs anyway".
		//
		// Deliberately not asserted by looking for the later instructions' files: the agent
		// container's filesystem does not survive the pod being replaced, so /tmp comes back empty
		// and any such check would be green whatever the agent did. plan-state and the checkpoint
		// live in the Secret, which does survive, so they are the only honest evidence here.
		Consistently(func() planapi.PlanState { return currentPlanState(ctx) },
			30*time.Second, 5*time.Second).Should(Equal(planapi.PlanStatePaused),
			"a restarted agent must not resume a plan whose pause annotation is still set")

		after := framework.GetPlanProgress(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(after).NotTo(BeNil())
		Expect(after["paused"]).To(Equal(true))
		Expect(after["completedInstructions"]).To(Equal(before["completedInstructions"]),
			"the restarted agent must keep its predecessor's progress rather than recompute it from a reconcile with no apply in flight")

		By("Re-creating the gate so a wrongly re-executed first instruction would finish rather than hang")
		// The first instruction blocks on the gate, which the restart also wiped. Without this a
		// lost checkpoint would resume from instruction zero and sit at the gate until its cap,
		// and the spec would fail by timing out. With it, the spec fails on the marker below
		// reading "one\ntwo\nthree" instead of "two\nthree", which says exactly what went wrong.
		execInAgent(ctx, podName, "touch "+paths.gate)

		By("Removing " + planapi.PlanPausedAnnotation)
		Expect(framework.RemoveSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanPausedAnnotation)).To(Succeed())

		By("Waiting for plan-state to become succeeded")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateSucceeded },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying only the instructions the checkpoint had not accounted for ran")
		// "one" is absent because the restart reset the container's /tmp, not because the first
		// instruction ran and lost its line. The assertion that matters is that it did not run
		// again: were the checkpoint ignored, this file would read "one\ntwo\nthree".
		Expect(nodeFileContent(ctx, podName, paths.marker)).To(Equal("two\nthree"),
			"the resumed apply must run only the instructions the durable checkpoint had not accounted for")
	})

	It(`should release a hold when the annotation is set to "false", exactly as removing it does`, func() {
		ctx := context.Background()
		paths := newPausePaths("e2e-pause-false")
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		pauseAtFirstBoundary(ctx, podName, paths, pausePlan(paths).Build())

		By(`Setting ` + planapi.PlanPausedAnnotation + ` to "false" rather than removing it`)
		// An absent annotation is "false", so the two are indistinguishable to the agent. This
		// spec exists because an orchestrator that clears a hold by rewriting the value rather
		// than by deleting the key must get the same behaviour.
		Expect(framework.SetSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanPausedAnnotation, "false")).To(Succeed())

		By("Waiting for plan-state to become succeeded")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateSucceeded },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying the instruction that had already completed was not re-executed")
		Expect(nodeFileContent(ctx, podName, paths.marker)).To(Equal("one\ntwo\nthree"),
			`releasing a hold with "false" must resume from the checkpoint exactly as removing the annotation does`)
	})

	It("should refuse to act on an invalid annotation value, and write nothing at all", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)

		By(`Creating a pending plan already carrying ` + planapi.PlanPausedAnnotation + `: "True"`)
		// The only valid values are "true" and "false". "True" is a configuration error rather
		// than a value to guess at, and the agent's response is to execute nothing, interrupt
		// nothing and write nothing, returning an error so the workqueue retries. The annotation
		// has to be present at creation: setting it afterwards races the apply.
		plan := framework.NewPlan().
			WithInstruction("should-not-run", "/bin/sh",
				[]string{"-c", "touch " + pauseInvalidRan}, true).
			Build()

		Expect(framework.CreatePlanSecretWithAnnotations(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStatePending)},
			map[string]string{planapi.PlanPausedAnnotation: "True"})).To(Succeed())

		resourceVersion := framework.GetSecretResourceVersion(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)

		By("Verifying the agent writes nothing while the value is unreadable")
		// This is the one assertion here a fake client cannot make, and it is cheap: it catches a
		// real API server behaving differently from the mock on "the agent wrote nothing". A write
		// on this path would also amplify the error into a write loop, since every retry re-enters
		// it.
		Consistently(func() string {
			return framework.GetSecretResourceVersion(ctx, cl,
				framework.E2ENamespace, framework.PlanSecretName)
		}, 30*time.Second, 3*time.Second).Should(Equal(resourceVersion),
			"an invalid annotation value must not produce a Secret write of any kind, including plan-state:failed")

		By("Verifying the plan did not advance")
		Expect(currentPlanState(ctx)).To(Equal(planapi.PlanStatePending))
		Expect(nodeFileExists(ctx, podName, pauseInvalidRan)).To(BeFalse())

		By(`Correcting the value to "true"`)
		Expect(framework.SetSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanPausedAnnotation, "true")).To(Succeed())

		By("Waiting for the hold to be recorded")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStatePaused },
			framework.WaitTimeout, 2*time.Second)

		Expect(nodeFileExists(ctx, podName, pauseInvalidRan)).To(BeFalse(),
			"correcting the value must record the hold, not release it")
	})

	It("should preserve a completed periodic instruction's output across a pause, so it is not re-run on resume", func() {
		ctx := context.Background()
		podName := framework.KubectlGetPodName(ctx, kubeconfigPath,
			framework.E2ENamespace, framework.AgentLabel)
		releaseGateOnCleanup(periodicPauseGate)

		By("Creating a succeeded plan with a long-period gated periodic instruction and an always-due one")
		// periodic-first only reports success once it returns, so its gate lets the spec pause
		// while it is genuinely in flight, exactly as the one-time instruction specs do. Its
		// 300-second period means it must not appear due again anywhere within this spec's
		// window: a lost LastSuccessfulRunTime is what would make periodicInstructionDue disagree.
		// periodic-second has never run, so it is always due and proves the plan resumes running
		// periodic instructions at all, rather than staying stuck.
		plan := framework.NewPlan().
			WithPeriodicInstruction("periodic-first", "/bin/sh",
				[]string{"-c", fmt.Sprintf("touch %s; %s; echo run >> %s",
					periodicPauseStarted, blockingScript(periodicPauseGate), periodicFirstMarker)},
				300).
			WithPeriodicInstruction("periodic-second", "/bin/sh",
				[]string{"-c", "touch " + periodicSecondRan}, 5).
			Build()

		By("Creating the plan Secret already at plan-state:succeeded")
		// plan-state:succeeded is set directly, as plan_state_test.go's "should not re-apply..."
		// spec does, rather than by first running the plan to completion: what matters here is
		// the periodic-only Apply pass a succeeded plan keeps running, not how it got there.
		Expect(framework.CreatePlanSecretWithData(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan,
			map[string][]byte{
				planapi.PlanStateKey:       []byte(planapi.PlanStateSucceeded),
				k8splan.AppliedChecksumKey: []byte("pre-existing-checksum"),
				k8splan.ProbeStatusesKey:   []byte("{}"),
			})).To(Succeed())

		By("Waiting for the long-period periodic instruction to start and block on its gate")
		Eventually(func() bool { return nodeFileExists(ctx, podName, periodicPauseStarted) },
			framework.WaitTimeout, time.Second).Should(BeTrue())

		By("Setting " + planapi.PlanPausedAnnotation + " while it is still running")
		Expect(framework.SetSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanPausedAnnotation, "true")).To(Succeed())

		By("Giving the interrupt watch time to observe the annotation before releasing the gate")
		// Mirrors pauseAtFirstBoundary: ten seconds is comfortably longer than the interrupt
		// watch's poll interval, so opening the gate below cannot race the pause being observed.
		Consistently(func() bool { return nodeFileExists(ctx, podName, periodicSecondRan) },
			10*time.Second, 2*time.Second).Should(BeFalse())

		By("Opening the gate so the long-period instruction completes")
		execInAgent(ctx, podName, "touch "+periodicPauseGate)

		By("Waiting for plan-state to become paused")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStatePaused },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying the always-due periodic instruction never ran: pause is a boundary")
		Expect(nodeFileExists(ctx, podName, periodicSecondRan)).To(BeFalse(),
			"a pause lets the running periodic instruction finish but must stop before the next one")

		By("Verifying the completed periodic instruction's output survived the interruption")
		periodicOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			k8splan.AppliedPeriodicOutputKey, 10*time.Second, time.Second)
		outputMap, err := framework.DecodePeriodicOutput(periodicOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(outputMap).To(HaveKey("periodic-first"))
		firstRunTime := outputMap["periodic-first"].LastSuccessfulRunTime
		Expect(firstRunTime).NotTo(BeEmpty(),
			"the interrupted apply must persist the periodic output it produced before the interruption, "+
				"or the stale timestamp left on the Secret will make the instruction look due again on resume")

		By("Removing " + planapi.PlanPausedAnnotation)
		Expect(framework.RemoveSecretAnnotation(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanPausedAnnotation)).To(Succeed())

		By("Waiting for plan-state to become succeeded again")
		framework.WaitForSecretFieldCondition(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			planapi.PlanStateKey,
			func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStateSucceeded },
			framework.WaitTimeout, 2*time.Second)

		By("Verifying the always-due periodic instruction resumes running")
		Eventually(func() bool { return nodeFileExists(ctx, podName, periodicSecondRan) },
			framework.WaitTimeout, 2*time.Second).Should(BeTrue(),
			"resuming a paused succeeded plan must let periodic instructions run again")

		By("Verifying the long-period instruction was NOT re-run: its timestamp must have survived the pause")
		Expect(nodeFileContent(ctx, podName, periodicFirstMarker)).To(Equal("run"),
			"a lost LastSuccessfulRunTime would make periodicInstructionDue treat the instruction as due "+
				"again, re-running it and producing a second line here")

		updatedOutput := framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			k8splan.AppliedPeriodicOutputKey, 10*time.Second, time.Second)
		updatedMap, err := framework.DecodePeriodicOutput(updatedOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(updatedMap["periodic-first"].LastSuccessfulRunTime).To(Equal(firstRunTime),
			"the preserved run time must be exactly what the interrupted apply recorded, not recomputed")
	})
})

// pausePaths names the node paths one pause fixture uses. Each spec takes its own prefix: the
// suite's AfterEach deletes the plan Secret but nothing cleans the agent container's filesystem,
// and an "instruction never ran" assertion that inherits a file from an earlier spec is worthless.
type pausePaths struct {
	// marker is appended to once by every instruction, so a re-execution shows up as a duplicate
	// line rather than as no difference at all.
	marker string
	// gate is created by the spec to let the first instruction return.
	gate string
	// stepTwo and stepThree are touched by the instructions after the boundary, so their absence
	// is direct evidence that the hold suppressed execution.
	stepTwo   string
	stepThree string
}

func newPausePaths(prefix string) pausePaths {
	return pausePaths{
		marker:    "/tmp/" + prefix + "-marker.txt",
		gate:      "/tmp/" + prefix + "-gate",
		stepTwo:   "/tmp/" + prefix + "-step-two.txt",
		stepThree: "/tmp/" + prefix + "-step-three.txt",
	}
}

// pausePlan builds the fixture the pause specs share: three one-time instructions, each appending
// a distinct line to a single marker file, with the first blocking on a gate file the spec creates
// (see blockingScript for why it is a gate rather than a sleep, and for the cap that bounds it).
func pausePlan(paths pausePaths) *framework.PlanBuilder {
	return framework.NewPlan().
		WithInstruction("step-one", "/bin/sh",
			[]string{"-c", fmt.Sprintf("echo one >> %s; %s", paths.marker, blockingScript(paths.gate))}, true).
		WithInstruction("step-two", "/bin/sh",
			[]string{"-c", fmt.Sprintf("echo two >> %s; touch %s", paths.marker, paths.stepTwo)}, true).
		WithInstruction("step-three", "/bin/sh",
			[]string{"-c", fmt.Sprintf("echo three >> %s; touch %s", paths.marker, paths.stepThree)}, true)
}

// pauseAtFirstBoundary delivers the plan with plan-state:pending, waits for the first instruction
// to start, sets the pause annotation, releases the instruction, and waits for the agent to record
// the hold. On return, the plan is paused and exactly one instruction has completed.
func pauseAtFirstBoundary(ctx context.Context, podName string, paths pausePaths, plan []byte) {
	GinkgoHelper()

	releaseGateOnCleanup(paths.gate)

	By("Creating the plan Secret with plan-state:pending")
	Expect(framework.CreatePlanSecretWithData(ctx, cl,
		framework.E2ENamespace, framework.PlanSecretName, plan,
		map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStatePending)})).To(Succeed())

	By("Waiting for the first instruction to start and block on its gate")
	Eventually(func() string { return nodeFileContent(ctx, podName, paths.marker) },
		framework.WaitTimeout, time.Second).Should(Equal("one"),
		"the first instruction should have appended its line and be waiting on the gate")

	By("Setting " + planapi.PlanPausedAnnotation)
	Expect(framework.SetSecretAnnotation(ctx, cl,
		framework.E2ENamespace, framework.PlanSecretName,
		planapi.PlanPausedAnnotation, "true")).To(Succeed())

	By("Verifying the pause does not interrupt the instruction already running")
	// Pause is a boundary, not a kill: it lets the running instruction finish and stops before the
	// next one. This assertion also makes the gate release below deterministic - ten seconds is
	// comfortably longer than the interrupt watch's poll interval, so the agent will have observed
	// the annotation before the gate opens.
	Consistently(func() planapi.PlanState { return currentPlanState(ctx) },
		10*time.Second, 2*time.Second).Should(Equal(planapi.PlanStateInProgress),
		"a pause must let the running instruction finish rather than interrupt it")

	By("Opening the gate so the first instruction returns")
	execInAgent(ctx, podName, "touch "+paths.gate)

	By("Waiting for plan-state to become paused")
	framework.WaitForSecretFieldCondition(ctx, cl,
		framework.E2ENamespace, framework.PlanSecretName,
		planapi.PlanStateKey,
		func(val []byte) bool { return planapi.PlanState(val) == planapi.PlanStatePaused },
		framework.WaitTimeout, 2*time.Second)
}

// deleteAgentPod deletes the agent DaemonSet pod and waits until it is actually gone.
//
// Waiting is important because KubectlWaitForPodsReady selects by the DaemonSet's label. If this
// returns while the old pod is still Terminating, it could wait on a pod that will never become
// Ready alongside its replacement.
func deleteAgentPod(ctx context.Context, podName string) {
	GinkgoHelper()

	result := &framework.RunCommandResult{}
	framework.RunCommand(ctx, framework.RunCommandInput{
		Command: "kubectl",
		Args: []string{
			"--kubeconfig", kubeconfigPath, "delete", "pod", podName,
			"-n", framework.E2ENamespace, "--wait=true", "--timeout=120s",
		},
	}, result)
	Expect(result.Error).NotTo(HaveOccurred(),
		"failed to delete agent pod %s: %s", podName, string(result.Stderr))
}

// probeStatus returns a probe's recorded status from the plan Secret, or nil if the probe has not
// been recorded yet. Callers can index the result directly: reading from a nil map is safe, and
// every ProbeStatus field uses omitempty, so an absent key and a zero value are equivalent on the
// wire.
func probeStatus(ctx context.Context, probe string) map[string]any {
	statuses := framework.GetProbeStatuses(ctx, cl, framework.E2ENamespace, framework.PlanSecretName)
	status, _ := statuses[probe].(map[string]any)
	return status
}
