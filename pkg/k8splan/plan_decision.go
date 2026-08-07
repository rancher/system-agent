package k8splan

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/sirupsen/logrus"
)

// planStateResult is the outcome of evaluating the plan-state flow (the flow used when the
// orchestrator writes plan-state — see the currentPlanState != "" branch in reconcileSecret).
type planStateResult struct {
	NeedsApplied bool
	// ResetPlanAttempt is true only for PlanStatePending: a freshly delivered plan always starts
	// its one-time instructions at attempt 1, regardless of any prior failure count.
	ResetPlanAttempt bool
	// Logs explains the decision; the caller emits them via emitDecisionLogs.
	Logs []decisionLog
}

// decidePlanStateAction mirrors the plan-state switch: pending and in-progress both require
// (re-)execution, every other state (including states not yet known to this build) is terminal.
func decidePlanStateAction(state planapi.PlanState) planStateResult {
	switch state {
	case planapi.PlanStatePending:
		return planStateResult{
			NeedsApplied:     true,
			ResetPlanAttempt: true,
			Logs:             []decisionLog{infoDecision("plan-state is %q; applying new plan content", state)},
		}
	case planapi.PlanStateInProgress:
		return planStateResult{
			NeedsApplied: true,
			Logs:         []decisionLog{infoDecision("plan-state is %q on startup; re-executing plan (crash recovery)", state)},
		}
	default:
		return planStateResult{
			NeedsApplied: false,
			Logs:         []decisionLog{debugDecision("plan-state is %q (terminal); not applying", state)},
		}
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
	// Logs explains the decision; the caller emits them via emitDecisionLogs.
	Logs []decisionLog
}

// decideChecksumFlowAction mirrors the pre-refactor checksum-flow branch of the OnChange closure.
// data is the Secret's data map (read-only here); planChecksum is the checksum of the plan
// currently being evaluated; resourceVersionUnchanged reports whether the Secret's resource
// version matches the last one this watcher processed.
func decideChecksumFlowAction(data map[string][]byte, planChecksum string, hasRunOnce bool, failureCount int, currentTime, lastApplyTime time.Time, cooldownPeriod time.Duration, resourceVersionUnchanged bool, lastAppliedResourceVersion string) checksumFlowResult {
	needsApplied := true
	wasFailedPlan := false
	clearAppliedChecksum := false
	var logs []decisionLog

	if secretChecksumData, ok := data[AppliedChecksumKey]; ok {
		logs = append(logs, traceDecision("Remote plan had an applied checksum value of %s", string(secretChecksumData)))
		if string(secretChecksumData) == planChecksum {
			logs = append(logs, debugDecision("Applied checksum was the same as the plan from remote. Not applying."))
			needsApplied = false
		}
	}

	if !hasRunOnce {
		logs = append(logs, infoDecision("Detected first start, force-applying one-time instruction set"))
		needsApplied = true
		hasRunOnce = true
		clearAppliedChecksum = true
	}

	maxFailureThreshold, thresholdLogs := parseMaxFailures(data)
	logs = append(logs, thresholdLogs...)

	if failureCount != 0 {
		if rFC, ok := data[FailedChecksumKey]; ok {
			if string(rFC) == planChecksum {
				logs = append(logs, debugDecision("Plan appears to have failed before, failure count was %d", failureCount))
				wasFailedPlan = true
				switch {
				case failureCount >= maxFailureThreshold && maxFailureThreshold != -1:
					logs = append(logs, errorDecision("Maximum failure threshold exceeded for plan with checksum value of %s, (failures: %d, threshold: %d)", planChecksum, failureCount, maxFailureThreshold))
					needsApplied = false
				case !currentTime.Equal(lastApplyTime) && !currentTime.After(lastApplyTime.Add(cooldownPeriod)):
					logs = append(logs, debugDecision("%f second cooldown timer for failed plan application has not passed yet.", cooldownPeriod.Seconds()))
					needsApplied = false
				}
			} else {
				logs = append(logs, errorDecision("Received plan checksum (%s) did not match failed plan checksum (%s) and failure count was greater than zero. Cancelling failure cooldown.", planChecksum, string(rFC)))
			}
		}
	}

	if resourceVersionUnchanged && !wasFailedPlan {
		logs = append(logs, debugDecision("last applied resource version (%s) did not change. running probes, skipping apply.", lastAppliedResourceVersion))
		needsApplied = false
	}

	return checksumFlowResult{
		NeedsApplied:         needsApplied,
		WasFailedPlan:        wasFailedPlan,
		HasRunOnce:           hasRunOnce,
		ClearAppliedChecksum: clearAppliedChecksum,
		Logs:                 logs,
	}
}

// parseLastApplyTime reads LastApplyTimeKey from data, falling back to currentTime when the key
// is absent or its value cannot be parsed with time.UnixDate.
func parseLastApplyTime(data map[string][]byte, currentTime time.Time) time.Time {
	rawLAT, ok := data[LastApplyTimeKey]
	if !ok {
		return currentTime
	}
	parsed, err := time.Parse(time.UnixDate, string(rawLAT))
	if err != nil {
		logrus.Errorf("[k8splan] error parsing last apply time %s, using current time", string(rawLAT))
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
		logrus.Errorf("[k8splan] error parsing duration %ss, using default", string(rawPeriod))
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
		logrus.Errorf("[k8splan] error while parsing probe statuses: %v", err)
		return make(map[string]planapi.ProbeStatus)
	}
	return probeStatuses
}

// parseFailureCount reads FailureCountKey from data. planAttempt is failureCount+1 when the key is
// present, non-empty, and parses as a number; otherwise it is 1 (matching pre-refactor behavior).
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

// parseMaxFailures reads MaxFailuresKey, returning -1 (no threshold) when the key is absent,
// empty, or unparsable. An unparsable value is an operator-visible misconfiguration, so it is
// reported at error level rather than silently defaulted.
func parseMaxFailures(data map[string][]byte) (int, []decisionLog) {
	raw, ok := data[MaxFailuresKey]
	if !ok || len(raw) == 0 {
		return -1, nil
	}
	threshold, err := strconv.Atoi(string(raw))
	if err != nil {
		return -1, []decisionLog{errorDecision("error parsing max-failures: %s: %v", string(raw), err)}
	}
	return threshold, []decisionLog{traceDecision("Parsed max failure value of %d and setting as maxFailureThreshold", threshold)}
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
