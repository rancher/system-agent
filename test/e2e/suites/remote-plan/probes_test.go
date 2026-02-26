//go:build e2e

package remoteplan_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rancher/system-agent/test/e2e"
	"github.com/rancher/system-agent/test/framework"
)

var _ = Describe("Remote Plan - Probes", Label(framework.ShortTestLabel), func() {
	BeforeEach(func() {
		ctx := context.Background()
		By("Deploying HTTP test server for probe tests")
		framework.DeployHTTPTestServer(ctx, kubeconfigPath, framework.E2ENamespace, e2e.HTTPTestServerManifestTemplate)
	})

	AfterEach(func() {
		ctx := context.Background()
		By("Cleaning up HTTP test server")
		framework.CleanupHTTPTestServer(ctx, kubeconfigPath, framework.E2ENamespace)
	})

	It("should report healthy for a valid HTTP probe", func() {
		ctx := context.Background()

		probeURL := "http://" + framework.HTTPTestServerName + "." + framework.E2ENamespace + ".svc.cluster.local:8080/index.html"

		By("Creating a plan with an HTTP probe")
		plan := framework.NewPlan().
			WithProbe("http-healthy", probeURL, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for probe-statuses to appear")
		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"probe-statuses", 120*time.Second, 5*time.Second)

		By("Waiting for the probe to report healthy")
		Eventually(func() bool {
			statuses := framework.GetProbeStatuses(ctx, cl,
				framework.E2ENamespace, framework.PlanSecretName)
			if statuses == nil {
				return false
			}
			s, ok := statuses["http-healthy"]
			if !ok {
				return false
			}
			sMap, ok := s.(map[string]interface{})
			if !ok {
				return false
			}
			healthy, ok := sMap["healthy"]
			return ok && healthy == true
		}, 120*time.Second, 5*time.Second).Should(BeTrue(),
			"Probe should eventually report healthy")
	})

	It("should report unhealthy for an invalid HTTP probe endpoint", func() {
		ctx := context.Background()

		// Use a path that doesn't exist on the HTTP server -> 404 -> probe failure.
		probeURL := "http://" + framework.HTTPTestServerName + "." + framework.E2ENamespace + ".svc.cluster.local:8080/nonexistent"

		By("Creating a plan with a probe targeting a non-existent path")
		plan := framework.NewPlan().
			WithProbe("http-unhealthy", probeURL, true).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for probe-statuses to appear")
		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"probe-statuses", 120*time.Second, 5*time.Second)

		By("Waiting for the probe to have failure count >= failure threshold")
		Eventually(func() bool {
			statuses := framework.GetProbeStatuses(ctx, cl,
				framework.E2ENamespace, framework.PlanSecretName)
			if statuses == nil {
				return false
			}
			s, ok := statuses["http-unhealthy"]
			if !ok {
				return false
			}
			sMap, ok := s.(map[string]interface{})
			if !ok {
				return false
			}
			// Check either healthy is explicitly false or failureCount >= 3
			if healthy, hOk := sMap["healthy"]; hOk && healthy == false {
				if fc, fOk := sMap["failureCount"]; fOk {
					if count, ok := fc.(float64); ok && count >= 3 {
						return true
					}
				}
			}
			// Also check if failureCount alone is >= 3
			if fc, fOk := sMap["failureCount"]; fOk {
				if count, ok := fc.(float64); ok && count >= 3 {
					return true
				}
			}
			return false
		}, 120*time.Second, 5*time.Second).Should(BeTrue(),
			"Probe should report unhealthy after failure threshold")
	})

	// The agent writes applied-checksum and probe-statuses in a single atomic
	// secret update.  DoProbes runs synchronously (including the initialDelay
	// sleep) *before* the update, so we cannot observe a window where the
	// checksum exists but the probe hasn't fired yet.  Instead we verify that
	// the initial delay actually blocks the first secret write by checking that
	// the elapsed time between secret creation and applied-checksum appearance
	// is at least initialDelaySeconds.
	It("should respect initialDelaySeconds before reporting probe results", func() {
		ctx := context.Background()
		const initialDelay = 15

		probeURL := "http://" + framework.HTTPTestServerName + "." + framework.E2ENamespace + ".svc.cluster.local:8080/index.html"

		By(fmt.Sprintf("Creating a plan with a probe that has initialDelaySeconds=%d", initialDelay))
		plan := framework.NewPlan().
			WithProbeCustomThresholds("delayed-probe", probeURL, true,
				1, 3, initialDelay, 5).
			Build()

		By("Creating the plan Secret")
		creationTime := time.Now()
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for applied-checksum (blocked by initialDelay inside DoProbes)")
		framework.WaitForSecretField(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName,
			"applied-checksum", 120*time.Second, 2*time.Second)
		elapsed := time.Since(creationTime)

		By(fmt.Sprintf("Verifying the plan took at least %d seconds (elapsed: %.1fs)", initialDelay, elapsed.Seconds()))
		Expect(elapsed.Seconds()).To(BeNumerically(">=", float64(initialDelay)),
			"Plan application should have been delayed by initialDelaySeconds")

		By("Verifying the probe is healthy after the delay")
		statuses := framework.GetProbeStatuses(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		Expect(statuses).NotTo(BeNil(), "probe-statuses should be present")
		s, ok := statuses["delayed-probe"]
		Expect(ok).To(BeTrue(), "delayed-probe should be in probe-statuses")
		sMap, ok := s.(map[string]interface{})
		Expect(ok).To(BeTrue())
		healthy, _ := sMap["healthy"]
		Expect(healthy).To(Equal(true),
			"Probe should be healthy after initial delay elapsed")
	})

	It("should not report healthy until custom success threshold is met", func() {
		ctx := context.Background()

		probeURL := "http://" + framework.HTTPTestServerName + "." + framework.E2ENamespace + ".svc.cluster.local:8080/index.html"

		By("Creating a plan with a probe with successThreshold=3")
		plan := framework.NewPlan().
			WithProbeCustomThresholds("threshold-probe", probeURL, true,
				3, 3, 0, 5).
			Build()

		By("Creating the plan Secret")
		err := framework.CreatePlanSecret(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName, plan)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the probe to eventually become healthy (requires 3 consecutive successes)")
		Eventually(func() bool {
			statuses := framework.GetProbeStatuses(ctx, cl,
				framework.E2ENamespace, framework.PlanSecretName)
			if statuses == nil {
				return false
			}
			s, ok := statuses["threshold-probe"]
			if !ok {
				return false
			}
			sMap, ok := s.(map[string]interface{})
			if !ok {
				return false
			}
			healthy, _ := sMap["healthy"]
			return healthy == true
		}, 180*time.Second, 5*time.Second).Should(BeTrue(),
			"Probe should become healthy after reaching success threshold")

		By("Verifying success count is at least 3")
		statuses := framework.GetProbeStatuses(ctx, cl,
			framework.E2ENamespace, framework.PlanSecretName)
		sMap := statuses["threshold-probe"].(map[string]interface{})
		successCount := sMap["successCount"].(float64)
		Expect(successCount).To(BeNumerically(">=", 3),
			"Success count should be at least the threshold value")
	})
})
