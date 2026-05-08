//go:build ignore

package systemagent

import (
	"strconv"
	"testing"
	"time"

	provisioningv1api "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	"github.com/rancher/rancher/pkg/capr"
	"github.com/rancher/rancher/tests/v2prov/clients"
	"github.com/rancher/rancher/tests/v2prov/cluster"
	"github.com/rancher/rancher/tests/v2prov/defaults"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Plan secret data keys written by system-agent's k8splan watcher.
// These match the constants in pkg/k8splan/watcher.go.
const (
	appliedChecksumKey = "applied-checksum"
	failedChecksumKey  = "failed-checksum"
	failureCountKey    = "failure-count"
	lastApplyTimeKey   = "last-apply-time"
	successCountKey    = "success-count"
)

// Test_SystemAgent_PlanSecretStatus provisions a single-node cluster and
// verifies that system-agent populated the plan secret with the expected
// status keys. This directly exercises the k8splan watcher's updateSecret
// path — the core feedback loop between system-agent and Rancher.
func Test_SystemAgent_PlanSecretStatus(t *testing.T) {
	t.Log("creating v2prov clients")
	clients, err := clients.New()
	if err != nil {
		t.Fatal(err)
	}
	defer clients.Close()

	t.Log("creating single-node all-roles cluster")
	c, err := cluster.New(clients, &provisioningv1api.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-systemagent-plan-secret-status",
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

	t.Logf("inspecting plan secret %s/%s for system-agent status keys", machine.Namespace, planSecretName)
	secret, err := clients.Core.Secret().Get(machine.Namespace, planSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("plan secret not found: %v", err)
	}

	// applied-checksum must be non-empty: system-agent writes it after
	// successfully applying the plan (k8splan/watcher.go).
	if v := string(secret.Data[appliedChecksumKey]); v == "" {
		t.Error("applied-checksum is empty — system-agent did not write it back")
	} else {
		t.Logf("applied-checksum: %s", v)
	}

	// success-count must be >= 1 after a successful bootstrap.
	if raw, ok := secret.Data[successCountKey]; !ok || len(raw) == 0 {
		t.Error("success-count missing or empty")
	} else if n, err := strconv.Atoi(string(raw)); err != nil || n < 1 {
		t.Errorf("success-count should be >= 1, got %q", string(raw))
	} else {
		t.Logf("success-count: %d", n)
	}

	// last-apply-time must be parseable as time.UnixDate (the format
	// system-agent uses in k8splan/watcher.go).
	if raw, ok := secret.Data[lastApplyTimeKey]; !ok || len(raw) == 0 {
		t.Error("last-apply-time missing or empty")
	} else if _, err := time.Parse(time.UnixDate, string(raw)); err != nil {
		t.Errorf("last-apply-time %q is not valid UnixDate: %v", string(raw), err)
	} else {
		t.Logf("last-apply-time: %s", string(raw))
	}

	// failure-count should be "0" after a successful apply.
	if raw := secret.Data[failureCountKey]; len(raw) > 0 && string(raw) != "0" {
		t.Errorf("failure-count should be 0 after success, got %q", string(raw))
	}

	// failed-checksum should be empty — no failures occurred.
	if v := string(secret.Data[failedChecksumKey]); v != "" {
		t.Errorf("failed-checksum should be empty, got %q", v)
	}

	t.Log("deleting cluster and waiting for cleanup")
	if err := clients.Provisioning.Cluster().Delete(c.Namespace, c.Name, &metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = cluster.WaitForDelete(clients, c); err != nil {
		t.Fatal(err)
	}
	t.Log("cluster deleted successfully")
}
