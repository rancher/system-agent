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
