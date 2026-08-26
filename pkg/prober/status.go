package prober

import planapi "github.com/rancher/rancher/pkg/plan"

const (
	defaultSuccessThreshold = 1
	defaultFailureThreshold = 3
	// defaultTimeoutSeconds matches the documented default on planapi.Probe.TimeoutSeconds. It
	// must be applied: a zero timeout means http.Client{Timeout: 0}, i.e. no timeout at all, and a
	// hung probe blocks DoProbes' WaitGroup, stalling the watcher's reconcile loop indefinitely.
	defaultTimeoutSeconds = 1
)

// orDefault returns configured if non-zero, otherwise def.
// Used for the probe schema's optional integer fields, which treat zero as "unset".
func orDefault(configured, def int) int {
	if configured == 0 {
		return def
	}
	return configured
}

// applyProbeResult updates status in place to reflect one probe outcome.
// Consecutive successes count toward successThreshold before Healthy is set true.
// Consecutive failures count toward failureThreshold before Healthy is set false.
// Each outcome resets the opposite counter.
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
