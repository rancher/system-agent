package k8splan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/system-agent/pkg/config"
	"github.com/rancher/wrangler/v3/pkg/generic/fake"
	"github.com/sirupsen/logrus"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func TestReconcileSecretNilSecretIsANoOp(t *testing.T) {
	t.Parallel()

	// No EXPECT() calls configured: a nil secret must return before touching the Kubernetes API.
	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)

	w := newTestWatcher(t, true, "")
	result, err := w.reconcileSecret(context.Background(), sc, nil, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if result != nil {
		t.Errorf("expected a nil result for a nil secret, got %+v", result)
	}
}

func TestReconcileSecretNoPlanDataEnqueuesAndReturns(t *testing.T) {
	t.Parallel()

	sc := newMockSecretController(t)
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any())

	w := newTestWatcher(t, true, "42")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data:       map[string][]byte{},
	}

	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected the secret to be returned unchanged, got nil")
	}
}

func TestReconcileSecretInvalidPlanJSONReturnsError(t *testing.T) {
	t.Parallel()

	// No EXPECT() calls configured: a CalculatePlan failure must return before any API call.
	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)

	w := newTestWatcher(t, true, "42")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data:       map[string][]byte{PlanKey: []byte("not valid json")},
	}

	if _, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second); err == nil {
		t.Fatal("expected an error for unparsable plan JSON, got nil")
	}
}

func TestReconcileSecretPlanStateFirstObservationSetsHasRunOnce(t *testing.T) {
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

	w := newTestWatcher(t, false, "42") // hasRunOnce starts false, unlike the other plan-state cases
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data: map[string][]byte{
			PlanKey:              planBytes,
			planapi.PlanStateKey: []byte(planapi.PlanStateSucceeded),
		},
	}

	if _, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second); err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if !w.hasRunOnce {
		t.Error("expected hasRunOnce to become true after observing any plan-state, even a terminal one")
	}
}

func TestReconcileSecretPendingCommitFailurePropagatesError(t *testing.T) {
	t.Parallel()

	planBytes, _ := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "ok", Command: "sh", Args: []string{"-c", "true"}}, SaveOutput: true},
		},
	})

	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	// A non-conflict Update error on the pending -> in-progress commit propagates immediately;
	// the retry/merge machinery in updateSecret only special-cases conflicts, and
	// retry.DefaultBackoff also retries plain errors, so this mock keeps returning the same
	// error until the backoff is exhausted and the original error surfaces.
	sc.EXPECT().Update(gomock.Any()).Return(nil, errors.New("etcd is unavailable")).AnyTimes()

	w := newTestWatcher(t, true, "42")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data: map[string][]byte{
			PlanKey:              planBytes,
			planapi.PlanStateKey: []byte(planapi.PlanStatePending),
		},
	}

	if _, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second); err == nil {
		t.Fatal("expected an error when the pending -> in-progress commit fails, got nil")
	}
}

func TestReconcileSecretSteadyStateSkipsUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	planBytes, _ := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "ok", Command: "sh", Args: []string{"-c", "true"}}, SaveOutput: true},
		},
	})

	w := newTestWatcher(t, false, "")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data:       map[string][]byte{PlanKey: planBytes},
	}

	// First reconcile force-applies (first start) and establishes steady-state secret data —
	// exact gzip-encoded byte values (periodic output, one-time output) are an implementation
	// detail of Applyinator, not hand-computed here.
	sc1 := newMockSecretController(t)
	sc1.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any())
	settled, err := w.reconcileSecret(context.Background(), sc1, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("first reconcileSecret returned error: %v", err)
	}

	// Second reconcile against the exact same (now steady-state) data and an unchanged resource
	// version: nothing should differ, so reconcileSecret must skip the Update call entirely. No
	// Update EXPECT() is configured on this mock, so an unexpected call fails the test.
	ctrl := gomock.NewController(t)
	sc2 := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	sc2.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any())

	result, err := w.reconcileSecret(context.Background(), sc2, settled.DeepCopy(), 30*time.Second)
	if err != nil {
		t.Fatalf("second reconcileSecret returned error: %v", err)
	}
	if result.ResourceVersion != settled.ResourceVersion {
		t.Errorf("expected the unchanged secret to be returned as-is, got resource version %q", result.ResourceVersion)
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

// TestReconcileSecretStaleResourceVersionSkipped pins that a delivery older than the agent's own
// last write is a benign skip rather than an error.
//
// The handler is always invoked with the object held by the informer cache, and that cache catches
// up to the agent's own writes asynchronously, so this delivery is routine after any reconcile that
// writes the Secret — and an interrupt reconcile writes it twice. Skipping is what protects the
// newer state; returning an error would additionally requeue the plan under the workqueue's
// exponential rate limiter and stall the probes with it, over a Secret that is not stale in the way
// the check exists to catch: it carries the same plan.
func TestReconcileSecretStaleResourceVersionSkipped(t *testing.T) {
	t.Parallel()

	planBytes, _ := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "must-not-run", Command: "sh", Args: []string{"-c", "true"}}},
		},
	})

	// EnqueueAfter is the only permitted call: the mock fails the test if reconcileSecret reads or
	// writes the Secret, proving the skip returns before doing any work.
	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	var enqueued []time.Duration
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any()).Do(
		func(_, _ string, d time.Duration) { enqueued = append(enqueued, d) }).AnyTimes()

	w := newTestWatcher(t, true, "100")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "1"},
		Data:       map[string][]byte{PlanKey: planBytes},
	}

	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("expected a stale delivery to be skipped without an error, got %v", err)
	}
	if len(enqueued) != 1 || enqueued[0] != w.probePeriod {
		t.Errorf("expected a single re-enqueue after %v so the probes keep their cadence, got %v", w.probePeriod, enqueued)
	}
	if result.ResourceVersion != "1" {
		t.Errorf("expected the input secret to be returned untouched at resource version %q, got %q", "1", result.ResourceVersion)
	}
	if w.lastAppliedResourceVersion != "100" {
		t.Errorf("expected the skip to leave lastAppliedResourceVersion at %q, got %q", "100", w.lastAppliedResourceVersion)
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
	// The exact Update call count and the in-progress-before-Apply ordering are asserted in
	// TestReconcileSecretCommitsInProgressBeforeApply.
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

// TestReconcileSecretCommitsInProgressBeforeApply verifies in-progress commit ordering.
// Ensure pending -> in-progress and plan-revision reach the API server before Apply runs.
// This write lets a crashed agent re-execute the plan on restart.
func TestReconcileSecretCommitsInProgressBeforeApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	// The instruction records when Apply ran by touching a marker file, so ordering can be
	// asserted against the observed Update calls rather than inferred.
	markerDir := t.TempDir()
	marker := filepath.Join(markerDir, "apply-ran")
	planBytes, _ := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "marker", Command: "sh", Args: []string{"-c", "touch " + marker}}},
		},
	})

	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any()).AnyTimes()

	type observedUpdate struct {
		planState    string
		planRevision string
		applyHadRun  bool
	}
	var observed []observedUpdate
	sc.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		_, statErr := os.Stat(marker)
		observed = append(observed, observedUpdate{
			planState:    string(s.Data[planapi.PlanStateKey]),
			planRevision: string(s.Data[planapi.PlanRevisionKey]),
			applyHadRun:  statErr == nil,
		})
		return s, nil
	}).AnyTimes()

	w := newTestWatcher(t, false, "")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data: map[string][]byte{
			PlanKey:                 planBytes,
			planapi.PlanStateKey:    []byte(planapi.PlanStatePending),
			planapi.PlanRevisionKey: []byte("7"),
		},
	}

	if _, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second); err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	if len(observed) != 2 {
		t.Fatalf("expected exactly 2 Update calls (in-progress, then the outcome), got %d: %+v", len(observed), observed)
	}
	first := observed[0]
	if first.planState != string(planapi.PlanStateInProgress) {
		t.Errorf("expected the first Update to commit plan-state %q, got %q", planapi.PlanStateInProgress, first.planState)
	}
	if first.applyHadRun {
		t.Error("expected the in-progress write to reach the API server BEFORE Apply ran; crash recovery depends on this ordering")
	}
	if first.planRevision != "8" {
		t.Errorf("expected plan-revision to be incremented to 8 in the same write as in-progress, got %q", first.planRevision)
	}
	if observed[1].planState != string(planapi.PlanStateSucceeded) {
		t.Errorf("expected the second Update to commit the outcome %q, got %q", planapi.PlanStateSucceeded, observed[1].planState)
	}
	if !observed[1].applyHadRun {
		t.Error("expected the outcome write to happen after Apply ran")
	}
}

// TestReconcileSecretInProgressOnStartupReExecutes covers the crash-recovery entry point: an agent
// that restarts and finds plan-state already in-progress must re-execute the plan rather than
// treating it as terminal.
func TestReconcileSecretInProgressOnStartupReExecutes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "re-executed")
	planBytes, _ := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "marker", Command: "sh", Args: []string{"-c", "touch " + marker}}},
		},
	})

	sc := newMockSecretController(t)
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any()).AnyTimes()

	w := newTestWatcher(t, false, "")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, ResourceVersion: "42"},
		Data: map[string][]byte{
			PlanKey:              planBytes,
			planapi.PlanStateKey: []byte(planapi.PlanStateInProgress),
		},
	}

	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("expected the plan to be re-executed on an in-progress startup, marker missing: %v", statErr)
	}
	if planapi.PlanState(result.Data[planapi.PlanStateKey]) != planapi.PlanStateSucceeded {
		t.Errorf("expected final plan-state %q, got %q", planapi.PlanStateSucceeded, result.Data[planapi.PlanStateKey])
	}
}

// --- interrupt wiring helpers -------------------------------------------------------------------

// interruptRecorder models the API server for the interrupt paths. writeInterruptOutcome does a
// fresh Get before every write and verifies the Secret still carries the plan, so a mock that
// served a fixed Secret would hide the read half of that read-modify-write; this one serves back
// whatever was last written.
type interruptRecorder struct {
	mu       sync.Mutex
	server   *corev1.Secret
	updates  []*corev1.Secret
	enqueued []time.Duration
}

func newInterruptRecorder(secret *corev1.Secret) *interruptRecorder {
	return &interruptRecorder{server: secret.DeepCopy()}
}

func (r *interruptRecorder) get() *corev1.Secret {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.server.DeepCopy()
}

// update records the write and stores it, bumping the resourceVersion exactly as the API server
// would.
//
// The bump is not cosmetic and must not be dropped: without it a Secret that WAS written comes
// back at the resourceVersion it went in with, so every "the returned resourceVersion is
// byte-identical to the input's" assertion in this package passes whether or not a write happened
// — which is precisely the inference those assertions exist to replace. The recorded write keeps
// the resourceVersion the agent submitted, so a test can still assert which object a write was
// built on.
func (r *interruptRecorder) update(s *corev1.Secret) *corev1.Secret {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, s.DeepCopy())
	stored := s.DeepCopy()
	stored.ResourceVersion = strconv.Itoa(toInt(s.ResourceVersion) + 1)
	r.server = stored
	return stored.DeepCopy()
}

func (r *interruptRecorder) enqueue(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueued = append(r.enqueued, d)
}

// setAnnotations replaces the annotations on the server-side copy, modelling the one write the
// agent never makes: the orchestrator owns these keys, and the agent only ever reads them. Writes
// merge into the freshly fetched object, so an annotation set here survives every later update.
func (r *interruptRecorder) setAnnotations(annotations map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.server.Annotations = annotations
}

// writes returns the Secrets handed to each Update call, in order.
func (r *interruptRecorder) writes() []*corev1.Secret {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*corev1.Secret(nil), r.updates...)
}

func (r *interruptRecorder) enqueuePeriods() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.enqueued...)
}

// newInterruptTestController wires a SecretController backed by r.
//
// Cache() is deliberately left unstubbed: reconcileSecret reaches the informer cache only through
// startInterruptWatch, so any test that does not explicitly opt in fails outright if an interrupt
// watch is started. That is what makes "the checksum flow starts no interrupt channels" an
// assertion rather than a claim.
func newInterruptTestController(t *testing.T, r *interruptRecorder) *fake.MockControllerInterface[*corev1.Secret, *corev1.SecretList] {
	t.Helper()
	return newInterruptTestControllerWithHook(t, r, nil)
}

// newInterruptTestControllerWithHook is newInterruptTestController with a hook invoked on every
// Update, so a test can record what had already happened by the time each write reached the API
// server.
func newInterruptTestControllerWithHook(t *testing.T, r *interruptRecorder, onUpdate func(*corev1.Secret),
) *fake.MockControllerInterface[*corev1.Secret, *corev1.SecretList] {
	t.Helper()

	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).DoAndReturn(
		func(string, string, metav1.GetOptions) (*corev1.Secret, error) { return r.get(), nil },
	).AnyTimes()
	sc.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		if onUpdate != nil {
			onUpdate(s)
		}
		return r.update(s), nil
	}).AnyTimes()
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any()).Do(
		func(_, _ string, d time.Duration) { r.enqueue(d) },
	).AnyTimes()
	return sc
}

// newInterruptTestSecret builds the plan Secret handed to reconcileSecret.
func newInterruptTestSecret(planBytes []byte, annotations map[string]string, data map[string][]byte) *corev1.Secret {
	full := map[string][]byte{PlanKey: planBytes}
	maps.Copy(full, data)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            testSecret,
			ResourceVersion: "42",
			Annotations:     annotations,
		},
		Data: full,
	}
}

// observedUpdate is one Update call as seen by observeUpdateOrdering.
type observedUpdate struct {
	planState   string
	paused      bool
	applyHadRun bool
}

// observeUpdateOrdering wires a SecretController backed by rec that records, for every Update, the
// plan-state and checkpoint the write carried and whether applyMarker existed at the moment it
// reached the API server.
//
// That last field is the whole point: the ordering rules in this package — the pending ->
// in-progress pre-commit, and the resume commit — are about a write landing before Apply RUNS, not
// before some other write. Observing an actual side effect of the apply is the only way to assert
// that; call order alone cannot.
//
// The returned func reports the observations so far, in order.
func observeUpdateOrdering(t *testing.T, rec *interruptRecorder, applyMarker string,
) (*fake.MockControllerInterface[*corev1.Secret, *corev1.SecretList], func() []observedUpdate) {
	t.Helper()

	var mu sync.Mutex
	var observed []observedUpdate
	sc := newInterruptTestControllerWithHook(t, rec, func(s *corev1.Secret) {
		_, statErr := os.Stat(applyMarker)
		// Decoded leniently rather than through checkpointIn: a terminal outcome write clears
		// this key to an empty value, which is absence and not corruption.
		var progress PlanCheckpoint
		_ = json.Unmarshal(s.Data[planapi.PlanCheckpointKey], &progress)

		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, observedUpdate{
			planState:   string(s.Data[planapi.PlanStateKey]),
			paused:      progress.Paused,
			applyHadRun: statErr == nil,
		})
	})
	return sc, func() []observedUpdate {
		mu.Lock()
		defer mu.Unlock()
		return append([]observedUpdate(nil), observed...)
	}
}

// assertResumeCommitLandedFirst asserts the ordering the resume path exists to guarantee: the write
// that releases a suspension is the FIRST write of the reconcile and reaches the API server BEFORE
// Apply runs.
func assertResumeCommitLandedFirst(t *testing.T, observed []observedUpdate, wantState planapi.PlanState) {
	t.Helper()

	if len(observed) < 1 {
		t.Fatal("expected at least the resume commit to be written")
	}
	first := observed[0]
	if first.planState != string(wantState) {
		t.Errorf("expected the resume commit to write the resolved plan-state %q, got %q", wantState, first.planState)
	}
	if first.paused {
		t.Error("expected the resume commit to clear the checkpoint's Paused flag; a lingering flag would make a second pause a no-op")
	}
	if first.applyHadRun {
		t.Error("expected the resume commit to reach the API server BEFORE Apply ran; a plan that is executing must not report paused")
	}
}

// checkpointIn decodes the resume checkpoint out of a Secret data map.
func checkpointIn(t *testing.T, data map[string][]byte) PlanCheckpoint {
	t.Helper()
	raw, ok := data[planapi.PlanCheckpointKey]
	if !ok {
		t.Fatalf("expected a %q checkpoint, got only the keys %v", planapi.PlanCheckpointKey, keysOf(data))
	}
	var p PlanCheckpoint
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("failed to decode the %q checkpoint %q: %v", planapi.PlanCheckpointKey, raw, err)
	}
	return p
}

// touchInstruction returns a one-time instruction that creates sentinel when it runs, so a test
// can assert whether the plan executed at all.
func touchInstruction(name, sentinel string) planapi.OneTimeInstruction {
	return planapi.OneTimeInstruction{
		CommonInstruction: planapi.CommonInstruction{Name: name, Command: "sh", Args: []string{"-c", "touch " + sentinel}},
		SaveOutput:        true,
	}
}

// gatedTouchInstruction is touchInstruction that then blocks until gate exists. It is how a test
// holds an apply open at a known point, long enough for the interrupt watch to observe an
// annotation, without depending on any timing.
func gatedTouchInstruction(name, sentinel, gate string) planapi.OneTimeInstruction {
	return planapi.OneTimeInstruction{
		CommonInstruction: planapi.CommonInstruction{
			Name:    name,
			Command: "sh",
			Args:    []string{"-c", "touch " + sentinel + "; while [ ! -f " + gate + " ]; do sleep 0.02; done"},
		},
		SaveOutput: true,
	}
}

func assertPathAbsent(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("expected %s not to exist: %s", path, why)
	}
}

// serveInterruptOnceApplyStarted stubs the interrupt watch's informer cache so annotation appears
// only after startedSentinel exists — that is, only once the apply is genuinely in flight, and
// never before Apply's pre-lock interruption check, which would abandon the apply before a single
// instruction ran and make the resulting checkpoint meaningless.
//
// onServed is invoked with the number of polls that have served the annotation so far, on the same
// goroutine as the watch. A caller can therefore act on served > 1 knowing that pollInterrupts has
// already closed its channel on the previous poll, which is how a test releases a gated
// instruction at an exactly-known point instead of guessing at a sleep.
func serveInterruptOnceApplyStarted(t *testing.T, sc *fake.MockControllerInterface[*corev1.Secret, *corev1.SecretList],
	annotation, startedSentinel string, onServed func(served int),
) {
	t.Helper()

	var mu sync.Mutex
	var served int
	cache := fake.NewMockCacheInterface[*corev1.Secret](gomock.NewController(t))
	sc.EXPECT().Cache().Return(cache).AnyTimes()
	cache.EXPECT().Get(testNamespace, testSecret).DoAndReturn(func(string, string) (*corev1.Secret, error) {
		mu.Lock()
		defer mu.Unlock()
		observed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret}}
		if _, err := os.Stat(startedSentinel); err != nil {
			return observed, nil // the apply has not reached the instruction yet
		}
		served++
		if onServed != nil {
			onServed(served)
		}
		observed.Annotations = map[string]string{annotation: "true"}
		return observed, nil
	}).AnyTimes()
}

// syncBuffer is an io.Writer safe to read while logrus is writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs redirects logrus's standard logger into a buffer for the duration of the test and
// returns a func reading everything written so far.
//
// The t.Setenv call sets nothing anybody reads: it is a tripwire, exactly as in
// withInterruptPollInterval. t.Setenv panics if the test, or any ancestor of it, has called
// t.Parallel() — which is precisely the condition under which swapping the process-wide logger
// output would interleave with, and steal output from, every other test in the package.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	t.Setenv("K8SPLAN_LOG_CAPTURE_GUARD", "1")

	buf := &syncBuffer{}
	original := logrus.StandardLogger().Out
	logrus.SetOutput(buf)
	t.Cleanup(func() { logrus.SetOutput(original) })
	return buf.String
}

// --- interrupt wiring tests ---------------------------------------------------------------------

// TestReconcileSecretInterruptAtEntrySuppressesTheApply is the wiring half of the suppression
// invariant: in the plan-state flow an interrupt annotation is read before any decision to execute
// is taken, so the plan is recorded as held and nothing runs. The sentinel file is the assertion
// that matters — a plan-state of "paused" written by an agent that ran the plan anyway would be
// worse than no feature at all. Task 6 owns the exhaustive matrix.
func TestReconcileSecretInterruptAtEntrySuppressesTheApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	tests := []struct {
		name            string
		annotation      string
		planState       planapi.PlanState
		wantState       planapi.PlanState
		wantPaused      bool
		wantResumeState planapi.PlanState
	}{
		{
			name:       "cancel on in-progress records a terminal cancellation and a report, not a suspension",
			annotation: planapi.PlanCanceledAnnotation,
			planState:  planapi.PlanStateInProgress,
			wantState:  planapi.PlanStateCanceled,
			wantPaused: false,
		},
		{
			name:            "pause on pending records a suspension that resumes into pending",
			annotation:      planapi.PlanPausedAnnotation,
			planState:       planapi.PlanStatePending,
			wantState:       planapi.PlanStatePaused,
			wantPaused:      true,
			wantResumeState: planapi.PlanStatePending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sentinel := filepath.Join(t.TempDir(), "plan-ran")
			planBytes, checksum := marshalPlan(t, planapi.Plan{
				OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("ran", sentinel)},
			})

			secret := newInterruptTestSecret(planBytes,
				map[string]string{tt.annotation: "true"},
				map[string][]byte{planapi.PlanStateKey: []byte(tt.planState)})
			rec := newInterruptRecorder(secret)
			sc := newInterruptTestController(t, rec)

			w := newTestWatcher(t, false, "42")
			result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
			if err != nil {
				t.Fatalf("reconcileSecret returned error: %v", err)
			}

			assertPathAbsent(t, sentinel, "an interrupt observed at reconcile entry must suppress the apply entirely")
			if w.hasRunOnce {
				t.Error("expected the interrupt entry path not to set hasRunOnce; it returns above the decision that owns that flag, " +
					"and mutable state must not live on the far side of the safety returns")
			}
			if planapi.PlanState(result.Data[planapi.PlanStateKey]) != tt.wantState {
				t.Errorf("expected plan-state %q, got %q", tt.wantState, result.Data[planapi.PlanStateKey])
			}
			if len(result.Data[AppliedChecksumKey]) != 0 {
				t.Errorf("expected applied-checksum not to be written for a plan that never ran, got %q", result.Data[AppliedChecksumKey])
			}

			got := checkpointIn(t, result.Data)
			if got.Paused != tt.wantPaused {
				t.Errorf("expected checkpoint Paused=%v, got %+v", tt.wantPaused, got)
			}
			if got.ResumeState != tt.wantResumeState {
				t.Errorf("expected checkpoint ResumeState %q, got %+v", tt.wantResumeState, got)
			}
			if got.Checksum != checksum || got.Total != 1 {
				t.Errorf("expected the checkpoint to be scoped to checksum %q with Total 1, got %+v", checksum, got)
			}

			if len(rec.writes()) != 1 {
				t.Errorf("expected exactly one Update (the interrupt outcome), got %d", len(rec.writes()))
			}
			if periods := rec.enqueuePeriods(); len(periods) != 1 || periods[0] != w.probePeriod {
				t.Errorf("expected a single re-enqueue after %v, got %v", w.probePeriod, periods)
			}
		})
	}
}

// TestReconcileSecretInvalidAnnotationValueWritesNothing pins the narrowest branch in the
// reconcile. An unreadable annotation executes nothing, interrupts nothing and writes nothing, so
// resourceVersion is stable for as long as the error persists and the error cannot amplify into a
// write loop. It deliberately does not record plan-state: failed — "failed" is a claim about the
// node, and nothing ran.
//
// The resourceVersion assertion is what makes "writes nothing" checked rather than inferred from a
// call count.
func TestReconcileSecretInvalidAnnotationValueWritesNothing(t *testing.T) {
	t.Parallel()

	planBytes, _ := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "ok", Command: "sh", Args: []string{"-c", "true"}}},
		},
	})

	secret := newInterruptTestSecret(planBytes,
		map[string]string{planapi.PlanPausedAnnotation: "yes"},
		map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStateInProgress)})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, true, "42")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err == nil {
		t.Fatal("expected an error for an uninterpretable annotation value, got nil")
	}
	if len(rec.writes()) != 0 {
		t.Errorf("expected zero Update calls, got %d", len(rec.writes()))
	}
	if periods := rec.enqueuePeriods(); len(periods) != 0 {
		t.Errorf("expected no re-enqueue (the workqueue's rate limiter owns the retry), got %v", periods)
	}
	if result.ResourceVersion != "42" {
		t.Errorf("expected the returned secret's resource version to be byte-identical to the input's (%q), got %q", "42", result.ResourceVersion)
	}
}

// TestReconcileSecretInterruptStillRunsProbes pins that an interrupt suppresses execution but
// never observation. Freezing probe statuses would feed stale health data to Rancher's
// MachineHealthCheck on exactly the nodes most likely to be unhealthy: a plan stopped mid-flight
// leaves the node in a partial state.
func TestReconcileSecretInterruptStillRunsProbes(t *testing.T) {
	t.Parallel()

	probes, probeHits := countingProbe(t)
	planBytes, _ := marshalPlan(t, planapi.Plan{Probes: probes})

	secret := newInterruptTestSecret(planBytes,
		map[string]string{planapi.PlanPausedAnnotation: "true"},
		map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStateInProgress)})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, true, "42")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	writes := rec.writes()
	if len(writes) != 1 {
		t.Fatalf("expected exactly one Update, got %d", len(writes))
	}
	assertProbeStatusPersisted(t, writes[0].Data, probeHits)
	if planapi.PlanState(result.Data[planapi.PlanStateKey]) != planapi.PlanStatePaused {
		t.Errorf("expected the plan to still be recorded as %q, got %q", planapi.PlanStatePaused, result.Data[planapi.PlanStateKey])
	}
}

// TestReconcileSecretChecksumFlowIgnoresAnnotations pins the compatibility rule: legacy
// orchestrators that never write plan-state get ordinary checksum reconciliation, and the
// interrupt annotations have no effect there at all. Silently honouring them would give a
// half-working feature against a server that has no way to clear the resulting state.
//
// Not parallel: captureLogs swaps the process-wide logrus output.
func TestReconcileSecretChecksumFlowIgnoresAnnotations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	logs := captureLogs(t)

	sentinel := filepath.Join(t.TempDir(), "plan-ran")
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("ran", sentinel)},
	})

	// No plan-state key: the checksum flow. The annotation is set, and must change nothing.
	secret := newInterruptTestSecret(planBytes, map[string]string{planapi.PlanCanceledAnnotation: "true"}, nil)
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, false, "")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Errorf("expected the plan to be applied under ordinary checksum semantics, sentinel missing: %v", statErr)
	}
	if string(result.Data[AppliedChecksumKey]) != checksum {
		t.Errorf("expected applied-checksum %q, got %q", checksum, result.Data[AppliedChecksumKey])
	}
	if len(result.Data[planapi.PlanStateKey]) != 0 {
		t.Errorf("expected the checksum flow never to write plan-state, got %q", result.Data[planapi.PlanStateKey])
	}
	// Key absent, not merely empty: the checksum flow must not so much as invent this key on a
	// Secret owned by an orchestrator that knows nothing about the feature.
	if value, ok := result.Data[planapi.PlanCheckpointKey]; ok {
		t.Errorf("expected the checksum flow never to write %q at all, got %q", planapi.PlanCheckpointKey, value)
	}

	want := "ignoring unsupported annotation in checksum flow key=" + planapi.PlanCanceledAnnotation + " value=true"
	if !strings.Contains(logs(), want) {
		t.Errorf("expected a warning containing %q, got:\n%s", want, logs())
	}
}

// TestReconcileSecretChecksumFlowStartsNoInterruptWatch is the structural half of the rule above.
// The controller returned by newInterruptTestController does not stub Cache(), which
// startInterruptWatch's polling goroutine is the only thing in reconcileSecret that reaches for —
// so a watch started in the checksum flow fails this test on an unexpected call.
//
// Two details make that assertion real rather than nominal, and both were got wrong first time
// round. The watch touches the cache only on its first tick, so the poll interval must be short
// AND the apply must outlive several ticks; otherwise stopWatch() closes stopCh before a single
// poll happens and an unconditionally-started watch goes undetected. Hence the sleeping
// instruction, and hence no t.Parallel() — withInterruptPollInterval writes a package-level var.
func TestReconcileSecretChecksumFlowStartsNoInterruptWatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	withInterruptPollInterval(t, 5*time.Millisecond)

	sentinel := filepath.Join(t.TempDir(), "plan-ran")
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			// Runs for ~100 poll intervals, so a watch that should not exist has every
			// opportunity to reach the unstubbed cache.
			{CommonInstruction: planapi.CommonInstruction{
				Name: "ran", Command: "sh", Args: []string{"-c", "touch " + sentinel + "; sleep 0.5"},
			}},
		},
	})

	secret := newInterruptTestSecret(planBytes,
		map[string]string{planapi.PlanPausedAnnotation: "true", planapi.PlanCanceledAnnotation: "true"}, nil)
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, false, "")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Errorf("expected the plan to be applied with both annotations present throughout, sentinel missing: %v", statErr)
	}
	if string(result.Data[AppliedChecksumKey]) != checksum {
		t.Errorf("expected applied-checksum %q, got %q", checksum, result.Data[AppliedChecksumKey])
	}
}

// countingProbe returns a plan's Probes map pointed at a freshly started httptest server, plus a
// func reporting how many times it has been hit. The server is closed on test cleanup.
func countingProbe(t *testing.T) (map[string]planapi.Probe, func() int64) {
	t.Helper()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return map[string]planapi.Probe{
		"health": {HTTPGetAction: planapi.HTTPGetAction{URL: server.URL, Insecure: true}},
	}, hits.Load
}

// assertProbeStatusPersisted asserts that countingProbe's probe ran and that its status reached
// the given Secret data — the whole point of "an interrupt suppresses execution, never
// observation" is that both halves happen on every interrupt path.
func assertProbeStatusPersisted(t *testing.T, data map[string][]byte, hits func() int64) {
	t.Helper()

	if hits() == 0 {
		t.Error("expected the probe to be executed while the plan was interrupted")
	}
	raw, ok := data[ProbeStatusesKey]
	if !ok {
		t.Fatalf("expected %q to be persisted by the interrupt write, got the keys %v", ProbeStatusesKey, keysOf(data))
	}
	var statuses map[string]planapi.ProbeStatus
	if err := json.Unmarshal(raw, &statuses); err != nil {
		t.Fatalf("failed to decode probe statuses %q: %v", raw, err)
	}
	if !statuses["health"].Healthy {
		t.Errorf("expected the probe status to be persisted as healthy, got %+v", statuses)
	}
}

// TestReconcileSecretPauseDuringApplyIsNotRecordedAsAFailure pins the ordering Task 1 established:
// an interrupted apply reports OneTimeApplySucceeded: false alongside its Interruption, so the
// caller must test Interruption first. Routing this outcome through buildSecretDataUpdates would
// record plan-state: failed — and a failure count, and a failed-checksum — for a plan the operator
// stopped on purpose.
//
// It also carries the probe assertions for the interrupted-outcome path, which is the second place
// "an interrupt suppresses execution, never observation" has to hold.
//
// Not parallel: it shortens the package-level interruptPollInterval.
func TestReconcileSecretPauseDuringApplyIsNotRecordedAsAFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	withInterruptPollInterval(t, 2*time.Millisecond)

	dir := t.TempDir()
	firstSentinel := filepath.Join(dir, "first-ran")
	secondSentinel := filepath.Join(dir, "second-ran")
	gate := filepath.Join(dir, "gate")

	probes, probeHits := countingProbe(t)
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			gatedTouchInstruction("first", firstSentinel, gate),
			touchInstruction("second", secondSentinel),
		},
		Probes: probes,
	})

	// The input Secret carries no annotation: the pause must arrive mid-apply, through the
	// interrupt watch, not at reconcile entry.
	secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePending),
	})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	// The interrupt watch's view of the Secret. The annotation appears only once instruction 0 has
	// started, and the gate is released only on the poll *after* the one that served it — by which
	// point pollInterrupts has certainly closed the pause channel, since both polls run in the
	// same goroutine. So the pause lands strictly between the two instructions, with no timing
	// assumption and no race against Apply's pre-lock interruption check.
	serveInterruptOnceApplyStarted(t, sc, planapi.PlanPausedAnnotation, firstSentinel, func(served int) {
		if served > 1 {
			if writeErr := os.WriteFile(gate, nil, 0600); writeErr != nil {
				t.Errorf("failed to release the gate: %v", writeErr)
			}
		}
	})

	w := newTestWatcher(t, true, "42")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	if planapi.PlanState(result.Data[planapi.PlanStateKey]) != planapi.PlanStatePaused {
		t.Errorf("expected plan-state %q, got %q; an interrupted apply must not be reported as a plan failure",
			planapi.PlanStatePaused, result.Data[planapi.PlanStateKey])
	}
	if len(result.Data[AppliedChecksumKey]) != 0 {
		t.Errorf("expected applied-checksum NOT to be written for an unapplied plan, got %q", result.Data[AppliedChecksumKey])
	}
	if len(result.Data[AppliedOutputKey]) == 0 {
		t.Error("expected applied-output to be written so SaveOutput results from completed instructions survive the hold")
	}
	if len(result.Data[FailedChecksumKey]) != 0 || len(result.Data[FailureCountKey]) != 0 {
		t.Errorf("expected the failure bookkeeping to be untouched, got failed-checksum %q and failure-count %q",
			result.Data[FailedChecksumKey], result.Data[FailureCountKey])
	}
	assertPathAbsent(t, secondSentinel, "a pause stops the apply at the next instruction boundary")
	assertProbeStatusPersisted(t, result.Data, probeHits)

	got := checkpointIn(t, result.Data)
	want := PlanCheckpoint{Checksum: checksum, Completed: 1, Total: 2, ResumeState: planapi.PlanStateInProgress, Paused: true}
	if got != want {
		t.Errorf("expected checkpoint %+v, got %+v", want, got)
	}
	if periods := rec.enqueuePeriods(); len(periods) != 1 || periods[0] != w.probePeriod {
		t.Errorf("expected a single re-enqueue after %v, got %v", w.probePeriod, periods)
	}
}

// TestReconcileSecretResumeCommitLandsBeforeTheApply follows
// TestReconcileSecretCommitsInProgressBeforeApply's pattern for the same reason: ordering is the
// whole point. A plan that is executing must not report "paused" on the wire, so the write that
// clears the suspension has to reach the API server before Apply runs, not after it.
func TestReconcileSecretResumeCommitLandsBeforeTheApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "apply-ran")
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("marker", marker)},
	})

	// A plan held at instruction 0, whose annotation the operator has just removed.
	secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePaused),
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{
			Checksum: checksum, Completed: 0, Total: 1, ResumeState: planapi.PlanStateInProgress, Paused: true,
		}),
	})

	rec := newInterruptRecorder(secret)
	sc, observations := observeUpdateOrdering(t, rec, marker)

	w := newTestWatcher(t, true, "42")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	assertResumeCommitLandedFirst(t, observations(), planapi.PlanStateInProgress)
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("expected the resumed plan to actually be applied, marker missing: %v", statErr)
	}
	if planapi.PlanState(result.Data[planapi.PlanStateKey]) != planapi.PlanStateSucceeded {
		t.Errorf("expected the resumed plan to finish as %q, got %q", planapi.PlanStateSucceeded, result.Data[planapi.PlanStateKey])
	}
}

// TestReconcileSecretResumeCommitDoesNotBumpPlanRevision pins that leaving a suspension is not a
// new revision of the plan: the content has not changed, only the agent's permission to act on it.
// A bump here would look to Rancher like the orchestrator had delivered something new.
//
// The fixture resumes into a terminal state on purpose — that is the case where the resume commit
// is the only lifecycle write the reconcile makes, so a bump could not be blamed on anything else.
func TestReconcileSecretResumeCommitDoesNotBumpPlanRevision(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	sentinel := filepath.Join(t.TempDir(), "plan-ran")
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("ran", sentinel)},
	})

	// The likeliest operator action of all: pausing a node whose plan already succeeded and which
	// is only running periodic instructions, then unpausing it.
	secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
		planapi.PlanStateKey:    []byte(planapi.PlanStatePaused),
		planapi.PlanRevisionKey: []byte("7"),
		AppliedChecksumKey:      []byte(checksum),
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{
			Checksum: checksum, Completed: 1, Total: 1, ResumeState: planapi.PlanStateSucceeded, Paused: true,
		}),
	})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, true, "42")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	writes := rec.writes()
	if len(writes) == 0 {
		t.Fatal("expected the resume commit to be written")
	}
	if got := string(writes[0].Data[planapi.PlanStateKey]); got != string(planapi.PlanStateSucceeded) {
		t.Errorf("expected the resume commit to restore plan-state %q, got %q", planapi.PlanStateSucceeded, got)
	}
	for i, write := range writes {
		if got := string(write.Data[planapi.PlanRevisionKey]); got != "7" {
			t.Errorf("expected plan-revision to stay at 7 in write %d, got %q", i, got)
		}
	}
	if got := string(result.Data[planapi.PlanRevisionKey]); got != "7" {
		t.Errorf("expected the resulting plan-revision to stay at 7, got %q", got)
	}
	assertPathAbsent(t, sentinel, "resuming into a terminal state monitors only; it must not re-execute the plan")
	if got := checkpointIn(t, writes[0].Data); got.Paused {
		t.Errorf("expected the resume commit to clear the checkpoint's Paused flag, got %+v", got)
	}
	// The terminal outcome write that follows clears the checkpoint outright — it has served its
	// purpose, and a stale one must not leak into a later run.
	if len(result.Data[planapi.PlanCheckpointKey]) != 0 {
		t.Errorf("expected the checkpoint to be cleared by the outcome write, got %q", result.Data[planapi.PlanCheckpointKey])
	}
}

// TestReconcileSecretCancelDuringApplyIsNotRecordedAsAFailure is the sharper half of the rule
// above, and the one that pins Task 1's ordering contract literally. A cancel kills the in-flight
// instruction, so the apply reports OneTimeApplySucceeded: false — testing that flag before
// Interruption would record plan-state: failed, a failure count and a failed-checksum for a plan
// the operator stopped deliberately, and would put it into the max-failures machinery it never
// belonged in.
//
// (The pause case cannot pin this on its own: a pause lands at an instruction boundary with
// nothing failed, so it reports OneTimeApplySucceeded: true and the same bug surfaces as a
// spurious "succeeded" instead.)
//
// Not parallel: it shortens the package-level interruptPollInterval.
func TestReconcileSecretCancelDuringApplyIsNotRecordedAsAFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	withInterruptPollInterval(t, 2*time.Millisecond)

	sentinel := filepath.Join(t.TempDir(), "sleeper-started")
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{
				Name: "sleeper", Command: "sh", Args: []string{"-c", "touch " + sentinel + "; sleep 60"},
			}},
		},
	})

	secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePending),
	})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)
	// A cancel is prompt rather than a boundary, so no gate is needed: the instruction is killed
	// where it stands.
	serveInterruptOnceApplyStarted(t, sc, planapi.PlanCanceledAnnotation, sentinel, nil)

	w := newTestWatcher(t, true, "42")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	if planapi.PlanState(result.Data[planapi.PlanStateKey]) != planapi.PlanStateCanceled {
		t.Errorf("expected plan-state %q, got %q; a cancel-induced kill must not be reported as a plan failure",
			planapi.PlanStateCanceled, result.Data[planapi.PlanStateKey])
	}
	if len(result.Data[FailedChecksumKey]) != 0 || len(result.Data[FailureCountKey]) != 0 || len(result.Data[FailedOutputKey]) != 0 {
		t.Errorf("expected the failure bookkeeping to be untouched, got failed-checksum %q, failure-count %q, failed-output of %d bytes",
			result.Data[FailedChecksumKey], result.Data[FailureCountKey], len(result.Data[FailedOutputKey]))
	}
	if len(result.Data[AppliedChecksumKey]) != 0 {
		t.Errorf("expected applied-checksum NOT to be written for an unapplied plan, got %q", result.Data[AppliedChecksumKey])
	}

	got := checkpointIn(t, result.Data)
	want := PlanCheckpoint{Checksum: checksum, Completed: 0, Total: 1}
	if got != want {
		t.Errorf("expected a cancellation report %+v (never a suspension: nothing resumes from it), got %+v", want, got)
	}
	if periods := rec.enqueuePeriods(); len(periods) != 1 || periods[0] != w.probePeriod {
		t.Errorf("expected a single re-enqueue after %v, got %v", w.probePeriod, periods)
	}
}

// TestRecordInterruptAfterApplyReportsIncompleteTermination pins the reporting half of the
// cancellation contract. plan-state canceled says the plan is over; the checkpoint's
// terminationIncomplete says whether the node was left quiescent. Those are separate claims, and
// making the first one does not entitle the agent to make the second.
//
// Driven through recordInterruptAfterApply rather than a real apply because the interesting input is
// an ApplyOutput the agent cannot be made to produce on demand: it requires a process that outlives
// SIGKILL. applyinator's own tests cover where the flag comes from.
func TestRecordInterruptAfterApplyReportsIncompleteTermination(t *testing.T) {
	t.Parallel()

	planBytes, _ := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("one", filepath.Join(t.TempDir(), "never-run"))},
	})
	cp, err := applyinator.CalculatePlan(planBytes)
	if err != nil {
		t.Fatalf("CalculatePlan returned error: %v", err)
	}

	testCases := []struct {
		name string
		// stored is a terminationIncomplete already on the Secret's checkpoint, from an earlier apply
		// of the same plan.
		stored bool
		// reported is what this apply observed.
		reported bool
		want     bool
	}{
		{name: "this apply could not confirm the process tree was gone", reported: true, want: true},
		{
			// The flag reports a hazard on the node, and this apply cannot see processes an earlier one
			// left behind, so a stored true has to survive. Over-reporting a hazard that has since been
			// cleaned up is the safe direction.
			name: "an earlier apply's report is not retracted", stored: true, want: true,
		},
		{name: "a confirmed termination reports nothing", want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStateInProgress)}
			if tc.stored {
				data[planapi.PlanCheckpointKey] = marshalPlanCheckpoint(PlanCheckpoint{Checksum: cp.Checksum, Total: 1, TerminationIncomplete: true})
			}
			secret := newInterruptTestSecret(planBytes, nil, data)
			sc := newInterruptTestController(t, newInterruptRecorder(secret))
			w := newTestWatcher(t, true, "")

			result, err := w.recordInterruptAfterApply(sc, secret.DeepCopy(), cp, planapi.PlanStateInProgress, true,
				applyinator.ApplyOutput{Interruption: applyinator.InterruptionCanceled, TerminationIncomplete: tc.reported},
				map[string]planapi.ProbeStatus{})
			if err != nil {
				t.Fatalf("recordInterruptAfterApply returned error: %v", err)
			}

			if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != planapi.PlanStateCanceled {
				t.Errorf("wrote plan-state %q, want %q: an unconfirmed termination does not change the plan's outcome", got, planapi.PlanStateCanceled)
			}
			if got := checkpointIn(t, result.Data).TerminationIncomplete; got != tc.want {
				t.Errorf("checkpoint recorded terminationIncomplete=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconcileSecretCanceledTerminalPlanMonitorsOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	periodicSentinel := filepath.Join(t.TempDir(), "periodic-ran")
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		PeriodicInstructions: []planapi.PeriodicInstruction{
			{CommonInstruction: planapi.CommonInstruction{
				Name: "periodic", Command: "sh", Args: []string{"-c", "touch " + periodicSentinel},
			}},
		},
	})

	cancelReport := marshalPlanCheckpoint(PlanCheckpoint{Checksum: checksum, Completed: 1, Total: 2})
	secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
		planapi.PlanStateKey:      []byte(planapi.PlanStateCanceled),
		AppliedChecksumKey:        []byte(""),
		planapi.PlanCheckpointKey: cancelReport,
		ProbeStatusesKey:          []byte("{}"),
	})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, true, "42")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	assertPathAbsent(t, periodicSentinel, "a canceled terminal plan should be monitored only")
	if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != planapi.PlanStateCanceled {
		t.Errorf("expected plan-state to remain %q, got %q", planapi.PlanStateCanceled, got)
	}
	if got := string(result.Data[AppliedChecksumKey]); got != "" {
		t.Errorf("expected applied-checksum to remain empty for a canceled plan, got %q", got)
	}
	if !bytes.Equal(result.Data[planapi.PlanCheckpointKey], cancelReport) {
		t.Errorf("expected cancellation report to be preserved, got %q want %q", result.Data[planapi.PlanCheckpointKey], cancelReport)
	}
	if writes := rec.writes(); len(writes) != 0 {
		t.Fatalf("expected no lifecycle write in monitoring-only mode, got %d", len(writes))
	}
	if periods := rec.enqueuePeriods(); len(periods) != 1 || periods[0] != w.probePeriod {
		t.Errorf("expected a single re-enqueue after %v, got %v", w.probePeriod, periods)
	}
}

func TestReconcileSecretFailedTerminalPlanMonitorsOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	periodicSentinel := filepath.Join(t.TempDir(), "failed-periodic-ran")
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		PeriodicInstructions: []planapi.PeriodicInstruction{
			{CommonInstruction: planapi.CommonInstruction{
				Name: "periodic", Command: "sh", Args: []string{"-c", "touch " + periodicSentinel},
			}},
		},
	})

	progressReport := marshalPlanCheckpoint(PlanCheckpoint{Checksum: checksum, Completed: 0, Total: 1})
	secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
		planapi.PlanStateKey:      []byte(planapi.PlanStateFailed),
		planapi.PlanCheckpointKey: progressReport,
		FailedChecksumKey:         []byte(checksum),
		FailureCountKey:           []byte("2"),
		ProbeStatusesKey:          []byte("{}"),
	})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, true, "42")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	assertPathAbsent(t, periodicSentinel, "a failed terminal plan should be monitored only")
	if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != planapi.PlanStateFailed {
		t.Errorf("expected plan-state to remain %q, got %q", planapi.PlanStateFailed, got)
	}
	if got := string(result.Data[AppliedChecksumKey]); got != "" {
		t.Errorf("expected applied-checksum to remain unset for a failed terminal plan, got %q", got)
	}
	if !bytes.Equal(result.Data[planapi.PlanCheckpointKey], progressReport) {
		t.Errorf("expected plan-progress report to be preserved, got %q want %q", result.Data[planapi.PlanCheckpointKey], progressReport)
	}
	if got := string(result.Data[FailureCountKey]); got != "2" {
		t.Errorf("expected failure count to be preserved, got %q", got)
	}
	if got := string(result.Data[FailedChecksumKey]); got != checksum {
		t.Errorf("expected failed-checksum to be preserved, got %q want %q", got, checksum)
	}
	if writes := rec.writes(); len(writes) != 0 {
		t.Fatalf("expected no lifecycle write in monitoring-only mode, got %d", len(writes))
	}
	if periods := rec.enqueuePeriods(); len(periods) != 1 || periods[0] != w.probePeriod {
		t.Errorf("expected a single re-enqueue after %v, got %v", w.probePeriod, periods)
	}
}

// TestReconcileSecretInterruptDuringAPeriodicApplyOfATerminalPlan pins that the mid-apply
// interrupt path applies cancel's terminal write-once guard exactly where the reconcile-entry path
// applies it, and — the other half, which is what makes the guard's cancel-only-ness load-bearing
// — that pause is NOT subject to it.
//
// The window is real, not theoretical. A succeeded plan is reconciled forever: it still runs its
// periodic instructions on every pass, so an apply is in flight for part of every cycle even
// though needsApplied is false. Without the guard the same `kubectl annotate canceled=true` on
// the same node yields "succeeded" (untouched) when it lands at reconcile entry and "canceled"
// when it lands during that periodic apply — a coin flip decided by which side of a 2s poll the
// operator's write falls on. Cancel is terminal, so the losing side of that flip is permanent.
//
// The pause row additionally pins that applied-output is not materialised when there is no
// one-time output to write: on the periodic-only path it is selectExistingOutput's empty slice,
// and inventing an empty applied-output on a Secret that never had one writes a key the agent
// does not own.
//
// Not parallel: it shortens the package-level interruptPollInterval.
func TestReconcileSecretInterruptDuringAPeriodicApplyOfATerminalPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	withInterruptPollInterval(t, 2*time.Millisecond)

	tests := []struct {
		name       string
		annotation string
		// wantRecorded is whether the interrupt should reach the Secret at all.
		wantRecorded bool
	}{
		{
			name:       "cancelling writes nothing: plan-state is already terminal",
			annotation: planapi.PlanCanceledAnnotation,
		},
		{
			name:         "pausing still records, because the resume reads the checkpoint back",
			annotation:   planapi.PlanPausedAnnotation,
			wantRecorded: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			started := filepath.Join(dir, "periodic-started")
			gate := filepath.Join(dir, "gate")
			mustNotRun := filepath.Join(dir, "second-periodic-ran")

			// Two periodic instructions: the first blocks until the gate opens, so the interrupt
			// lands while the apply is genuinely in flight; the second must never run, whichever
			// interrupt it was. A cancel kills the first where it stands, so its gate is never
			// opened.
			planBytes, checksum := marshalPlan(t, planapi.Plan{
				PeriodicInstructions: []planapi.PeriodicInstruction{
					{CommonInstruction: planapi.CommonInstruction{
						Name: "blocker", Command: "sh",
						Args: []string{"-c", "touch " + started + "; while [ ! -f " + gate + " ]; do sleep 0.02; done"},
					}},
					{CommonInstruction: planapi.CommonInstruction{
						Name: "second", Command: "sh", Args: []string{"-c", "touch " + mustNotRun},
					}},
				},
			})

			// A converged node: the plan succeeded, and this reconcile exists only to run the
			// periodic instructions and the probes. probe-statuses is pre-seeded at its steady
			// state so that "no lifecycle write" can be asserted as "no Update at all" — probes
			// deliberately keep running on the interrupt paths, and an absent key would make the
			// marshalled empty map a genuine change.
			secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
				planapi.PlanStateKey: []byte(planapi.PlanStateSucceeded),
				AppliedChecksumKey:   []byte(checksum),
				ProbeStatusesKey:     []byte("{}"),
			})
			rec := newInterruptRecorder(secret)
			sc := newInterruptTestController(t, rec)
			serveInterruptOnceApplyStarted(t, sc, tt.annotation, started, func(served int) {
				// Pause is a boundary, so the blocker has to be let go before the periodic loop
				// can reach its next-instruction check. Released on the poll AFTER the one that
				// served the annotation, by which point pollInterrupts has certainly closed the
				// pause channel: both polls run on the same goroutine.
				if tt.wantRecorded && served > 1 {
					if writeErr := os.WriteFile(gate, nil, 0600); writeErr != nil {
						t.Errorf("failed to release the gate: %v", writeErr)
					}
				}
			})

			w := newTestWatcher(t, true, "42")
			result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
			if err != nil {
				t.Fatalf("reconcileSecret returned error: %v", err)
			}

			assertPathAbsent(t, mustNotRun, "an interrupt must stop the periodic loop before the next instruction")
			if periods := rec.enqueuePeriods(); len(periods) != 1 || periods[0] != w.probePeriod {
				t.Errorf("expected a single re-enqueue after %v, got %v", w.probePeriod, periods)
			}

			writes := rec.writes()
			if !tt.wantRecorded {
				if len(writes) != 0 {
					t.Errorf("expected no Secret write at all, got %d: %v", len(writes), writes[0].Data)
				}
				if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != planapi.PlanStateSucceeded {
					t.Errorf("expected plan-state to stay %q, got %q; a converged node must not be downgraded by a cancel it would have ignored a moment earlier",
						planapi.PlanStateSucceeded, got)
				}
				if _, ok := result.Data[planapi.PlanCheckpointKey]; ok {
					t.Errorf("expected no checkpoint on a terminal plan, got %q", result.Data[planapi.PlanCheckpointKey])
				}
				return
			}

			if len(writes) != 1 {
				t.Fatalf("expected exactly one Secret write, got %d", len(writes))
			}
			if got := planapi.PlanState(writes[0].Data[planapi.PlanStateKey]); got != planapi.PlanStatePaused {
				t.Errorf("wrote plan-state %q, want %q; a pause on a terminal plan must still be recorded", got, planapi.PlanStatePaused)
			}
			want := PlanCheckpoint{Checksum: checksum, ResumeState: planapi.PlanStateSucceeded, Paused: true}
			if got := checkpointIn(t, writes[0].Data); got != want {
				t.Errorf("wrote checkpoint %+v, want %+v; the resume must restore succeeded rather than re-execute the plan", got, want)
			}
			if _, ok := writes[0].Data[AppliedOutputKey]; ok {
				t.Errorf("expected applied-output NOT to be materialised when there is no one-time output, got %q",
					writes[0].Data[AppliedOutputKey])
			}
		})
	}
}

// TestReconcileSecretResumeAbandonedWhenANewerPlanLanded is a regression test for a destructive
// race, not a tidiness rule.
//
// writeInterruptOutcome abandons its write, WITHOUT an error, when the Secret no longer carries the
// plan being resumed. If the reconcile then carries on, it holds a copy whose PlanKey is the OLD
// plan while wearing the NEW plan's resourceVersion — so the final updateSecret writes the old plan
// back over the orchestrator's new one and marks it applied, with no 409 to stop it because the
// resourceVersion matches. Rancher would then compute InSync for a plan its planner never
// delivered. Without the resourceVersion adoption the same race merely 409s.
//
// The window is real: "unpause, then push a corrected plan" is an ordinary operator sequence, and
// both writes can land in the gap before the resume reconcile runs.
func TestReconcileSecretResumeAbandonedWhenANewerPlanLanded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	dir := t.TempDir()
	oldSentinel := filepath.Join(dir, "old-plan-ran")
	oldPlanBytes, oldChecksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("old", oldSentinel)},
	})
	newPlanBytes, newChecksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("new", filepath.Join(dir, "new-plan-ran"))},
	})

	// The in-hand copy: suspended on the old plan, annotation just cleared.
	secret := newInterruptTestSecret(oldPlanBytes, nil, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePaused),
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{
			Checksum: oldChecksum, Completed: 0, Total: 1, ResumeState: planapi.PlanStateInProgress, Paused: true,
		}),
	})

	// The server has already moved on to the orchestrator's new plan.
	server := newInterruptTestSecret(newPlanBytes, nil, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePending),
	})
	server.ResourceVersion = "99"
	rec := newInterruptRecorder(server)
	sc := newInterruptTestControllerWithHook(t, rec, nil)

	w := newTestWatcher(t, true, "42")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	assertPathAbsent(t, oldSentinel, "a plan the server has already replaced must not be applied")
	for i, write := range rec.writes() {
		if planChecksumOf(t, write) != newChecksum {
			t.Errorf("write %d carries the OLD plan (checksum %s); the orchestrator's new plan was overwritten",
				i, planChecksumOf(t, write))
		}
		if got := string(write.Data[AppliedChecksumKey]); got == oldChecksum {
			t.Errorf("write %d marks the OLD plan applied (applied-checksum %q); Rancher would compute InSync "+
				"for a plan the planner never delivered", i, got)
		}
	}
	if string(result.Data[PlanKey]) != string(newPlanBytes) {
		t.Error("expected the newer plan's Secret to be handed back so its own reconcile owns the state")
	}
}

// planChecksumOf returns the checksum of the plan a Secret carries, for asserting which plan a
// write was made against.
func planChecksumOf(t *testing.T, secret *corev1.Secret) string {
	t.Helper()
	cp, err := applyinator.CalculatePlan(secret.Data[PlanKey])
	if err != nil {
		t.Fatalf("failed to calculate the plan checksum: %v", err)
	}
	return cp.Checksum
}

// TestReconcileSecretChecksumFlowMakesNoResumeCommit covers the fixture the resume commit's own
// guard cannot reject on its own: a legacy Secret with no plan-state that happens to carry a
// suspended checkpoint — left behind by a downgrade, or by an orchestrator that stopped writing
// plan-state. "Is there a suspension to release" answers yes there, so the gate has to be on the
// flow. Otherwise the agent materialises BOTH plan-state and plan-progress on a Secret owned by an
// orchestrator that understands neither, which Step A forbids twice over.
func TestReconcileSecretChecksumFlowMakesNoResumeCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	sentinel := filepath.Join(t.TempDir(), "plan-ran")
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("ran", sentinel)},
	})

	// No plan-state: the checksum flow. The leftover checkpoint must be inert, not a trigger.
	secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{
			Checksum: checksum, Completed: 1, Total: 1, ResumeState: planapi.PlanStateSucceeded, Paused: true,
		}),
	})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, false, "")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	for i, write := range rec.writes() {
		if value, ok := write.Data[planapi.PlanStateKey]; ok {
			t.Errorf("write %d materialised plan-state %q on a checksum-flow Secret", i, value)
		}
	}
	if value, ok := result.Data[planapi.PlanStateKey]; ok {
		t.Errorf("expected no plan-state on a checksum-flow Secret, got %q", value)
	}
	// The checkpoint is left exactly as found: not rewritten by a resume commit, and not cleared
	// by the outcome write either — the checksum flow does not own this key in any direction.
	if got := checkpointIn(t, result.Data); !got.Paused || got.ResumeState != planapi.PlanStateSucceeded {
		t.Errorf("expected the leftover checkpoint to be left untouched, got %+v", got)
	}
	// Ordinary checksum semantics are unaffected by the checkpoint's presence.
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Errorf("expected the plan to be applied under ordinary checksum semantics, sentinel missing: %v", statErr)
	}
	if string(result.Data[AppliedChecksumKey]) != checksum {
		t.Errorf("expected applied-checksum %q, got %q", checksum, result.Data[AppliedChecksumKey])
	}
}

// TestClampResumeFrom pins the boundary guard on an operator-controllable input path.
//
// The resume checkpoint lives in the plan Secret, and resolveResume hands its stored Completed
// back verbatim — so a hand-edited or truncated plan-progress can deliver 999, or a negative, to
// the index an apply starts at. pkg/applyinator clamps internally too; this is defence in depth at
// the boundary where the operator-controllable value enters, and defence in depth is exactly the
// kind of code that rots silently because nothing downstream fails when it stops working.
func TestClampResumeFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		resumeFrom int
		total      int
		want       int
	}{
		{name: "negative resumes from the first instruction", resumeFrom: -1, total: 3, want: 0},
		{name: "a large negative resumes from the first instruction", resumeFrom: -999, total: 3, want: 0},
		{name: "zero is unchanged", resumeFrom: 0, total: 3, want: 0},
		{name: "in range is unchanged", resumeFrom: 2, total: 3, want: 2},
		// Not off-by-one: Completed == len means every instruction ran, which is a legitimate
		// checkpoint and must survive unclamped.
		{name: "exactly the instruction count is unchanged", resumeFrom: 3, total: 3, want: 3},
		{name: "past the end clamps to the instruction count", resumeFrom: 4, total: 3, want: 3},
		{name: "far past the end clamps to the instruction count", resumeFrom: 999, total: 3, want: 3},
		{name: "a plan with no one-time instructions clamps everything to zero", resumeFrom: 999, total: 0, want: 0},
		{name: "a plan with no one-time instructions leaves zero alone", resumeFrom: 0, total: 0, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := clampResumeFrom(tt.resumeFrom, tt.total); got != tt.want {
				t.Errorf("clampResumeFrom(%d, %d) = %d, want %d", tt.resumeFrom, tt.total, got, tt.want)
			}
		})
	}
}
