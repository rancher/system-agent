package k8splan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/wrangler/v3/pkg/generic/fake"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file holds the safety-property matrix for the pause/cancel feature and the resume scenarios
// the per-scenario tests in reconcile_test.go each only touch one cell of.
//
// The property under test:
//
//	In the plan-state flow, the agent executes a plan only when both plan.cattle.io/paused and
//	plan.cattle.io/canceled are unambiguously not set — absent, or present with the value
//	"false". Anything else stops execution. No matter what plan-state says, no matter what the
//	checkpoint says, no matter how the agent arrived at this reconcile.
//
// The dangerous failure mode is not "pause does not work" — an operator notices that immediately —
// but "pause works until the agent restarts, and then the plan runs anyway", which is silent and
// arrives hours after the operator walked away. Every fixture below is therefore constructed as
// *the Secret a previous agent lifetime would have left behind*, and reconciled with a freshly
// constructed watcher, which is what "restart" means to this package.

// --- suppression fixtures -----------------------------------------------------------------------

// suppressionFixture is the plan every suppression row is reconciled against, together with the
// sentinel files that report whether Apply ran. watcher.applyinator is a concrete value rather
// than an interface and the design forbids changing Watch's signature, so there is no seam to
// count Apply calls; sentinel files are the assertion.
//
// BOTH a one-time and a periodic sentinel are load-bearing, and the periodic one is the reason
// this type exists rather than a bare marker path. Apply is still called, with
// RunOneTimeInstructions: false, on ordinary monitoring reconciles — so on the terminal-state rows
// a fixture carrying only a one-time sentinel would pass even with the suppression removed
// entirely: nothing would run because nothing was going to run. Only the periodic sentinel
// distinguishes "Apply was skipped entirely" from "Apply ran and applied nothing".
type suppressionFixture struct {
	planBytes []byte
	checksum  string
	oneTime   []string
	periodic  string
}

// newSuppressionFixture builds a three-instruction plan (so a Completed: 2 checkpoint is coherent)
// with a periodic instruction alongside it. It carries no probes: the zero-Update rows below
// depend on the probe-statuses write being a no-op, and a probe would make it a real change.
func newSuppressionFixture(t *testing.T) suppressionFixture {
	t.Helper()

	dir := t.TempDir()
	oneTime := []string{
		filepath.Join(dir, "one-time-0"),
		filepath.Join(dir, "one-time-1"),
		filepath.Join(dir, "one-time-2"),
	}
	periodic := filepath.Join(dir, "periodic")

	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			touchInstruction("first", oneTime[0]),
			touchInstruction("second", oneTime[1]),
			touchInstruction("third", oneTime[2]),
		},
		PeriodicInstructions: []planapi.PeriodicInstruction{touchPeriodicInstruction("watchdog", periodic)},
	})
	return suppressionFixture{planBytes: planBytes, checksum: checksum, oneTime: oneTime, periodic: periodic}
}

// assertApplyNeverRan asserts that Apply was not called at all.
func (f suppressionFixture) assertApplyNeverRan(t *testing.T) {
	t.Helper()

	for i, sentinel := range f.oneTime {
		assertPathAbsent(t, sentinel, fmt.Sprintf("one-time instruction %d must not run while the plan is held", i))
	}
	assertPathAbsent(t, f.periodic, "Apply must not be called AT ALL while the plan is held; a periodic instruction runs on "+
		"every apply, including the monitoring ones that pass RunOneTimeInstructions: false")
}

// assertApplyRanFully asserts the opposite: every instruction in the fixture executed.
func (f suppressionFixture) assertApplyRanFully(t *testing.T) {
	t.Helper()

	for i, sentinel := range f.oneTime {
		if _, err := os.Stat(sentinel); err != nil {
			t.Errorf("expected one-time instruction %d to run, sentinel missing: %v", i, err)
		}
	}
	if _, err := os.Stat(f.periodic); err != nil {
		t.Errorf("expected the periodic instruction to run, sentinel missing: %v", err)
	}
}

// touchPeriodicInstruction is touchInstruction's periodic counterpart. A periodic instruction with
// no recorded history is always due, so it runs on every Apply.
func touchPeriodicInstruction(name, sentinel string) planapi.PeriodicInstruction {
	return planapi.PeriodicInstruction{
		CommonInstruction: planapi.CommonInstruction{Name: name, Command: "sh", Args: []string{"-c", "touch " + sentinel}},
	}
}

// suppressionRow is one cell of the matrix: the Secret a previous agent lifetime would have left
// behind. Shared between the valid-value pass (TestPausedPlanNeverExecutes) and the invalid-value
// pass (TestPausedPlanWithAnInvalidValueWritesNothing) so the two cannot drift.
type suppressionRow struct {
	name       string
	annotation string // defaults to planapi.PlanPausedAnnotation
	planState  planapi.PlanState
	checkpoint *PlanCheckpoint // nil: no checkpoint at all

	// The fields below describe the valid-value pass only; the invalid-value pass asserts a
	// single stricter outcome that is identical on every row.
	wantUpdates    int
	wantState      planapi.PlanState
	wantCheckpoint *PlanCheckpoint // consulted only when wantUpdates > 0
}

func (r suppressionRow) annotationKey() string {
	if r.annotation == "" {
		return planapi.PlanPausedAnnotation
	}
	return r.annotation
}

// secretFor builds the Secret this row reconciles, with the row's annotation set to value.
func (r suppressionRow) secretFor(f suppressionFixture, value string) *corev1.Secret {
	data := map[string][]byte{
		planapi.PlanStateKey: []byte(r.planState),
		// Pre-seeded so the steady state is genuinely steady. An interrupt suppresses execution
		// but never observation: probes keep running and their statuses are persisted, so against
		// a Secret with no probe-statuses key the marshalled empty map is a real change and an
		// Update would legitimately follow — which would make the zero-Update rows unassertable.
		ProbeStatusesKey: []byte("{}"),
	}
	if r.checkpoint != nil {
		checkpoint := *r.checkpoint
		checkpoint.Checksum = f.checksum
		data[planapi.PlanCheckpointKey] = marshalPlanCheckpoint(checkpoint)
	}
	return newInterruptTestSecret(f.planBytes, map[string]string{r.annotationKey(): value}, data)
}

// suppressionMatrix is the row set. It is chosen so that at least one row would EXECUTE if the
// interrupt check were moved below resolveResume or below decidePlanStateAction: in-progress
// re-executes on restart (crash recovery) and pending starts. Do not prune it.
func suppressionMatrix() []suppressionRow {
	return []suppressionRow{
		{
			name:           "paused with a suspended checkpoint: the ordinary held plan",
			planState:      planapi.PlanStatePaused,
			checkpoint:     &PlanCheckpoint{Completed: 2, Total: 3, ResumeState: planapi.PlanStateInProgress, Paused: true},
			wantUpdates:    0, // the write-once guard honoring a checkpoint this process did not write
			wantState:      planapi.PlanStatePaused,
			wantCheckpoint: nil,
		},
		{
			name:        "paused with no checkpoint: paused before the first instruction finished",
			planState:   planapi.PlanStatePaused,
			wantUpdates: 1, // there is a suspension to record for the first time
			wantState:   planapi.PlanStatePaused,
			// ResumeState is deliberately empty: resuming *into* paused is a permanent stall, so
			// handlePause refuses to record it and lets resolveResume's in-progress default win.
			wantCheckpoint: &PlanCheckpoint{Completed: 0, Total: 3, ResumeState: "", Paused: true},
		},
		{
			// The contract that must NOT fire while the plan is held. An agent that restarts and
			// finds in-progress re-executes the plan from the beginning — unless it is paused.
			name:           "in-progress with no checkpoint: crash mid-apply must not re-execute while held",
			planState:      planapi.PlanStateInProgress,
			wantUpdates:    1,
			wantState:      planapi.PlanStatePaused,
			wantCheckpoint: &PlanCheckpoint{Completed: 0, Total: 3, ResumeState: planapi.PlanStateInProgress, Paused: true},
		},
		{
			// plan-state and the checkpoint disagree. Pause's write-once guard keys off the
			// checkpoint, because the checkpoint is the thing that must not be recomputed — so
			// nothing is written and the wire state is left exactly as found.
			name:        "in-progress with a suspended checkpoint: the write-once guard keys off the checkpoint, not plan-state",
			planState:   planapi.PlanStateInProgress,
			checkpoint:  &PlanCheckpoint{Completed: 2, Total: 3, ResumeState: planapi.PlanStateInProgress, Paused: true},
			wantUpdates: 0,
			wantState:   planapi.PlanStateInProgress,
		},
		{
			name:           "pending with no checkpoint: a plan delivered already paused",
			planState:      planapi.PlanStatePending,
			wantUpdates:    1,
			wantState:      planapi.PlanStatePaused,
			wantCheckpoint: &PlanCheckpoint{Completed: 0, Total: 3, ResumeState: planapi.PlanStatePending, Paused: true},
		},
		{
			// Terminal: there is nothing to suppress here, so this row asserts no regression
			// rather than a new behaviour. It is also the row the periodic sentinel exists for —
			// a monitoring reconcile calls Apply, so a one-time sentinel alone proves nothing.
			name:           "succeeded with no checkpoint: terminal, nothing to suppress",
			planState:      planapi.PlanStateSucceeded,
			wantUpdates:    1,
			wantState:      planapi.PlanStatePaused,
			wantCheckpoint: &PlanCheckpoint{Completed: 0, Total: 3, ResumeState: planapi.PlanStateSucceeded, Paused: true},
		},
		{
			// A cancel report carries Paused: false, so it is NOT an already-recorded suspension:
			// the pause is recorded on top of it, keeping the report's Completed.
			name:           "canceled with a cancel report: a report is not a suspension",
			planState:      planapi.PlanStateCanceled,
			checkpoint:     &PlanCheckpoint{Completed: 2, Total: 3},
			wantUpdates:    1,
			wantState:      planapi.PlanStatePaused,
			wantCheckpoint: &PlanCheckpoint{Completed: 2, Total: 3, ResumeState: planapi.PlanStateCanceled, Paused: true},
		},
		{
			// A SUCCEEDED plan the operator then cancels, deliberately, rather than an
			// already-canceled one. Unlike the other terminal states, succeeded keeps executing
			// periodic instructions, so cancel's write-once guard must NOT treat it as already
			// inert: the cancellation is recorded, moving plan-state from succeeded to canceled.
			// Not reachable from any paused-annotation row, so the row carries its own annotation.
			name:           "succeeded then canceled: still recorded, because a succeeded plan keeps running periodic instructions",
			annotation:     planapi.PlanCanceledAnnotation,
			planState:      planapi.PlanStateSucceeded,
			checkpoint:     &PlanCheckpoint{Completed: 2, Total: 3},
			wantUpdates:    1,
			wantState:      planapi.PlanStateCanceled,
			wantCheckpoint: &PlanCheckpoint{Completed: 2, Total: 3},
		},
	}
}

// --- Part 1: the valid-value matrix -------------------------------------------------------------

// TestPausedPlanNeverExecutes is the safety property itself. Every row constructs the Secret a
// previous agent lifetime would have left behind, reconciles it with a FRESHLY CONSTRUCTED watcher
// — which is what "the agent restarted" means to this package — and asserts that Apply was never
// called, whatever plan-state and the checkpoint say.
func TestPausedPlanNeverExecutes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	for _, row := range suppressionMatrix() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			f := newSuppressionFixture(t)
			secret := row.secretFor(f, "true")
			rec := newInterruptRecorder(secret)
			sc := newInterruptTestController(t, rec)

			w := newTestWatcher(t, false, "")
			result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
			if err != nil {
				t.Fatalf("reconcileSecret returned error: %v", err)
			}

			f.assertApplyNeverRan(t)
			if w.hasRunOnce {
				t.Error("expected the interrupt entry path not to set hasRunOnce; mutable state must not live on the far side " +
					"of the safety returns, and a set flag here means the annotation was read too late")
			}
			if len(result.Data[AppliedChecksumKey]) != 0 {
				t.Errorf("expected applied-checksum not to be written for a plan that never ran, got %q", result.Data[AppliedChecksumKey])
			}
			if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != row.wantState {
				t.Errorf("expected plan-state %q, got %q", row.wantState, got)
			}
			if got := len(rec.writes()); got != row.wantUpdates {
				t.Errorf("expected exactly %d Update call(s), got %d", row.wantUpdates, got)
			}

			if row.wantUpdates == 0 {
				// Nothing was written, so the record the previous agent lifetime left behind has
				// to come back verbatim — including the resourceVersion, which is how "writes
				// nothing" is checked rather than inferred from a call count.
				if !bytes.Equal(result.Data[planapi.PlanCheckpointKey], secret.Data[planapi.PlanCheckpointKey]) {
					t.Errorf("expected the checkpoint to be honored verbatim, got %q want %q",
						result.Data[planapi.PlanCheckpointKey], secret.Data[planapi.PlanCheckpointKey])
				}
				if result.ResourceVersion != secret.ResourceVersion {
					t.Errorf("expected the resource version to be byte-identical to the input's (%q), got %q",
						secret.ResourceVersion, result.ResourceVersion)
				}
			} else {
				want := *row.wantCheckpoint
				want.Checksum = f.checksum
				if got := checkpointIn(t, result.Data); got != want {
					t.Errorf("expected checkpoint %+v, got %+v", want, got)
				}
			}

			if periods := rec.enqueuePeriods(); len(periods) != 1 || periods[0] != w.probePeriod {
				t.Errorf("expected a single re-enqueue after %v, got %v", w.probePeriod, periods)
			}
		})
	}
}

// TestSuppressionMatrixBaselineExecutesWithoutAnAnnotation is the control for the matrix above,
// deliberately run as its own case rather than as a row: the checksum flow ignores the annotations
// by design, so it is the one fixture where "no annotation influence" and "the plan applies" are
// the same statement. Without it the matrix would pass just as well against a fixture whose plan
// could never have run for some unrelated reason.
func TestSuppressionMatrixBaselineExecutesWithoutAnAnnotation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	f := newSuppressionFixture(t)
	// No plan-state (the checksum flow), no checkpoint, and no annotation.
	secret := newInterruptTestSecret(f.planBytes, nil, nil)
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, false, "")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	f.assertApplyRanFully(t)
	if got := string(result.Data[AppliedChecksumKey]); got != f.checksum {
		t.Errorf("expected applied-checksum %q, got %q", f.checksum, got)
	}
}

// --- Parts 2 and 3: the invalid-value matrix ----------------------------------------------------

// invalidAnnotationValues are the spellings readInterrupt refuses. "True" and "yes" are the two an
// operator actually types; "" is what a poorly-written controller writes when it means to remove
// the annotation. None of them may be coerced in either direction.
var invalidAnnotationValues = []string{"True", "yes", ""}

// TestPausedPlanWithAnInvalidValueWritesNothing runs the same matrix with the annotation value
// replaced by an uninterpretable one and asserts the STRICTER outcome: nothing runs, nothing at
// all is written on ANY row — not even the rows that would otherwise record a suspension — and
// reconcileSecret returns an error.
//
// That last assertion is what distinguishes this from a fail-closed read. A typo must not be able
// to masquerade as a working pause, so it is not enough that the plan is held: the reconcile has
// to say something is wrong.
//
// The brief's Parts 2 and 3 ride on the same reconcile. The resourceVersion assertion (Part 3) is
// a statement about the object this reconcile returned, so re-running the matrix a third time to
// make it would cost runtime and add no signal; it is made below alongside the Update count, which
// is exactly the point — "writes nothing" is checked, not inferred from a call count.
func TestPausedPlanWithAnInvalidValueWritesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	for _, value := range invalidAnnotationValues {
		for _, row := range suppressionMatrix() {
			t.Run(fmt.Sprintf("%s/value=%q", row.name, value), func(t *testing.T) {
				t.Parallel()

				f := newSuppressionFixture(t)
				secret := row.secretFor(f, value)
				rec := newInterruptRecorder(secret)
				sc := newInterruptTestController(t, rec)

				w := newTestWatcher(t, false, "")
				result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
				if err == nil {
					t.Fatal("expected an error for an uninterpretable annotation value; a typo must not be able to " +
						"masquerade as a working pause")
				}
				if !strings.Contains(err.Error(), row.annotationKey()) {
					t.Errorf("expected the error to name the offending annotation %q, got %v", row.annotationKey(), err)
				}

				f.assertApplyNeverRan(t)
				if w.hasRunOnce {
					t.Error("expected the invalid-value path not to set hasRunOnce")
				}
				if got := len(rec.writes()); got != 0 {
					t.Errorf("expected zero Update calls on every row, including the ones that would otherwise "+
						"record a suspension, got %d", got)
				}
				if periods := rec.enqueuePeriods(); len(periods) != 0 {
					t.Errorf("expected no re-enqueue (the workqueue's rate limiter owns the retry), got %v", periods)
				}
				if result.ResourceVersion != secret.ResourceVersion {
					t.Errorf("expected the returned secret's resource version to be byte-identical to the input's (%q), got %q",
						secret.ResourceVersion, result.ResourceVersion)
				}
				if !reflect.DeepEqual(result.Data, secret.Data) {
					t.Errorf("expected the returned secret's data to be byte-identical to the input's; the error path must not "+
						"leave residue for a corrected reconcile to trip over.\n got %v\nwant %v", result.Data, secret.Data)
				}
			})
		}
	}
}

// --- Part 4: the checksum flow does not validate, it ignores -------------------------------------

// TestChecksumFlowIgnoresAnInvalidAnnotationValue pins that the compatibility rule holds for
// invalid values too. The checksum flow does not validate the annotations because it does not act
// on them at all: a legacy orchestrator has no way to clear any state the agent might record, so
// half-honoring a misspelled pause there would be worse than ignoring it. The plan applies under
// ordinary checksum semantics and a warning is emitted on EVERY reconcile — the condition, an
// operator expecting a pause that is never going to happen, persists as long as the annotation.
//
// Not parallel: captureLogs swaps the process-wide logrus output.
func TestChecksumFlowIgnoresAnInvalidAnnotationValue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}

	for _, value := range invalidAnnotationValues {
		t.Run(fmt.Sprintf("value=%q", value), func(t *testing.T) {
			logs := captureLogs(t)

			f := newSuppressionFixture(t)
			// No plan-state key: the checksum flow.
			secret := newInterruptTestSecret(f.planBytes, map[string]string{planapi.PlanPausedAnnotation: value}, nil)
			rec := newInterruptRecorder(secret)
			sc := newInterruptTestController(t, rec)

			w := newTestWatcher(t, false, "")
			result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
			if err != nil {
				t.Fatalf("first reconcileSecret returned error: %v", err)
			}

			f.assertApplyRanFully(t)
			if got := string(result.Data[AppliedChecksumKey]); got != f.checksum {
				t.Errorf("expected applied-checksum %q, got %q", f.checksum, got)
			}

			// A second reconcile of the settled Secret: nothing applies, but the warning repeats.
			settled, err := w.reconcileSecret(context.Background(), sc, result.DeepCopy(), 30*time.Second)
			if err != nil {
				t.Fatalf("second reconcileSecret returned error: %v", err)
			}

			for i, data := range []map[string][]byte{result.Data, settled.Data} {
				if value, ok := data[planapi.PlanStateKey]; ok {
					t.Errorf("reconcile %d materialised plan-state %q on a checksum-flow Secret", i, value)
				}
				if value, ok := data[planapi.PlanCheckpointKey]; ok {
					t.Errorf("reconcile %d materialised %q on a checksum-flow Secret, got %q", i, planapi.PlanCheckpointKey, value)
				}
			}

			want := "ignoring unsupported annotation in checksum flow key=" + planapi.PlanPausedAnnotation + " value=" + value
			if got := strings.Count(logs(), want); got != 2 {
				t.Errorf("expected the warning %q on every reconcile (2), saw it %d time(s) in:\n%s", want, got, logs())
			}
		})
	}
}

// --- Part 5: the resume half ---------------------------------------------------------------------

// TestRestartThenUnpauseResumesAtTheCheckpoint is the case the whole change exists for: an agent
// that comes back up after the operator unpaused a held plan must resume it AT THE CHECKPOINT the
// previous lifetime recorded, not from instruction 0. Without process independence the plan
// silently re-runs from the beginning and every other assertion in this package still passes.
//
// It is written as a table over the two ways an operator releases a pause — deleting the key and
// setting it to "false" — and closes by asserting the two produce identical Secret data, so the
// two release forms cannot drift apart.
func TestRestartThenUnpauseResumesAtTheCheckpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	// One shared plan across both rows, deliberately: different sentinel paths would mean
	// different plan bytes, a different checksum and therefore a different applied-checksum, and
	// the data-equality assertion at the bottom would be vacuous. The sentinels are removed at the
	// top of each row instead, which is why the rows run sequentially rather than in parallel.
	dir := t.TempDir()
	sentinels := []string{filepath.Join(dir, "instruction-0"), filepath.Join(dir, "instruction-1"), filepath.Join(dir, "instruction-2")}
	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			touchInstruction("first", sentinels[0]),
			touchInstruction("second", sentinels[1]),
			touchInstruction("third", sentinels[2]),
		},
	})

	releases := []struct {
		name        string
		annotations map[string]string
	}{
		{name: "the operator removed the annotation while the agent was down", annotations: nil},
		{name: "the operator set the annotation to false", annotations: map[string]string{planapi.PlanPausedAnnotation: "false"}},
	}

	settled := make([]map[string][]byte, len(releases))
	for i, release := range releases {
		t.Run(release.name, func(t *testing.T) {
			for _, sentinel := range sentinels {
				if err := os.Remove(sentinel); err != nil && !os.IsNotExist(err) {
					t.Fatalf("failed to clear sentinel %s: %v", sentinel, err)
				}
			}

			// A plan held at instruction 2 by a previous agent lifetime.
			secret := newInterruptTestSecret(planBytes, release.annotations, map[string][]byte{
				planapi.PlanStateKey: []byte(planapi.PlanStatePaused),
				planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{
					Checksum: checksum, Completed: 2, Total: 3, ResumeState: planapi.PlanStateInProgress, Paused: true,
				}),
			})

			rec := newInterruptRecorder(secret)
			// The apply marker is instruction 2's sentinel: it is the first thing the resumed
			// apply does, so a write that lands before it genuinely predates the apply.
			sc, observations := observeUpdateOrdering(t, rec, sentinels[2])

			w := newTestWatcher(t, false, "") // a fresh agent: the checkpoint is the only memory it has
			result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
			if err != nil {
				t.Fatalf("reconcileSecret returned error: %v", err)
			}

			assertResumeCommitLandedFirst(t, observations(), planapi.PlanStateInProgress)

			// ResumeFromOneTimeInstruction is not directly observable — this is what 2 looks like.
			assertPathAbsent(t, sentinels[0], "instruction 0 completed in a previous agent lifetime and must never be re-run")
			assertPathAbsent(t, sentinels[1], "instruction 1 completed in a previous agent lifetime and must never be re-run")
			if _, statErr := os.Stat(sentinels[2]); statErr != nil {
				t.Errorf("expected the resumed plan to continue at instruction 2, sentinel missing: %v", statErr)
			}
			if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != planapi.PlanStateSucceeded {
				t.Errorf("expected the resumed plan to finish as %q, got %q", planapi.PlanStateSucceeded, got)
			}
			if len(result.Data[LastApplyTimeKey]) == 0 {
				t.Error("expected last-apply-time to be written; it is excluded from the cross-form comparison below")
			}
			settled[i] = result.Data
		})
	}

	if settled[0] == nil || settled[1] == nil {
		t.Fatal("expected both release forms to produce settled Secret data")
	}
	// "false" is not a second, subtly different way to unpause: it must be indistinguishable from
	// removing the key. last-apply-time is the one key that cannot be compared — it has one-second
	// resolution and the two rows may straddle a second boundary. That both rows write it at all
	// is asserted above.
	withoutApplyTime := func(data map[string][]byte) map[string][]byte {
		out := maps.Clone(data)
		delete(out, LastApplyTimeKey)
		return out
	}
	if !reflect.DeepEqual(withoutApplyTime(settled[0]), withoutApplyTime(settled[1])) {
		t.Errorf("expected setting the annotation to \"false\" to produce exactly the Secret data that removing it does.\n"+
			"removed: %v\n  false: %v", withoutApplyTime(settled[0]), withoutApplyTime(settled[1]))
	}
}

// TestOnlyASuspendedCheckpointGrantsAResume pins the sole gate on a checkpoint's Completed being
// honored: PlanCheckpoint.Paused. A Paused: false record is a REPORT — of how far a canceled plan
// got, or of where a resume commit released a plan — and a report must never position an apply.
//
// The two rows are the two distinct returns resolveResume can take to reach that answer, and each
// is reachable only from its own plan-state:
//
//   - plan-state in-progress leaves at the "not paused either way" return (plan_progress.go:78);
//   - plan-state paused falls past it into the !p.Paused branch (plan_progress.go:81), which the
//     in-progress row cannot enter at all.
//
// The third shape — in-progress with no checkpoint whatsoever — is the plain crash-recovery
// contract and is already pinned by TestReconcileSecretInProgressOnStartupReExecutes; it is
// deliberately not repeated here.
func TestOnlyASuspendedCheckpointGrantsAResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	tests := []struct {
		name      string
		planState planapi.PlanState
	}{
		{
			// A checkpoint a resume commit left behind. The crash-recovery contract is UNCHANGED
			// for a plan that was running rather than held: it re-executes from instruction 0.
			name:      "in-progress under a released checkpoint: crash recovery re-executes from the beginning",
			planState: planapi.PlanStateInProgress,
		},
		{
			// A cancel report on a plan someone then paused and unpaused. The report records how
			// far the canceled plan got; mistaking it for a resume point would skip real work.
			name:      "paused under a cancel report: a report is never mistaken for a resume point",
			planState: planapi.PlanStatePaused,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newSuppressionFixture(t)
			secret := newInterruptTestSecret(f.planBytes, nil, map[string][]byte{
				planapi.PlanStateKey: []byte(tt.planState),
				// Paused: false — deliberately claiming progress that must NOT be honored.
				planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{Checksum: f.checksum, Completed: 2, Total: 3}),
			})
			rec := newInterruptRecorder(secret)
			sc := newInterruptTestController(t, rec)

			w := newTestWatcher(t, false, "")
			result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
			if err != nil {
				t.Fatalf("reconcileSecret returned error: %v", err)
			}

			// Every instruction, including the two the report claims are complete.
			f.assertApplyRanFully(t)
			if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != planapi.PlanStateSucceeded {
				t.Errorf("expected the re-executed plan to finish as %q, got %q", planapi.PlanStateSucceeded, got)
			}
		})
	}
}

// TestCorrectedAnnotationValueSelfHeals walks one watcher — as a real agent would be — through the
// operator's whole correction sequence: a typo, the fix, then the release. The point is that the
// error path leaves NO RESIDUE: no half-written state for the corrected reconcile to trip over,
// and no lost progress.
func TestCorrectedAnnotationValueSelfHeals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	f := newSuppressionFixture(t)
	// in-progress rather than pending, so the resume commit in the third pass is a write of its
	// own rather than folded into a pending -> in-progress pre-commit.
	base := newInterruptTestSecret(f.planBytes, nil, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStateInProgress),
		ProbeStatusesKey:     []byte("{}"),
	})
	rec := newInterruptRecorder(base)
	sc := newInterruptTestController(t, rec)
	w := newTestWatcher(t, false, "")

	// Pass 1: the typo.
	rec.setAnnotations(map[string]string{planapi.PlanPausedAnnotation: "yes"})
	mistyped := rec.get()
	result, err := w.reconcileSecret(context.Background(), sc, mistyped, 30*time.Second)
	if err == nil {
		t.Fatal("expected an error for the mistyped annotation value, got nil")
	}
	f.assertApplyNeverRan(t)
	if got := len(rec.writes()); got != 0 {
		t.Fatalf("expected the error path to write nothing, got %d Update call(s)", got)
	}
	if !reflect.DeepEqual(result.Data, mistyped.Data) {
		t.Error("expected the error path to leave the Secret data untouched")
	}

	// Pass 2: the value corrected to "true". The suspension is recorded normally.
	rec.setAnnotations(map[string]string{planapi.PlanPausedAnnotation: "true"})
	if _, err = w.reconcileSecret(context.Background(), sc, rec.get(), 30*time.Second); err != nil {
		t.Fatalf("reconcileSecret returned error after the value was corrected: %v", err)
	}
	f.assertApplyNeverRan(t)
	writes := rec.writes()
	if len(writes) != 1 {
		t.Fatalf("expected exactly one Update recording the suspension, got %d", len(writes))
	}
	if got := planapi.PlanState(writes[0].Data[planapi.PlanStateKey]); got != planapi.PlanStatePaused {
		t.Errorf("expected plan-state %q, got %q", planapi.PlanStatePaused, got)
	}
	wantHeld := PlanCheckpoint{Checksum: f.checksum, Completed: 0, Total: 3, ResumeState: planapi.PlanStateInProgress, Paused: true}
	if got := checkpointIn(t, writes[0].Data); got != wantHeld {
		t.Errorf("expected the suspension to be recorded as %+v, got %+v", wantHeld, got)
	}

	// Pass 3: released. The resume commit fires and the plan runs.
	rec.setAnnotations(map[string]string{planapi.PlanPausedAnnotation: "false"})
	if _, err = w.reconcileSecret(context.Background(), sc, rec.get(), 30*time.Second); err != nil {
		t.Fatalf("reconcileSecret returned error after the plan was released: %v", err)
	}
	writes = rec.writes()
	if len(writes) < 2 {
		t.Fatalf("expected the resume commit to be written, got only %d Update call(s) in total", len(writes))
	}
	resume := writes[1]
	if got := planapi.PlanState(resume.Data[planapi.PlanStateKey]); got != planapi.PlanStateInProgress {
		t.Errorf("expected the resume commit to restore plan-state %q, got %q", planapi.PlanStateInProgress, got)
	}
	if got := checkpointIn(t, resume.Data); got.Paused {
		t.Errorf("expected the resume commit to clear the checkpoint's Paused flag, got %+v", got)
	}
	f.assertApplyRanFully(t)
}

// TestResumeThenPauseAgainRecordsTheNewerCheckpoint proves successive pause/resume cycles COMPOSE
// rather than reset. The agent resumes a plan held at instruction 1, the operator pauses it again
// mid-apply, and the second checkpoint must report the newer position — 2 — rather than the value
// carried over from the first pause. A resume commit that failed to re-arm the write-once guard,
// or a resume that ignored the checkpoint, both surface here.
//
// Not parallel: it shortens the package-level interruptPollInterval.
func TestResumeThenPauseAgainRecordsTheNewerCheckpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	withInterruptPollInterval(t, 2*time.Millisecond)

	dir := t.TempDir()
	first := filepath.Join(dir, "instruction-0")
	second := filepath.Join(dir, "instruction-1")
	third := filepath.Join(dir, "instruction-2")
	gate := filepath.Join(dir, "gate")

	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			touchInstruction("first", first),
			gatedTouchInstruction("second", second, gate),
			touchInstruction("third", third),
		},
	})

	// Held at instruction 1 by a previous pause, and just released.
	secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePaused),
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{
			Checksum: checksum, Completed: 1, Total: 3, ResumeState: planapi.PlanStateInProgress, Paused: true,
		}),
	})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)
	// The second pause arrives mid-apply. The gate is released on the poll AFTER the one that
	// served the annotation, by which point pollInterrupts has certainly closed the pause channel,
	// so the pause lands strictly between instruction 1 and instruction 2.
	serveInterruptOnceApplyStarted(t, sc, planapi.PlanPausedAnnotation, second, func(served int) {
		if served > 1 {
			if writeErr := os.WriteFile(gate, nil, 0600); writeErr != nil {
				t.Errorf("failed to release the gate: %v", writeErr)
			}
		}
	})

	w := newTestWatcher(t, false, "")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	assertPathAbsent(t, first, "the resume must skip the instruction the first pause had already completed")
	if _, statErr := os.Stat(second); statErr != nil {
		t.Fatalf("expected the resumed plan to run instruction 1, sentinel missing: %v", statErr)
	}
	assertPathAbsent(t, third, "the second pause stops the apply at the next instruction boundary")

	if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != planapi.PlanStatePaused {
		t.Errorf("expected plan-state %q after the second pause, got %q", planapi.PlanStatePaused, got)
	}
	want := PlanCheckpoint{Checksum: checksum, Completed: 2, Total: 3, ResumeState: planapi.PlanStateInProgress, Paused: true}
	if got := checkpointIn(t, result.Data); got != want {
		t.Errorf("expected the second checkpoint to report the NEWER position %+v — successive pause/resume cycles must "+
			"compose, not reset — got %+v", want, got)
	}

	// Exactly one resume commit, not one per reconcile pass through the machinery.
	var resumeCommits int
	for _, write := range rec.writes() {
		if progress := checkpointIn(t, write.Data); !progress.Paused {
			resumeCommits++
		}
	}
	if resumeCommits != 1 {
		t.Errorf("expected the resume commit to be issued exactly once, saw %d write(s) clearing the Paused flag", resumeCommits)
	}
}

// --- Part 6: the interrupt-path re-entry no-op ----------------------------------------------------

// TestInterruptPathReEntryIsANoOp pins the write-once guard against the periodic re-enqueue. While
// a plan is held the agent re-enters the interrupt path every probe period for the whole duration
// of the pause; without the guard that rewrites the Secret on every pass and — worse — recomputes
// the checkpoint from a reconcile where no apply is in flight, which can silently reset a record
// that had just captured real progress.
//
// The first reconcile arrives on plan-state in-progress so that there IS a suspension to record;
// the second is the re-entry, by which point plan-state is already paused. Starting both reconciles
// from an already-suspended checkpoint would make the whole test a zero-Update no-op and could not
// distinguish the guard from its absence.
func TestInterruptPathReEntryIsANoOp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	f := newSuppressionFixture(t)
	secret := newInterruptTestSecret(f.planBytes, map[string]string{planapi.PlanPausedAnnotation: "true"}, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStateInProgress),
		// Real progress a previous apply recorded, carrying Paused: false — a report, not a
		// suspension, so the first reconcile has something to record.
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{Checksum: f.checksum, Completed: 2, Total: 3}),
		ProbeStatusesKey:          []byte("{}"),
	})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, false, "")
	if _, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second); err != nil {
		t.Fatalf("first reconcileSecret returned error: %v", err)
	}
	if got := len(rec.writes()); got != 1 {
		t.Fatalf("expected the first reconcile to record the suspension in exactly one Update, got %d", got)
	}

	// The re-enqueue arrives and the same watcher reconciles the Secret it just wrote.
	result, err := w.reconcileSecret(context.Background(), sc, rec.get(), 30*time.Second)
	if err != nil {
		t.Fatalf("second reconcileSecret returned error: %v", err)
	}

	f.assertApplyNeverRan(t)
	if got := len(rec.writes()); got != 1 {
		t.Errorf("expected exactly one Update across BOTH reconciles; the re-entry must be a complete no-op, got %d", got)
	}
	want := PlanCheckpoint{Checksum: f.checksum, Completed: 2, Total: 3, ResumeState: planapi.PlanStateInProgress, Paused: true}
	if got := checkpointIn(t, result.Data); got != want {
		t.Errorf("expected the re-entry to leave the checkpoint exactly as the first reconcile wrote it, %+v, got %+v", want, got)
	}
}

// --- Part 7: the conflict cases ------------------------------------------------------------------

// newConflictingInterruptTestController is newInterruptTestController whose first `conflicts`
// Update calls return a 409 before the recorder ever sees them.
//
// The conflict on the interrupt path is not a rare race, it is the NORMAL path: the operator's
// annotation write bumps the Secret's resourceVersion while the agent still holds a copy read
// before the apply started, so the outcome Update is guaranteed to conflict.
func newConflictingInterruptTestController(t *testing.T, r *interruptRecorder, conflicts int,
) *fake.MockControllerInterface[*corev1.Secret, *corev1.SecretList] {
	t.Helper()

	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).DoAndReturn(
		func(string, string, metav1.GetOptions) (*corev1.Secret, error) { return r.get(), nil },
	).AnyTimes()

	var mu sync.Mutex
	remaining := conflicts
	sc.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		mu.Lock()
		reject := remaining > 0
		if reject {
			remaining--
		}
		mu.Unlock()
		if reject {
			return nil, apierrors.NewConflict(corev1.Resource("secrets"), s.Name, errors.New("the operator's annotation write got there first"))
		}
		return r.update(s), nil
	}).AnyTimes()
	sc.EXPECT().EnqueueAfter(testNamespace, testSecret, gomock.Any()).Do(
		func(_, _ string, d time.Duration) { r.enqueue(d) },
	).AnyTimes()
	return sc
}

// TestInterruptedApplyOutcomeSurvivesAConflictingWrite is a regression test for silent data loss,
// not a tidiness rule. Under updateSecret's conflict handling the interrupt outcome would be
// abandoned — its merge only carries data over when the ALREADY APPLIED checksum matches the plan
// now on the server, and the interrupted path deliberately does not write applied-checksum — so
// cancel self-healed by crashing and pause did not: the checkpoint and the accumulated
// applied-output were lost, and unpausing re-ran the plan from instruction 0.
//
// Not parallel: it shortens the package-level interruptPollInterval.
func TestInterruptedApplyOutcomeSurvivesAConflictingWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	withInterruptPollInterval(t, 2*time.Millisecond)

	dir := t.TempDir()
	firstSentinel := filepath.Join(dir, "instruction-0")
	secondSentinel := filepath.Join(dir, "instruction-1")
	gate := filepath.Join(dir, "gate")

	planBytes, checksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			gatedTouchInstruction("first", firstSentinel, gate),
			touchInstruction("second", secondSentinel),
		},
	})

	// The in-hand copy: read before the apply started, so it predates the operator's annotation.
	// in-progress rather than pending, so the first Update of the reconcile is the interrupt
	// outcome rather than a pending -> in-progress pre-commit.
	secret := newInterruptTestSecret(planBytes, nil, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStateInProgress),
	})

	// The server: the same plan, but already bearing the operator's annotation at a newer
	// resourceVersion. That is what makes the agent's outcome write conflict.
	server := newInterruptTestSecret(planBytes, map[string]string{planapi.PlanPausedAnnotation: "true"}, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStateInProgress),
	})
	server.ResourceVersion = "99"
	rec := newInterruptRecorder(server)
	sc := newConflictingInterruptTestController(t, rec, 1)
	serveInterruptOnceApplyStarted(t, sc, planapi.PlanPausedAnnotation, firstSentinel, func(served int) {
		if served > 1 {
			if writeErr := os.WriteFile(gate, nil, 0600); writeErr != nil {
				t.Errorf("failed to release the gate: %v", writeErr)
			}
		}
	})

	w := newTestWatcher(t, false, "")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("expected the conflicting write to be retried rather than escalated, got error: %v", err)
	}

	assertPathAbsent(t, secondSentinel, "the pause stops the apply at the next instruction boundary")
	if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != planapi.PlanStatePaused {
		t.Errorf("expected plan-state %q, got %q", planapi.PlanStatePaused, got)
	}
	// The actual regression: Completed must survive the conflict. Losing it means unpause re-runs
	// the plan from instruction 0, silently defeating the feature.
	want := PlanCheckpoint{Checksum: checksum, Completed: 1, Total: 2, ResumeState: planapi.PlanStateInProgress, Paused: true}
	if got := checkpointIn(t, result.Data); got != want {
		t.Errorf("expected the checkpoint %+v to survive the conflicting write, got %+v", want, got)
	}
	if len(result.Data[AppliedOutputKey]) == 0 {
		t.Error("expected the accumulated applied-output to survive the conflicting write")
	}

	writes := rec.writes()
	if len(writes) != 1 {
		t.Fatalf("expected exactly one successful Update (the retry), got %d", len(writes))
	}
	// Built on the freshly fetched object, not on the stale in-hand copy: the retry re-reads.
	if writes[0].ResourceVersion != "99" {
		t.Errorf("expected the retried write to be built on the fresh object (resource version 99), got %q", writes[0].ResourceVersion)
	}
	if writes[0].Annotations[planapi.PlanPausedAnnotation] != "true" {
		t.Errorf("expected the retried write to carry the operator's annotation from the fresh object, got %v", writes[0].Annotations)
	}
}

// TestInterruptOutcomeAbandonedWhenANewerPlanLanded is the companion case: when the re-read shows
// the Secret no longer carries the plan that was interrupted, the write is abandoned without an
// error and that plan's own reconcile is left to own the state. Recording an interrupt outcome
// against a plan the orchestrator has already replaced would attribute it to the wrong plan.
func TestInterruptOutcomeAbandonedWhenANewerPlanLanded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	f := newSuppressionFixture(t)
	newPlanBytes, newChecksum := marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("new", filepath.Join(t.TempDir(), "new-plan-ran"))},
	})

	// The in-hand copy carries the old plan and the operator's pause.
	secret := newInterruptTestSecret(f.planBytes, map[string]string{planapi.PlanPausedAnnotation: "true"}, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStateInProgress),
	})
	// The server has already moved on.
	server := newInterruptTestSecret(newPlanBytes, map[string]string{planapi.PlanPausedAnnotation: "true"}, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePending),
	})
	server.ResourceVersion = "99"
	rec := newInterruptRecorder(server)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, false, "")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("abandoning the write is not an error condition, got: %v", err)
	}

	f.assertApplyNeverRan(t)
	if got := len(rec.writes()); got != 0 {
		t.Errorf("expected the interrupt outcome to be abandoned without any Update, got %d", got)
	}
	if got := planChecksumOf(t, result); got != newChecksum {
		t.Errorf("expected the newer plan's Secret to be handed back so its own reconcile owns the state, got checksum %s", got)
	}
	if value, ok := result.Data[planapi.PlanCheckpointKey]; ok {
		t.Errorf("expected no checkpoint to be attributed to the newer plan, got %q", value)
	}
}

// --- Part 8: the externally mutilated Secret ------------------------------------------------------

// TestHandEditedResumeStateMarksThePlanAppliedWithoutRunningIt DOCUMENTS current behaviour; it is
// not an endorsement of it.
//
// buildSecretDataUpdates gates its plan-progress clear on UsesPlanState, so a Paused: true
// checkpoint survives the checksum flow indefinitely. If plan-state later returns on the SAME
// checksum, resolveResume honours the stored ResumeState verbatim — and a ResumeState of
// "succeeded" therefore marks the plan applied without running a single one-time instruction.
//
// The agent cannot produce this Secret on its own: nothing writes plan-progress without also
// writing plan-state, and nothing writes an empty plan-state. Reaching it requires either an
// externally hand-edited Secret or a pause/downgrade/unpause/upgrade sequence across two Rancher
// versions. The ruling for this task is that this is the behaviour, not a defect.
func TestHandEditedResumeStateMarksThePlanAppliedWithoutRunningIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	f := newSuppressionFixture(t)
	secret := newInterruptTestSecret(f.planBytes, nil, map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStatePending),
		// The checkpoint a pause of an already-succeeded plan leaves behind — byte-identical to
		// what Part 1's "succeeded with no checkpoint" row asserts handlePause writes. Nothing is
		// forged here; what has been mutilated is plan-state, moved out from under the checkpoint
		// to a non-terminal value that the agent itself would never pair with this record.
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(PlanCheckpoint{
			Checksum: f.checksum, Completed: 0, Total: 3, ResumeState: planapi.PlanStateSucceeded, Paused: true,
		}),
	})
	rec := newInterruptRecorder(secret)
	sc := newInterruptTestController(t, rec)

	w := newTestWatcher(t, false, "")
	result, err := w.reconcileSecret(context.Background(), sc, secret, 30*time.Second)
	if err != nil {
		t.Fatalf("reconcileSecret returned error: %v", err)
	}

	for i, sentinel := range f.oneTime {
		assertPathAbsent(t, sentinel, fmt.Sprintf("documented behaviour: the hand-edited ResumeState resolves to a terminal "+
			"state, so one-time instruction %d never runs", i))
	}
	if got := planapi.PlanState(result.Data[planapi.PlanStateKey]); got != planapi.PlanStateSucceeded {
		t.Errorf("documented behaviour: expected the stored ResumeState to be honoured verbatim as plan-state %q, got %q",
			planapi.PlanStateSucceeded, got)
	}
	if got := string(result.Data[AppliedChecksumKey]); got != f.checksum {
		t.Errorf("documented behaviour: expected the plan to be marked applied (%q) without having run, got %q", f.checksum, got)
	}
	// The checkpoint is cleared by the outcome write, so the mutilation is self-limiting: it costs
	// one wrongly-skipped plan rather than persisting.
	if len(result.Data[planapi.PlanCheckpointKey]) != 0 {
		t.Errorf("expected the checkpoint to be cleared by the outcome write, got %q", result.Data[planapi.PlanCheckpointKey])
	}
}
