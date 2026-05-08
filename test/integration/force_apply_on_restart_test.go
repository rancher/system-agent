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
	"github.com/rancher/rancher/tests/v2prov/clients"
	"github.com/rancher/rancher/tests/v2prov/cluster"
	"github.com/rancher/rancher/tests/v2prov/defaults"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/cluster-api/controllers/external"
)

// Test_SystemAgent_ForceApplyOnRestart provisions a single-node cluster,
// restarts system-agent, and verifies:
//  1. last-apply-time in the plan secret changes (proving the hasRunOnce
//     force-reapply logic in k8splan/watcher.go fired).
//  2. The cluster remains Ready (proving reconnection works).
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

	t.Logf("recording pre-restart last-apply-time from plan secret %s/%s", machine.Namespace, planSecretName)
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
	t.Log("systemctl restart succeeded, polling plan secret for updated last-apply-time")

	// Poll until last-apply-time changes — this proves hasRunOnce triggered
	// a force-reapply after the restart.
	ctx, cancel := context.WithTimeout(clients.Ctx, 3*time.Minute)
	defer cancel()

	err = wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		s, err := clients.Core.Secret().Get(machine.Namespace, planSecretName, metav1.GetOptions{})
		if err != nil {
			t.Logf("poll: error reading plan secret (will retry): %v", err)
			return false, nil
		}
		current := string(s.Data[lastApplyTimeKey])
		if current != preRestartTime && current != "" {
			t.Logf("poll: last-apply-time changed to %s (was %s)", current, preRestartTime)
			return true, nil
		}
		t.Logf("poll: last-apply-time still %s, waiting...", current)
		return false, nil
	})
	if err != nil {
		t.Fatalf("last-apply-time did not change within 3 minutes after restart: %v", err)
	}

	t.Log("waiting for cluster to converge back to ready after restart")
	c, err = cluster.WaitForCreate(clients, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("cluster still ready after system-agent restart")

	t.Log("deleting cluster and waiting for cleanup")
	if err := clients.Provisioning.Cluster().Delete(c.Namespace, c.Name, &metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = cluster.WaitForDelete(clients, c); err != nil {
		t.Fatal(err)
	}
	t.Log("cluster deleted successfully")
}
