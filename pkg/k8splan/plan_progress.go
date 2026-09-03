package k8splan

import (
	"encoding/json"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/sirupsen/logrus"
)

type PlanCheckpoint planapi.PlanCheckpoint

// parsePlanCheckpoint decodes the checkpoint stored under planapi.PlanCheckpointKey.
//   - key absent, malformed JSON, or a checksum that does not match the plan being reconciled:
//     the zero value.
//   - checksum matches: returned verbatim, whichever agent lifetime wrote it.
func parsePlanCheckpoint(data map[string][]byte, checksum string) PlanCheckpoint {
	raw, ok := data[planapi.PlanCheckpointKey]
	if !ok || len(raw) == 0 {
		// An empty value represents a cleared checkpoint, not malformed data. Treat it as absent to avoid
		// logging a spurious decode error on every reconcile. The checkpoint must be cleared by writing an
		// empty value rather than deleting the key; see secretConflictMergeKeys.
		return PlanCheckpoint{}
	}
	var p PlanCheckpoint
	if err := json.Unmarshal(raw, &p); err != nil {
		// Operator-visible corruption, but not fatal: the zero value costs at worst a
		// re-execution, which is always safe.
		logrus.Errorf("[k8splan] error while parsing plan progress: %v", err)
		return PlanCheckpoint{}
	}
	if p.Checksum != checksum {
		logrus.Debugf("[k8splan] discarding plan progress recorded for checksum %s while reconciling checksum %s", p.Checksum, checksum)
		return PlanCheckpoint{}
	}
	return p
}

// marshalPlanCheckpoint encodes p for storage under planapi.PlanCheckpointKey.
func marshalPlanCheckpoint(p PlanCheckpoint) []byte {
	raw, err := json.Marshal(p)
	if err != nil {
		// Unreachable: a struct of strings, ints and a bool cannot fail to marshal.
		logrus.Errorf("[k8splan] error while marshalling plan progress: %v", err)
		return nil
	}
	return raw
}

// resolveResume determines the state and resume position for a reconcile based on the stored plan
// state and checkpoint. A plan is considered suspended when either plan-state is paused or a valid
// checkpoint claims the plan. The checkpoint case preserves resumability across agent restarts and
// handles cases where an external write changed plan-state without clearing the checkpoint.
// All other states pass through unchanged with resumeFrom set to 0.
//
// Precondition: both interrupt annotations have already been validated as explicitly false. This
// function only determines how to leave an existing suspension; it does not decide whether the
// plan may leave one. A plan that remains held, or whose annotation cannot be parsed, never reaches
// this function.
func resolveResume(state planapi.PlanState, data map[string][]byte, checksum string) (planapi.PlanState, int) {
	if state == "" { // checksum flow: no checkpoint is ever written, nothing to resolve
		return state, 0
	}
	p := parsePlanCheckpoint(data, checksum)
	if state != planapi.PlanStatePaused && !p.Paused {
		return state, 0
	}
	if !p.Paused {
		// plan-state says paused but no suspended checkpoint vouches for any progress: a
		// hand-edited Secret, or a cancel report on a plan someone then paused. Resume the state,
		// not the position.
		return orDefault(sanitizeResumeState(p.ResumeState), planapi.PlanStateInProgress), 0
	}
	return orDefault(sanitizeResumeState(p.ResumeState), planapi.PlanStateInProgress), p.Completed
}

// sanitizeResumeState rejects a stored ResumeState of planapi.PlanStatePaused and treats it as unset, allowing
// orDefault to fall back to PlanStateInProgress.
//
// Resuming into paused would cause a silent permanent stall: decidePlanStateAction treats unknown
// states as terminal, so the plan would never execute again or leave the paused state once the
// annotation is removed. handleInterrupt prevents writing such a checkpoint; this guard handles
// hand-edited Secrets and checkpoints written by older versions that lacked that protection.
// Restarting from instruction 0 is safe; silently stalling is not.
func sanitizeResumeState(state planapi.PlanState) planapi.PlanState {
	if state != planapi.PlanStatePaused {
		return state
	}
	logrus.Warnf("[k8splan] ignoring a stored resume state of %q, which would stall the plan permanently; resuming into %q instead",
		planapi.PlanStatePaused, planapi.PlanStateInProgress)
	return ""
}

// orDefault returns v unless it is the zero value, in which case it returns def.
func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}
