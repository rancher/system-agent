//go:build !windows

package applyinator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// startGroupLeader starts a long-lived "sleep" process as the leader of its own process group and
// assigns it, for tests that only need a single occupied group rather than a multi-process tree.
func startGroupLeader(t *testing.T) *exec.Cmd {
	t.Helper()

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
	t.Cleanup(func() {
		// Best effort: most tests already kill and wait on cmd themselves. killProcessTree and
		// cmd.Wait() are both safe to call again on an already-terminated/reaped command.
		_ = killProcessTree(cmd)
		_ = cmd.Wait()
	})
	return cmd
}

// TestConfigureProcessGroupSetsSetpgid covers the state configureProcessGroup must leave behind
// before cmd.Start(): Setpgid requested with a zero Pgid, so the child leads a brand new group
// rather than joining one that already exists.
func TestConfigureProcessGroupSetsSetpgid(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sleep", "60")
	if err := configureProcessGroup(cmd); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("expected Setpgid to be requested, got %+v", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Pgid != 0 {
		t.Errorf("expected a zero Pgid so the child leads a new group, got %d", cmd.SysProcAttr.Pgid)
	}
}

// TestIsOwnProcessGroupLeaderGuards pins every guard on the load-bearing check documented on
// isOwnProcessGroupLeader: DO NOT relax it. A false positive here does not just mis-set a book-
// keeping flag, it turns killProcessTree's kill(-pgid, ...) into a broadcast that reaches the
// agent's own process group. None of these cases need a real process: the function reads only
// cmd.SysProcAttr and cmd.Process.Pid.
func TestIsOwnProcessGroupLeaderGuards(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		attr *syscall.SysProcAttr
		pid  int
		want bool
	}{
		{
			name: "configureProcessGroup never ran",
			attr: nil,
			pid:  99999,
			want: false,
		},
		{
			name: "Setpgid was not requested",
			attr: &syscall.SysProcAttr{},
			pid:  99999,
			want: false,
		},
		{
			name: "an explicit non-zero Pgid joins an existing group rather than leading a new one",
			attr: &syscall.SysProcAttr{Setpgid: true, Pgid: 123},
			pid:  99999,
			want: false,
		},
		{
			// The backstop documented on isOwnProcessGroupLeader: even with Setpgid and a zero
			// Pgid, a pid that coincides with the agent's own process group must not be treated as
			// a leader, because that is exactly the condition under which kill(-pgid, ...) would
			// reach the agent itself.
			name: "a pid coinciding with the agent's own process group is never a leader",
			attr: &syscall.SysProcAttr{Setpgid: true},
			pid:  syscall.Getpgrp(),
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := &exec.Cmd{SysProcAttr: tc.attr, Process: &os.Process{Pid: tc.pid}}
			if got := isOwnProcessGroupLeader(cmd); got != tc.want {
				t.Errorf("isOwnProcessGroupLeader() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsOwnProcessGroupLeaderTrueForARealGroupLeader is the positive case the guards in
// TestIsOwnProcessGroupLeaderGuards all protect: a child actually started with configureProcessGroup
// must be recognized as leading its own group.
func TestIsOwnProcessGroupLeaderTrueForARealGroupLeader(t *testing.T) {
	t.Parallel()

	cmd := startGroupLeader(t)

	if !isOwnProcessGroupLeader(cmd) {
		t.Error("expected a freshly Setpgid-started child to be its own group leader")
	}
}

// TestAssignProcessTreeIsANoOpWhenNeverStarted covers the first guard in assignProcessTree: without
// a successful cmd.Start(), there is nothing to record, and it must not error.
func TestAssignProcessTreeIsANoOpWhenNeverStarted(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sleep", "60")
	if err := assignProcessTree(cmd); err != nil {
		t.Fatalf("expected assignProcessTree to be a no-op for an unstarted command, got %v", err)
	}
	if _, ok := lookupProcessGroup(cmd); ok {
		t.Error("expected no group to be recorded")
	}
}

// TestAssignProcessTreeRecordsALeaderWhenConfigured covers the successful path: after a real
// cmd.Start() following configureProcessGroup, assignProcessTree must record a leading group whose
// pgid is the child's own pid.
func TestAssignProcessTreeRecordsALeaderWhenConfigured(t *testing.T) {
	t.Parallel()

	cmd := startGroupLeader(t)

	group, ok := lookupProcessGroup(cmd)
	if !ok || !group.isLeader {
		t.Fatalf("expected a recorded, leading group, got %+v (found=%v)", group, ok)
	}
	if group.pgid != cmd.Process.Pid {
		t.Errorf("expected the group id to equal the child's pid, got pgid=%d pid=%d", group.pgid, cmd.Process.Pid)
	}
}

// TestAssignProcessTreeRecordsANonLeaderWithoutConfigure covers the documented "never fails"
// fallback: a child started without configureProcessGroup shares this process's own group, and
// assignProcessTree must still record an entry for it, just with isLeader=false, so cancellation
// falls back to signaling the direct child instead of the (shared) group.
func TestAssignProcessTreeRecordsANonLeaderWithoutConfigure(t *testing.T) {
	t.Parallel()

	// configureProcessGroup is deliberately skipped.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if err := assignProcessTree(cmd); err != nil {
		t.Fatalf("assignProcessTree: %v", err)
	}

	group, ok := lookupProcessGroup(cmd)
	if !ok {
		t.Fatal("expected a group entry to be recorded even when the child did not lead its own group")
	}
	if group.isLeader {
		t.Error("expected isLeader=false: the child shares this process's group, so cancellation must fall back to signaling it directly")
	}
}

// TestKillProcessTreeNoOpWhenNeverStarted covers signalProcessTree's first guard: a command with no
// Process (cmd.Start() was never called) must not panic and must report success, since there is
// nothing left to terminate.
func TestKillProcessTreeNoOpWhenNeverStarted(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sleep", "60")
	if err := killProcessTree(cmd); err != nil {
		t.Errorf("expected a no-op for a command that was never started, got %v", err)
	}
}

// TestKillProcessTreeFallsBackWhenNotGroupLeader covers the fallback half of the same guard
// TestIsOwnProcessGroupLeaderGuards protects from the other direction: when the recorded group is
// not a leader, killProcessTree must signal the direct child, never kill(-pgid, ...), because pgid
// in that case is the agent's own group.
func TestKillProcessTreeFallsBackWhenNotGroupLeader(t *testing.T) {
	t.Parallel()

	// configureProcessGroup is deliberately skipped, so the child shares this process's group.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := assignProcessTree(cmd); err != nil {
		t.Fatalf("assignProcessTree: %v", err)
	}

	if err := killProcessTree(cmd); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected the killed process to report an error, got a clean exit")
	}
}

// TestKillProcessTreeTerminatesTheWholeGroup is the core Unix guarantee: killing the process group
// must remove every process in it, including a grandchild the *exec.Cmd never knew about, not just
// the direct child sh process execute started.
func TestKillProcessTreeTerminatesTheWholeGroup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sentinel := filepath.Join(dir, "grandchild-writes")
	started := filepath.Join(dir, "started")

	// Mirrors backgroundedWriterCommand in applyinator_test.go: the grandchild is not the process
	// *exec.Cmd knows about, and neither it nor the direct child calls setpgid, so both inherit the
	// group configureProcessGroup created for the direct child.
	script := "sh -c 'while true; do echo x >> " + sentinel + "; touch " + started + "; sleep 0.05; done' & sleep 60"
	cmd := exec.Command("sh", "-c", script)
	if err := configureProcessGroup(cmd); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := assignProcessTree(cmd); err != nil {
		t.Fatalf("assignProcessTree: %v", err)
	}
	waitForPath(t, started, 5*time.Second)

	if err := killProcessTree(cmd); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected the killed command to report an error, got a clean exit")
	}

	assertFileStopsGrowing(t, sentinel, time.Second)
}

// TestTerminateProcessTreeSendsAGracefulSignal covers terminateProcessTree's contract: it must send
// a signal the target can trap and act on, not an unconditional kill, so a well-behaved instruction
// gets the chance to exit cleanly.
func TestTerminateProcessTreeSendsAGracefulSignal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	script := "trap 'exit 0' TERM; touch " + started + "; while true; do sleep 0.02; done"
	cmd := exec.Command("sh", "-c", script)
	if err := configureProcessGroup(cmd); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := assignProcessTree(cmd); err != nil {
		t.Fatalf("assignProcessTree: %v", err)
	}
	waitForPath(t, started, 5*time.Second)

	if err := terminateProcessTree(cmd); err != nil {
		t.Fatalf("terminateProcessTree: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("expected the trap to exit cleanly on a graceful signal, got %v", err)
	}
}

// TestKillProcessTreeSendsAnUnconditionalSignal covers the other half of the same contract: unlike
// terminateProcessTree, killProcessTree must terminate the group even when SIGTERM is trapped away,
// which only an unblockable signal such as SIGKILL can guarantee.
func TestKillProcessTreeSendsAnUnconditionalSignal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	script := `trap "" TERM; touch ` + started + `; while true; do sleep 0.02; done`
	cmd := exec.Command("sh", "-c", script)
	if err := configureProcessGroup(cmd); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := assignProcessTree(cmd); err != nil {
		t.Fatalf("assignProcessTree: %v", err)
	}
	waitForPath(t, started, 5*time.Second)

	if err := killProcessTree(cmd); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected the process to be killed even with SIGTERM trapped away, got a clean exit")
	}
}

// TestReleaseProcessTreeIsSafeWithoutAGroupAndIdempotent covers releaseProcessTree's documented
// safety: a no-op when nothing was ever recorded, and safe to call more than once for the same cmd.
func TestReleaseProcessTreeIsSafeWithoutAGroupAndIdempotent(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sleep", "60")

	// Must not panic when nothing was ever recorded.
	releaseProcessTree(cmd)

	if err := configureProcessGroup(cmd); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := assignProcessTree(cmd); err != nil {
		t.Fatalf("assignProcessTree: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	releaseProcessTree(cmd)
	releaseProcessTree(cmd) // Must not panic when called twice.

	if _, ok := lookupProcessGroup(cmd); ok {
		t.Error("expected the group state to be removed after release")
	}
}

// TestProcessTreeExitedWithoutALeadingGroup covers the two trivial branches: no group at all, and a
// recorded group that is not a leader. Both must report the tree as already gone, since neither case
// has anything group-wide left to confirm.
func TestProcessTreeExitedWithoutALeadingGroup(t *testing.T) {
	t.Parallel()

	unconfigured := exec.Command("sleep", "60")
	if !processTreeExited(unconfigured, time.Now()) {
		t.Error("expected true when no group was ever recorded")
	}

	notLeader := exec.Command("sleep", "60")
	if err := notLeader.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = notLeader.Process.Kill()
		_ = notLeader.Wait()
	})
	if err := assignProcessTree(notLeader); err != nil {
		t.Fatalf("assignProcessTree: %v", err)
	}
	if !processTreeExited(notLeader, time.Now()) {
		t.Error("expected true when the recorded group is not a leader")
	}
}

// TestProcessTreeExitedReportsFalsePastAnExpiredDeadline covers the timeout arm of the polling loop:
// a still-running, leading group with a deadline that has already passed must be reported as not
// exited.
func TestProcessTreeExitedReportsFalsePastAnExpiredDeadline(t *testing.T) {
	t.Parallel()

	cmd := startGroupLeader(t)

	if processTreeExited(cmd, time.Now().Add(-time.Second)) {
		t.Error("expected false for a still-running group past its deadline")
	}
}

// TestProcessTreeExitedPollsUntilTheGroupDrains exercises the loop's success path for real, rather
// than the degenerate case of a group that is already empty on the first check: the group is still
// occupied when processTreeExited is called and only drains, from a concurrent kill, partway through
// the deadline. A result of true here can only come from the loop actually re-checking.
//
// The goroutine calls cmd.Wait() itself, not just killProcessTree: kill(-pgid, 0) still succeeds for
// an unreaped zombie leader, so processTreeExited cannot see the group as empty until the direct
// child has actually been reaped, exactly as documented on processTreeExited.
func TestProcessTreeExitedPollsUntilTheGroupDrains(t *testing.T) {
	t.Parallel()

	cmd := startGroupLeader(t)

	const killAfter = 150 * time.Millisecond
	go func() {
		time.Sleep(killAfter)
		_ = killProcessTree(cmd)
		_ = cmd.Wait()
	}()

	start := time.Now()
	if !processTreeExited(cmd, start.Add(5*time.Second)) {
		t.Fatal("expected the group to be confirmed gone once the delayed kill lands")
	}
	if elapsed := time.Since(start); elapsed < killAfter/2 {
		t.Errorf("expected processTreeExited to poll rather than return immediately, took only %s", elapsed)
	}
}

// TestProcessTreeExitedPollsThenReportsFalseAtDeadline covers the loop's timeout path when it
// actually has to poll more than once: unlike the already-expired-deadline case above, the deadline
// here is still in the future on the first check, so a false result can only come from the loop
// re-checking, finding the group still occupied, and then running out of time on a later iteration.
func TestProcessTreeExitedPollsThenReportsFalseAtDeadline(t *testing.T) {
	t.Parallel()

	cmd := startGroupLeader(t)

	const budget = 80 * time.Millisecond
	start := time.Now()
	if processTreeExited(cmd, start.Add(budget)) {
		t.Fatal("expected false: the group is still running and nothing killed it before the deadline")
	}
	if elapsed := time.Since(start); elapsed < budget/2 {
		t.Errorf("expected processTreeExited to poll at least once before giving up, took only %s", elapsed)
	}
}

// TestIgnoreProcessGone pins the outcomes ignoreProcessGone must produce: syscall.ESRCH and
// os.ErrProcessDone are both absorbed, nil stays nil, and any other error passes through unchanged.
func TestIgnoreProcessGone(t *testing.T) {
	t.Parallel()

	if err := ignoreProcessGone(syscall.ESRCH); err != nil {
		t.Errorf("expected syscall.ESRCH to be absorbed, got %v", err)
	}
	if err := ignoreProcessGone(os.ErrProcessDone); err != nil {
		t.Errorf("expected os.ErrProcessDone to be absorbed, got %v", err)
	}
	if err := ignoreProcessGone(nil); err != nil {
		t.Errorf("expected nil to stay nil, got %v", err)
	}
	sentinel := errors.New("boom")
	if err := ignoreProcessGone(sentinel); !errors.Is(err, sentinel) {
		t.Errorf("expected an unrelated error to pass through unchanged, got %v", err)
	}
}
