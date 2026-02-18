//go:build e2e

package remoteplan_test

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	capiframework "sigs.k8s.io/cluster-api/test/framework"

	"github.com/rancher/system-agent/test/e2e"
	"github.com/rancher/system-agent/test/framework"
	"github.com/rancher/system-agent/test/testenv"
)

var (
	ctx                   context.Context
	e2eConfig             *e2e.E2EConfig
	setupClusterResult    *testenv.SetupTestClusterResult
	bootstrapClusterProxy capiframework.ClusterProxy
	cl                    client.Client
	kubeconfigPath        string
)

func TestRemotePlan(t *testing.T) {
	RegisterFailHandler(Fail)
	ctrl.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	RunSpecs(t, "Remote Plan E2E Suite")
}

var _ = SynchronizedBeforeSuite(
	func() []byte {
		ctx = context.Background()
		e2eConfig = e2e.LoadE2EConfig()

		setupClusterResult = testenv.SetupTestCluster(ctx, testenv.SetupTestClusterInput{
			E2EConfig: e2eConfig,
			Scheme:    e2e.InitScheme(),
		})

		// Deploy system-agent in remote mode
		testenv.DeployRemoteAgent(ctx, setupClusterResult)

		By("System-agent is running and ready for remote plan tests")

		data, err := json.Marshal(e2e.Setup{
			ClusterName:    setupClusterResult.ClusterName,
			KubeconfigPath: setupClusterResult.KubeconfigPath,
		})
		Expect(err).ToNot(HaveOccurred())
		return data
	},
	func(sharedData []byte) {
		setup := e2e.Setup{}
		Expect(json.Unmarshal(sharedData, &setup)).To(Succeed())

		e2eConfig = e2e.LoadE2EConfig()

		bootstrapClusterProxy = capiframework.NewClusterProxy(
			setup.ClusterName,
			setup.KubeconfigPath,
			e2e.InitScheme(),
		)
		Expect(bootstrapClusterProxy).ToNot(BeNil(), "cluster proxy should not be nil")

		cl = bootstrapClusterProxy.GetClient()
		kubeconfigPath = bootstrapClusterProxy.GetKubeconfigPath()
	},
)

var _ = SynchronizedAfterSuite(
	func() {},
	func() {
		if e2eConfig != nil && e2eConfig.SkipCleanup {
			return
		}

		if setupClusterResult != nil {
			testenv.CleanupTestCluster(context.Background(), testenv.CleanupTestClusterInput{
				SetupTestClusterResult: *setupClusterResult,
			})
		}
	},
)

var _ = AfterEach(func() {
	// Clean up the plan secret after each test to avoid interference
	Expect(framework.DeleteSecret(context.Background(), cl,
		framework.E2ENamespace, framework.PlanSecretName)).To(Succeed())
})
