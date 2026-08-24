package applyinator

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func TestDrainRemovesLeakedInterlock(t *testing.T) {
	e := newTestEnv(t)
	writeOwner(t, e.active, newInterlockOwner(time.Now()))

	e.a.Drain(context.Background())
	mustNotExist(t, e.active, "interlock after drain")
}

func TestDrainIsANoOpWhenNothingToClean(t *testing.T) {
	e := newTestEnv(t)
	e.a.Drain(context.Background()) // must not panic on a missing file
	mustNotExist(t, e.active, "interlock after drain")
}

// TestDrainWithoutInterlockDir: interlocks are optional, and Drain runs on every
// shutdown path.
func TestDrainWithoutInterlockDir(t *testing.T) {
	a := NewApplyinator(t.TempDir(), false, "", "", nil)
	a.Drain(context.Background()) // must not panic
}

// TestDrainWaitsForInFlightApply is the point of the drain: on SIGTERM an apply
// may still be running, and its deferred cleanup must be allowed to land rather
// than the process exiting out from under it.
func TestDrainWaitsForInFlightApply(t *testing.T) {
	e := newTestEnv(t)
	writeOwner(t, e.active, newInterlockOwner(time.Now()))

	// Stand in for an in-flight Apply, which holds a.mu for its duration.
	e.a.mu.Lock()

	drained := make(chan struct{})
	go func() {
		e.a.Drain(context.Background())
		close(drained)
	}()

	select {
	case <-drained:
		e.a.mu.Unlock()
		t.Fatal("Drain returned while an apply was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	// The "apply" finishes and removes its own interlock, as the defer in Apply
	// does; Drain must then return promptly.
	if err := os.Remove(e.active); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Unlock()

	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("Drain did not return after the in-flight apply completed")
	}
	mustNotExist(t, e.active, "interlock after drain")
}

// TestDrainTimesOutAndClearsAnyway: wrangler hard-exits on a second SIGTERM, so
// the drain must be bounded and must still clear the file when it gives up.
func TestDrainTimesOutAndClearsAnyway(t *testing.T) {
	e := newTestEnv(t)
	writeOwner(t, e.active, newInterlockOwner(time.Now()))

	var wg sync.WaitGroup
	wg.Add(1)
	e.a.mu.Lock()
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Second)
		e.a.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		e.a.Drain(ctx)
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("Drain took %v, want it bounded by the 50ms context deadline", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Drain did not honour its context deadline")
	}
	mustNotExist(t, e.active, "interlock after a timed-out drain")
	wg.Wait()
}
