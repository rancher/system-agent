//go:build !windows

package applyinator

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestWatchForTerminationReportsAProcessTreeItCannotConfirmGone covers the watchdog's final verdict:
// terminating a process tree is a request, and stop() must report whether the request was actually
// carried out rather than assume it was.
//
// Unix-only because holding a process group occupied deterministically requires placing a process
// into it by pgid, which has no Windows equivalent.
func TestWatchForTerminationReportsAProcessTreeItCannotConfirmGone(t *testing.T) {
	// Not parallel: both cases rewrite package-level durations. withTerminationGrace enforces this.
	withTerminationGrace(t, 200*time.Millisecond)

	testCases := []struct {
		name string
		// occupyGroup leaves a process in the instruction's process group that the watchdog's kill
		// cannot remove.
		occupyGroup bool
		// exitTimeout bounds the final confirmation. The occupied case has to spend all of it, so it is
		// short; the empty case returns as soon as the group drains, so it can afford to be generous.
		exitTimeout    time.Duration
		wantIncomplete bool
	}{
		{
			name:           "group drains",
			occupyGroup:    false,
			exitTimeout:    5 * time.Second,
			wantIncomplete: false,
		},
		{
			name:           "group still occupied",
			occupyGroup:    true,
			exitTimeout:    200 * time.Millisecond,
			wantIncomplete: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			withProcessTreeExitTimeout(t, tc.exitTimeout)

			// sleep rather than a shell one-liner: a shell forks a child per loop iteration, and those
			// grandchildren are reparented to init when the shell dies, so "the group is empty" would
			// depend on how quickly init reaps them. A single execed process makes the empty case exact.
			cmd := exec.Command("sleep", "60")
			if err := configureProcessGroup(cmd); err != nil {
				t.Fatalf("configureProcessGroup: %v", err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			if err := assignProcessTree(cmd); err != nil {
				t.Fatalf("assignProcessTree: %v", err)
			}
			if tc.occupyGroup {
				occupyProcessGroup(t, cmd.Process.Pid)
			}

			ctx, cancel := context.WithCancel(context.Background())
			stop := watchForTermination(ctx, cmd)
			cancel()

			// Mirror execute's ordering. processTreeExited cannot tell a surviving descendant from the
			// command's own unreaped zombie, so calling stop() before cmd.Wait() would report every
			// cancellation as incomplete.
			if err := cmd.Wait(); err == nil {
				t.Fatal("expected the terminated command to report a signal, got a clean exit")
			}

			if got := stop(); got != tc.wantIncomplete {
				t.Errorf("stop() reported incomplete termination = %v, want %v", got, tc.wantIncomplete)
			}
		})
	}
}

// occupyProcessGroup starts a short-lived process inside pgid and leaves it unreaped until the test
// ends, so the group keeps a member for the whole of it.
//
// A zombie is still a process, so kill(-pgid, 0) succeeds while one is present. That is what makes
// this deterministic: a genuinely running survivor could always be removed by the SIGKILL the
// watchdog sends, whereas nothing the watchdog does can clear a process only this test can reap. It
// stands in for the case the confirmation exists to catch, a descendant that outlived the kill.
func occupyProcessGroup(t *testing.T, pgid int) {
	t.Helper()

	member := exec.Command("sleep", "60")
	// Setpgid with an explicit Pgid joins an existing group instead of creating one.
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := member.Start(); err != nil {
		t.Fatalf("starting a process in process group %d: %v", pgid, err)
	}
	// The watchdog's kill reaches this process too, turning it into a zombie rather than removing it
	// from the group. Reap it at the end of the test so nothing is left behind for the rest of the run.
	t.Cleanup(func() {
		_ = member.Process.Kill()
		_ = member.Wait()
	})
}
