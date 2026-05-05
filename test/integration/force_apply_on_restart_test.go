//go:build ignore

package systemagent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	provisioningv1api "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	"github.com/rancher/rancher/pkg/capr"
	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/rancher/tests/v2prov/clients"
	"github.com/rancher/rancher/tests/v2prov/cluster"
	"github.com/rancher/rancher/tests/v2prov/defaults"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/cluster-api/controllers/external"
)

// Test_SystemAgent_ForceApplyOnRestart provisions a single-node cluster,
// restarts system-agent, and verifies:
//  1. The agent respects terminal plan-state (succeeded) on restart and does
//     NOT re-apply without an explicit plan-state:pending signal.
//  2. After the orchestrator sets plan-state:pending, the restarted agent
//     picks it up, applies, and transitions to plan-state:succeeded.
//  3. last-apply-time in the plan secret changes after the re-apply.
//  4. The cluster remains Ready throughout (proving reconnection works).
func Test_SystemAgent_ForceApplyOnRestart(t *testing.T) {
	t.Log("creating v2prov clients")
	clients, err := clients.New()
	if err != nil {
		t.Fatal(err)
	}
	defer clients.Close()

	t.Log("creating single-node all-roles cluster")
	c, err := cluster.New(clients, &provisioningv1api.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-systemagent-force-apply-restart",
		},
		Spec: provisioningv1api.ClusterSpec{
			KubernetesVersion: defaults.SomeK8sVersion,
			RKEConfig: &provisioningv1api.RKEConfig{
				MachinePools: []provisioningv1api.RKEMachinePool{{
					EtcdRole:         true,
					ControlPlaneRole: true,
					WorkerRole:       true,
					Quantity:         &defaults.One,
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("waiting for cluster to become ready (this may take several minutes)")
	c, err = cluster.WaitForCreate(clients, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cluster %s/%s is ready", c.Namespace, c.Name)

	t.Log("listing CAPI machines")
	machines, err := cluster.Machines(clients, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(machines.Items) != 1 {
		t.Fatalf("expected 1 machine, got %d", len(machines.Items))
	}

	machine := machines.Items[0]
	planSecretName := capr.PlanSecretFromBootstrapName(machine.Spec.Bootstrap.ConfigRef.Name)

	t.Logf("reading plan secret %s/%s", machine.Namespace, planSecretName)
	secret, err := clients.Core.Secret().Get(machine.Namespace, planSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("plan secret not found: %v", err)
	}
	preRestartTime := string(secret.Data[lastApplyTimeKey])
	t.Logf("pre-restart last-apply-time: %s", preRestartTime)

	t.Log("resolving PodMachine pod for system-agent restart")
	im, err := external.GetObjectFromContractVersionedRef(clients.Ctx, clients.Client,
		machine.Spec.InfrastructureRef, machine.Namespace)
	if err != nil {
		t.Fatalf("failed to get infra machine: %v", err)
	}

	podName := strings.ReplaceAll(im.GetName(), ".", "-")
	podNamespace := im.GetNamespace()
	t.Logf("restarting rancher-system-agent in pod %s/%s", podNamespace, podName)

	cmd := exec.Command("kubectl", "-n", podNamespace, "exec", podName, "--",
		"systemctl", "restart", "rancher-system-agent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to restart system-agent: %v\noutput: %s", err, string(out))
	}
	t.Log("systemctl restart succeeded")

	// Poll for up to 30s to read the plan secret, retrying transient API errors
	// that can occur right after a system-agent restart disrupts k3s connections.
	t.Log("verifying agent does NOT re-apply with plan-state:succeeded (terminal) after restart")
	verifyCtx, verifyCancel := context.WithTimeout(clients.Ctx, 30*time.Second)
	defer verifyCancel()
	var terminalData map[string][]byte
	if err := wait.PollUntilContextCancel(verifyCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		s, err := clients.Core.Secret().Get(machine.Namespace, planSecretName, metav1.GetOptions{})
		if err != nil {
			t.Logf("transient error reading plan secret (will retry): %v", err)
			return false, nil
		}
		terminalData = s.Data
		return true, nil
	}); err != nil {
		t.Fatalf("could not read plan secret within 30s after restart: %v", err)
	}
	if current := string(terminalData[lastApplyTimeKey]); current != preRestartTime {
		t.Errorf("last-apply-time changed from %q to %q after restart with terminal state — agent should not re-apply", preRestartTime, current)
	}
	t.Log("confirmed: no re-apply in terminal state after restart")

	// Now simulate the orchestrator delivering a new plan by setting plan-state:pending.
	// The restarted agent must pick this up and apply.
	t.Log("patching plan-state:pending to trigger re-apply on the restarted agent")
	if err := patchSecretData(clients, machine.Namespace, planSecretName, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePending),
	}); err != nil {
		t.Fatalf("patch failed: %v", err)
	}

	// Poll until plan-state:succeeded and last-apply-time has changed.
	ctx, cancel := context.WithTimeout(clients.Ctx, 3*time.Minute)
	defer cancel()

	t.Log("waiting for plan-state:succeeded and updated last-apply-time")
	err = wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		s, err := clients.Core.Secret().Get(machine.Namespace, planSecretName, metav1.GetOptions{})
		if err != nil {
			return false, nil // retry
		}
		state := planapi.PlanState(s.Data[planapi.PlanStateKey])
		current := string(s.Data[lastApplyTimeKey])
		if state == planapi.PlanStateSucceeded && current != preRestartTime && current != "" {
			t.Logf("plan-state:succeeded reached; last-apply-time changed to %s (was %s)", current, preRestartTime)
			return true, nil
		}
		t.Logf("poll: plan-state=%q last-apply-time=%q, waiting...", state, current)
		return false, nil
	})
	if err != nil {
		t.Fatalf("plan-state did not reach succeeded or last-apply-time did not change within 3 minutes: %v", err)
	}

	t.Log("waiting for cluster to remain ready after restart and re-apply")
	c, err = cluster.WaitForCreate(clients, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("cluster still ready after system-agent restart and re-apply")

	t.Log("deleting cluster and waiting for cleanup")
	if err := clients.Provisioning.Cluster().Delete(c.Namespace, c.Name, &metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = cluster.WaitForDelete(clients, c); err != nil {
		t.Fatal(err)
	}
	t.Log("cluster deleted successfully")
}
