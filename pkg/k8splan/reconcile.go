package k8splan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/prober"
	corecontrollers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
)

// reconcileSecret is the per-Secret-change handler registered with core.Secret().OnChange. It
// decides whether the plan should be (re-)applied, runs the apply, and writes the outcome back to
// the Secret.
func (w *watcher) reconcileSecret(ctx context.Context, sc corecontrollers.SecretController, secret *corev1.Secret, cooldownPeriod time.Duration) (*corev1.Secret, error) {
	if secret == nil {
		logrus.Debugf("[k8splan] Received nil secret (object deleted from cache), skipping")
		return nil, nil
	}
	originalSecret := secret.DeepCopy()
	secret = secret.DeepCopy()

	var err error
	currentTime := time.Now()
	lastApplyTime := parseLastApplyTime(secret.Data, currentTime)
	w.probePeriod = parseProbePeriodOverride(secret.Data, w.probePeriod)
	logrus.Debugf("[k8splan] Processing secret %s in namespace %s at generation %d with resource version %s", secret.Name, secret.Namespace, secret.Generation, secret.ResourceVersion)
	// needsApplied indicates whether the one-time instructions should be run. It is always set by
	// decidePlanStateAction or decideChecksumFlowAction below before being read.
	var needsApplied bool

	uidChanged := w.secretUID != "" && w.secretUID != string(secret.UID)
	rvIsOlder := toInt(w.lastAppliedResourceVersion) > toInt(secret.ResourceVersion)

	switch {
	case uidChanged:
		// Secret was deleted and recreated with a new UID; reset state so the new secret is force-applied.
		logrus.Infof("[k8splan] Received secret with new UID (%s, previously %s); secret was recreated — resetting agent state", secret.UID, w.secretUID)
		w.secretUID = ""
		w.lastAppliedResourceVersion = ""
		w.hasRunOnce = false
	case rvIsOlder:
		logrus.Errorf("[k8splan] Received secret to process that was older than the last secret operated on (%s vs %s)", secret.ResourceVersion, w.lastAppliedResourceVersion)
		return secret, errors.New("secret received was too old")
	}

	planData, ok := secret.Data[PlanKey]
	if !ok {
		sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
		return secret, nil
	}

	logrus.Tracef("[k8splan] Byte data: %v", planData)
	logrus.Tracef("[k8splan] Plan string was %s", string(planData))

	probeStatuses := parseProbeStatuses(secret.Data)

	cp, err := applyinator.CalculatePlan(planData)
	if err != nil {
		return secret, err
	}
	logrus.Tracef("[k8splan] Calculated checksum to be %s", cp.Checksum)

	currentPlanState := planapi.PlanState(secret.Data[planapi.PlanStateKey])

	wasFailedPlan := false
	failureCount, planAttempt := parseFailureCount(secret.Data)

	if currentPlanState != "" {
		psResult := decidePlanStateAction(currentPlanState)
		needsApplied = psResult.NeedsApplied
		if psResult.ResetPlanAttempt {
			planAttempt = 1
		}
		if !w.hasRunOnce {
			w.hasRunOnce = true
		}
	} else {
		csResult := decideChecksumFlowAction(secret.Data, cp.Checksum, w.hasRunOnce, failureCount, currentTime, lastApplyTime, cooldownPeriod, w.lastAppliedResourceVersion == secret.ResourceVersion)
		needsApplied = csResult.NeedsApplied
		wasFailedPlan = csResult.WasFailedPlan
		w.hasRunOnce = csResult.HasRunOnce
		if csResult.ClearAppliedChecksum {
			secret.Data[AppliedChecksumKey] = []byte("")
		}
	}

	output := selectExistingOutput(secret.Data, wasFailedPlan)

	periodicOutput := secret.Data[AppliedPeriodicOutputKey]

	if currentPlanState == planapi.PlanStatePending && needsApplied {
		secret.Data[planapi.PlanStateKey] = []byte(planapi.PlanStateInProgress)
		secret.Data[planapi.PlanRevisionKey] = incrementCount(secret.Data[planapi.PlanRevisionKey])
		var inProgressErr error
		if secret, inProgressErr = w.updateSecret(sc, secret); inProgressErr != nil {
			return nil, fmt.Errorf("failed to commit plan-state:%s to API server: %w", planapi.PlanStateInProgress, inProgressErr)
		}
	}

	input := applyinator.ApplyInput{
		CalculatedPlan:             cp,
		ReconcileFiles:             needsApplied,
		ExistingOneTimeOutput:      output,
		ExistingPeriodicOutput:     periodicOutput,
		RunOneTimeInstructions:     needsApplied,
		OneTimeInstructionAttempts: planAttempt,
	}

	applyOutput, err := w.applyinator.Apply(ctx, input)
	if err != nil {
		return secret, fmt.Errorf("error encountered when running apply: %w", err)
	}

	output = applyOutput.OneTimeOutput
	periodicOutput = applyOutput.PeriodicOutput

	outcomeUpdates := buildSecretDataUpdates(applyOutcomeInput{
		Checksum:              cp.Checksum,
		CurrentTime:           currentTime,
		NeedsApplied:          needsApplied,
		WasFailedPlan:         wasFailedPlan,
		UsesPlanState:         currentPlanState != "",
		OneTimeOutput:         output,
		OneTimeApplySucceeded: applyOutput.OneTimeApplySucceeded,
		PeriodicOutput:        periodicOutput,
		PriorFailureCount:     secret.Data[FailureCountKey],
		PriorSuccessCount:     secret.Data[SuccessCountKey],
	})
	for k, v := range outcomeUpdates {
		secret.Data[k] = v
	}

	prober.DoProbes(cp.Plan.Probes, probeStatuses, needsApplied)

	marshalledProbeStatus, err := json.Marshal(probeStatuses)
	if err != nil {
		logrus.Errorf("[k8splan] Error while marshalling probe statuses: %v", err)
	} else {
		secret.Data[ProbeStatusesKey] = marshalledProbeStatus
	}

	if applyOutput.OneTimeApplySucceeded == needsApplied {
		logrus.Debugf("[k8splan] Enqueueing after %f seconds", w.probePeriod.Seconds())
		sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
	}

	if reflect.DeepEqual(originalSecret.Data, secret.Data) && reflect.DeepEqual(originalSecret.StringData, secret.StringData) {
		logrus.Debugf("[k8splan] Secret data/string-data did not change, not updating secret")
		return originalSecret, nil
	}
	secret, err = w.updateSecret(sc, secret)
	if err != nil {
		logrus.Fatalf("[k8splan] Encountered an error while attempting to update the secret: %v", err)
		return nil, nil
	}
	return secret, nil
}
