package k8splan

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/lasso/pkg/cache"
	"github.com/rancher/lasso/pkg/client"
	"github.com/rancher/lasso/pkg/controller"
	"github.com/rancher/lasso/pkg/scheme"
	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	corecontrollers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
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
	// AppliedChecksumKey is the Secret data key for the applied plan checksum.
	AppliedChecksumKey = "applied-checksum"
	// AppliedOutputKey is the Secret data key for the applied one-time instruction output.
	AppliedOutputKey = "applied-output"
	// AppliedPeriodicOutputKey is the Secret data key for the applied periodic instruction output.
	AppliedPeriodicOutputKey = "applied-periodic-output"
	// FailedChecksumKey is the Secret data key for the failed plan checksum.
	FailedChecksumKey = "failed-checksum"
	// FailedOutputKey is the Secret data key for the failed instruction output.
	FailedOutputKey = "failed-output"
	// FailureCountKey is the Secret data key for the failure count.
	FailureCountKey = "failure-count"
	// LastApplyTimeKey is the Secret data key for the last apply time.
	LastApplyTimeKey = "last-apply-time"
	// SuccessCountKey is the Secret data key for the success count.
	SuccessCountKey = "success-count"
	// MaxFailuresKey is the Secret data key for the max-failures threshold.
	MaxFailuresKey = "max-failures"
	// ProbeStatusesKey is the Secret data key for probe statuses.
	ProbeStatusesKey = "probe-statuses"
	// ProbePeriodKey is the Secret data key for the probe period in seconds.
	ProbePeriodKey = "probe-period-seconds"
	// PlanKey is the Secret data key for the plan payload.
	PlanKey = "plan"

	enqueueAfterDuration  = "5s"
	cooldownTimerDuration = "30s"
)

// secretConflictMergeKeys lists Secret data keys that updateSecret merges on conflict.
// When retrying after an Update conflict, these keys are carried from the attempted write
// into the freshly fetched secret before retrying the Update.
var secretConflictMergeKeys = []string{
	ProbeStatusesKey,
	AppliedPeriodicOutputKey,
	FailedChecksumKey,
	FailureCountKey,
	FailedOutputKey,
	SuccessCountKey,
	LastApplyTimeKey,
	AppliedChecksumKey,
	AppliedOutputKey,
	planapi.PlanStateKey,
	planapi.PlanRevisionKey,
}

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
	// hasRunOnce and probePeriod are mutated by reconcileSecret and persist across calls.
	// probePeriod is sticky: when a Secret sets probe-period-seconds it becomes the default
	// for subsequent Secrets until overridden.
	hasRunOnce  bool
	probePeriod time.Duration
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
		logrus.Fatal("[k8splan] CAData in provided kubeconfig was empty while strict verify was enabled, aborting startup")
	}

	if err := connectWithCAFallback(ctx, kc, strictVerify); err != nil {
		logrus.Fatalf("[k8splan] %v", err)
		return
	}

	clientFactory, err := client.NewSharedClientFactory(kc, nil)
	if err != nil {
		logrus.Fatalf("[k8splan] error while instantiating new shared client factory: %v", err)
		return
	}

	cacheFactory := cache.NewSharedCachedFactory(clientFactory, &cache.SharedCacheFactoryOptions{
		DefaultNamespace: w.connInfo.Namespace,
		DefaultTweakList: func(options *metav1.ListOptions) {
			options.FieldSelector = fmt.Sprintf("metadata.name=%s", w.connInfo.SecretName)
		},
	})

	controllerFactory := controller.NewSharedControllerFactory(cacheFactory, &controller.SharedControllerFactoryOptions{
		DefaultRateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[any](1*time.Minute, 5*time.Minute),
		DefaultWorkers:     1,
	})
	core := corecontrollers.New(controllerFactory)
	w.probePeriod, err = time.ParseDuration(enqueueAfterDuration)
	if err != nil {
		panic(err)
	}

	cooldownPeriod, err := time.ParseDuration(cooldownTimerDuration)
	if err != nil {
		panic(err)
	}

	core.Secret().OnChange(ctx, "secret-watch", func(_ string, secret *corev1.Secret) (*corev1.Secret, error) {
		return w.reconcileSecret(ctx, core.Secret(), secret, cooldownPeriod)
	})

	if err := controllerFactory.Start(ctx, 1); err != nil {
		panic(err)
	}
}

// updateSecret attempts to update the secret up to the DefaultBackoff retry policy.
// It discontinues if there is a conflict and the re-fetched secret carries a different plan.
func (w *watcher) updateSecret(sc corecontrollers.SecretController, secret *corev1.Secret) (*corev1.Secret, error) {
	var resultingSecret *corev1.Secret
	var latestSecretUpdateAttempted bool
	err := retry.OnError(retry.DefaultBackoff,
		func(err error) bool {
			if apierrors.IsConflict(err) {
				if latestSecretUpdateAttempted {
					return false
				}
				// If we get a conflict, we can retrieve the latest secret and compare plan data to see if the plan changed.
				latestSecret, getErr := sc.Get(secret.Namespace, secret.Name, metav1.GetOptions{})
				if getErr == nil {
					// if the get error is nil, then we can go ahead and compare secrets and try again.
					if pd, ok := latestSecret.Data[PlanKey]; ok {
						ck, calculateErr := applyinator.CalculatePlan(pd)
						if calculateErr != nil {
							return false
						}
						if ck.Checksum == string(secret.Data[AppliedChecksumKey]) {
							logrus.Debugf("[k8splan] secret %s/%s resource version changed from %s to %s but plan checksum still matches, updating latest secret", secret.Namespace, secret.Name, secret.ResourceVersion, latestSecret.ResourceVersion)
							// we can go ahead copy the relevant data out of the "old" secret and return true to let it update the secret.
							for _, key := range secretConflictMergeKeys {
								latestSecret.Data[key] = secret.Data[key]
							}
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
			resultingSecret, err = sc.Update(secret)
			return err
		})
	if err == nil {
		logrus.Infof("[k8splan] updated plan secret %s/%s with feedback", secret.Namespace, secret.Name)
		logrus.Debugf("[k8splan] updating lastAppliedResourceVersion to %s", resultingSecret.ResourceVersion)
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

// connectWithCAFallback validates connectivity to the Kubernetes API server described by kc,
// retrying once without CAData if the initial attempt fails with an unknown-authority error and
// strictVerify does not forbid it.
func connectWithCAFallback(ctx context.Context, kc *rest.Config, strictVerify bool) error {
	if err := validateKC(ctx, kc); err != nil {
		if strings.Contains(err.Error(), "x509: certificate signed by unknown authority") && len(kc.CAData) != 0 && !strictVerify {
			logrus.Infof("[k8splan] initial connection to Kubernetes cluster failed with error %v, removing CAData and trying again", err)
			kc.CAData = nil // nullify the provided CAData
			if err := validateKC(ctx, kc); err != nil {
				return fmt.Errorf("error while connecting to Kubernetes cluster with nullified CAData: %w", err)
			}
			return nil
		}
		return fmt.Errorf("error while connecting to Kubernetes cluster: %w", err)
	}
	return nil
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
