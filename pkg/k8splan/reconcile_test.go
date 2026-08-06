package k8splan

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	"github.com/rancher/wrangler/v3/pkg/generic/fake"
	"go.uber.org/mock/gomock"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testNamespace = "test-ns"
	testSecret    = "test-secret"
)

func newTestWatcher(t *testing.T, hasRunOnce bool, lastAppliedRV string) *watcher {
	t.Helper()
	return &watcher{
		connInfo:                   config.ConnectionInfo{Namespace: testNamespace, SecretName: testSecret},
		applyinator:                *applyinator.NewApplyinator(t.TempDir(), false, "", "", nil),
		hasRunOnce:                 hasRunOnce,
		probePeriod:                5 * time.Second,
		lastAppliedResourceVersion: lastAppliedRV,
	}
}

func newMockSecretController(t *testing.T) *fake.MockControllerInterface[*corev1.Secret, *corev1.SecretList] {
	t.Helper()
	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	sc.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		return s, nil
	}).AnyTimes()
	return sc
}

func marshalPlan(t *testing.T, plan planapi.Plan) (raw []byte, checksum string) {
	t.Helper()
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("failed to marshal plan: %v", err)
	}
	return raw, planapi.Checksum(raw)
}

func TestReconcileSecretScenarios(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	successPlanBytes, successChecksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "ok", Command: "sh", Args: []string{"-c", "true"}}, SaveOutput: true},
		},
	})

	tests := []struct {
		name                string
		planBytes           []byte
		initialData         map[string][]byte
		hasRunOnce          bool
		lastAppliedRV       string
		wantEnqueueAfter    bool
		wantAppliedChecksum string // "" to skip this assertion
	}{
		{
			name:                "first start force-applies",
			planBytes:           successPlanBytes,
			initialData:         map[string][]byte{},
			hasRunOnce:          false,
			wantEnqueueAfter:    true,
			wantAppliedChecksum: successChecksum,
		},
		{
			name:      "checksum flow, checksum already applied and RV unchanged: no-op",
			planBytes: successPlanBytes,
			initialData: map[string][]byte{
				AppliedChecksumKey: []byte(successChecksum),
			},
			hasRunOnce:          true,
			lastAppliedRV:       "42",
			wantEnqueueAfter:    true,
			wantAppliedChecksum: successChecksum,
		},
		{
			name:      "checksum flow, checksum changed: re-applies",
			planBytes: successPlanBytes,
			initialData: map[string][]byte{
				AppliedChecksumKey: []byte("stale-checksum"),
			},
			hasRunOnce:          true,
			lastAppliedRV:       "42",
			wantEnqueueAfter:    true,
			wantAppliedChecksum: successChecksum,
		},
		{
			name:      "plan-state terminal (succeeded): monitors only",
			planBytes: successPlanBytes,
			initialData: map[string][]byte{
				planapi.PlanStateKey: []byte(planapi.PlanStateSucceeded),
			},
			hasRunOnce:       true,
			lastAppliedRV:    "42",
			wantEnqueueAfter: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sc := newMockSecretController(t)
			if tt.wantEnqueueAfter {
				sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any())
			}

			w := newTestWatcher(t, tt.hasRunOnce, tt.lastAppliedRV)
			data := map[string][]byte{PlanKey: tt.planBytes}
			for k, v := range tt.initialData {
				data[k] = v
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: tt.lastAppliedRV},
				Data:       data,
			}

			result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
			if err != nil {
				t.Fatalf("reconcileSecret returned error: %v", err)
			}
			if tt.wantAppliedChecksum != "" && string(result.Data[AppliedChecksumKey]) != tt.wantAppliedChecksum {
				t.Errorf("expected applied checksum %q, got %q", tt.wantAppliedChecksum, result.Data[AppliedChecksumKey])
			}
		})
	}
}

func TestReconcileSecretFailureCooldownActive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "bad", Command: "sh", Args: []string{"-c", "false"}}, SaveOutput: true},
		},
	})

	sc := newMockSecretController(t)
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any())

	w := newTestWatcher(t, true, "42")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data: map[string][]byte{
			PlanKey:            planBytes,
			AppliedChecksumKey: []byte(""),
			FailedChecksumKey:  []byte(checksum),
			FailureCountKey:    []byte("1"),
			LastApplyTimeKey:   []byte(time.Now().Format(time.UnixDate)),
		},
	}

	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if string(result.Data[FailedChecksumKey]) != checksum {
		t.Errorf("expected failed checksum to remain %q, got %q", checksum, result.Data[FailedChecksumKey])
	}
	if string(result.Data[FailureCountKey]) != "1" {
		t.Errorf("expected failure count to stay at 1 (no re-apply attempted), got %q", result.Data[FailureCountKey])
	}
}

func TestReconcileSecretFailureCooldownElapsedRetries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "bad", Command: "sh", Args: []string{"-c", "false"}}, SaveOutput: true},
		},
	})

	sc := newMockSecretController(t)
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any()).AnyTimes()

	w := newTestWatcher(t, true, "42")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data: map[string][]byte{
			PlanKey:            planBytes,
			AppliedChecksumKey: []byte(""),
			FailedChecksumKey:  []byte(checksum),
			FailureCountKey:    []byte("1"),
			LastApplyTimeKey:   []byte(time.Now().Add(-time.Hour).Format(time.UnixDate)),
		},
	}

	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if string(result.Data[FailureCountKey]) != "2" {
		t.Errorf("expected failure count to increment to 2 (retry attempted and failed again), got %q", result.Data[FailureCountKey])
	}
}

func TestReconcileSecretMaxFailureThresholdExceeded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "bad", Command: "sh", Args: []string{"-c", "false"}}, SaveOutput: true},
		},
	})

	sc := newMockSecretController(t)
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any())

	w := newTestWatcher(t, true, "42")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data: map[string][]byte{
			PlanKey:            planBytes,
			AppliedChecksumKey: []byte(""),
			FailedChecksumKey:  []byte(checksum),
			FailureCountKey:    []byte("3"),
			MaxFailuresKey:     []byte("3"),
			LastApplyTimeKey:   []byte(time.Now().Add(-time.Hour).Format(time.UnixDate)),
		},
	}

	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if string(result.Data[FailureCountKey]) != "3" {
		t.Errorf("expected failure count to stay at 3 (threshold exceeded, no retry attempted), got %q", result.Data[FailureCountKey])
	}
}

func TestReconcileSecretUIDChangeResetsState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "ok", Command: "sh", Args: []string{"-c", "true"}}, SaveOutput: true},
		},
	})

	sc := newMockSecretController(t)
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any())

	w := newTestWatcher(t, true, "100")
	w.secretUID = "old-uid"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "1", UID: "new-uid"},
		Data: map[string][]byte{
			PlanKey:            planBytes,
			AppliedChecksumKey: []byte(checksum), // would be a no-op if the UID reset didn't force a re-apply
		},
	}

	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	// secretUID is reset to "" the moment the UID change is detected, but updateSecret's success
	// path re-populates it from the freshly-updated secret (see watcher.go's `if w.secretUID ==
	// "" { w.secretUID = string(resultingSecret.UID) }`) — so by the time reconcileSecret returns,
	// it has already been re-learned as the new UID, not left empty.
	if w.secretUID != "new-uid" {
		t.Errorf("expected secretUID to be re-learned as %q after the successful update, got %q", "new-uid", w.secretUID)
	}
	if string(result.Data[AppliedChecksumKey]) != checksum {
		t.Errorf("expected the plan to be force-re-applied despite matching checksum, got applied checksum %q", result.Data[AppliedChecksumKey])
	}
}

func TestReconcileSecretStaleResourceVersionRejected(t *testing.T) {
	t.Parallel()

	// No EXPECT() calls are configured: the mock fails the test if reconcileSecret makes any
	// Kubernetes API call at all, proving the stale-RV path returns before doing any work.
	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)

	w := newTestWatcher(t, true, "100")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "1"},
		Data:       map[string][]byte{},
	}

	if _, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second); err == nil {
		t.Fatal("expected an error for a stale resource version, got nil")
	}
}

func TestReconcileSecretPendingTransitionsThroughInProgress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	planBytes, _ := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "ok", Command: "sh", Args: []string{"-c", "true"}}, SaveOutput: true},
		},
	})

	sc := newMockSecretController(t)
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any())

	w := newTestWatcher(t, true, "42")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data: map[string][]byte{
			PlanKey:              planBytes,
			planapi.PlanStateKey: []byte(planapi.PlanStatePending),
		},
	}

	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if planapi.PlanState(result.Data[planapi.PlanStateKey]) != planapi.PlanStateSucceeded {
		t.Errorf("expected final plan-state %q, got %q", planapi.PlanStateSucceeded, result.Data[planapi.PlanStateKey])
	}
	// newMockSecretController's Update expectation is AnyTimes(), so this test doesn't assert an
	// exact call count, but the pending -> in-progress -> succeeded path exercises two Update
	// calls: one committing in-progress before Apply runs, one committing the final outcome.
}

func TestReconcileSecretUpdateConflictRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "ok", Command: "sh", Args: []string{"-c", "true"}}, SaveOutput: true},
		},
	})

	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any())

	firstAttempt := true
	sc.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		if firstAttempt {
			firstAttempt = false
			return nil, apierrors.NewConflict(corev1.Resource("secrets"), s.Name, errors.New("conflict"))
		}
		return s, nil
	}).Times(2)
	sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).Return(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "43"},
		Data: map[string][]byte{
			PlanKey:            planBytes,
			AppliedChecksumKey: []byte(checksum),
		},
	}, nil)

	w := newTestWatcher(t, false, "")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data:       map[string][]byte{PlanKey: planBytes},
	}

	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if result.ResourceVersion != "43" {
		t.Errorf("expected the retried update to return the latest secret (resource version 43), got %q", result.ResourceVersion)
	}
}
