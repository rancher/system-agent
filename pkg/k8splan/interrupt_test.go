package k8splan

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/wrangler/v3/pkg/generic/fake"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// absentAnnotation marks an annotation that is not present at all, which is deliberately distinct
// from a present-but-empty value: the latter is a configuration error, the former is "false".
const absentAnnotation = "\x00absent"

// interruptAnnotations renders a canceled/paused pair into an annotation map, omitting either key
// whose value is absentAnnotation.
func interruptAnnotations(canceled, paused string) map[string]string {
	annotations := map[string]string{}
	if canceled != absentAnnotation {
		annotations[planapi.PlanCanceledAnnotation] = canceled
	}
	if paused != absentAnnotation {
		annotations[planapi.PlanPausedAnnotation] = paused
	}
	return annotations
}

func TestParseInterruptAnnotation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
		wantErr     bool
	}{
		{name: "an absent annotation reads as false", annotations: map[string]string{}, want: false},
		{name: `the value "true" requests the interrupt`, annotations: map[string]string{planapi.PlanPausedAnnotation: "true"}, want: true},
		{name: `the value "false" does not request the interrupt`, annotations: map[string]string{planapi.PlanPausedAnnotation: "false"}, want: false},
		{name: "a capitalised spelling is a configuration error", annotations: map[string]string{planapi.PlanPausedAnnotation: "True"}, wantErr: true},
		{name: "a present but empty value is a configuration error", annotations: map[string]string{planapi.PlanPausedAnnotation: ""}, wantErr: true},
		{name: "a trailing space is a configuration error", annotations: map[string]string{planapi.PlanPausedAnnotation: "true "}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseInterruptAnnotation(tt.annotations, planapi.PlanPausedAnnotation)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseInterruptAnnotation returned error %v, wantErr %t", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseInterruptAnnotation returned %t, want %t", got, tt.want)
			}
			if !tt.wantErr {
				return
			}
			// The message has to be actionable on its own: an operator reading it from journalctl
			// needs to know which annotation is wrong, what it currently says, and what it may say.
			//
			// The offending value is asserted quoted rather than raw. A raw substring check is
			// vacuous for the present-but-empty row — strings.Contains(msg, "") is always true —
			// which is exactly the row where "the message names the value" is hardest to get
			// right and most needed, since an unquoted empty value renders as nothing at all.
			msg := err.Error()
			wants := []string{planapi.PlanPausedAnnotation, strconv.Quote(tt.annotations[planapi.PlanPausedAnnotation]), `"true"`, `"false"`}
			for _, want := range wants {
				if !strings.Contains(msg, want) {
					t.Errorf("error message %q does not mention %q", msg, want)
				}
			}
		})
	}
}

// TestParseInterruptAnnotationRejectsParseBoolSpellings pins the deliberate choice not to use
// strconv.ParseBool. Every value here is accepted by ParseBool and must be rejected here, so that
// the accepted set stays exactly the one that can be documented and validated by an admission
// check rather than eleven spellings of each value plus a rejected twelfth.
func TestParseInterruptAnnotationRejectsParseBoolSpellings(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"True", "TRUE", "t", "T", "1", "False", "FALSE", "f", "F", "0"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			if _, err := parseInterruptAnnotation(map[string]string{planapi.PlanCanceledAnnotation: value}, planapi.PlanCanceledAnnotation); err == nil {
				t.Errorf("value %q was accepted; only the exact spellings \"true\" and \"false\" are valid", value)
			}
		})
	}
}

func TestReadInterrupt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		canceled string
		paused   string
		want     applyinator.Interruption
		wantErr  bool
		// wantErrMentions are substrings the joined error must contain.
		wantErrMentions []string
	}{
		{
			name:     "neither annotation is set: the plan may run",
			canceled: absentAnnotation, paused: absentAnnotation,
			want: applyinator.InterruptionNone,
		},
		{
			name:     "both annotations explicitly false: the plan may run",
			canceled: "false", paused: "false",
			want: applyinator.InterruptionNone,
		},
		{
			name:     "paused alone holds the plan",
			canceled: absentAnnotation, paused: "true",
			want: applyinator.InterruptionPaused,
		},
		{
			name:     "canceled alone cancels the plan",
			canceled: "true", paused: absentAnnotation,
			want: applyinator.InterruptionCanceled,
		},
		{
			name:     "cancel wins when both are set",
			canceled: "true", paused: "true",
			want: applyinator.InterruptionCanceled,
		},
		{
			name:     "a valid cancel is never blocked by an invalid pause value",
			canceled: "true", paused: "yes",
			want: applyinator.InterruptionCanceled,
		},
		{
			name:     "a capitalised cancel value is a configuration error, not a cancellation",
			canceled: "True", paused: absentAnnotation,
			want: applyinator.InterruptionNone, wantErr: true,
		},
		{
			name:     "a ParseBool-style cancel value is a configuration error, not a cancellation",
			canceled: "1", paused: absentAnnotation,
			want: applyinator.InterruptionNone, wantErr: true,
		},
		{
			name:     "a present but empty pause value is a configuration error, not an absent annotation",
			canceled: absentAnnotation, paused: "",
			want: applyinator.InterruptionNone, wantErr: true,
		},
		{
			name:     "both values invalid: one joined error naming both keys",
			canceled: "maybe", paused: "nope",
			want: applyinator.InterruptionNone, wantErr: true,
			wantErrMentions: []string{planapi.PlanCanceledAnnotation, planapi.PlanPausedAnnotation},
		},
		{
			name:     "an invalid cancel value does not let a valid pause take effect",
			canceled: "nope", paused: "true",
			want: applyinator.InterruptionNone, wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := readInterrupt(interruptAnnotations(tt.canceled, tt.paused))
			if (err != nil) != tt.wantErr {
				t.Fatalf("readInterrupt returned error %v, wantErr %t", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("readInterrupt returned %q, want %q", got, tt.want)
			}
			for _, want := range tt.wantErrMentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("joined error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

// decodeProgress decodes the checkpoint handleInterrupt wrote, so the assertions are made against
// the struct rather than against a particular JSON encoding.
func decodeProgress(t *testing.T, updates map[string][]byte) PlanCheckpoint {
	t.Helper()

	raw, ok := updates[planapi.PlanCheckpointKey]
	if !ok {
		t.Fatalf("updates carry no %s key: %v", planapi.PlanCheckpointKey, updates)
	}
	var p PlanCheckpoint
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("failed to decode the %s value %q: %v", planapi.PlanCheckpointKey, string(raw), err)
	}
	return p
}

func TestHandleInterrupt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		interrupt        applyinator.Interruption
		currentPlanState planapi.PlanState
		data             map[string][]byte
		total            int
		wantEmpty        bool
		wantPlanState    planapi.PlanState
		wantProgress     PlanCheckpoint
	}{
		{
			name:      "cancelling a pending plan records the cancellation",
			interrupt: applyinator.InterruptionCanceled, currentPlanState: planapi.PlanStatePending, total: 4,
			wantPlanState: planapi.PlanStateCanceled,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 0, Total: 4},
		},
		{
			name:      "cancelling an in-progress plan records the cancellation",
			interrupt: applyinator.InterruptionCanceled, currentPlanState: planapi.PlanStateInProgress, total: 4,
			wantPlanState: planapi.PlanStateCanceled,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 0, Total: 4},
		},
		{
			name:      "cancelling reports how far the plan got, but never as a resumable suspension",
			interrupt: applyinator.InterruptionCanceled, currentPlanState: planapi.PlanStateInProgress, total: 4,
			data: progressData(PlanCheckpoint{
				Checksum: progressChecksum, Completed: 3, Total: 4, ResumeState: planapi.PlanStateInProgress, Paused: true,
			}),
			wantPlanState: planapi.PlanStateCanceled,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 3, Total: 4, ResumeState: "", Paused: false},
		},
		{
			name:      "cancelling a succeeded plan writes nothing: it is already terminal",
			interrupt: applyinator.InterruptionCanceled, currentPlanState: planapi.PlanStateSucceeded, total: 4,
			wantEmpty: true,
		},
		{
			name:      "cancelling an already canceled plan writes nothing: the write-once rule",
			interrupt: applyinator.InterruptionCanceled, currentPlanState: planapi.PlanStateCanceled, total: 4,
			data:      progressData(PlanCheckpoint{Checksum: progressChecksum, Completed: 3, Total: 4}),
			wantEmpty: true,
		},
		{
			name:      "pausing a pending plan resumes into pending",
			interrupt: applyinator.InterruptionPaused, currentPlanState: planapi.PlanStatePending, total: 4,
			wantPlanState: planapi.PlanStatePaused,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 0, Total: 4, ResumeState: planapi.PlanStatePending, Paused: true},
		},
		{
			name:      "pausing an in-progress plan resumes into in-progress",
			interrupt: applyinator.InterruptionPaused, currentPlanState: planapi.PlanStateInProgress, total: 4,
			wantPlanState: planapi.PlanStatePaused,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 0, Total: 4, ResumeState: planapi.PlanStateInProgress, Paused: true},
		},
		{
			// A pause landing on a plan that is only running periodic instructions must not
			// re-execute a completed Day 2 operation when it is unpaused.
			name:      "pausing a succeeded plan resumes into succeeded, not in-progress",
			interrupt: applyinator.InterruptionPaused, currentPlanState: planapi.PlanStateSucceeded, total: 4,
			wantPlanState: planapi.PlanStatePaused,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 0, Total: 4, ResumeState: planapi.PlanStateSucceeded, Paused: true},
		},
		{
			name:      "pausing a failed plan resumes into failed",
			interrupt: applyinator.InterruptionPaused, currentPlanState: planapi.PlanStateFailed, total: 4,
			wantPlanState: planapi.PlanStatePaused,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 0, Total: 4, ResumeState: planapi.PlanStateFailed, Paused: true},
		},
		{
			name:      "pausing when a suspension is already recorded writes nothing: the write-once rule",
			interrupt: applyinator.InterruptionPaused, currentPlanState: planapi.PlanStatePaused, total: 4,
			data:      progressData(PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 4, ResumeState: planapi.PlanStateInProgress, Paused: true}),
			wantEmpty: true,
		},
		{
			// The guard keys off the checkpoint, not off plan-state: plan-state paused with no
			// checkpoint beneath it is a suspension that has never been recorded.
			name:      "pausing when plan-state is paused but no checkpoint exists records the suspension",
			interrupt: applyinator.InterruptionPaused, currentPlanState: planapi.PlanStatePaused, total: 4,
			wantPlanState: planapi.PlanStatePaused,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 0, Total: 4, ResumeState: "", Paused: true},
		},
		{
			name:      "pausing preserves completed from a checksum-matching checkpoint",
			interrupt: applyinator.InterruptionPaused, currentPlanState: planapi.PlanStateInProgress, total: 4,
			data:          progressData(PlanCheckpoint{Checksum: progressChecksum, Completed: 3, Total: 4}),
			wantPlanState: planapi.PlanStatePaused,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 3, Total: 4, ResumeState: planapi.PlanStateInProgress, Paused: true},
		},
		{
			// Neither path can observe the node's processes: no apply is in flight at reconcile entry.
			// Dropping the flag would silently retract a warning that is still true, so both rewrites of
			// the checkpoint carry it forward.
			name:      "cancelling preserves a recorded incomplete termination",
			interrupt: applyinator.InterruptionCanceled, currentPlanState: planapi.PlanStateInProgress, total: 4,
			data:          progressData(PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 4, TerminationIncomplete: true}),
			wantPlanState: planapi.PlanStateCanceled,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 4, TerminationIncomplete: true},
		},
		{
			name:      "pausing preserves a recorded incomplete termination",
			interrupt: applyinator.InterruptionPaused, currentPlanState: planapi.PlanStateInProgress, total: 4,
			data:          progressData(PlanCheckpoint{Checksum: progressChecksum, Completed: 2, Total: 4, TerminationIncomplete: true}),
			wantPlanState: planapi.PlanStatePaused,
			wantProgress: PlanCheckpoint{
				Checksum: progressChecksum, Completed: 2, Total: 4, ResumeState: planapi.PlanStateInProgress, Paused: true, TerminationIncomplete: true,
			},
		},
		{
			name:      "pausing ignores a checkpoint recorded for a different plan",
			interrupt: applyinator.InterruptionPaused, currentPlanState: planapi.PlanStateInProgress, total: 4,
			data:          progressData(PlanCheckpoint{Checksum: otherChecksum, Completed: 3, Total: 9, ResumeState: planapi.PlanStateInProgress, Paused: true}),
			wantPlanState: planapi.PlanStatePaused,
			wantProgress:  PlanCheckpoint{Checksum: progressChecksum, Completed: 0, Total: 4, ResumeState: planapi.PlanStateInProgress, Paused: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			updates := handleInterrupt(tt.interrupt, tt.currentPlanState, tt.data, progressChecksum, tt.total)
			if tt.wantEmpty {
				if len(updates) != 0 {
					t.Fatalf("expected no Secret writes, got %d keys: %v", len(updates), updates)
				}
				return
			}
			if got := planapi.PlanState(updates[planapi.PlanStateKey]); got != tt.wantPlanState {
				t.Errorf("wrote plan-state %q, want %q", got, tt.wantPlanState)
			}
			if got := decodeProgress(t, updates); got != tt.wantProgress {
				t.Errorf("wrote checkpoint %+v, want %+v", got, tt.wantProgress)
			}
		})
	}
}

// TestHandleInterruptWithNoInterruptionWritesNothing covers the by-contract-unreachable input: the
// caller only reaches handleInterrupt for a real interrupt, and writing nothing is the safe
// response to an input that was not supposed to arrive.
func TestHandleInterruptWithNoInterruptionWritesNothing(t *testing.T) {
	t.Parallel()

	updates := handleInterrupt(applyinator.InterruptionNone, planapi.PlanStateInProgress, nil, progressChecksum, 4)
	if len(updates) != 0 {
		t.Errorf("expected no Secret writes, got %v", updates)
	}
}

// withInterruptPollInterval shortens the interrupt watch's poll interval for the duration of the
// test and restores it afterwards.
//
// The t.Setenv call sets nothing anybody reads: it is a tripwire. t.Setenv panics if the test, or
// any ancestor of it, has called t.Parallel() — which is exactly the condition under which writing
// this package-level var from a test becomes a data race with every other test in the package. CI
// runs no -race job, so this panic is the only thing that would catch a future parallel test
// reaching for this helper.
func withInterruptPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	t.Setenv("K8SPLAN_POLL_GUARD", "1")

	original := interruptPollInterval
	interruptPollInterval = d
	t.Cleanup(func() { interruptPollInterval = original })
}

// annotationSequence serves one annotation map per poll and records how many polls it has served.
// Once the sequence is exhausted the last entry repeats, so a test can append a new entry with
// push and have the very next poll observe it.
type annotationSequence struct {
	mu    sync.Mutex
	reads int
	steps []map[string]string
}

func (a *annotationSequence) next() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()

	i := min(a.reads, len(a.steps)-1)
	a.reads++
	return a.steps[i]
}

func (a *annotationSequence) served() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads
}

func (a *annotationSequence) push(step map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steps = append(a.steps, step)
}

// newInterruptWatchController wires a SecretController whose informer cache serves seq. When
// cacheErr is non-nil every cache read fails with it and the live client serves seq instead, which
// exercises the watch's fallback.
func newInterruptWatchController(t *testing.T, seq *annotationSequence, cacheErr error,
) *fake.MockControllerInterface[*corev1.Secret, *corev1.SecretList] {
	t.Helper()

	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	cache := fake.NewMockCacheInterface[*corev1.Secret](ctrl)
	sc.EXPECT().Cache().Return(cache).AnyTimes()

	serve := func() (*corev1.Secret, error) {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret, Annotations: seq.next()},
		}, nil
	}
	if cacheErr != nil {
		cache.EXPECT().Get(testNamespace, testSecret).Return(nil, cacheErr).AnyTimes()
		sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).DoAndReturn(func(string, string, metav1.GetOptions) (*corev1.Secret, error) {
			return serve()
		}).AnyTimes()
		return sc
	}
	cache.EXPECT().Get(testNamespace, testSecret).DoAndReturn(func(string, string) (*corev1.Secret, error) {
		return serve()
	}).AnyTimes()
	return sc
}

func waitForClose(t *testing.T, name string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("expected the %s channel to be closed", name)
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func waitForPolls(t *testing.T, seq *annotationSequence, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if seq.served() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the interrupt watch served only %d polls, wanted at least %d", seq.served(), n)
}

// TestStartInterruptWatchObservesAnnotations is deliberately not parallel: it rewrites the
// package-level interruptPollInterval via withInterruptPollInterval.
func TestStartInterruptWatchObservesAnnotations(t *testing.T) {
	tests := []struct {
		name       string
		steps      []map[string]string
		cacheErr   error
		wantCancel bool
		wantPause  bool
	}{
		{
			name:       "a cancellation appearing on a later poll closes the cancel channel",
			steps:      []map[string]string{{}, {planapi.PlanCanceledAnnotation: "true"}},
			wantCancel: true,
		},
		{
			name:      "a pause appearing on a later poll closes the pause channel",
			steps:     []map[string]string{{}, {planapi.PlanPausedAnnotation: "true"}},
			wantPause: true,
		},
		{
			name:       "cancel wins when both annotations appear at once",
			steps:      []map[string]string{{}, {planapi.PlanCanceledAnnotation: "true", planapi.PlanPausedAnnotation: "true"}},
			wantCancel: true,
		},
		{
			name:      "a failing cache read falls back to the API server and still observes the interrupt",
			steps:     []map[string]string{{}, {planapi.PlanPausedAnnotation: "true"}},
			cacheErr:  errors.New("cache has not synced"),
			wantPause: true,
		},
		{
			name:       "a failing cache read falls back to the API server and still observes a cancellation",
			steps:      []map[string]string{{}, {planapi.PlanCanceledAnnotation: "true"}},
			cacheErr:   errors.New("cache has not synced"),
			wantCancel: true,
		},
		{
			name:  "explicitly false annotations close nothing",
			steps: []map[string]string{{planapi.PlanCanceledAnnotation: "false", planapi.PlanPausedAnnotation: "false"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withInterruptPollInterval(t, 2*time.Millisecond)

			seq := &annotationSequence{steps: tt.steps}
			sc := newInterruptWatchController(t, seq, tt.cacheErr)
			w := newTestWatcher(t, true, "")

			cancelCh, pauseCh, stop := w.startInterruptWatch(context.Background(), sc)
			defer stop()

			if tt.wantCancel {
				waitForClose(t, "cancel", cancelCh)
			}
			if tt.wantPause {
				waitForClose(t, "pause", pauseCh)
			}
			if !tt.wantCancel && !tt.wantPause {
				// Nothing should ever close, so give the watch several polls to get it wrong.
				waitForPolls(t, seq, 4)
			}
			if !tt.wantCancel && isClosed(cancelCh) {
				t.Error("the cancel channel was closed but no cancellation was requested")
			}
			if !tt.wantPause && isClosed(pauseCh) {
				t.Error("the pause channel was closed but no pause was requested")
			}
		})
	}
}

// TestStartInterruptWatchIgnoresInvalidValuesUntilCorrected pins the one place where this file's
// two paths diverge. Interrupting an in-flight apply is destructive — for cancel, irreversibly so
// — so a value the agent cannot parse is reported and otherwise ignored, rather than acted on as a
// guess. The reconcile-entry path can afford to be strict because refusing to *start* costs
// nothing; the watch cannot. Without this test, "the watch ignores invalid values" is a sentence
// in a design document rather than a behaviour.
//
// Not parallel: it rewrites the package-level interruptPollInterval.
func TestStartInterruptWatchIgnoresInvalidValuesUntilCorrected(t *testing.T) {
	withInterruptPollInterval(t, 2*time.Millisecond)

	seq := &annotationSequence{steps: []map[string]string{{planapi.PlanPausedAnnotation: "yes"}}}
	sc := newInterruptWatchController(t, seq, nil)
	w := newTestWatcher(t, true, "")

	cancelCh, pauseCh, stop := w.startInterruptWatch(context.Background(), sc)
	defer stop()

	// Several polls all returning an unparsable value must interrupt nothing.
	waitForPolls(t, seq, 5)
	if isClosed(pauseCh) {
		t.Fatalf("the pause channel closed on the unparsable value %q", "yes")
	}
	if isClosed(cancelCh) {
		t.Fatalf("the cancel channel closed on the unparsable value %q", "yes")
	}

	// The watch must still be polling, so correcting the value takes effect on the next poll.
	seq.push(map[string]string{planapi.PlanPausedAnnotation: "true"})
	waitForClose(t, "pause", pauseCh)
	if isClosed(cancelCh) {
		t.Error("the cancel channel closed but only a pause was requested")
	}
}

// TestStartInterruptWatchStopIsIdempotent also pins that stop() does not hang: it waits for the
// polling goroutine to exit, so a returned stop() is a guarantee that nothing is still reading the
// Secret. Not parallel: it rewrites the package-level interruptPollInterval.
func TestStartInterruptWatchStopIsIdempotent(t *testing.T) {
	withInterruptPollInterval(t, 2*time.Millisecond)

	seq := &annotationSequence{steps: []map[string]string{{}}}
	sc := newInterruptWatchController(t, seq, nil)
	w := newTestWatcher(t, true, "")

	_, _, stop := w.startInterruptWatch(context.Background(), sc)
	waitForPolls(t, seq, 2)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		stop()
		stop()
		stop()
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("stop() hung; it must be idempotent and must return once the goroutine has exited")
	}

	settled := seq.served()
	time.Sleep(50 * time.Millisecond) // many poll intervals
	if got := seq.served(); got != settled {
		t.Errorf("the watch served %d more polls after stop() returned; stop() must wait for the goroutine to exit", got-settled)
	}
}

// TestStartInterruptWatchStopsOnContextCancellation is not parallel: it rewrites the package-level
// interruptPollInterval.
func TestStartInterruptWatchStopsOnContextCancellation(t *testing.T) {
	withInterruptPollInterval(t, 2*time.Millisecond)

	seq := &annotationSequence{steps: []map[string]string{{}}}
	sc := newInterruptWatchController(t, seq, nil)
	w := newTestWatcher(t, true, "")

	ctx, cancel := context.WithCancel(context.Background())
	_, _, stop := w.startInterruptWatch(ctx, sc)
	t.Cleanup(stop)

	waitForPolls(t, seq, 2)
	cancel()

	// The goroutine must stop polling on its own, without stop() being called.
	time.Sleep(50 * time.Millisecond)
	settled := seq.served()
	time.Sleep(50 * time.Millisecond)
	if got := seq.served(); got != settled {
		t.Errorf("the watch served %d more polls after its context was canceled", got-settled)
	}

	// And stop() must still return promptly on an already-exited goroutine.
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		stop()
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("stop() hung after the context was canceled")
	}
}

// interruptTestPlan returns a plan whose only purpose is to have a stable checksum.
func interruptTestPlan(t *testing.T, name string) (raw []byte, checksum string) {
	t.Helper()
	return marshalPlan(t, planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: name, Command: "sh", Args: []string{"-c", "true"}}},
		},
	})
}

// interruptTestSecret builds the plan Secret the API server would return, carrying the operator's
// pause annotation — the very write whose resourceVersion bump makes the outcome Update conflict.
func interruptTestSecret(planBytes []byte, resourceVersion, uid string, data map[string][]byte) *corev1.Secret {
	full := map[string][]byte{}
	if planBytes != nil {
		full[PlanKey] = planBytes
	}
	maps.Copy(full, data)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            testSecret,
			ResourceVersion: resourceVersion,
			UID:             types.UID(uid),
			Annotations:     map[string]string{planapi.PlanPausedAnnotation: "true"},
		},
		Data: full,
	}
}

// TestWriteInterruptOutcomeRetriesOnConflictAndPreservesTheCheckpoint covers the regression this
// function exists to fix. The conflict is not a rare race, it is the normal path: the operator's
// annotation write bumps the Secret's resourceVersion while the agent holds a copy read before the
// apply started. updateSecret would abandon the merge here (its predicate compares the fetched
// plan against applied-checksum, which the interrupted path deliberately never writes) and
// reconcileSecret would turn that into a logrus.Fatalf — losing the checkpoint, so unpause would
// re-run from instruction 0 and silently defeat the feature.
func TestWriteInterruptOutcomeRetriesOnConflictAndPreservesTheCheckpoint(t *testing.T) {
	t.Parallel()

	planBytes, checksum := interruptTestPlan(t, "ok")

	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).DoAndReturn(func(string, string, metav1.GetOptions) (*corev1.Secret, error) {
		return interruptTestSecret(planBytes, "43", "uid-1", map[string][]byte{
			planapi.PlanStateKey: []byte(planapi.PlanStateInProgress),
		}), nil
	}).Times(2)

	var observed []*corev1.Secret
	conflicted := false
	sc.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		observed = append(observed, s)
		if !conflicted {
			conflicted = true
			return nil, apierrors.NewConflict(corev1.Resource("secrets"), s.Name, errors.New("conflict"))
		}
		return s, nil
	}).Times(2)

	checkpoint := PlanCheckpoint{Checksum: checksum, Completed: 3, Total: 5, ResumeState: planapi.PlanStateInProgress, Paused: true}
	updates := map[string][]byte{
		planapi.PlanStateKey:      []byte(planapi.PlanStatePaused),
		planapi.PlanCheckpointKey: marshalPlanCheckpoint(checkpoint),
	}

	w := newTestWatcher(t, true, "")
	result, err := w.writeInterruptOutcome(sc, checksum, "recorded the test outcome", updates)
	if err != nil {
		t.Fatalf("writeInterruptOutcome returned error: %v", err)
	}
	if len(observed) != 2 {
		t.Fatalf("expected the read-modify-write to be retried once, got %d Update calls", len(observed))
	}

	written := observed[len(observed)-1]
	if written.ResourceVersion != "43" {
		t.Errorf("the retry wrote resource version %q; the whole read-modify-write must be retried against a freshly fetched Secret",
			written.ResourceVersion)
	}
	if written.Annotations[planapi.PlanPausedAnnotation] != "true" {
		t.Error("the updates did not land on the freshly fetched object: the operator's annotation is missing")
	}
	if got := planapi.PlanState(written.Data[planapi.PlanStateKey]); got != planapi.PlanStatePaused {
		t.Errorf("wrote plan-state %q, want %q", got, planapi.PlanStatePaused)
	}
	if got := decodeProgress(t, written.Data); got != checkpoint {
		t.Errorf("the checkpoint was lost across the conflict retry: wrote %+v, want %+v", got, checkpoint)
	}
	if result == nil || result.ResourceVersion != "43" {
		t.Errorf("expected the resulting Secret to be returned, got %+v", result)
	}
}

func TestWriteInterruptOutcomeSkipsTheUpdate(t *testing.T) {
	t.Parallel()

	planBytes, checksum := interruptTestPlan(t, "ok")
	otherPlanBytes, otherPlanChecksum := interruptTestPlan(t, "some-other-plan")
	if checksum == otherPlanChecksum {
		t.Fatal("the two test plans must have different checksums")
	}

	tests := []struct {
		name       string
		serverPlan []byte
		serverData map[string][]byte
		updates    map[string][]byte
	}{
		{
			// A newer plan has landed; that plan's own reconcile owns the state. Writing the old
			// plan's outcome onto it would attribute an interrupt to the wrong plan.
			name:       "a different plan on the server abandons the write",
			serverPlan: otherPlanBytes,
			updates:    map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStateCanceled)},
		},
		{
			name:       "no plan key on the server abandons the write",
			serverPlan: nil,
			updates:    map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStateCanceled)},
		},
		{
			name:       "an unparsable plan on the server abandons the write",
			serverPlan: []byte("not valid json"),
			updates:    map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStateCanceled)},
		},
		{
			name:       "updates that change nothing skip the update",
			serverPlan: planBytes,
			serverData: map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStatePaused)},
			updates:    map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStatePaused)},
		},
		{
			// This is what the write-once guard produces, and it is the common case for the whole
			// duration of a pause.
			name:       "an empty updates map skips the update",
			serverPlan: planBytes,
			serverData: map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStatePaused)},
			updates:    map[string][]byte{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// No Update EXPECT() is configured: an unexpected Update call fails the test.
			ctrl := gomock.NewController(t)
			sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
			sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).Return(interruptTestSecret(tt.serverPlan, "43", "uid-1", tt.serverData), nil)

			w := newTestWatcher(t, true, "")
			result, err := w.writeInterruptOutcome(sc, checksum, "recorded the test outcome", tt.updates)
			if err != nil {
				t.Fatalf("writeInterruptOutcome returned error: %v", err)
			}
			if result == nil {
				t.Fatal("expected the fetched Secret to be returned, got nil")
			}
			if w.lastAppliedResourceVersion != "" {
				t.Errorf("no write happened, so lastAppliedResourceVersion must be untouched, got %q", w.lastAppliedResourceVersion)
			}
		})
	}
}

// TestWriteInterruptOutcomeReturnsErrorsRatherThanExiting pins that this path never becomes fatal:
// reconcileSecret propagates the error and the workqueue retries under its exponential rate
// limiter. A crashing agent on a paused plan would lose the checkpoint entirely.
func TestWriteInterruptOutcomeReturnsErrorsRatherThanExiting(t *testing.T) {
	t.Parallel()

	planBytes, checksum := interruptTestPlan(t, "ok")

	tests := []struct {
		name   string
		getErr error
	}{
		{name: "a failing Get is returned", getErr: errors.New("etcd is unavailable")},
		{name: "a non-conflict Update error is returned"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctrl := gomock.NewController(t)
			sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
			if tt.getErr != nil {
				sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).Return(nil, tt.getErr)
			} else {
				sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).Return(interruptTestSecret(planBytes, "43", "uid-1", nil), nil)
				sc.EXPECT().Update(gomock.Any()).Return(nil, errors.New("etcd is unavailable"))
			}

			w := newTestWatcher(t, true, "")
			if _, err := w.writeInterruptOutcome(sc, checksum, "recorded the test outcome", map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStateCanceled)}); err == nil {
				t.Fatal("expected the error to be returned to the caller, got nil")
			}
		})
	}
}

// TestWriteInterruptOutcomeRecordsResourceVersionAndUID pins the bookkeeping updateSecret also
// does: without it, reconcileSecret's rvIsOlder check rejects the next cache delivery as stale.
func TestWriteInterruptOutcomeRecordsResourceVersionAndUID(t *testing.T) {
	t.Parallel()

	planBytes, checksum := interruptTestPlan(t, "ok")

	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).Return(interruptTestSecret(planBytes, "43", "uid-1", nil), nil)
	sc.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		updated := s.DeepCopy()
		updated.ResourceVersion = "44"
		return updated, nil
	})

	w := newTestWatcher(t, true, "")
	if _, err := w.writeInterruptOutcome(sc, checksum, "recorded the test outcome", map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStateCanceled)}); err != nil {
		t.Fatalf("writeInterruptOutcome returned error: %v", err)
	}
	if w.lastAppliedResourceVersion != "44" {
		t.Errorf("recorded lastAppliedResourceVersion %q, want %q", w.lastAppliedResourceVersion, "44")
	}
	if w.secretUID != "uid-1" {
		t.Errorf("recorded secretUID %q, want %q", w.secretUID, "uid-1")
	}
}

// TestWriteInterruptOutcomeDoesNotMutateTheFetchedSecret pins that the merge happens on a copy: the
// live client and the informer may hand back a shared object.
func TestWriteInterruptOutcomeDoesNotMutateTheFetchedSecret(t *testing.T) {
	t.Parallel()

	planBytes, checksum := interruptTestPlan(t, "ok")
	fetched := interruptTestSecret(planBytes, "43", "uid-1", map[string][]byte{
		planapi.PlanStateKey: []byte(planapi.PlanStateInProgress),
	})

	ctrl := gomock.NewController(t)
	sc := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	sc.EXPECT().Get(testNamespace, testSecret, gomock.Any()).Return(fetched, nil)
	sc.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) { return s, nil })

	w := newTestWatcher(t, true, "")
	if _, err := w.writeInterruptOutcome(sc, checksum, "recorded the test outcome", map[string][]byte{planapi.PlanStateKey: []byte(planapi.PlanStateCanceled)}); err != nil {
		t.Fatalf("writeInterruptOutcome returned error: %v", err)
	}
	if got := planapi.PlanState(fetched.Data[planapi.PlanStateKey]); got != planapi.PlanStateInProgress {
		t.Errorf("the fetched Secret was mutated in place: plan-state is now %q", got)
	}
}
