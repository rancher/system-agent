package k8splan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
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
		logrus.Debugf("[K8s] received nil secret (object deleted from cache), skipping")
		return nil, nil
	}
	originalSecret := secret.DeepCopy()
	secret = secret.DeepCopy()

	var err error
	currentTime := time.Now()

	var lastApplyTime time.Time
	if rawLAT, ok := secret.Data[LastApplyTimeKey]; ok {
		lastApplyTime, err = time.Parse(time.UnixDate, string(rawLAT))
		if err != nil {
			logrus.Errorf("[K8s] error parsing last apply time %s, using current time", string(rawLAT))
			lastApplyTime = currentTime
		}
	} else {
		lastApplyTime = currentTime
	}

	if rawPeriod, ok := secret.Data[ProbePeriodKey]; ok {
		if parsedPeriod, err := time.ParseDuration(fmt.Sprintf("%ss", string(rawPeriod))); err != nil {
			logrus.Errorf("[K8s] error parsing duration %ss, using default", string(rawPeriod))
		} else {
			w.probePeriod = parsedPeriod
		}
	}
	logrus.Debugf("[K8s] Processing secret %s in namespace %s at generation %d with resource version %s", secret.Name, secret.Namespace, secret.Generation, secret.ResourceVersion)
	needsApplied := true // needsApplied indicates whether the one-time instructions should be run

	uidChanged := w.secretUID != "" && w.secretUID != string(secret.UID)
	rvIsOlder := toInt(w.lastAppliedResourceVersion) > toInt(secret.ResourceVersion)

	switch {
	case uidChanged:
		// Secret was deleted and recreated with a new UID; reset state so the new secret is force-applied.
		logrus.Infof("[K8s] received secret with new UID (%s, previously %s); secret was recreated — resetting agent state", secret.UID, w.secretUID)
		w.secretUID = ""
		w.lastAppliedResourceVersion = ""
		w.hasRunOnce = false
	case rvIsOlder:
		logrus.Errorf("[K8s] received secret to process that was older than the last secret operated on. (%s vs %s)", secret.ResourceVersion, w.lastAppliedResourceVersion)
		return secret, errors.New("secret received was too old")
	}

	planData, ok := secret.Data[PlanKey]
	if !ok {
		sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
		return secret, nil
	}

	logrus.Tracef("[K8s] Byte data: %v", planData)
	logrus.Tracef("[K8s] Plan string was %s", string(planData))

	var probeStatuses map[string]planapi.ProbeStatus
	if rawProbeStatusByteData, ok := secret.Data[ProbeStatusesKey]; ok {
		if err := json.Unmarshal(rawProbeStatusByteData, &probeStatuses); err != nil {
			logrus.Errorf("[K8s] error while parsing probe statuses: %v", err)
			probeStatuses = make(map[string]planapi.ProbeStatus, 0)
		}
	} else {
		probeStatuses = make(map[string]planapi.ProbeStatus, 0)
	}

	cp, err := applyinator.CalculatePlan(planData)
	if err != nil {
		return secret, err
	}
	logrus.Tracef("[K8s] Calculated checksum to be %s", cp.Checksum)

	currentPlanState := planapi.PlanState(secret.Data[planapi.PlanStateKey])

	wasFailedPlan := false
	var failureCount int
	planAttempt := 1
	if rawFailureCount, ok := secret.Data[FailureCountKey]; ok && len(rawFailureCount) > 0 {
		if fc, parseErr := strconv.Atoi(string(rawFailureCount)); parseErr == nil {
			failureCount = fc
		}
		planAttempt = failureCount + 1
	}

	if currentPlanState != "" {
		switch currentPlanState {
		case planapi.PlanStatePending:
			logrus.Infof("[K8s] plan-state is %q; applying new plan content", currentPlanState)
			needsApplied = true
			planAttempt = 1
		case planapi.PlanStateInProgress:
			logrus.Infof("[K8s] plan-state is %q on startup; re-executing plan (crash recovery)", currentPlanState)
			needsApplied = true
		default:
			logrus.Debugf("[K8s] plan-state is %q (terminal); not applying", currentPlanState)
			needsApplied = false
		}
		if !w.hasRunOnce {
			w.hasRunOnce = true
		}
	} else {
		if secretChecksumData, ok := secret.Data[AppliedChecksumKey]; ok {
			secretChecksum := string(secretChecksumData)
			logrus.Tracef("[K8s] Remote plan had an applied checksum value of %s", secretChecksum)
			if secretChecksum == cp.Checksum {
				logrus.Debugf("[K8s] Applied checksum was the same as the plan from remote. Not applying.")
				needsApplied = false
			}
		}

		if !w.hasRunOnce {
			logrus.Infof("Detected first start, force-applying one-time instruction set")
			needsApplied = true
			w.hasRunOnce = true
			secret.Data[AppliedChecksumKey] = []byte("")
		}

		var maxFailureThreshold int
		if rawMaxFailureThreshold, ok := secret.Data[MaxFailuresKey]; ok && len(rawMaxFailureThreshold) > 0 {
			maxFailureThreshold, err = strconv.Atoi(string(rawMaxFailureThreshold))
			if err != nil {
				logrus.Errorf("error parsing max-failures: %s: %v", string(rawMaxFailureThreshold), err)
				maxFailureThreshold = -1
			} else {
				logrus.Tracef("[K8s] Parsed max failure value of %d and setting as maxFailureThreshold", maxFailureThreshold)
			}
		} else {
			maxFailureThreshold = -1
		}

		if failureCount != 0 {
			if rFC, ok := secret.Data[FailedChecksumKey]; ok {
				if string(rFC) == cp.Checksum {
					logrus.Debugf("[K8s] Plan appears to have failed before, failure count was %d", failureCount)
					wasFailedPlan = true
					if failureCount >= maxFailureThreshold && maxFailureThreshold != -1 {
						logrus.Errorf("[K8s] Maximum failure threshold exceeded for plan with checksum value of %s, (failures: %d, threshold: %d)", cp.Checksum, failureCount, maxFailureThreshold)
						needsApplied = false
					} else {
						if !currentTime.Equal(lastApplyTime) && !currentTime.After(lastApplyTime.Add(cooldownPeriod)) {
							logrus.Debugf("[K8s] %f second cooldown timer for failed plan application has not passed yet.", cooldownPeriod.Seconds())
							needsApplied = false
						}
					}
				} else {
					logrus.Errorf("[K8s] Received plan checksum (%s) did not match failed plan checksum (%s) and failure count was greater than zero. Cancelling failure cooldown.", cp.Checksum, string(rFC))
				}
			}
		}

		if w.lastAppliedResourceVersion == secret.ResourceVersion && !wasFailedPlan {
			logrus.Debugf("[K8s] last applied resource version (%s) did not change. running probes, skipping apply.", w.lastAppliedResourceVersion)
			needsApplied = false
		}
	}

	var output []byte
	if wasFailedPlan {
		output, ok = secret.Data[FailedOutputKey]
		if !ok {
			output = []byte{}
		}
	} else {
		output, ok = secret.Data[AppliedOutputKey]
		if !ok {
			output = []byte{}
		}
	}

	periodicOutput := secret.Data[AppliedPeriodicOutputKey]

	if currentPlanState == planapi.PlanStatePending && needsApplied {
		secret.Data[planapi.PlanStateKey] = []byte(planapi.PlanStateInProgress)
		secret.Data[planapi.PlanRevisionKey] = incrementCount(secret.Data[planapi.PlanRevisionKey])
		var inProgressErr error
		if secret, inProgressErr = w.updateSecret(sc, secret); inProgressErr != nil {
			return nil, fmt.Errorf("[K8s] failed to commit plan-state:%s to API server: %w", planapi.PlanStateInProgress, inProgressErr)
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

	secret.Data[AppliedPeriodicOutputKey] = periodicOutput

	if (needsApplied && !applyOutput.OneTimeApplySucceeded) || (!needsApplied && wasFailedPlan) {
		logrus.Debugf("[K8s] one-time-instructions with checksum (%s) either failed or was already failed (and cooldown period hasn't elapsed) during application", cp.Checksum)
		secret.Data[FailedChecksumKey] = []byte(cp.Checksum)
		if needsApplied {
			secret.Data[FailureCountKey] = incrementCount(secret.Data[FailureCountKey])
			secret.Data[FailedOutputKey] = output
			secret.Data[SuccessCountKey] = []byte("0")
			secret.Data[LastApplyTimeKey] = []byte(currentTime.Format(time.UnixDate))
			if currentPlanState != "" {
				secret.Data[planapi.PlanStateKey] = []byte(planapi.PlanStateFailed)
			}
		}
	} else {
		logrus.Debugf("[K8s] writing an applied checksum value of %s to the remote plan", cp.Checksum)
		secret.Data[AppliedChecksumKey] = []byte(cp.Checksum)
		secret.Data[AppliedOutputKey] = output
		secret.Data[FailureCountKey] = []byte("0")
		secret.Data[FailedOutputKey] = []byte{}
		secret.Data[FailedChecksumKey] = []byte{}
		if needsApplied {
			secret.Data[LastApplyTimeKey] = []byte(currentTime.Format(time.UnixDate))
			secret.Data[SuccessCountKey] = incrementCount(secret.Data[SuccessCountKey])
			if currentPlanState != "" {
				secret.Data[planapi.PlanStateKey] = []byte(planapi.PlanStateSucceeded)
			}
		}
	}

	prober.DoProbes(cp.Plan.Probes, probeStatuses, needsApplied)

	marshalledProbeStatus, err := json.Marshal(probeStatuses)
	if err != nil {
		logrus.Errorf("error while marshalling probe statuses: %v", err)
	} else {
		secret.Data[ProbeStatusesKey] = marshalledProbeStatus
	}

	if applyOutput.OneTimeApplySucceeded == needsApplied {
		logrus.Debugf("[K8s] Enqueueing after %f seconds", w.probePeriod.Seconds())
		sc.EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, w.probePeriod)
	}

	if reflect.DeepEqual(originalSecret.Data, secret.Data) && reflect.DeepEqual(originalSecret.StringData, secret.StringData) {
		logrus.Debugf("[K8s] secret data/string-data did not change, not updating secret")
		return originalSecret, nil
	}
	secret, err = w.updateSecret(sc, secret)
	if err != nil {
		logrus.Fatalf("[K8s] encountered an error while attempting to update the secret: %v", err)
		return nil, nil
	}
	return secret, nil
}
