package k8splan

// This file enforces the plan-state execution invariant:
//
// The agent executes a plan only when both PlanPausedAnnotation and PlanCanceledAnnotation are
// explicitly inactive: absent or set to "false". Any other value suppresses execution, regardless
// of the plan state, checkpoint, or how the agent reached reconciliation.
//
// This is intentionally a whitelist of executable states rather than a blacklist of interrupted
// states. Unknown values therefore fail closed and suppress execution without requiring an explicit
// rule for every possible value. This protects against a particularly dangerous failure mode:
// an interruption appears to work, but the plan executes after the agent restarts.
//
// An interrupt suppresses execution, not observation. While either annotation is active, the agent
// skips Apply but continues running probes and persisting their statuses. This keeps health data
// current for Rancher's MachineHealthCheck, especially for nodes that may be unhealthy. As a result,
// handleInterrupt returns only lifecycle-key updates; the caller merges probe statuses into the same map.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/applyinator"
	corecontrollers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

// parseInterruptAnnotation reports whether the given annotation requests an interrupt. The only
// accepted values are "true" and "false"; an absent annotation is treated as "false". Any other
// value is considered an operator configuration error and is returned as an error rather than
// silently coerced.
func parseInterruptAnnotation(annotations map[string]string, key string) (bool, error) {
	v, ok := annotations[key]
	if !ok {
		return false, nil
	}
	switch v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("annotation %s has invalid value %q, must be \"true\" or \"false\"", key, v)
	}
}

// readInterrupt evaluates both interrupt annotations and determines whether the plan should run.
// The annotations are evaluated in this order:
//  1. A valid canceled == "true" takes precedence, even if the pause value is invalid.
//  2. If cancellation is not active, any invalid annotation value is returned as an error.
//  3. A valid paused == "true" pauses the plan.
//  4. Otherwise, the plan may run.
func readInterrupt(annotations map[string]string) (applyinator.Interruption, error) {
	canceled, cancelErr := parseInterruptAnnotation(annotations, planapi.PlanCanceledAnnotation)
	paused, pauseErr := parseInterruptAnnotation(annotations, planapi.PlanPausedAnnotation)

	if cancelErr == nil && canceled {
		return applyinator.InterruptionCanceled, nil
	}
	if cancelErr != nil || pauseErr != nil {
		return applyinator.InterruptionNone, errors.Join(cancelErr, pauseErr)
	}
	if paused {
		return applyinator.InterruptionPaused, nil
	}
	return applyinator.InterruptionNone, nil
}

// interruptPollInterval controls how often the interrupt watch re-reads the plan Secret during an
// in-flight apply.
// It is a variable rather than a constant so tests can shorten the interval;
// see withInterruptPollInterval.
var interruptPollInterval = 2 * time.Second

// startInterruptWatch polls the plan Secret while an apply is in flight and closes the channel
// corresponding to the first interrupt it observes. The controller cannot handle the annotation
// change directly because Applyinator.Apply runs synchronously in the OnChange handler and
// DefaultWorkers is 1, leaving the workqueue worker occupied for the duration of the apply.
// The informer's indexer is updated independently of the worker, so reads from the cache continue
// to reflect changes while an apply is running.
//
// The caller must defer the returned stop function.
func (w *watcher) startInterruptWatch(ctx context.Context, sc corecontrollers.SecretController) (cancelCh, pauseCh <-chan struct{}, stop func()) {
	cancel := make(chan struct{})
	pause := make(chan struct{})
	stopCh := make(chan struct{})
	done := make(chan struct{})

	go w.pollInterrupts(ctx, sc, cancel, pause, stopCh, done)

	var once sync.Once
	return cancel, pause, func() {
		once.Do(func() { close(stopCh) })
		// Wait for the goroutine to exit so a returned stop() is a guarantee that nothing is
		// still reading the Secret. Receiving from a closed channel never blocks, so calling
		// stop() again is free.
		<-done
	}
}

// pollInterrupts is the goroutine body for startInterruptWatch. It is the sole writer to cancel
// and pause, so plain bools are sufficient to ensure each channel is closed at most once; no
// synchronization primitive such as sync.Once is needed.
func (w *watcher) pollInterrupts(ctx context.Context, sc corecontrollers.SecretController, cancel, pause, stopCh, done chan struct{}) {
	defer close(done)

	ticker := time.NewTicker(interruptPollInterval)
	defer ticker.Stop()

	var cancelClosed, pauseClosed, errorLogged bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
		}

		annotations, ok := w.readInterruptAnnotations(sc)
		if !ok {
			continue // a transient read failure must not interrupt anything
		}

		// Delegated to readInterrupt so validity is decided in exactly one place.
		interrupt, err := readInterrupt(annotations)
		if err != nil {
			// This is the deliberate divergence between the reconcile-entry and in-flight paths. Interrupting
			// an in-flight apply is destructive — especially cancellation — and acting on an invalid value would
			// mean guessing the operator's intent. At reconcile entry, the agent can safely reject invalid
			// input because no work has started yet. During an apply, the watcher instead reports the invalid
			// value and leaves the apply running until a subsequent poll sees a valid value.
			//
			// The distinction is between "do not start" and "do not kill", not between "hold" and "run".
			if !errorLogged {
				// Rate-limited to once per watch: the alternative is this line every 2s for the
				// whole duration of the apply.
				errorLogged = true
				logrus.Errorf("[k8splan] not interrupting the in-flight apply: %v", err)
			}
			continue
		}

		switch interrupt {
		case applyinator.InterruptionCanceled:
			if !cancelClosed {
				logrus.Infof("[k8splan] %s observed during an in-flight apply; cancelling it", planapi.PlanCanceledAnnotation)
				close(cancel)
				cancelClosed = true
			}
		case applyinator.InterruptionPaused:
			if !pauseClosed {
				logrus.Infof("[k8splan] %s observed during an in-flight apply; pausing at the next instruction boundary", planapi.PlanPausedAnnotation)
				close(pause)
				pauseClosed = true
			}
		case applyinator.InterruptionNone:
		}

		if cancelClosed && pauseClosed {
			return // nothing left to observe
		}
	}
}

// readInterruptAnnotations reads the plan Secret's annotations from the informer cache, falling
// back to the live client when the cache lookup fails. If both reads fail, it returns !ok and logs
// at debug level; the caller keeps polling rather than treating a transient read failure as an
// interrupt.
//
// Informer cache objects are shared and must be treated as read-only. This function only reads
// Annotations, does not mutate the object, and does not retain the object or annotation map beyond
// the current poll.
func (w *watcher) readInterruptAnnotations(sc corecontrollers.SecretController) (map[string]string, bool) {
	secret, err := sc.Cache().Get(w.connInfo.Namespace, w.connInfo.SecretName)
	if err == nil {
		return secret.Annotations, true
	}
	logrus.Debugf("[k8splan] interrupt watch could not read secret %s/%s from cache, falling back to the API server: %v",
		w.connInfo.Namespace, w.connInfo.SecretName, err)

	secret, err = sc.Get(w.connInfo.Namespace, w.connInfo.SecretName, metav1.GetOptions{})
	if err != nil {
		logrus.Debugf("[k8splan] interrupt watch could not read secret %s/%s: %v", w.connInfo.Namespace, w.connInfo.SecretName, err)
		return nil, false
	}
	return secret.Annotations, true
}

// handleInterrupt computes the Secret data updates for an interrupt detected at reconcile entry.
// It returns an empty map when the interrupt has already been recorded; the caller merges any
// returned updates into the Secret and emits the corresponding logs.
//
// The interrupt is recorded only once. Reconciliation runs periodically while a plan is paused,
// so rewriting the Secret on every pass would repeatedly update it and could recompute Completed
// when no apply is in flight, potentially overwriting a checkpoint that contains real progress.
// Because the checkpoint is persisted, the same rule also preserves progress across agent restarts:
// if the annotation remains set, the agent recognizes the existing suspension and leaves its
// predecessor's checkpoint intact. ResumeState is therefore captured exactly once, when the
// suspension is first recorded.
//
// The agent does not modify the interrupt annotations; they are owned by the orchestrator.
func handleInterrupt(interrupt applyinator.Interruption, currentPlanState planapi.PlanState, data map[string][]byte,
	checksum string, totalOneTimeInstructions int) map[string][]byte {
	switch interrupt {
	case applyinator.InterruptionCanceled:
		return handleCancellation(currentPlanState, data, checksum, totalOneTimeInstructions)
	case applyinator.InterruptionPaused:
		return handlePause(currentPlanState, data, checksum, totalOneTimeInstructions)
	case applyinator.InterruptionNone:
		// Unreachable by contract: the caller only reaches this function for a real interrupt.
		// Writing nothing is the safe response to an input that was not supposed to arrive.
		logrus.Debugf("[k8splan] handleInterrupt called with no interruption; nothing to record")
		return map[string][]byte{}
	}
	logrus.Debugf("[k8splan] handleInterrupt called with unknown interruption %q; nothing to record", interrupt)
	return map[string][]byte{}
}

// handleCancellation records cancellation as a terminal plan state together with a report of how
// far the plan progressed. Unlike a suspension, cancellation does not preserve resumable state:
// Paused is false and ResumeState is empty because there is nothing to resume.
func handleCancellation(currentPlanState planapi.PlanState, data map[string][]byte, checksum string, totalOneTimeInstructions int) map[string][]byte {
	if currentPlanState.IsTerminal() {
		// This also enforces cancellation's write-once rule. The guard is based on plan state rather than
		// the checkpoint: PlanStateCanceled is terminal, so an already-recorded cancellation produces no
		// further writes. Cancellation uses plan state because it does not create a resumable checkpoint,
		// unlike pause, which can use the checkpoint to determine whether the suspension was already recorded.
		logrus.Debugf("[k8splan] plan-state is %q (terminal); not recording the cancellation", currentPlanState)
		return map[string][]byte{}
	}

	existing := parsePlanCheckpoint(data, checksum)
	updates := map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStateCanceled),
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{
			Checksum:  checksum,
			Completed: existing.Completed,
			Total:     totalOneTimeInstructions,
			// Carried over rather than recomputed: no apply is in flight on this path, so this reconcile
			// cannot observe anything about the node's processes, and dropping the flag would silently
			// retract a warning that is still true.
			TerminationIncomplete: existing.TerminationIncomplete,
			// ResumeState and Paused are deliberately left zero: a cancellation is a report, and
			// only a suspended checkpoint ever grants a resume.
		}),
	}
	logrus.Infof("[k8splan] %s is observed; recording plan-state %q after %d of %d one-time instructions",
		planapi.PlanCanceledAnnotation, planapi.PlanStateCanceled, existing.Completed, totalOneTimeInstructions)
	partialCancellationLogs(existing.Completed, totalOneTimeInstructions)
	return updates
}

// handlePause records a suspension by preserving a non-terminal plan state and the checkpoint from
// which the plan should resume once the pause annotation is removed.
func handlePause(currentPlanState planapi.PlanState, data map[string][]byte, checksum string, totalOneTimeInstructions int) map[string][]byte {
	existing := parsePlanCheckpoint(data, checksum)
	if existing.Paused {
		// Pause's write-once guard keys off the checkpoint, not off plan-state. The checkpoint is
		// the thing that must not be recomputed, so it is the thing to test; and it is the more
		// precise signal, because plan-state == paused with no checkpoint beneath it is a state
		// this guard must NOT suppress — there is a suspension to record for the first time.
		logrus.Debugf("[k8splan] suspension already recorded for checksum %s at %d of %d one-time instructions; not rewriting it",
			checksum, existing.Completed, existing.Total)
		return map[string][]byte{}
	}

	resumeState := currentPlanState
	if resumeState == planapi.PlanStatePaused {
		// The plan state says paused, but there is no checkpoint to support it - either the Secret was edited
		// manually or the checkpoint was lost. A paused state without a checkpoint cannot safely be used as
		// a resume point: resolveResume would pass it to decidePlanStateAction, which treats unknown states
		// as terminal and could leave the plan permanently stalled after the pause is removed.
		// Leave the resumeState empty so resolveResume falls back to its in-progress default and restarts
		// from instruction 0. Re-executing from the beginning is safe; silently stalling is not.
		resumeState = ""
	}

	updates := map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePaused),
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{
			Checksum:  checksum,
			Completed: existing.Completed,
			Total:     totalOneTimeInstructions,
			// Carried over for the same reason as in handleCancellation: no apply is in flight here.
			TerminationIncomplete: existing.TerminationIncomplete,
			ResumeState:           resumeState,
			Paused:                true,
		}),
	}
	if resumeState.IsTerminal() {
		logrus.Infof("[k8splan] %s is set; the plan is not executing one-time instructions, holding it in plan-state %q to resume into %q",
			planapi.PlanPausedAnnotation, planapi.PlanStatePaused, resumeState)
		return updates
	}
	logrus.Infof("[k8splan] %s is set; holding the plan at %d of %d one-time instructions, to resume into plan-state %q",
		planapi.PlanPausedAnnotation, existing.Completed, totalOneTimeInstructions, orDefault(resumeState, planapi.PlanStateInProgress))
	return updates
}

// writeInterruptOutcome persists the outcome of an interrupted apply using a fresh copy of the Secret.
// It re-reads the Secret from the API server, verifies that it still represents the interrupted plan,
// merges the updates, and retries the entire read-modify-write operation on conflict. If there are no
// changes to write, it skips the Update. Errors are returned to the caller rather than treated as fatal.
//
// This uses a separate write path from updateSecret because updateSecret's conflict handling only
// preserves data when ck.Checksum matches the AppliedChecksumKey currently stored on the server. The
// interrupted path intentionally does not write the applied checksum, so a newly canceled plan can
// fail that check against the previous plan's checksum. updateSecret would then return an error that
// reconcileSecret treats as fatal.
//
// A conflict is expected here rather than exceptional: the operator's annotation update changes the
// Secret's resourceVersion while the agent may still hold the copy read before the apply began. The
// outcome write must therefore retry from a fresh Secret. Using updateSecret could cause cancellation
// to crash while pause might lose the checkpoint and applied output, causing an unpause to restart
// from instruction 0 and defeating resumability.
//
// An empty updates map is valid and commonly produced by the write-once guard. In that case the
// Secret is still read to verify freshness, but no Update is issued.
//
// reason names what the write records, in the past tense, and is the subject of the log line emitted
// on a successful Update. It exists because this function serves both ends of an interrupt: the write
// that records a hold and the write that clears one.
func (w *watcher) writeInterruptOutcome(sc corecontrollers.SecretController, checksum, reason string, updates map[string][]byte) (*corev1.Secret, error) {
	var result *corev1.Secret
	// The retry wraps the whole read-modify-write, not just the Update: a conflict means the copy
	// being merged into is stale, so it has to be re-read.
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest, err := sc.Get(w.connInfo.Namespace, w.connInfo.SecretName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !secretCarriesPlan(latest, checksum) {
			// A newer plan has landed; that plan's own reconcile owns the state. Not an error.
			logrus.Infof("[k8splan] secret %s/%s no longer carries the plan with checksum %s; abandoning the interrupt write",
				w.connInfo.Namespace, w.connInfo.SecretName, checksum)
			result = latest
			return nil
		}

		updated := latest.DeepCopy()
		if len(updates) > 0 && updated.Data == nil {
			// Defensive: secretCarriesPlan means Data is non-nil here today. Only initialise when
			// there is something to write, so an empty updates map cannot turn a nil Data into an
			// empty map and make the DeepEqual below spuriously report a change.
			updated.Data = map[string][]byte{}
		}
		maps.Copy(updated.Data, updates)
		if reflect.DeepEqual(updated.Data, latest.Data) {
			logrus.Debugf("[k8splan] interrupt outcome for secret %s/%s changed nothing, not updating secret", w.connInfo.Namespace, w.connInfo.SecretName)
			result = latest
			return nil
		}

		resulting, updateErr := sc.Update(updated)
		if updateErr != nil {
			return updateErr
		}
		// Recorded exactly as updateSecret does, so the next cache delivery is not rejected as
		// stale by reconcileSecret's rvIsOlder check.
		if w.secretUID == "" {
			w.secretUID = string(resulting.UID)
		}
		w.lastAppliedResourceVersion = resulting.ResourceVersion
		result = resulting
		logrus.Infof("[k8splan] %s on plan secret %s/%s", reason, w.connInfo.Namespace, w.connInfo.SecretName)
		return nil
	})
	return result, err
}

// secretCarriesPlan reports whether secret still holds the plan identified by checksum. An absent
// or unparsable plan counts as "not this plan": either way the agent has nothing to attribute an
// interrupt outcome to.
func secretCarriesPlan(secret *corev1.Secret, checksum string) bool {
	planData, ok := secret.Data[PlanKey]
	if !ok {
		return false
	}
	cp, err := applyinator.CalculatePlan(planData)
	if err != nil {
		return false
	}
	return cp.Checksum == checksum
}
