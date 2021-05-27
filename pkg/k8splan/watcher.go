package k8splan

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rancher/lasso/pkg/cache"
	"github.com/rancher/lasso/pkg/client"
	"github.com/rancher/lasso/pkg/controller"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	"github.com/rancher/system-agent/pkg/prober"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

const (
	appliedChecksumKey   = "applied-checksum"
	appliedOutputKey     = "applied-output"
	probeStatusesKey     = "probe-statuses"
	probePeriodKey       = "probe-period"
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
		if rawPeriod, ok := secret.Data[probePeriodKey]; ok {
			if parsedPeriod, err := time.ParseDuration(fmt.Sprintf("%ss", string(rawPeriod))); err != nil {
				logrus.Errorf("[K8s] error parsing duration %ss, using default", string(rawPeriod))
			} else {
				probePeriod = parsedPeriod
			}
		}
		if secret == nil {
			logrus.Debugf("[K8s] Secret was nil")
			core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, probePeriod)
			return secret, nil
		}
		secret = secret.DeepCopy()
		logrus.Debugf("[K8s] Processing secret %s in namespace %s at generation %d with resource version %s", secret.Name, secret.Namespace, secret.Generation, secret.ResourceVersion)
		if w.lastAppliedResourceVersion == secret.ResourceVersion {
			logrus.Debugf("last applied resource version (%s) did not change. skipping apply.", w.lastAppliedResourceVersion)
			core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, probePeriod)
			return secret, nil
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
			}
			// calculate the checksum of the plan from the provided data
			cp, err := applyinator.CalculatePlan(planData)
			if err != nil {
				return secret, err
			}
			logrus.Debugf("[K8s] Calculated checksum to be %s", cp.Checksum)

			needsApplied := true
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

			var wg sync.WaitGroup
			var mu sync.Mutex

			for probeName, probe := range cp.Plan.Probes {
				wg.Add(1)
				go func(probeName string, probe prober.Probe, wg *sync.WaitGroup) {
					defer wg.Done()
					logrus.Debugf("[K8s] (%s) running probe", probeName)
					mu.Lock()
					logrus.Debugf("[K8s] (%s) retrieving existing probe status from map if existing", probeName)
					probeStatus, ok := probeStatuses[probeName]
					mu.Unlock()
					if !ok {
						logrus.Debugf("[K8s] (%s) probe status was not present in map, initializing", probeName)
						probeStatus = prober.ProbeStatus{}
					}
					if err := prober.DoProbe(probe, &probeStatus, needsApplied); err != nil {
						logrus.Errorf("error running probe %s", probeName)
					}
					mu.Lock()
					logrus.Debugf("[K8s] (%s) writing probe status to map", probeName)
					probeStatuses[probeName] = probeStatus
					mu.Unlock()
				}(probeName, probe, &wg)
			}
			// wait for all probes to complete
			wg.Wait()

			marshalledProbeStatus, err := json.Marshal(probeStatuses)
			if err != nil {
				logrus.Errorf("error while marshalling probe statuses: %v", err)
			} else {
				secret.Data[probeStatusesKey] = marshalledProbeStatus
			}

			// secret.Data should always have already been initialized because otherwise we would have failed out above.
			secret.Data[appliedChecksumKey] = []byte(cp.Checksum)
			secret.Data[appliedOutputKey] = output
			logrus.Debugf("[K8s] writing an applied checksum value of %s to the remote plan", cp.Checksum)
			core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, probePeriod)

			var resultingSecret *v1.Secret

			if err := retry.OnError(retry.DefaultRetry,
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
