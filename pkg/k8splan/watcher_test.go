package k8splan

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestToInt(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"0", 0},
		{"839896925", 839896925},
		{"notanumber", 0},
	} {
		if got := toInt(tc.in); got != tc.want {
			t.Errorf("toInt(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLiveResourceVersion(t *testing.T) {
	if got := liveResourceVersion(nil); got != "" {
		t.Errorf("liveResourceVersion(nil) = %q, want empty", got)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "42"}}
	if got := liveResourceVersion(secret); got != "42" {
		t.Errorf("liveResourceVersion = %q, want %q", got, "42")
	}
}

// TestResolveStaleReadUsesLive is the fix for the reported wedge: an informer
// relist against a lagging apiserver hands the agent an older resourceVersion
// than the one it last wrote. Confirming with a live read establishes ground
// truth and lets the reconcile continue, instead of returning a hard error.
func TestResolveStaleReadUsesLive(t *testing.T) {
	for _, tc := range []struct{ name, lastApplied, live string }{
		{"live is newer", "839896925", "839896999"},
		{"live is exactly the last applied", "839896925", "839896925"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &watcher{lastAppliedResourceVersion: tc.lastApplied, staleReadCount: 1}
			if got := w.resolveStaleRead(tc.live, nil); got != staleUseLive {
				t.Errorf("action = %v, want staleUseLive", got)
			}
			if w.staleReadCount != 0 {
				t.Errorf("staleReadCount = %d, want it reset to 0", w.staleReadCount)
			}
			if w.lastAppliedResourceVersion != tc.lastApplied {
				t.Errorf("lastAppliedResourceVersion = %q, want it untouched (%q)",
					w.lastAppliedResourceVersion, tc.lastApplied)
			}
		})
	}
}

// TestResolveStaleReadGetError: a failed live read is not evidence of anything.
// Retry without engaging the controller's failure rate limiter, and without
// advancing the streak toward a forced resync.
func TestResolveStaleReadGetError(t *testing.T) {
	w := &watcher{lastAppliedResourceVersion: "839896925", staleReadCount: 1}
	if got := w.resolveStaleRead("", errors.New("apiserver unavailable")); got != staleRetry {
		t.Errorf("action = %v, want staleRetry", got)
	}
	if w.staleReadCount != 1 {
		t.Errorf("staleReadCount = %d, want it unchanged at 1", w.staleReadCount)
	}
	if w.lastAppliedResourceVersion != "839896925" {
		t.Errorf("lastAppliedResourceVersion = %q, want it untouched", w.lastAppliedResourceVersion)
	}
}

// TestResolveStaleReadForcesResyncAfterThreshold: when even the live read stays
// behind, the agent must eventually stop trusting its remembered resourceVersion
// rather than looping forever. This is the escape hatch from the reported
// deadlock.
func TestResolveStaleReadForcesResyncAfterThreshold(t *testing.T) {
	w := &watcher{lastAppliedResourceVersion: "839896925"}

	for i := 1; i < maxConsecutiveStaleReads; i++ {
		if got := w.resolveStaleRead("839896677", nil); got != staleRetry {
			t.Fatalf("attempt %d: action = %v, want staleRetry", i, got)
		}
		if w.staleReadCount != i {
			t.Fatalf("attempt %d: staleReadCount = %d, want %d", i, w.staleReadCount, i)
		}
		if w.lastAppliedResourceVersion == "" {
			t.Fatalf("attempt %d: lastAppliedResourceVersion was reset too early", i)
		}
	}

	if got := w.resolveStaleRead("839896677", nil); got != staleForceResync {
		t.Fatalf("attempt %d: action = %v, want staleForceResync", maxConsecutiveStaleReads, got)
	}
	if w.lastAppliedResourceVersion != "" {
		t.Errorf("lastAppliedResourceVersion = %q, want it cleared to force a resync",
			w.lastAppliedResourceVersion)
	}
	if w.staleReadCount != 0 {
		t.Errorf("staleReadCount = %d, want it reset to 0 after the resync", w.staleReadCount)
	}
}

// TestResolveStaleReadRecoveryResetsStreak: a recovered live read must clear the
// streak so an unrelated stale observation later does not tip a fresh agent
// straight into a forced resync.
func TestResolveStaleReadRecoveryResetsStreak(t *testing.T) {
	w := &watcher{lastAppliedResourceVersion: "100"}

	if got := w.resolveStaleRead("50", nil); got != staleRetry {
		t.Fatalf("action = %v, want staleRetry", got)
	}
	if got := w.resolveStaleRead("150", nil); got != staleUseLive {
		t.Fatalf("action = %v, want staleUseLive", got)
	}
	if w.staleReadCount != 0 {
		t.Fatalf("staleReadCount = %d, want 0 after recovery", w.staleReadCount)
	}

	// The streak restarts from scratch: it must take the full threshold again.
	for i := 1; i < maxConsecutiveStaleReads; i++ {
		if got := w.resolveStaleRead("50", nil); got != staleRetry {
			t.Fatalf("post-recovery attempt %d: action = %v, want staleRetry", i, got)
		}
	}
	if got := w.resolveStaleRead("50", nil); got != staleForceResync {
		t.Errorf("action = %v, want staleForceResync", got)
	}
}

// TestResolveStaleReadAfterResyncTreatsLiveAsCurrent: once
// lastAppliedResourceVersion is cleared, any live read is by definition current,
// so the next pass must proceed rather than immediately re-entering the streak.
func TestResolveStaleReadAfterResyncTreatsLiveAsCurrent(t *testing.T) {
	w := &watcher{lastAppliedResourceVersion: ""}
	if got := w.resolveStaleRead("50", nil); got != staleUseLive {
		t.Errorf("action = %v, want staleUseLive once the remembered version is cleared", got)
	}
}

// TestNoteInOrderEventEndsTheStreak guards the meaning of
// maxConsecutiveStaleReads. The watcher calls noteInOrderEvent from the default
// arm of its switch, on every event that arrives in order; drop that call and
// the counter becomes cumulative, so three unrelated stale observations spread
// over an agent's lifetime would force a resync — and a resync re-runs one-time
// instructions.
func TestNoteInOrderEventEndsTheStreak(t *testing.T) {
	w := &watcher{lastAppliedResourceVersion: "100", staleReadCount: maxConsecutiveStaleReads - 1}

	w.noteInOrderEvent()
	if w.staleReadCount != 0 {
		t.Fatalf("staleReadCount = %d, want 0", w.staleReadCount)
	}

	// One stale read after an in-order event must not be enough to force a
	// resync, which it would have been without the reset.
	if got := w.resolveStaleRead("50", nil); got != staleRetry {
		t.Errorf("action = %v, want staleRetry — the streak should have restarted", got)
	}
	if w.lastAppliedResourceVersion != "100" {
		t.Errorf("lastAppliedResourceVersion = %q, want it untouched", w.lastAppliedResourceVersion)
	}
}
