//go:build windows

package applyinator

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// startTreeCommand starts "cmd /C ping ... 127.0.0.1" under a Job Object and assigns it. cmd.exe
// waits for ping.exe to finish, so the two stay alive together for the life of the command, giving
// the job-object tests a real two-process tree without relying on a script that could itself fork
// unpredictably. Loopback ping needs no network access and, unlike a console-based sleep, does not
// depend on whether a console is attached to the test process.
func startTreeCommand(t *testing.T) *exec.Cmd {
	t.Helper()

	cmd := exec.Command("cmd", "/C", "ping", "-n", "60", "127.0.0.1")
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
		// releaseProcessTree are both safe to call again on an already-terminated/released command.
		_ = killProcessTree(cmd)
		releaseProcessTree(cmd)
	})
	return cmd
}

// waitForActiveProcesses polls the job until it reports at least want active processes, so tests do
// not race cmd.exe spawning the ping.exe grandchild.
func waitForActiveProcesses(t *testing.T, job windows.Handle, want uint32, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		active, err := activeProcessesInJob(job)
		if err != nil {
			t.Fatalf("activeProcessesInJob: %v", err)
		}
		if active >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d active processes in the job, last saw %d", want, active)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestJobObjectBasicAccountingInformationMatchesWindowsLayout pins the struct's size and the offset
// of the one field actually read, ActiveProcesses. golang.org/x/sys/windows does not expose
// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, so this local copy must keep the exact field order and
// widths the kernel writes; a future edit that got that wrong would not fail to compile, it would
// silently read garbage into ActiveProcesses instead.
func TestJobObjectBasicAccountingInformationMatchesWindowsLayout(t *testing.T) {
	var info jobObjectBasicAccountingInformation

	if got, want := unsafe.Sizeof(info), uintptr(48); got != want {
		t.Errorf("unexpected struct size: got %d bytes, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(info.ActiveProcesses), uintptr(40); got != want {
		t.Errorf("unexpected ActiveProcesses offset: got %d, want %d", got, want)
	}
}

// TestConfigureProcessGroupCreatesAnUnassignedJob covers the state configureProcessGroup must leave
// behind before cmd.Start(): a real job, not yet carrying any process.
func TestConfigureProcessGroupCreatesAnUnassignedJob(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit", "0")
	if err := configureProcessGroup(cmd); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}
	t.Cleanup(func() { releaseProcessTree(cmd) })

	job, ok := lookupProcessJob(cmd)
	if !ok {
		t.Fatal("expected a job to be recorded before Start")
	}
	if job.assigned {
		t.Error("expected the job to be unassigned before assignProcessTree runs")
	}
	if job.handle == 0 {
		t.Error("expected a valid job handle")
	}
}

// TestAssignProcessTreeMarksTheJobAssigned covers the successful path: after a real cmd.Start(),
// assignProcessTree must both mark the job assigned and actually add the child to it.
func TestAssignProcessTreeMarksTheJobAssigned(t *testing.T) {
	cmd := startTreeCommand(t)

	job, ok := lookupProcessJob(cmd)
	if !ok || !job.assigned {
		t.Fatalf("expected the job to be recorded and assigned, got %+v (found=%v)", job, ok)
	}

	active, err := activeProcessesInJob(job.handle)
	if err != nil {
		t.Fatalf("activeProcessesInJob: %v", err)
	}
	if active == 0 {
		t.Error("expected the assigned child to be active in the job")
	}
}

// TestAssignProcessTreeIsANoOpWithoutConfigure covers the documented fallback: if
// configureProcessGroup was never called (or failed), assignProcessTree must not error and must
// leave no job state behind for cancellation to trip over.
func TestAssignProcessTreeIsANoOpWithoutConfigure(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	if err := assignProcessTree(cmd); err != nil {
		t.Fatalf("expected assignProcessTree to be a no-op without configureProcessGroup, got %v", err)
	}
	if _, ok := lookupProcessJob(cmd); ok {
		t.Error("expected no job to be recorded")
	}
}

// TestAssignProcessTreeReturnsAnErrorWhenTheProcessAlreadyExited covers the one error return
// assignProcessTree does not swallow: cmd.Process.WithHandle fails once cmd.Wait() has released the
// process, and that failure must propagate rather than being reported as a successful assignment.
func TestAssignProcessTreeReturnsAnErrorWhenTheProcessAlreadyExited(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// Configured after Wait() so the job exists (assignProcessTree's early return is not what is
	// under test here) but the process handle behind cmd.Process is already released.
	if err := configureProcessGroup(cmd); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}
	t.Cleanup(func() { releaseProcessTree(cmd) })

	if err := assignProcessTree(cmd); err == nil {
		t.Error("expected an error when assigning an already-released process handle to the job")
	}
	if job, ok := lookupProcessJob(cmd); !ok || job.assigned {
		t.Errorf("expected the job to remain unassigned after a failed assignment, got %+v (found=%v)", job, ok)
	}
}

// TestKillProcessTreeTerminatesTheWholeTree is the core Windows guarantee: killing the Job Object
// must remove every process in it, not just the direct child cmd.exe that execute started.
func TestKillProcessTreeTerminatesTheWholeTree(t *testing.T) {
	cmd := startTreeCommand(t)

	job, ok := lookupProcessJob(cmd)
	if !ok || !job.assigned {
		t.Fatalf("expected an assigned job, got %+v (found=%v)", job, ok)
	}
	// Wait for the ping.exe grandchild so the kill below actually exercises a multi-process job
	// rather than a job that happens to hold only the direct child.
	waitForActiveProcesses(t, job.handle, 2, 5*time.Second)

	if err := killProcessTree(cmd); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected the terminated command to report an error, got a clean exit")
	}
	if !processTreeExited(cmd, time.Now().Add(5*time.Second)) {
		t.Error("expected the whole job to be confirmed gone after killProcessTree")
	}
}

// TestTerminateProcessTreeKillsTheJobImmediately covers terminateProcessTree, which on Windows has
// no graceful signal to send and so must behave exactly like killProcessTree.
func TestTerminateProcessTreeKillsTheJobImmediately(t *testing.T) {
	cmd := startTreeCommand(t)

	if err := terminateProcessTree(cmd); err != nil {
		t.Fatalf("terminateProcessTree: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected the terminated command to report an error, got a clean exit")
	}
}

// TestKillProcessTreeFallsBackWhenTheJobWasNeverAssigned covers the accepted caveat on
// assignProcessTree: if the child was never added to the job, killProcessTree must still terminate
// it by falling back to the direct child rather than becoming a no-op.
//
// A single process with no children is used deliberately: falling back to a direct kill, instead of
// terminating the (empty) job, would otherwise leave a spawned grandchild running past the test.
func TestKillProcessTreeFallsBackWhenTheJobWasNeverAssigned(t *testing.T) {
	cmd := exec.Command("ping", "-n", "60", "127.0.0.1")
	if err := configureProcessGroup(cmd); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}
	t.Cleanup(func() { releaseProcessTree(cmd) })
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// assignProcessTree is deliberately not called, so the job stays empty.

	if err := killProcessTree(cmd); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected the killed process to report an error, got a clean exit")
	}
}

// TestKillProcessTreeIgnoresAnAlreadyExitedProcess covers ignoreProcessGone on the fallback path:
// the watchdog can race with an instruction that already exited on its own, and that must be
// reported as success rather than an error.
func TestKillProcessTreeIgnoresAnAlreadyExitedProcess(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// No job was ever configured, so killProcessTree falls back to cmd.Process.Kill(). On Windows,
	// cmd.Wait() marks the process released rather than done, so Kill() on an already-reaped process
	// returns syscall.EINVAL rather than os.ErrProcessDone; ignoreProcessGone must absorb that too.
	if err := killProcessTree(cmd); err != nil {
		t.Errorf("expected an already-exited process to be treated as success, got %v", err)
	}
}

// TestKillProcessTreeNoOpWhenNeverStarted covers the last fallback in killProcessTree: a command
// that has neither a job nor a Process (cmd.Start() was never called) must not panic and must report
// success, since there is nothing left to terminate.
func TestKillProcessTreeNoOpWhenNeverStarted(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit", "0")

	if err := killProcessTree(cmd); err != nil {
		t.Errorf("expected a no-op for a command that was never started, got %v", err)
	}
}

// TestReleaseProcessTreeNoOpWithoutAJob covers releaseProcessTree's other documented safety: it must
// do nothing, not panic, when configureProcessGroup was never called for cmd at all.
func TestReleaseProcessTreeNoOpWithoutAJob(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit", "0")

	releaseProcessTree(cmd)
}

// TestReleaseProcessTreeIsIdempotent covers releaseProcessTree's documented safety: calling it twice
// must not panic, and it must actually close the handle rather than merely forgetting it.
func TestReleaseProcessTreeIsIdempotent(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit", "0")
	if err := configureProcessGroup(cmd); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}
	job, ok := lookupProcessJob(cmd)
	if !ok {
		t.Fatal("expected a job to be recorded")
	}

	releaseProcessTree(cmd)
	releaseProcessTree(cmd) // Must not panic or double-close the handle.

	if _, ok := lookupProcessJob(cmd); ok {
		t.Error("expected the job state to be removed after release")
	}
	if _, err := activeProcessesInJob(job.handle); err == nil {
		t.Error("expected the job handle to be closed after release")
	}
}

// TestProcessTreeExitedWithoutAnAssignedJob covers the two trivial branches: no job at all, and a
// job that was created but never assigned. Both must report the tree as already gone, since neither
// case has anything job-wide left to confirm.
func TestProcessTreeExitedWithoutAnAssignedJob(t *testing.T) {
	unconfigured := exec.Command("cmd", "/C", "exit", "0")
	if !processTreeExited(unconfigured, time.Now()) {
		t.Error("expected true when no job was ever recorded")
	}

	unassigned := exec.Command("cmd", "/C", "exit", "0")
	if err := configureProcessGroup(unassigned); err != nil {
		t.Fatalf("configureProcessGroup: %v", err)
	}
	t.Cleanup(func() { releaseProcessTree(unassigned) })
	if !processTreeExited(unassigned, time.Now()) {
		t.Error("expected true when the job was never assigned")
	}
}

// TestProcessTreeExitedReportsFalsePastAnExpiredDeadline covers the timeout arm of the polling loop:
// a still-running, assigned job with a deadline that has already passed must be reported as not
// exited on the very first check, without waiting to poll again.
func TestProcessTreeExitedReportsFalsePastAnExpiredDeadline(t *testing.T) {
	cmd := startTreeCommand(t)

	if processTreeExited(cmd, time.Now().Add(-time.Second)) {
		t.Error("expected processTreeExited to report false for a still-running job past its deadline")
	}
}

// TestProcessTreeExitedReturnsFalseWhenTheJobCannotBeQueried covers the one branch of
// processTreeExited that no amount of starting and killing real processes can reach: a job handle
// QueryInformationJobObject fails on. That must be treated as "not confirmed exited" rather than as
// a false all-clear, since the caller reports TerminationIncomplete from this result.
//
// A freshly closed handle stands in for the query failing for any reason: it is guaranteed invalid,
// unlike a made-up handle value that might coincidentally collide with something else open in the
// process. cmd is never started; it exists only as the map key processJobs is keyed on.
func TestProcessTreeExitedReturnsFalseWhenTheJobCannotBeQueried(t *testing.T) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject: %v", err)
	}
	if err := windows.CloseHandle(job); err != nil {
		t.Fatalf("CloseHandle: %v", err)
	}

	cmd := exec.Command("cmd", "/C", "exit", "0")
	processJobs.Store(cmd, processJob{handle: job, assigned: true})
	t.Cleanup(func() { processJobs.Delete(cmd) })

	if processTreeExited(cmd, time.Now().Add(5*time.Second)) {
		t.Error("expected false when the job cannot be queried, not a false confirmation that it exited")
	}
}

// TestProcessTreeExitedPollsUntilTheJobDrains exercises the loop's success path for real, rather
// than the degenerate case of a job that is already empty on the first check: the job is still
// occupied when processTreeExited is called and only drains, from a concurrent kill, partway through
// the deadline. A result of true here can only come from the loop actually re-checking rather than
// deciding everything on its first look.
func TestProcessTreeExitedPollsUntilTheJobDrains(t *testing.T) {
	cmd := startTreeCommand(t)

	const killAfter = 150 * time.Millisecond
	go func() {
		time.Sleep(killAfter)
		_ = killProcessTree(cmd)
	}()

	start := time.Now()
	if !processTreeExited(cmd, start.Add(5*time.Second)) {
		t.Fatal("expected the job to be confirmed gone once the delayed kill lands")
	}
	if elapsed := time.Since(start); elapsed < killAfter/2 {
		t.Errorf("expected processTreeExited to poll rather than return immediately, took only %s", elapsed)
	}
}

// TestProcessTreeExitedPollsThenReportsFalseAtDeadline covers the loop's timeout path when it
// actually has to poll more than once: unlike the already-expired-deadline case above, the deadline
// here is still in the future on the first check, so a false result can only come from the loop
// re-checking, finding the job still occupied, and then running out of time on a later iteration.
func TestProcessTreeExitedPollsThenReportsFalseAtDeadline(t *testing.T) {
	cmd := startTreeCommand(t)

	const budget = 80 * time.Millisecond
	start := time.Now()
	if processTreeExited(cmd, start.Add(budget)) {
		t.Fatal("expected false: the job is still running and nothing killed it before the deadline")
	}
	if elapsed := time.Since(start); elapsed < budget/2 {
		t.Errorf("expected processTreeExited to poll at least once before giving up, took only %s", elapsed)
	}
}

// TestIgnoreProcessGone pins the outcomes ignoreProcessGone must produce: os.ErrProcessDone and the
// Windows-only syscall.EINVAL a Kill() issued after Wait() returns are both absorbed, nil stays nil,
// and any other error passes through unchanged.
func TestIgnoreProcessGone(t *testing.T) {
	if err := ignoreProcessGone(os.ErrProcessDone); err != nil {
		t.Errorf("expected os.ErrProcessDone to be absorbed, got %v", err)
	}
	if err := ignoreProcessGone(syscall.EINVAL); err != nil {
		t.Errorf("expected syscall.EINVAL to be absorbed, got %v", err)
	}
	if err := ignoreProcessGone(nil); err != nil {
		t.Errorf("expected nil to stay nil, got %v", err)
	}
	sentinel := errors.New("boom")
	if err := ignoreProcessGone(sentinel); !errors.Is(err, sentinel) {
		t.Errorf("expected an unrelated error to pass through unchanged, got %v", err)
	}
}
