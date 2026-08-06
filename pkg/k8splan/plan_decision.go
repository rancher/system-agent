package k8splan

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/sirupsen/logrus"
)

// parseLastApplyTime reads LastApplyTimeKey from data, falling back to currentTime when the key
// is absent or its value cannot be parsed with time.UnixDate.
func parseLastApplyTime(data map[string][]byte, currentTime time.Time) time.Time {
	rawLAT, ok := data[LastApplyTimeKey]
	if !ok {
		return currentTime
	}
	parsed, err := time.Parse(time.UnixDate, string(rawLAT))
	if err != nil {
		logrus.Errorf("[K8s] error parsing last apply time %s, using current time", string(rawLAT))
		return currentTime
	}
	return parsed
}

// parseProbePeriodOverride reads ProbePeriodKey (a whole number of seconds) from data, returning
// current unchanged when the key is absent or its value cannot be parsed.
func parseProbePeriodOverride(data map[string][]byte, current time.Duration) time.Duration {
	rawPeriod, ok := data[ProbePeriodKey]
	if !ok {
		return current
	}
	parsedPeriod, err := time.ParseDuration(fmt.Sprintf("%ss", string(rawPeriod)))
	if err != nil {
		logrus.Errorf("[K8s] error parsing duration %ss, using default", string(rawPeriod))
		return current
	}
	return parsedPeriod
}

// parseProbeStatuses reads and JSON-decodes ProbeStatusesKey from data, returning an empty map
// when the key is absent or its value cannot be decoded.
func parseProbeStatuses(data map[string][]byte) map[string]planapi.ProbeStatus {
	rawProbeStatusByteData, ok := data[ProbeStatusesKey]
	if !ok {
		return make(map[string]planapi.ProbeStatus)
	}
	var probeStatuses map[string]planapi.ProbeStatus
	if err := json.Unmarshal(rawProbeStatusByteData, &probeStatuses); err != nil {
		logrus.Errorf("[K8s] error while parsing probe statuses: %v", err)
		return make(map[string]planapi.ProbeStatus)
	}
	return probeStatuses
}

// parseFailureCount reads FailureCountKey from data. planAttempt is failureCount+1 when the key
// is present and non-empty (even if its value fails to parse as a number, matching pre-refactor
// behavior), or 1 when the key is absent or empty.
func parseFailureCount(data map[string][]byte) (failureCount, planAttempt int) {
	rawFailureCount, ok := data[FailureCountKey]
	if !ok || len(rawFailureCount) == 0 {
		return 0, 1
	}
	if fc, err := strconv.Atoi(string(rawFailureCount)); err == nil {
		failureCount = fc
	}
	return failureCount, failureCount + 1
}

// planStateResult is the outcome of evaluating the plan-state flow (the flow used when the
// orchestrator writes plan-state — see the currentPlanState != "" branch in reconcileSecret).
//
// This is the seam the pause/cancel design's gate() function will later replace or extend with
// cancelled/paused/progress-checkpoint handling; it deliberately does not implement any of that.
type planStateResult struct {
	NeedsApplied bool
	// ResetPlanAttempt is true only for PlanStatePending: a freshly delivered plan always starts
	// its one-time instructions at attempt 1, regardless of any prior failure count.
	ResetPlanAttempt bool
}

// decidePlanStateAction mirrors the plan-state switch: pending and in-progress both require
// (re-)execution, every other state (including states not yet known to this build) is terminal.
func decidePlanStateAction(state planapi.PlanState) planStateResult {
	switch state {
	case planapi.PlanStatePending:
		return planStateResult{NeedsApplied: true, ResetPlanAttempt: true}
	case planapi.PlanStateInProgress:
		return planStateResult{NeedsApplied: true}
	default:
		return planStateResult{NeedsApplied: false}
	}
}

// checksumFlowResult is the outcome of evaluating the checksum flow (the backward-compatible flow
// used when the orchestrator never writes plan-state).
type checksumFlowResult struct {
	NeedsApplied bool
	// WasFailedPlan is true when the plan previously failed with the same checksum being
	// evaluated now.
	WasFailedPlan bool
	// HasRunOnce is the (possibly updated) value the caller should persist for the next call.
	HasRunOnce bool
	// ClearAppliedChecksum is true when the caller should reset AppliedChecksumKey to "" before
	// applying — set only on the very first run, so a subsequent crash-restart is unambiguous.
	ClearAppliedChecksum bool
}

// decideChecksumFlowAction mirrors the pre-refactor checksum-flow branch of the OnChange closure.
// data is the Secret's data map (read-only here); planChecksum is the checksum of the plan
// currently being evaluated; resourceVersionUnchanged reports whether the Secret's resource
// version matches the last one this watcher processed.
func decideChecksumFlowAction(data map[string][]byte, planChecksum string, hasRunOnce bool, failureCount int, currentTime, lastApplyTime time.Time, cooldownPeriod time.Duration, resourceVersionUnchanged bool) checksumFlowResult {
	needsApplied := true
	wasFailedPlan := false
	clearAppliedChecksum := false

	if secretChecksumData, ok := data[AppliedChecksumKey]; ok {
		if string(secretChecksumData) == planChecksum {
			needsApplied = false
		}
	}

	if !hasRunOnce {
		needsApplied = true
		hasRunOnce = true
		clearAppliedChecksum = true
	}

	// TODO(Task 12): replaced by the shared parseIntFromBytes dedup helper.
	maxFailureThreshold := parseIntFromBytes(data[MaxFailuresKey], -1)

	if failureCount != 0 {
		if rFC, ok := data[FailedChecksumKey]; ok && string(rFC) == planChecksum {
			wasFailedPlan = true
			switch {
			case failureCount >= maxFailureThreshold && maxFailureThreshold != -1:
				needsApplied = false
			case !currentTime.Equal(lastApplyTime) && !currentTime.After(lastApplyTime.Add(cooldownPeriod)):
				needsApplied = false
			}
		}
	}

	if resourceVersionUnchanged && !wasFailedPlan {
		needsApplied = false
	}

	return checksumFlowResult{
		NeedsApplied:         needsApplied,
		WasFailedPlan:        wasFailedPlan,
		HasRunOnce:           hasRunOnce,
		ClearAppliedChecksum: clearAppliedChecksum,
	}
}

// TODO(Task 12): replaced by the shared parseIntFromBytes dedup helper.
func parseIntFromBytes(raw []byte, fallback int) int {
	if len(raw) == 0 {
		return fallback
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil {
		return fallback
	}
	return n
}

// selectExistingOutput picks the existing one-time-instruction output to carry into the next
// apply: the failed output when the plan previously failed, the applied output otherwise. Returns
// an empty (non-nil) slice when the relevant key is absent.
func selectExistingOutput(data map[string][]byte, wasFailedPlan bool) []byte {
	key := AppliedOutputKey
	if wasFailedPlan {
		key = FailedOutputKey
	}
	output, ok := data[key]
	if !ok {
		return []byte{}
	}
	return output
}
