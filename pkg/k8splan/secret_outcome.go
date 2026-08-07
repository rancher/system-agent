package k8splan

import (
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
)

// applyOutcomeInput carries everything buildSecretDataUpdates needs to compute the Secret data
// writes for one reconcile, after Applyinator.Apply has returned.
type applyOutcomeInput struct {
	Checksum              string
	CurrentTime           time.Time
	NeedsApplied          bool
	WasFailedPlan         bool
	UsesPlanState         bool // true when currentPlanState != "" (the plan-state flow was used)
	OneTimeOutput         []byte
	OneTimeApplySucceeded bool
	PeriodicOutput        []byte
	PriorFailureCount     []byte // secret.Data[FailureCountKey] before this reconcile, for incrementCount
	PriorSuccessCount     []byte // secret.Data[SuccessCountKey] before this reconcile, for incrementCount
}

// buildSecretDataUpdates computes the Secret data key writes resulting from one apply, mirroring
// the pre-refactor post-apply mutation block in the OnChange closure. The caller is responsible
// for merging the returned map into secret.Data and emitting the returned logs.
func buildSecretDataUpdates(in applyOutcomeInput) (map[string][]byte, []decisionLog) {
	updates := map[string][]byte{
		AppliedPeriodicOutputKey: in.PeriodicOutput,
	}

	failed := (in.NeedsApplied && !in.OneTimeApplySucceeded) || (!in.NeedsApplied && in.WasFailedPlan)
	if failed {
		logs := []decisionLog{debugDecision("one-time-instructions with checksum (%s) either failed or was already failed (and cooldown period hasn't elapsed) during application", in.Checksum)}
		updates[FailedChecksumKey] = []byte(in.Checksum)
		if in.NeedsApplied {
			updates[FailureCountKey] = incrementCount(in.PriorFailureCount)
			updates[FailedOutputKey] = in.OneTimeOutput
			updates[SuccessCountKey] = []byte("0")
			updates[LastApplyTimeKey] = []byte(in.CurrentTime.Format(time.UnixDate))
			if in.UsesPlanState {
				updates[planapi.PlanStateKey] = []byte(planapi.PlanStateFailed)
			}
		}
		return updates, logs
	}

	logs := []decisionLog{debugDecision("writing an applied checksum value of %s to the remote plan", in.Checksum)}
	updates[AppliedChecksumKey] = []byte(in.Checksum)
	updates[AppliedOutputKey] = in.OneTimeOutput
	updates[FailureCountKey] = []byte("0")
	updates[FailedOutputKey] = []byte{}
	updates[FailedChecksumKey] = []byte{}
	if in.NeedsApplied {
		updates[LastApplyTimeKey] = []byte(in.CurrentTime.Format(time.UnixDate))
		updates[SuccessCountKey] = incrementCount(in.PriorSuccessCount)
		if in.UsesPlanState {
			updates[planapi.PlanStateKey] = []byte(planapi.PlanStateSucceeded)
		}
	}
	return updates, logs
}
