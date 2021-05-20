package k8splan

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rancher/system-agent/pkg/prober"

	"github.com/rancher/lasso/pkg/cache"
	"github.com/rancher/lasso/pkg/client"
	"github.com/rancher/lasso/pkg/controller"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

const appliedChecksumKey = "applied-checksum"
const appliedOutputKey = "applied-output"
const probeStatusesKey = "probe-statuses"
const planKey = "plan"
const enqueueAfterDuration = "5s"

func Watch(ctx context.Context, applyinator applyinator.Applyinator, connInfo config.ConnectionInfo) error {
	w := &watcher{
		connInfo:    connInfo,
		applyinator: applyinator,
	}

	go w.start(ctx)

	return nil
}

type watcher struct {
	connInfo    config.ConnectionInfo
	applyinator applyinator.Applyinator
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

	defaultHealthcheckDuration, err := time.ParseDuration(enqueueAfterDuration)
	if err != nil {
		panic(err)
	}

	core.Secret().OnChange(ctx, "secret-watch", func(s string, secret *v1.Secret) (*v1.Secret, error) {
		if secret == nil {
			core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, defaultHealthcheckDuration)
			return secret, nil
		}

		secret = secret.DeepCopy()
		logrus.Debugf("[K8s] Processing secret %s in namespace %s at generation %d", secret.Name, secret.Namespace, secret.Generation)
		if planData, ok := secret.Data[planKey]; ok {
			logrus.Tracef("[K8s] Byte data: %v", planData)
			logrus.Tracef("[K8s] Plan string was %s", string(planData))

			var probeStatuses map[string]prober.ProbeStatus

			if rawProbeStatusByteData, ok := secret.Data[probeStatusesKey]; ok {
				if err := json.Unmarshal(rawProbeStatusByteData, &probeStatuses); err != nil {
					logrus.Errorf("[K8s] error while parsing probe statuses: %v", err)
					probeStatuses = make(map[string]prober.ProbeStatus, 0)
				}
			}

			cp, err := applyinator.CalculatePlan(planData)
			if err != nil {
				return secret, err
			}

			output, ok := secret.Data[appliedOutputKey]
			if !ok {
				output = []byte{}
			}
			logrus.Debugf("[K8s] Calculated checksum to be %s", cp.Checksum)
			needsApplied := true
			initialApplication := true
			if secretChecksumData, ok := secret.Data[appliedChecksumKey]; ok {
				secretChecksum := string(secretChecksumData)
				logrus.Debugf("[K8s] Remote plan had an applied checksum value of %s", secretChecksum)
				if secretChecksum == cp.Checksum {
					logrus.Debugf("[K8s] Applied checksum was the same as the plan from remote. Not applying.")
					needsApplied = false
				}
			}

			if needsApplied {
				logrus.Debugf("[K8s] Calling Applyinator to apply the plan")
				output, err = w.applyinator.Apply(ctx, cp)
				if err != nil {
					return nil, fmt.Errorf("error applying plan: %v", err)
				}
			} else {
				initialApplication = false
			}

			var wg sync.WaitGroup
			var mu sync.Mutex

			for probeName, probe := range cp.Plan.Probes {
				wg.Add(1)

				go func() {
					defer wg.Done()
					logrus.Debugf("[K8s] (%s) running probe", probeName)
					mu.Lock()
					logrus.Debugf("[K8s] (%s) retrieving probe status from map", probeName)
					probeStatus, ok := probeStatuses[probeName]
					mu.Unlock()
					if !ok {
						logrus.Debugf("[K8s] (%s) probe status was not present in map, initializing", probeName)
						probeStatus = prober.ProbeStatus{}
					}
					if err := prober.DoProbe(probe, &probeStatus, initialApplication); err != nil {
						logrus.Errorf("error running probe %s", probeName)
					}
					mu.Lock()
					logrus.Debugf("[K8s] (%s) writing probe status from map", probeName)
					probeStatuses[probeName] = probeStatus
					mu.Unlock()
				}()
			}

			wg.Wait()

			rawProbeStatusByteData, err := json.Marshal(probeStatuses)
			if err != nil {
				logrus.Errorf("error while marshalling probe statuses: %v", err)
			} else {
				secret.Data[probeStatusesKey] = rawProbeStatusByteData
			}

			// secret.Data should always have already been initialized because otherwise we would have failed out above.
			secret.Data[appliedChecksumKey] = []byte(cp.Checksum)
			secret.Data[appliedOutputKey] = output
			logrus.Debugf("[K8s] writing an applied checksum value of %s to the remote plan", cp.Checksum)
			core.Secret().EnqueueAfter(w.connInfo.Namespace, w.connInfo.SecretName, defaultHealthcheckDuration)
			return core.Secret().Update(secret)
		}

		return secret, nil

	})

	if err := controllerFactory.Start(ctx, 1); err != nil {
		panic(err)
	}

}
