package k8splan

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/prober"
	corecontrollers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
)

// reconcileSecret handles Secret change events.
// It decides whether to apply the plan, runs the apply, and writes outcomes back to the Secret.
func (w *watcher) reconcileSecret(ctx context.Context, sc corecontrollers.SecretController, secret *corev1.Secret, cooldownPeriod time.Duration) (*corev1.Secret, error) {
	if secret == nil {
		logrus.Debugf("[k8splan] received nil secret (object deleted from cache), skipping")
		return nil, nil
	}
	originalSecret := secret.DeepCopy()
	secret = secret.DeepCopy()

	var err error
	currentTime := time.Now()
	lastApplyTime := parseLastApplyTime(secret.Data, currentTime)
	w.probePeriod = parseProbePeriodOverride(secret.Data, w.probePeriod)

	logrus.Debugf("[k8splan] processing secret %s in namespace %s at generation %d with resource version %s",
		secret.Name, secret.Namespace, secret.Generation, secret.ResourceVersion)

	// needsApplied indicates whether the one-time instructions should be run. It is always set by
	// decidePlanStateAction or decideChecksumFlowAction below before being read.
	var needsApplied bool

	uidChanged := w.secretUID != "" && w.secretUID != string(secret.UID)
	rvIsOlder := toInt(w.lastAppliedResourceVersion) > toInt(secret.ResourceVersion)

	switch {
	case uidChanged:
		// Secret was deleted and recreated with a new UID; reset state so the new secret is force-applied.
		logrus.Infof("[k8splan] received secret with new UID (%s, previously %s); secret was recreated — resetting agent state",
			secret.UID, w.secretUID)
		w.secretUID = ""
		w.lastAppliedResourceVersion = ""
		w.hasRunOnce = false

	case rvIsOlder:
		// The handler is always invoked with the object held by the informer cache, while
		// lastAppliedResourceVersion is taken from the response to the agent's own write. The cache
		// catches up to those writes asynchronously, so a delivery from inside that window is the agent
		// observing its own feedback, not an out-of-order plan. Interrupt reconciles write the Secret
		// twice, which widens the window and makes this delivery routine.
		//
		// Don't return an error, because it would only add a false alarm and
		// requeue the plan under the workqueue's exponential rate limiter, stalling the probes with it.
		logrus.Debugf("[k8splan] skipping secret %s/%s at resource version %s: the cache has not caught up with the last write (%s)",
			w.connInfo.Namespace, w.connInfo.SecretName, secret.ResourceVersion, w.lastAppliedResourceVersion)
		sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
		return secret, nil
	}

	planData, ok := secret.Data[PlanKey]
	if !ok {
		sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
		return secret, nil
	}

	logrus.Tracef("[k8splan] byte data: %v", planData)
	logrus.Tracef("[k8splan] plan string was %s", string(planData))

	probeStatuses := parseProbeStatuses(secret.Data)

	cp, err := applyinator.CalculatePlan(planData)
	if err != nil {
		return secret, err
	}
	logrus.Tracef("[k8splan] calculated checksum to be %s", cp.Checksum)

	wasFailedPlan := false

	// currentPlanState is non-empty when Rancher supports plan-state.
	// If absent, fall back to checksum-based logic for backward compatibility.
	currentPlanState := planapi.PlanState(secret.Data[planapi.PlanStateKey])

	// Parse failureCount unconditionally — needed for OneTimeInstructionAttempts in both flows.
	failureCount, planAttempt := parseFailureCount(secret.Data)

	// Step A: select the flow and resolve interrupts before reading the plan. Interrupt annotations
	// are ignored entirely in the checksum flow because that flow does not support them. In the
	// plan-state flow, they determine whether this reconcile may execute any work.
	if currentPlanState == "" {
		for _, key := range []string{planapi.PlanCanceledAnnotation, planapi.PlanPausedAnnotation} {
			if value, ok := secret.Annotations[key]; ok {
				logrus.Warnf("[k8splan] ignoring unsupported annotation in checksum flow key=%s value=%s", key, value)
			}
		}
	} else if result, done, checkErr := w.checkAndRecordInterrupt(sc, secret, cp, currentPlanState, probeStatuses); done {
		return result, checkErr
	}

	// Step B: everything below runs only in two scenarios:
	// - the checksum flow, or
	// - in the plan-state flow where both interrupt annotations are explicitly inactive.
	//
	// effectiveState is the plan state used by this reconcile; resumeFrom identifies the position
	// from which an apply that has already been approved should resume.
	effectiveState, resumeFrom := resolveResume(currentPlanState, secret.Data, cp.Checksum)
	resumeFrom = clampResumeFrom(resumeFrom, len(cp.Plan.OneTimeInstructions))

	// Step C: clear the suspension before any work is applied. Reaching this point means the interrupt
	// annotation is no longer active, so the plan is being resumed.
	//
	// The write serves three purposes:
	//  1. An executing plan must not continue to report itself as paused. Without this write, the
	//     paused state could persist throughout the resumed apply and, for terminal ResumeState,
	//     indefinitely because no later outcome write would clear it. This matters especially for
	//     plans that only run periodic instructions.
	//  2. Clearing the suspension re-arms the write-once guard. Leaving Paused: true would cause
	//     handleInterrupt to treat a subsequent pause as already recorded and preserve a stale
	//     Completed checkpoint.
	//  3. It limits the checkpoint's authority to the suspension that created it. If the resumed apply
	//     crashes, the plan falls back to the normal contract of restarting from instruction 0 rather
	//     than trusting a checkpoint from a plan that is no longer suspended at that boundary.
	//
	// This applies only to the plan-state flow. The checksum flow has no checkpoint and therefore no
	// resume commit. The flow check is intentional rather than relying on resumeCommitUpdates' own
	// guard: that guard detects whether a suspension exists, so a legacy Secret containing a leftover
	// checkpoint could otherwise cause plan state and progress to be written to a Secret owned by an
	// orchestrator that does not use them.
	var resumeUpdates map[string][]byte
	if effectiveState != "" {
		resumeUpdates = resumeCommitUpdates(currentPlanState, effectiveState, secret.Data, cp.Checksum)
	}

	if effectiveState != "" {
		psResult := decidePlanStateAction(effectiveState)
		needsApplied = psResult.NeedsApplied
		if psResult.ResetPlanAttempt {
			planAttempt = 1
		}
		if !w.hasRunOnce {
			w.hasRunOnce = true
		}
	} else {
		// Backward compatibility: old checksum-based needsApplied decision.
		csResult := decideChecksumFlowAction(secret.Data, cp.Checksum, w.hasRunOnce, failureCount, currentTime,
			lastApplyTime, cooldownPeriod, w.lastAppliedResourceVersion == secret.ResourceVersion, w.lastAppliedResourceVersion)

		needsApplied = csResult.NeedsApplied
		wasFailedPlan = csResult.WasFailedPlan
		w.hasRunOnce = csResult.HasRunOnce
		if csResult.ClearAppliedChecksum {
			secret.Data[AppliedChecksumKey] = []byte("")
		}
	}

	if resumeUpdates != nil {
		if needsApplied {
			logrus.Infof("[k8splan] the plan with checksum %s is no longer held; resuming into plan-state %q from one-time instruction %d of %d",
				cp.Checksum, effectiveState, resumeFrom, len(cp.Plan.OneTimeInstructions))
		} else {
			logrus.Infof("[k8splan] the plan with checksum %s is no longer held; resuming into plan-state %q, which owes no one-time instructions",
				cp.Checksum, effectiveState)
		}
	}

	// The pending pre-commit already writes plan state, so include the resume update in that write
	// rather than issuing a separate update.
	foldResumeIntoPreCommit := resumeUpdates != nil && effectiveState == planapi.PlanStatePending && needsApplied
	if resumeUpdates != nil && !foldResumeIntoPreCommit {
		// If this fails the reconcile returns and no apply runs until it lands.
		committed, resumeErr := w.writeInterruptOutcome(sc, cp.Checksum, "cleared the hold on the plan", resumeUpdates)
		if resumeErr != nil {
			return secret, fmt.Errorf("failed to commit the resume into plan-state:%s: %w", effectiveState, resumeErr)
		}
		if committed != nil && !secretCarriesPlan(committed, cp.Checksum) {
			// writeInterruptOutcome silently abandons its write when a newer plan has arrived. Do not continue
			// from here: the in-memory Secret still contains the old plan, and using the freshly fetched
			// resourceVersion would allow the final updateSecret to overwrite the orchestrator's new plan
			// without a conflict. It could also mark the old plan as applied, causing Rancher to report InSync
			// for a plan the planner never delivered. The newer plan owns this reconcile; return and let it be
			// processed by its own reconcile.
			logrus.Infof("[k8splan] secret %s/%s carries a newer plan than %s; not resuming the plan this reconcile holds",
				w.connInfo.Namespace, w.connInfo.SecretName, cp.Checksum)
			return committed, nil
		}
		if committed != nil {
			// committed is the fresh server object writeInterruptOutcome just wrote, which may carry
			// concurrent changes (an annotation, a label, an unrelated data key) that this reconcile's
			// stale in-hand secret does not. Continue from it rather than merging only resumeUpdates
			// into the stale copy: adopting committed's resourceVersion without also adopting its data
			// would let the final updateSecret below overwrite those concurrent changes without a
			// conflict, since the resourceVersion it submits would already match the latest one.
			secret = committed.DeepCopy()
		} else {
			maps.Copy(secret.Data, resumeUpdates)
		}
	}

	output := selectExistingOutput(secret.Data, wasFailedPlan)

	periodicOutput := secret.Data[AppliedPeriodicOutputKey]

	if effectiveState.IsTerminal() && effectiveState != planapi.PlanStateSucceeded && !needsApplied {
		// A non-succeeded terminal plan is monitored only. Do not execute instructions or mutate
		// lifecycle keys such as applied-checksum and plan-progress until new pending content arrives.
		//
		// Persisted through writeInterruptOutcome, like the other interrupt monitoring paths, rather
		// than updateSecret. updateSecret's conflict retry only re-applies the write when the fresh
		// plan's checksum matches AppliedChecksumKey, but a failed or canceled plan normally leaves
		// that key empty or pointing at an older plan, so any concurrent Secret write during this
		// probe-only update would hit an un-retried conflict and escalate to Fatalf, terminating the
		// agent over what should be a routine retry.
		updates := map[string][]byte{}
		mergeProbeStatuses(updates, cp.Plan.Probes, probeStatuses)

		committed, writeErr := w.writeInterruptOutcome(sc, cp.Checksum,
			"recorded probe statuses for a monitoring-only terminal plan", updates)
		if writeErr != nil {
			return secret, fmt.Errorf("failed to record probe statuses for a monitoring-only terminal plan: %w", writeErr)
		}
		logrus.Debugf("[k8splan] terminal plan-state %q is in monitoring-only mode; enqueueing after %f seconds",
			effectiveState, w.probePeriod.Seconds())
		sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
		if committed != nil {
			return committed, nil
		}
		return secret, nil
	}

	// Transition pending -> in-progress before executing so the state is durable
	// in the event of a crash. On restart the agent will see in-progress and re-execute.
	if effectiveState == planapi.PlanStatePending && needsApplied {
		secret.Data[planapi.PlanStateKey] = []byte(planapi.PlanStateInProgress)
		secret.Data[planapi.PlanRevisionKey] = incrementCount(secret.Data[planapi.PlanRevisionKey])
		if foldResumeIntoPreCommit {
			// Only the checkpoint's Paused: false update is folded in here. Do not copy plan-state from
			// resumeUpdates because the pending -> in-progress transition takes precedence. Bump plan-revision
			// as part of that state transition, not because of the resume: the resume commit does not change
			// the plan content and therefore must not increment the revision.
			secret.Data[planapi.PlanCheckpointKey] = resumeUpdates[planapi.PlanCheckpointKey]
		}
		// Commit the transition to the API server before calling Apply. This makes the in-progress state
		// durable: if the agent crashes during Apply, the next startup sees the plan as in progress and
		// re-executes it from the beginning.
		var inProgressErr error
		if secret, inProgressErr = w.updateSecret(sc, secret); inProgressErr != nil {
			return nil, fmt.Errorf("failed to commit plan-state:%s to API server: %w", planapi.PlanStateInProgress, inProgressErr)
		}
	}

	// Step D: revalidate the interrupt annotations synchronously, using whatever is currently in
	// secret.Data/Annotations, before starting the watch that will only observe them asynchronously.
	//
	// The resume commit and the pending -> in-progress precommit above can each return a Secret
	// freshly fetched from (or just written to) the API server, which may already carry a concurrent
	// pause/cancel update that Step A's earlier, staler read could not have seen. startInterruptWatch's
	// poller does not observe such an update until its first tick, up to interruptPollInterval later —
	// long enough for a short plan to run to completion entirely unimpeded despite the annotation
	// already being set by the time this reconcile reaches Apply. Skipped in the checksum flow, which
	// does not support these annotations, exactly like Step A.
	if effectiveState != "" {
		if result, done, checkErr := w.checkAndRecordInterrupt(sc, secret, cp, planapi.PlanState(secret.Data[planapi.PlanStateKey]), probeStatuses); done {
			return result, checkErr
		}
	}

	// Step E: start the interrupt watch only for the plan-state flow. In the checksum flow, both
	// channels remain nil, and applyinator.checkInterruption treats nil channels as never ready.
	// Consequently, the interrupted-outcome path below cannot be reached in that flow.
	var cancelCh, pauseCh <-chan struct{}
	if effectiveState != "" {
		var stopWatch func()
		cancelCh, pauseCh, stopWatch = w.startInterruptWatch(ctx, sc)
		defer stopWatch()
	}

	input := applyinator.ApplyInput{
		CalculatedPlan:               cp,
		ReconcileFiles:               needsApplied,
		ExistingOneTimeOutput:        output,
		ExistingPeriodicOutput:       periodicOutput,
		RunOneTimeInstructions:       needsApplied,
		OneTimeInstructionAttempts:   planAttempt,
		Cancel:                       cancelCh,
		Pause:                        pauseCh,
		ResumeFromOneTimeInstruction: resumeFrom,
	}

	applyOutput, err := w.applyinator.Apply(ctx, input)
	if err != nil {
		return secret, fmt.Errorf("error encountered when running apply: %w", err)
	}

	// Step F: Check Interruption before OneTimeApplySucceeded. This ordering is part of the contract:
	// when cancellation stops an instruction, OneTimeApplySucceeded is false and Interruption is
	// InterruptionCanceled. Handling success first would route the result through
	// buildSecretDataUpdates and incorrectly record plan-state: failed for a plan the operator
	// intentionally stopped.
	if applyOutput.Interruption != applyinator.InterruptionNone {
		return w.recordInterruptAfterApply(sc, secret, cp, effectiveState, needsApplied, applyOutput, probeStatuses)
	}

	output = applyOutput.OneTimeOutput
	periodicOutput = applyOutput.PeriodicOutput

	outcomeUpdates := buildSecretDataUpdates(applyOutcomeInput{
		Checksum:              cp.Checksum,
		CurrentTime:           currentTime,
		NeedsApplied:          needsApplied,
		WasFailedPlan:         wasFailedPlan,
		UsesPlanState:         effectiveState != "",
		OneTimeOutput:         output,
		OneTimeApplySucceeded: applyOutput.OneTimeApplySucceeded,
		PeriodicOutput:        periodicOutput,
		PriorFailureCount:     secret.Data[FailureCountKey],
		PriorSuccessCount:     secret.Data[SuccessCountKey],
	})
	maps.Copy(secret.Data, outcomeUpdates)

	prober.DoProbes(cp.Plan.Probes, probeStatuses, needsApplied)

	marshalledProbeStatus, err := json.Marshal(probeStatuses)
	if err != nil {
		logrus.Errorf("[k8splan] error while marshalling probe statuses: %v", err)
	} else {
		secret.Data[ProbeStatusesKey] = marshalledProbeStatus
	}

	if applyOutput.OneTimeApplySucceeded == needsApplied {
		// If the one-time instructions were successfully applied,
		// we should enqueue the secret for the period of a probe to attempt to guarantee timeliness on probe reactivity.
		logrus.Debugf("[k8splan] enqueueing after %f seconds", w.probePeriod.Seconds())
		sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
	}

	if reflect.DeepEqual(originalSecret.Data, secret.Data) && reflect.DeepEqual(originalSecret.StringData, secret.StringData) {
		logrus.Debugf("[k8splan] secret data/string-data did not change, not updating secret")
		return originalSecret, nil
	}
	secret, err = w.updateSecret(sc, secret)
	if err != nil {
		logrus.Fatalf("[k8splan] encountered an error while attempting to update the secret: %v", err)
		return nil, nil
	}
	return secret, nil
}

// checkAndRecordInterrupt evaluates secret.Annotations and, if they call for an interrupt (or are
// invalid), records the outcome and reports that the caller must stop reconciling.
//
// done is false when neither annotation is active: normal processing continues, using whatever
// secret and probeStatuses the caller already has. done is true in every other case, and the
// caller must immediately return (result, err) — for an invalid value, result is secret and err is
// non-nil; for a recorded interrupt, result is the freshly written Secret (or secret when
// writeInterruptOutcome's write-once guard made that write a no-op) and err is nil, matching
// writeInterruptOutcome's own "abandon silently, do not error" contract.
//
// currentPlanState is the plan-state to evaluate the interrupt against; callers at different points
// in reconcileSecret hold different Secrets and must pass whichever plan-state theirs currently
// carries, not a value cached from earlier in the reconcile.
func (w *watcher) checkAndRecordInterrupt(sc corecontrollers.SecretController, secret *corev1.Secret, cp applyinator.CalculatedPlan,
	currentPlanState planapi.PlanState, probeStatuses map[string]planapi.ProbeStatus,
) (result *corev1.Secret, done bool, err error) {
	interrupt, interruptErr := readInterrupt(secret.Annotations)
	if interruptErr != nil {
		// Keep this path deliberately narrow: do not write the Secret, run probes, call Apply, or schedule
		// an EnqueueAfter. The workqueue's exponential rate limiter handles retries, and correcting the
		// annotation will arrive through a watch event.
		logrus.Errorf("[k8splan] refusing to act on plan secret %s/%s: %v", w.connInfo.Namespace, w.connInfo.SecretName, interruptErr)
		return secret, true, interruptErr
	}
	if interrupt == applyinator.InterruptionNone {
		return nil, false, nil
	}

	updates := handleInterrupt(interrupt, currentPlanState, secret.Data, cp.Checksum, len(cp.Plan.OneTimeInstructions))

	// An interrupt suppresses execution, not observation. Merge probe status into the same update map so
	// both outcomes can be persisted together. When probe status is unchanged, writeInterruptOutcome's
	// DeepEqual guard avoids an unnecessary Secret update.
	mergeProbeStatuses(updates, cp.Plan.Probes, probeStatuses)

	committed, writeErr := w.writeInterruptOutcome(sc, cp.Checksum, fmt.Sprintf("recorded the plan as %s", interrupt), updates)
	if writeErr != nil {
		return secret, true, fmt.Errorf("failed to record the %s interrupt: %w", interrupt, writeErr)
	}
	// Re-enqueue on the probe period, the same cadence as an executing plan.
	// An interrupt suppresses execution, not observation.
	sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
	if committed != nil {
		return committed, true, nil
	}
	return secret, true, nil
}

// recordInterruptAfterApply persists the outcome of an apply that was interrupted in flight. It
// replaces buildSecretDataUpdates for interrupted applies because OneTimeApplySucceeded is false,
// which would otherwise be recorded as plan-state: failed.
//
// An interrupted apply does not write applied-checksum because the plan was not fully applied. It
// also leaves failure and success counters unchanged because the interruption is neither a failure
// nor a successful completion. Errors from this path are returned to the caller and are never fatal.
func (w *watcher) recordInterruptAfterApply(sc corecontrollers.SecretController, secret *corev1.Secret, cp applyinator.CalculatedPlan,
	effectiveState planapi.PlanState, needsApplied bool, applyOutput applyinator.ApplyOutput, probeStatuses map[string]planapi.ProbeStatus,
) (*corev1.Secret, error) {
	if applyOutput.TerminationIncomplete {
		// Logged before the write-once guard below so the operator is told even when the lifecycle write is
		// suppressed. This is the one interrupt outcome that says something about the node rather than about
		// the plan: the plan is over, but work it started may still be running and modifying the node.
		logrus.Warnf("[k8splan] the %s interrupt of the plan with checksum %s could not confirm that every process it terminated had exited; "+
			"work started by this plan may still be modifying the node", applyOutput.Interruption, cp.Checksum)
	}

	if applyOutput.Interruption == applyinator.InterruptionCanceled && effectiveState.IsTerminal() && effectiveState != planapi.PlanStateSucceeded {
		// Apply the same write-once rule used by handleCancellation at reconcile entry. A terminal
		// plan-state (other than succeeded) is owned by the orchestrator and already inert, so
		// re-recording a cancel would permanently overwrite a plan that may already have genuinely
		// converged, with nothing left to actually stop.
		//
		// PlanStateSucceeded is excluded for the same reason handleCancellation excludes it: a succeeded
		// plan keeps executing periodic instructions, so a cancellation arriving during that periodic
		// pass has something real to stop and must be recorded, not silently dropped.
		//
		// This guard applies only to cancellation. A pause must still be recorded for a terminal plan,
		// because pausing a succeeded plan that is running only periodic instructions is a normal operator
		// action, and the resulting checkpoint is needed for resume.
		//
		// Probes and other observation continue as normal; only the lifecycle write is suppressed.
		logrus.Debugf("[k8splan] plan-state is %q (terminal); not recording the cancellation of the interrupted apply", effectiveState)
		updates := map[string][]byte{}
		mergeProbeStatuses(updates, cp.Plan.Probes, probeStatuses)
		committed, err := w.writeInterruptOutcome(sc, cp.Checksum, "recorded the probe statuses of the interrupted apply", updates)
		if err != nil {
			return secret, fmt.Errorf("failed to record the probe statuses of the interrupted apply: %w", err)
		}
		sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
		if committed != nil {
			return committed, nil
		}
		return secret, nil
	}

	progress := PlanCheckpoint{Checksum: cp.Checksum}
	// OR-ed with the stored value rather than assigned. The flag reports a hazard on the node, and this
	// apply cannot observe processes left behind by an earlier one, so a stored true must survive. Erring
	// towards reporting a hazard that has since been cleaned up is the safe direction; the opposite would
	// silently retract the warning.
	progress.TerminationIncomplete = applyOutput.TerminationIncomplete || parsePlanCheckpoint(secret.Data, cp.Checksum).TerminationIncomplete
	if needsApplied {
		// The one-time set was already running, so the Secret was transitioned to in-progress before
		// Apply started. That is the state from which the plan should resume.
		progress.ResumeState = planapi.PlanStateInProgress
		progress.Completed = applyOutput.CompletedOneTimeInstructions
		progress.Total = len(cp.Plan.OneTimeInstructions)
	} else {
		// Periodic-only: restore the state from the monitoring reconcile. Defaulting to in-progress
		// would re-execute a completed Day 2 operation from the beginning on unpause. Nothing remains
		// to resume, so Completed and Total stay 0.
		progress.ResumeState = effectiveState
	}

	state := planapi.PlanStatePaused
	progress.Paused = true
	if applyOutput.Interruption == applyinator.InterruptionCanceled {
		// A cancellation is a report, not a suspension: nothing resumes from it.
		state = planapi.PlanStateCanceled
		progress.Paused = false
		progress.ResumeState = ""
		partialCancellationLogs(progress.Completed, progress.Total)
	}

	updates := map[string][]byte{
		planapi.PlanStateKey:      []byte(state),
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(progress),
	}
	if len(applyOutput.OneTimeOutput) > 0 {
		// applied-output is fed back as ExistingOneTimeOutput by selectExistingOutput on the next apply,
		// preserving the SaveOutput results from instructions that completed before the interruption.
		updates[AppliedOutputKey] = applyOutput.OneTimeOutput
	}
	if len(applyOutput.PeriodicOutput) > 0 {
		// applied-periodic-output is fed back as ExistingPeriodicOutput on the next apply. A periodic
		// instruction that completed before the interruption already advanced its LastSuccessfulRunTime
		// in applyOutput.PeriodicOutput; without this write that update is lost, and the stale timestamp
		// still on the Secret makes periodicInstructionDue treat the instruction as due again on resume.
		updates[AppliedPeriodicOutputKey] = applyOutput.PeriodicOutput
	}
	mergeProbeStatuses(updates, cp.Plan.Probes, probeStatuses)

	committed, err := w.writeInterruptOutcome(sc, cp.Checksum,
		fmt.Sprintf("recorded the %s outcome of the interrupted apply", applyOutput.Interruption), updates)
	if err != nil {
		return secret, fmt.Errorf("failed to record the %s outcome of the interrupted apply: %w", applyOutput.Interruption, err)
	}
	sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
	if committed != nil {
		return committed, nil
	}
	return secret, nil
}

// resumeCommitUpdates returns the Secret updates needed to release a suspension, or nil when the
// plan was not suspended. The caller has already established that both interrupt annotations are
// inactive, so the only remaining question is whether a suspension was recorded: plan-state is
// paused, or the checkpoint for this plan indicates one.
//
// Completed, Total and TerminationIncomplete are preserved as a record of what happened to the plan;
// only the fields that grant a resume are reset. Paused: false revokes the checkpoint's ability to
// trigger another resume, and ResumeState is cleared because it has already been consumed into
// effectiveState.
func resumeCommitUpdates(currentPlanState, effectiveState planapi.PlanState, data map[string][]byte, checksum string) map[string][]byte {
	progress := parsePlanCheckpoint(data, checksum)
	if currentPlanState != planapi.PlanStatePaused && !progress.Paused {
		return nil
	}
	// Set these fields explicitly: parsePlanProgress returns zero values when plan-state is paused
	// without a checkpoint, and an unscoped checkpoint would be discarded on the next read.
	progress.Checksum = checksum
	progress.Paused = false
	progress.ResumeState = ""
	return map[string][]byte{
		planapi.PlanStateKey:      []byte(effectiveState),
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(progress),
	}
}

// mergeProbeStatuses runs the plan's probes and merges their marshalled statuses into updates.
// Interrupts suppress execution, not observation: probe statuses must continue to update so
// Rancher's MachineHealthCheck does not consume stale health data from a node left in a partial
// state by an interrupted apply.
//
// initial is false because no plan is applied on either interrupt path, so the initial delay intended
// to let a freshly applied plan settle does not apply.
func mergeProbeStatuses(updates map[string][]byte, probes map[string]planapi.Probe, probeStatuses map[string]planapi.ProbeStatus) {
	prober.DoProbes(probes, probeStatuses, false)
	marshalled, err := json.Marshal(probeStatuses)
	if err != nil {
		logrus.Errorf("[k8splan] error while marshalling probe statuses: %v", err)
		return
	}
	updates[ProbeStatusesKey] = marshalled
}

// partialCancellationLogs reports a cancellation that arrived between instructions. This is the
// interrupt outcome that requires operator attention: the plan is terminal, so no later apply will
// complete the remaining work.
func partialCancellationLogs(completed, total int) {
	if completed <= 0 || completed >= total {
		return
	}
	logrus.Warnf("[k8splan] the plan was cancelled after %d of %d one-time instructions; this node may be left in an inconsistent state",
		completed, total)
}

// clampResumeFrom bounds a checkpoint's instruction index to the plan it will be applied to.
// resolveResume returns the stored Completed value unchanged, so a manually edited or truncated
// checkpoint could contain an out-of-range value such as 999 or a negative index. Applyinator also
// clamps the value internally; this check provides defense in depth at the boundary where an
// operator-controlled value enters the apply flow.
func clampResumeFrom(resumeFrom, total int) int {
	if resumeFrom < 0 {
		logrus.Warnf("[k8splan] resume checkpoint %d is negative; resuming from the first one-time instruction instead", resumeFrom)
		return 0
	}
	if resumeFrom > total {
		logrus.Warnf("[k8splan] resume checkpoint %d exceeds the plan's %d one-time instructions; resuming from %d instead", resumeFrom, total, total)
		return total
	}
	return resumeFrom
}
