package remoteplan

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/rancher/lasso/pkg/cache"
	"github.com/rancher/lasso/pkg/client"
	"github.com/rancher/lasso/pkg/controller"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	"github.com/rancher/system-agent/pkg/types"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

const appliedChecksumKey = "applied-checksum"

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

	core.Secret().OnChange(ctx, "secret-watch", func(s string, secret *v1.Secret) (*v1.Secret, error) {
		if secret == nil {
			return secret, nil
		}

		secret = secret.DeepCopy()
		logrus.Debugf("[remote] Processing secret %s in namespace %s at generation %d", secret.Name, secret.Namespace, secret.Generation)

		if planData, ok := secret.Data["plan"]; ok {
			logrus.Tracef("[remote] Byte data: %v", planData)
			var plan types.NodePlan
			planString := string(planData)
			logrus.Debugf("[remote] Plan string was %s", planString)
			err = w.parsePlan(planString, &plan)
			if err != nil {
				logrus.Errorf("error parsing plan from remote: %v", err)
				// we should do some intelligent error handling here
				return secret, nil
			}

			var anp types.AgentNodePlan
			anp.Plan = plan
			anp.Checksum = checksum([]byte(planString))

			logrus.Debugf("[remote] Calculated checksum to be %s", anp.Checksum)
			if secretChecksumData, ok := secret.Data[appliedChecksumKey]; ok {
				secretChecksum := string(secretChecksumData)
				logrus.Debugf("[remote] Remote plan had an applied checksum value of %s", secretChecksum)
				if secretChecksum == anp.Checksum {
					logrus.Debugf("[remote] Applied checksum was the same as the plan from remote. Not applying.")
					return secret, nil
				}
			}

			logrus.Debugf("[remote] Calling Applyinator to apply the plan")

			err := w.applyinator.Apply(ctx, anp)
			if err != nil {
				logrus.Errorf("error applying plan: %v", err)
				return secret, fmt.Errorf("error applying plan")
			}
			// secret.Data should always have already been initialized because otherwise we would have failed out above.
			secret.Data[appliedChecksumKey] = []byte(anp.Checksum)
			logrus.Debugf("[remote] writing an applied checksum value of %s to the remote plan", anp.Checksum)
			return core.Secret().Update(secret)
		}

		return secret, nil

	})

	if err := controllerFactory.Start(ctx, 1); err != nil {
		panic(err)
	}

}

func (w *watcher) parsePlan(content string, np interface{}) error {
	bytes := []byte(content)
	return json.Unmarshal(bytes, np)
}

func checksum(input []byte) string {
	h := sha256.New()
	h.Write(input)

	return fmt.Sprintf("%x", h.Sum(nil))
}
