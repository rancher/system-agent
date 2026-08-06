package prober

import planapi "github.com/rancher/rancher/pkg/plan"

const (
	defaultSuccessThreshold = 1
	defaultFailureThreshold = 3
)

// resolveThreshold returns configured if it is non-zero, otherwise def.
func resolveThreshold(configured, def int) int {
	if configured == 0 {
		return def
	}
	return configured
}

// applyProbeResult updates status in place to reflect one probe outcome: consecutive successes
// count toward successThreshold before Healthy is set true, consecutive failures count toward
// failureThreshold before Healthy is set false, and each outcome resets the opposite counter.
func applyProbeResult(status *planapi.ProbeStatus, succeeded bool, successThreshold, failureThreshold int) {
	if succeeded {
		if status.SuccessCount < successThreshold {
			status.SuccessCount++
			if status.SuccessCount >= successThreshold {
				status.Healthy = true
			}
		}
		status.FailureCount = 0
		return
	}

	if status.FailureCount < failureThreshold {
		status.FailureCount++
		if status.FailureCount >= failureThreshold {
			status.Healthy = false
		}
	}
	status.SuccessCount = 0
}
