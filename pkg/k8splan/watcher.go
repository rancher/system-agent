package k8splan

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

const (
	appliedChecksumKey   = "applied-checksum"
	appliedOutputKey     = "applied-output"
	probeStatusesKey     = "probe-statuses"
	probePeriodKey       = "probe-period-seconds"
	planKey              = "plan"
	enqueueAfterDuration = "5s"
)

func Watch(ctx context.Context, applyinator applyinator.Applyinator, connInfo config.ConnectionInfo) {
	w := &watcher{
		connInfo:    connInfo,
		applyinator: applyinator,
	}

	go w.start(ctx)
}

type watcher struct {
	connInfo                   config.ConnectionInfo
	applyinator                applyinator.Applyinator
	lastAppliedResourceVersion string
}

func (w *watcher) start(ctx context.Context) {
	kc, err := clientcmd.RESTConfigFromKubeConfig([]byte(w.connInfo.KubeConfig))
	if err != nil {
		panic(err)
	}

	if err := validateKC(ctx, kc); err != nil {
		if strings.Contains(err.Error(), "x509: certificate signed by unknown authority") && len(kc.CAData) != 0 {
			logrus.Infof("Initial connection to Kubernetes cluster failed with error %v, removing CA data and trying again", err)
			kc.CAData = nil // nullify the provided CA data
			if err := validateKC(ctx, kc); err != nil {
				panic(fmt.Errorf("error while connecting to Kubernetes cluster with nullified CA data: %v", err))
			}
		} else {
			panic(fmt.Errorf("error while connecting to Kubernetes cluster: %v", err))
		}
	}

	clientFactory, err := client.NewSharedClientFactory(kc, nil)
	if err != nil {
		panic(err)
	}

	cacheFactory := cache.NewSharedCachedFactory(clientFactory, &cache.SharedCacheFactoryOptions{
		DefaultNamespace: w.connInfo.Namespace,
		DefaultTweakList: func(options *metav1.ListOptions) {
			options.FieldSelector = fmt.Sprintf("metadata.name=%s", w.connInfo.SecretName)
		},
	})

	controllerFactory := controller.NewSharedControllerFactory(cacheFactory, nil)
	core := corecontrollers.New(controllerFactory)

	probePeriod, err := time.ParseDuration(enqueueAfterDuration)
	if err != nil {
		panic(err)
	}

	core.Secret().OnChange(ctx, "secret-watch", func(s string, secret *v1.Secret) (*v1.Secret, error) {
		if secret == nil {
			logrus.Debugf("[K8s] Secret was nil")
			core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, probePeriod)
			return secret, nil
		}
		originalSecret := secret.DeepCopy()
		secret = secret.DeepCopy()
		if rawPeriod, ok := secret.Data[probePeriodKey]; ok {
			if parsedPeriod, err := time.ParseDuration(fmt.Sprintf("%ss", string(rawPeriod))); err != nil {
				logrus.Errorf("[K8s] error parsing duration %ss, using default", string(rawPeriod))
			} else {
				probePeriod = parsedPeriod
			}
		}
		logrus.Debugf("[K8s] Processing secret %s in namespace %s at generation %d with resource version %s", secret.Name, secret.Namespace, secret.Generation, secret.ResourceVersion)
		needsApplied := true
		if w.lastAppliedResourceVersion == secret.ResourceVersion {
			logrus.Debugf("last applied resource version (%s) did not change. skipping apply.", w.lastAppliedResourceVersion)
			needsApplied = false
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
			logrus.Debugf("[K8s] Calculated checksum to be %s", cp.Checksum)

			if secretChecksumData, ok := secret.Data[appliedChecksumKey]; ok {
				secretChecksum := string(secretChecksumData)
				logrus.Debugf("[K8s] Remote plan had an applied checksum value of %s", secretChecksum)
				if secretChecksum == cp.Checksum {
					logrus.Debugf("[K8s] Applied checksum was the same as the plan from remote. Not applying.")
					needsApplied = false
				}
			}

			var output []byte

			if needsApplied {
				logrus.Debugf("[K8s] Calling Applyinator to apply the plan")
				output, err = w.applyinator.Apply(ctx, cp)
				if err != nil {
					return nil, fmt.Errorf("error applying plan: %w", err)
				}
			} else {
				// retrieve output from the previous run if we aren't applying
				output, ok = secret.Data[appliedOutputKey]
				if !ok {
					output = []byte{}
				}
			}

			prober.DoProbes(cp.Plan.Probes, probeStatuses, needsApplied)

			marshalledProbeStatus, err := json.Marshal(probeStatuses)
			if err != nil {
				logrus.Errorf("error while marshalling probe statuses: %v", err)
			} else {
				secret.Data[probeStatusesKey] = marshalledProbeStatus
			}

			// secret.Data should always have already been initialized because otherwise we would have failed out above.
			secret.Data[appliedChecksumKey] = []byte(cp.Checksum)
			secret.Data[appliedOutputKey] = output

			core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, probePeriod)

			if reflect.DeepEqual(originalSecret.Data, secret.Data) && reflect.DeepEqual(originalSecret.StringData, secret.StringData) {
				logrus.Debugf("[K8s] secret data/string-data did not change, not updating secret")
				return originalSecret, nil
			}

			logrus.Debugf("[K8s] writing an applied checksum value of %s to the remote plan", cp.Checksum)

			var resultingSecret *v1.Secret

			if err := retry.OnError(retry.DefaultBackoff,
				func(err error) bool {
					if apierrors.IsConflict(err) {
						return false
					}
					return true
				},
				func() error {
					var err error
					resultingSecret, err = core.Secret().Update(secret)
					return err
				}); err != nil {
				return resultingSecret, err
			}

			logrus.Debugf("[K8s] updating lastAppliedResourceVersion to %s", resultingSecret.ResourceVersion)
			w.lastAppliedResourceVersion = resultingSecret.ResourceVersion
			return resultingSecret, nil
		}
		core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, probePeriod)
		return secret, nil
	})

	if err := controllerFactory.Start(ctx, 1); err != nil {
		panic(err)
	}
}

func validateKC(ctx context.Context, config *rest.Config) error {
	config = rest.CopyConfig(config)
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
