//go:build ignore

package systemagent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	provisioningv1api "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	"github.com/rancher/rancher/pkg/capr"
	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/rancher/tests/v2prov/clients"
	"github.com/rancher/rancher/tests/v2prov/cluster"
	"github.com/rancher/rancher/tests/v2prov/defaults"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// provisionSingleNodeCluster is a helper that provisions a single-node// all-roles cluster and returns the plan secret name and namespace.
func provisionSingleNodeCluster(t *testing.T, clients *clients.Clients, name string) (namespace, planSecret string, c *provisioningv1api.Cluster) {
	t.Helper()
	t.Logf("creating single-node all-roles cluster %q", name)
	c, err := cluster.New(clients, &provisioningv1api.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
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

	t.Log("waiting for cluster to become ready")
	c, err = cluster.WaitForCreate(clients, c)
	if err != nil {
		t.Fatal(err)
	}

	machines, err := cluster.Machines(clients, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(machines.Items) != 1 {
		t.Fatalf("expected 1 machine, got %d", len(machines.Items))
	}
	machine := machines.Items[0]
	planSecretName := capr.PlanSecretFromBootstrapName(machine.Spec.Bootstrap.ConfigRef.Name)
	return machine.Namespace, planSecretName, c
}

// patchSecretData applies a strategic-merge patch that sets the given data
// keys on the named Secret.
func patchSecretData(clients *clients.Clients, namespace, name string, data map[string][]byte) error {
	patch := struct {
		Data map[string][]byte `json:"data"`
	}{Data: data}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	_, err = clients.Core.Secret().Patch(namespace, name, types.MergePatchType, patchBytes)
	return err
}

// waitForPlanState polls until the plan secret's plan-state data key equals
// the expected value, or the context deadline is exceeded.
func waitForPlanState(ctx context.Context, clients *clients.Clients, namespace, name string, expected planapi.PlanState) (*corev1.Secret, error) {
	var latest *corev1.Secret
	err := wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		s, err := clients.Core.Secret().Get(namespace, name, metav1.GetOptions{})
		if err != nil {
			return false, nil // retry
		}
		latest = s
		return planapi.PlanState(s.Data[planapi.PlanStateKey]) == expected, nil
	})
	return latest, err
}

// Test_SystemAgent_PlanState_PendingToSucceeded simulates the orchestrator
// writing plan-state:pending and verifies the agent transitions the secret
// through in-progress to succeeded, incrementing plan-revision.
func Test_SystemAgent_PlanState_PendingToSucceeded(t *testing.T) {
	clients, err := clients.New()
	if err != nil {
		t.Fatal(err)
	}
	defer clients.Close()

	ns, secretName, c := provisionSingleNodeCluster(t, clients, "test-planstate-pending-to-succeeded")
	defer func() {
		_ = clients.Provisioning.Cluster().Delete(c.Namespace, c.Name, &metav1.DeleteOptions{})
		_, _ = cluster.WaitForDelete(clients, c)
	}()

	// Read the plan secret before we inject plan-state.
	secret, err := clients.Core.Secret().Get(ns, secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("plan secret not found: %v", err)
	}
	priorRevision := string(secret.Data[planapi.PlanRevisionKey])
	t.Logf("plan-revision before inject: %q", priorRevision)

	// Simulate orchestrator: write plan-state:pending.
	// The existing plan content is unchanged — the agent must still execute
	// because the state key is now authoritative.
	t.Log("patching plan secret with plan-state:pending")
	if err := patchSecretData(clients, ns, secretName, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePending),
	}); err != nil {
		t.Fatalf("patch failed: %v", err)
	}

	// Agent should transition: pending -> in-progress -> succeeded.
	ctx, cancel := context.WithTimeout(clients.Ctx, 3*time.Minute)
	defer cancel()

	t.Log("waiting for plan-state:succeeded")
	latest, err := waitForPlanState(ctx, clients, ns, secretName, planapi.PlanStateSucceeded)
	if err != nil {
		t.Fatalf("plan-state did not reach %q within timeout: %v", planapi.PlanStateSucceeded, err)
	}

	// plan-revision must have been incremented by the agent on the
	// pending -> in-progress transition.
	newRevision := string(latest.Data[planapi.PlanRevisionKey])
	t.Logf("plan-revision after apply: %q (was %q)", newRevision, priorRevision)
	if newRevision == "" {
		t.Error("plan-revision is empty after apply — agent did not increment it")
	}
	if newRevision == priorRevision {
		t.Errorf("plan-revision unchanged (%q) — agent did not execute the pending plan", newRevision)
	}
}

// Test_SystemAgent_PlanState_CrashRecovery simulates a crash mid-apply by
// writing plan-state:in-progress directly.  The running agent must detect the
// in-progress state, re-execute the plan, and set succeeded.
func Test_SystemAgent_PlanState_CrashRecovery(t *testing.T) {
	clients, err := clients.New()
	if err != nil {
		t.Fatal(err)
	}
	defer clients.Close()

	ns, secretName, c := provisionSingleNodeCluster(t, clients, "test-planstate-crash-recovery")
	defer func() {
		_ = clients.Provisioning.Cluster().Delete(c.Namespace, c.Name, &metav1.DeleteOptions{})
		_, _ = cluster.WaitForDelete(clients, c)
	}()

	// Inject plan-state:in-progress to simulate a crash that left the secret
	// in a mid-apply state.  The agent's watch handler will detect this and
	// re-execute (crash-recovery path in watcher.go).
	t.Log("patching plan secret with plan-state:in-progress (simulating crash)")
	if err := patchSecretData(clients, ns, secretName, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStateInProgress),
	}); err != nil {
		t.Fatalf("patch failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(clients.Ctx, 3*time.Minute)
	defer cancel()

	t.Log("waiting for plan-state:succeeded after crash-recovery re-execution")
	if _, err := waitForPlanState(ctx, clients, ns, secretName, planapi.PlanStateSucceeded); err != nil {
		t.Fatalf("plan-state did not reach %q after crash-recovery inject: %v", planapi.PlanStateSucceeded, err)
	}
	t.Log("crash-recovery succeeded — agent re-executed and set plan-state:succeeded")
}

// Test_SystemAgent_PlanState_TerminalNoReapply verifies that once plan-state
// is succeeded the agent does NOT re-apply when the secret is read again
// (i.e. terminal state is respected).  We confirm this by checking that
// last-apply-time does not change over a short observation window.
func Test_SystemAgent_PlanState_TerminalNoReapply(t *testing.T) {
	clients, err := clients.New()
	if err != nil {
		t.Fatal(err)
	}
	defer clients.Close()

	ns, secretName, c := provisionSingleNodeCluster(t, clients, "test-planstate-terminal-no-reapply")
	defer func() {
		_ = clients.Provisioning.Cluster().Delete(c.Namespace, c.Name, &metav1.DeleteOptions{})
		_, _ = cluster.WaitForDelete(clients, c)
	}()

	// Drive the secret to succeeded first.
	t.Log("patching plan-state:pending to reach succeeded")
	if err := patchSecretData(clients, ns, secretName, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePending),
	}); err != nil {
		t.Fatalf("patch failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(clients.Ctx, 3*time.Minute)
	defer cancel()
	secret, err := waitForPlanState(ctx, clients, ns, secretName, planapi.PlanStateSucceeded)
	if err != nil {
		t.Fatalf("plan-state did not reach succeeded: %v", err)
	}
	cancel()

	applyTimeAtSucceeded := string(secret.Data["last-apply-time"])
	t.Logf("last-apply-time at succeeded: %q", applyTimeAtSucceeded)

	// Wait 20 seconds and confirm last-apply-time has not changed —
	// the agent must not re-apply while in a terminal state.
	t.Log("observing for 20s to confirm no re-apply in terminal state")
	time.Sleep(20 * time.Second)

	latest, err := clients.Core.Secret().Get(ns, secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading plan secret: %v", err)
	}
	if planapi.PlanState(latest.Data[planapi.PlanStateKey]) != planapi.PlanStateSucceeded {
		t.Errorf("plan-state changed from succeeded to %q unexpectedly", string(latest.Data[planapi.PlanStateKey]))
	}
	if lat := string(latest.Data["last-apply-time"]); lat != applyTimeAtSucceeded {
		t.Errorf("last-apply-time changed from %q to %q — agent re-applied in terminal state", applyTimeAtSucceeded, lat)
	}
	t.Log("confirmed: no re-apply in terminal state")
}
