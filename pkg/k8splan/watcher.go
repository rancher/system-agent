package k8splan

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/lasso/pkg/cache"
	"github.com/rancher/lasso/pkg/client"
	"github.com/rancher/lasso/pkg/controller"
	"github.com/rancher/lasso/pkg/scheme"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	"github.com/rancher/system-agent/pkg/prober"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/transport"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
)

const (
	appliedChecksumKey       = "applied-checksum"
	appliedOutputKey         = "applied-output"
	appliedPeriodicOutputKey = "applied-periodic-output"
	failedChecksumKey        = "failed-checksum"
	failedOutputKey          = "failed-output"
	failureCountKey          = "failure-count"
	lastApplyTimeKey         = "last-apply-time"
	successCountKey          = "success-count"
	maxFailuresKey           = "max-failures"
	probeStatusesKey         = "probe-statuses"
	probePeriodKey           = "probe-period-seconds"
	planKey                  = "plan"
	enqueueAfterDuration     = "5s"
	cooldownTimerDuration    = "30s"
)

func Watch(ctx context.Context, applyinator applyinator.Applyinator, connInfo config.ConnectionInfo, strictVerify bool) {
	w := &watcher{
		connInfo:    connInfo,
		applyinator: applyinator,
	}

	go w.start(ctx, strictVerify)
}

type watcher struct {
	connInfo                   config.ConnectionInfo
	applyinator                applyinator.Applyinator
	lastAppliedResourceVersion string
	secretUID                  string
}

func toInt(resourceVersion string) int {
	// we assume this is always a valid number
	n, _ := strconv.Atoi(resourceVersion)
	return n
}

func incrementCount(count []byte) []byte {
	if len(count) > 0 {
		if failureCount, err := strconv.Atoi(string(count)); err == nil {
			failureCount++
			return []byte(strconv.Itoa(failureCount))
		}
	}
	return []byte("1")
}

func (w *watcher) start(ctx context.Context, strictVerify bool) {
	kc, err := clientcmd.RESTConfigFromKubeConfig([]byte(w.connInfo.KubeConfig))
	if err != nil {
		panic(err)
	}

	if strictVerify && len(kc.CAData) == 0 {
		logrus.Fatal("CA Data in provided kubeconfig was empty while strict verify was enabled. Aborting startup.")
	}

	if err := validateKC(ctx, kc); err != nil {
		if strings.Contains(err.Error(), "x509: certificate signed by unknown authority") && len(kc.CAData) != 0 && !strictVerify {
			logrus.Infof("Initial connection to Kubernetes cluster failed with error %v, removing CA data and trying again", err)
			kc.CAData = nil // nullify the provided CA data
			if err := validateKC(ctx, kc); err != nil {
				logrus.Fatalf("error while connecting to Kubernetes cluster with nullified CA data: %v", err)
				return
			}
		} else {
			logrus.Fatalf("error while connecting to Kubernetes cluster: %v", err)
			return
		}
	}

	clientFactory, err := client.NewSharedClientFactory(kc, nil)
	if err != nil {
		logrus.Fatalf("error while instantiating new shared client factory: %v", err)
		return
	}

	cacheFactory := cache.NewSharedCachedFactory(clientFactory, &cache.SharedCacheFactoryOptions{
		DefaultNamespace: w.connInfo.Namespace,
		DefaultTweakList: func(options *metav1.ListOptions) {
			options.FieldSelector = fmt.Sprintf("metadata.name=%s", w.connInfo.SecretName)
		},
	})

	controllerFactory := controller.NewSharedControllerFactory(cacheFactory, &controller.SharedControllerFactoryOptions{
		DefaultRateLimiter: workqueue.NewItemExponentialFailureRateLimiter(1*time.Minute, 5*time.Minute),
		DefaultWorkers:     1,
	})
	core := corecontrollers.New(controllerFactory)
	probePeriod, err := time.ParseDuration(enqueueAfterDuration)
	if err != nil {
		panic(err)
	}

	cooldownPeriod, err := time.ParseDuration(cooldownTimerDuration)
	if err != nil {
		panic(err)
	}

	hasRunOnce := false

	core.Secret().OnChange(ctx, "secret-watch", func(_ string, secret *v1.Secret) (*v1.Secret, error) {
		if secret == nil {
			logrus.Fatalf("[K8s] received nil secret that was nil, stopping")
			return nil, nil
		}
		originalSecret := secret.DeepCopy()
		secret = secret.DeepCopy()

		var lastApplyTime, currentTime time.Time

		currentTime = time.Now()

		if rawLAT, ok := secret.Data[lastApplyTimeKey]; ok {
			lastApplyTime, err = time.Parse(time.UnixDate, string(rawLAT))
			if err != nil {
				logrus.Errorf("[K8s] error parsing last apply time %s, using current time", string(rawLAT))
				lastApplyTime = currentTime
			}
		} else {
			lastApplyTime = currentTime
		}

		if rawPeriod, ok := secret.Data[probePeriodKey]; ok {
			if parsedPeriod, err := time.ParseDuration(fmt.Sprintf("%ss", string(rawPeriod))); err != nil {
				logrus.Errorf("[K8s] error parsing duration %ss, using default", string(rawPeriod))
			} else {
				probePeriod = parsedPeriod
			}
		}
		logrus.Debugf("[K8s] Processing secret %s in namespace %s at generation %d with resource version %s", secret.Name, secret.Namespace, secret.Generation, secret.ResourceVersion)
		needsApplied := true // needsApplied indicates whether the one-time instructions should be run
		if rvMismatch, uidMismatch := toInt(w.lastAppliedResourceVersion) > toInt(secret.ResourceVersion), w.secretUID != "" && w.secretUID != string(secret.UID); rvMismatch || uidMismatch {
			if rvMismatch {
				logrus.Errorf("[K8s] received secret to process that was older than the last secret operated on. (%s vs %s)", secret.ResourceVersion, w.lastAppliedResourceVersion)
			}
			if uidMismatch {
				logrus.Fatalf("[K8s] received secret UID that differed from existing secret UID. (%s vs %s)", secret.UID, w.secretUID)
				return nil, nil
			}
			return secret, errors.New("secret received was too old")
		}
		if planData, ok := secret.Data[planKey]; ok {
			logrus.Tracef("[K8s] Byte data: %v", planData)
			logrus.Tracef("[K8s] Plan string was %s", string(planData))

			var probeStatuses map[string]prober.ProbeStatus
			// retrieve existing probe statuses from the secret if they exist
			if rawProbeStatusByteData, ok := secret.Data[probeStatusesKey]; ok {
				if err := json.Unmarshal(rawProbeStatusByteData, &probeStatuses); err != nil {
					logrus.Errorf("[K8s] error while parsing probe statuses: %v", err)
					probeStatuses = make(map[string]prober.ProbeStatus, 0)
				}
			} else {
				probeStatuses = make(map[string]prober.ProbeStatus, 0)
			}
			// calculate the checksum of the plan from the provided data
			cp, err := applyinator.CalculatePlan(planData)
			if err != nil {
				return secret, err
			}
			logrus.Tracef("[K8s] Calculated checksum to be %s", cp.Checksum)

			if secretChecksumData, ok := secret.Data[appliedChecksumKey]; ok {
				secretChecksum := string(secretChecksumData)
				logrus.Tracef("[K8s] Remote plan had an applied checksum value of %s", secretChecksum)
				if secretChecksum == cp.Checksum {
					logrus.Debugf("[K8s] Applied checksum was the same as the plan from remote. Not applying.")
					needsApplied = false
				}
			}

			if !hasRunOnce {
				logrus.Infof("Detected first start, force-applying one-time instruction set")
				needsApplied = true
				hasRunOnce = true
			}

			// Check to see if we've exceeded our failure count threshold
			var maxFailureThreshold int
			if rawMaxFailureThreshold, ok := secret.Data[maxFailuresKey]; ok && len(rawMaxFailureThreshold) > 0 {
				// max failure threshold is defined. parse and compare
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

			wasFailedPlan := false
			var failureCount int
			if rawFailureCount, ok := secret.Data[failureCountKey]; ok {
				failureCount, err = strconv.Atoi(string(rawFailureCount))
				if err != nil {
					logrus.Errorf("[K8s] Error while parsing raw failure count: %v", err)
					failureCount = 0
				}
				if failureCount != 0 {
					if rFC, ok := secret.Data[failedChecksumKey]; ok {
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
			}

			if w.lastAppliedResourceVersion == secret.ResourceVersion && !wasFailedPlan {
				logrus.Debugf("[K8s] last applied resource version (%s) did not change. running probes, skipping apply.", w.lastAppliedResourceVersion)
				needsApplied = false
			}

			var output []byte

			if wasFailedPlan {
				output, ok = secret.Data[failedOutputKey]
				if !ok {
					output = []byte{}
				}
			} else {
				output, ok = secret.Data[appliedOutputKey]
				if !ok {
					output = []byte{}
				}
			}

			periodicOutput := secret.Data[appliedPeriodicOutputKey]

			input := applyinator.ApplyInput{
				CalculatedPlan:             cp,
				ReconcileFiles:             needsApplied,
				ExistingOneTimeOutput:      output,
				ExistingPeriodicOutput:     periodicOutput,
				RunOneTimeInstructions:     needsApplied,
				OneTimeInstructionAttempts: failureCount + 1,
			}

			applyOutput, err := w.applyinator.Apply(ctx, input)
			if err != nil {
				return secret, fmt.Errorf("error encountered when running apply: %w", err)
			}

			output = applyOutput.OneTimeOutput
			periodicOutput = applyOutput.PeriodicOutput

			secret.Data[appliedPeriodicOutputKey] = periodicOutput

			if (needsApplied && !applyOutput.OneTimeApplySucceeded) || (!needsApplied && wasFailedPlan) {
				logrus.Debugf("[K8s] one-time-instructions with checksum (%s) either failed or was already failed (and cooldown period hasn't elapsed) during application", cp.Checksum)
				// Update the corresponding counts/outputs
				secret.Data[failedChecksumKey] = []byte(cp.Checksum)
				if needsApplied {
					secret.Data[failureCountKey] = incrementCount(secret.Data[failureCountKey])
					secret.Data[failedOutputKey] = output
					secret.Data[successCountKey] = []byte("0")
					secret.Data[lastApplyTimeKey] = []byte(currentTime.Format(time.UnixDate))
				}
			} else {
				// secret.Data should always have already been initialized because otherwise we would have failed out above.
				logrus.Debugf("[K8s] writing an applied checksum value of %s to the remote plan", cp.Checksum)
				secret.Data[appliedChecksumKey] = []byte(cp.Checksum)
				secret.Data[appliedOutputKey] = output
				// On a successful application, we should blank out the corresponding failure keys.
				secret.Data[failureCountKey] = []byte("0")
				secret.Data[failedOutputKey] = []byte{}
				secret.Data[failedChecksumKey] = []byte{}
				if needsApplied {
					secret.Data[lastApplyTimeKey] = []byte(currentTime.Format(time.UnixDate))
					secret.Data[successCountKey] = incrementCount(secret.Data[successCountKey])
				}
			}

			prober.DoProbes(cp.Plan.Probes, probeStatuses, needsApplied)

			marshalledProbeStatus, err := json.Marshal(probeStatuses)
			if err != nil {
				logrus.Errorf("error while marshalling probe statuses: %v", err)
			} else {
				secret.Data[probeStatusesKey] = marshalledProbeStatus
			}

			if applyOutput.OneTimeApplySucceeded == needsApplied {
				// If the one-time instructions were successfully applied, we should enqueue the secret for the period of a probe to attempt to guarantee timeliness on probe reactivity.
				logrus.Debugf("[K8s] Enqueueing after %f seconds", probePeriod.Seconds())
				core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, probePeriod)
			}

			if reflect.DeepEqual(originalSecret.Data, secret.Data) && reflect.DeepEqual(originalSecret.StringData, secret.StringData) {
				logrus.Debugf("[K8s] secret data/string-data did not change, not updating secret")
				return originalSecret, nil
			}
			secret, err = w.updateSecret(core, secret)
			if err != nil {
				logrus.Fatalf("[K8s] encountered an error while attempting to update the secret: %v", err)
				return nil, nil
			}
			return secret, nil
		}
		core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, probePeriod)
		return secret, nil
	})

	if err := controllerFactory.Start(ctx, 1); err != nil {
		panic(err)
	}
}

// updateSecret attempts to update the secret 4 times (the DefaultBackoff) -- if there is a conflict and the new plan doesn't match the plan being applied in the current iteration, it will discontinue.
func (w *watcher) updateSecret(core corecontrollers.Interface, secret *v1.Secret) (*v1.Secret, error) {
	var resultingSecret *v1.Secret
	var latestSecretUpdateAttempted bool
	err := retry.OnError(retry.DefaultBackoff,
		func(err error) bool {
			if apierrors.IsConflict(err) {
				if latestSecretUpdateAttempted {
					return false
				}
				// If we get a conflict, we can retrieve the latest secret and compare plan data to see if the plan changed.
				latestSecret, getErr := core.Secret().Get(secret.Namespace, secret.Name, metav1.GetOptions{})
				if getErr == nil {
					// if the get error is nil, then we can go ahead and compare secrets and try again.
					if pd, ok := latestSecret.Data[planKey]; ok {
						ck, calculateErr := applyinator.CalculatePlan(pd)
						if calculateErr != nil {
							return false
						}
						if ck.Checksum == string(secret.Data[appliedChecksumKey]) {
							logrus.Debugf("[K8s] secret %s/%s resource version changed from %s to %s but plan checksum still matches, updating latest secret", secret.Namespace, secret.Name, secret.ResourceVersion, latestSecret.ResourceVersion)
							// we can go ahead copy the relevant data out of the "old" secret and return true to let it update the secret.
							latestSecret.Data[probeStatusesKey] = secret.Data[probeStatusesKey]
							latestSecret.Data[appliedPeriodicOutputKey] = secret.Data[appliedPeriodicOutputKey]
							latestSecret.Data[failedChecksumKey] = secret.Data[failedChecksumKey]
							latestSecret.Data[failureCountKey] = secret.Data[failureCountKey]
							latestSecret.Data[failedOutputKey] = secret.Data[failedOutputKey]
							latestSecret.Data[successCountKey] = secret.Data[successCountKey]
							latestSecret.Data[lastApplyTimeKey] = secret.Data[lastApplyTimeKey]
							latestSecret.Data[appliedChecksumKey] = secret.Data[appliedChecksumKey]
							latestSecret.Data[appliedOutputKey] = secret.Data[appliedOutputKey]
							secret = latestSecret
							latestSecretUpdateAttempted = true
							return true
						}
					}
				}
				return false
			}
			return true
		},
		func() error {
			var err error
			resultingSecret, err = core.Secret().Update(secret)
			return err
		})
	if err == nil {
		logrus.Infof("[K8s] updated plan secret %s/%s with feedback", secret.Namespace, secret.Name)
		logrus.Debugf("[K8s] updating lastAppliedResourceVersion to %s", resultingSecret.ResourceVersion)
		if w.secretUID == "" {
			w.secretUID = string(resultingSecret.UID)
		}
		w.lastAppliedResourceVersion = resultingSecret.ResourceVersion
	}
	return resultingSecret, err
}

func validateKC(ctx context.Context, config *rest.Config) error {
	var (
		conn *tls.Conn
		err  error
	)

	config = rest.CopyConfig(config)
	transportConfig, err := config.TransportConfig()
	if err != nil {
		return err
	}
	tlsConfig, err := transport.TLSConfigFor(transportConfig)
	if err != nil {
		return err
	}

	config.Transport = utilnet.SetTransportDefaults(&http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: tlsConfig,
		DialTLSContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			conn, err = tls.Dial(network, addr, tlsConfig)
			return conn, err
		},
	})
	config.WrapTransport = transportConfig.WrapTransport
	if transportConfig.DialHolder != nil && transportConfig.DialHolder.Dial != nil {
		config.Dial = transportConfig.DialHolder.Dial
	}

	// Overwrite TLS-related fields from config to avoid collision with
	// Transport field.
	config.TLSClientConfig = rest.TLSClientConfig{}

	config.NegotiatedSerializer = unstructuredNegotiator{
		NegotiatedSerializer: serializer.NewCodecFactory(scheme.All).WithoutConversion(),
	}
	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	rest, err := rest.UnversionedRESTClientFor(config)
	if err != nil {
		return err
	}
	_, err = rest.Get().AbsPath("/version").Do(ctx).Raw()
	return err
}

type unstructuredNegotiator struct {
	runtime.NegotiatedSerializer
}

func (u unstructuredNegotiator) DecoderToVersion(serializer runtime.Decoder, gv runtime.GroupVersioner) runtime.Decoder {
	result := u.NegotiatedSerializer.DecoderToVersion(serializer, gv)
	return unstructuredDecoder{
		Decoder: result,
	}
}

type unstructuredDecoder struct {
	runtime.Decoder
}

func (u unstructuredDecoder) Decode(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	obj, gvk, err := u.Decoder.Decode(data, defaults, into)
	if into == nil && runtime.IsNotRegisteredError(err) {
		return u.Decode(data, defaults, &unstructured.Unstructured{})
	}
	return obj, gvk, err
}
