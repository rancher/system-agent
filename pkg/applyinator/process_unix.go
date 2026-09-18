//go:build !windows

package applyinator

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// processGroup records the process group established for a command.
//
// The group is captured once, immediately after the command starts, rather than re-derived from the
// child's pid every time a signal is sent. Re-deriving it leaves the kill's aim at the mercy of how
// the kernel treats a zombie, in exactly the situation cancellation cares about: by the time the
// watchdog escalates to a kill, the direct child has usually already died from the graceful signal,
// but cmd.Wait() cannot have reaped it yet because it is blocked behind the output pipes that the
// surviving descendants still hold, so the group leader is a zombie.
//
// This is hardening rather than a fix for an observed production failure. Linux answers Getpgid for a
// zombie, so on the platform the agent is deployed to the re-derived id was correct. It is worth doing
// regardless: darwin returns ESRCH for the same call, which silently degrades the group-wide kill into
// a signal aimed at the already-dead direct child and lets the descendants that forced the escalation
// survive. Darwin is not a deployment target but it is where the cancellation tests are usually run,
// and a suite that cannot be believed there is worth as little as one that fails. Capturing the id is
// also cheaper than the syscall it replaces.
type processGroup struct {
	pgid int
	// isLeader records whether the child really became the leader of its own group. If it did not, it
	// shares the agent's process group and must only ever be signaled directly; see signalProcessTree.
	isLeader bool
}

// processGroups holds the process group captured for each running command, keyed by the *exec.Cmd it
// belongs to, for the interval between assignProcessTree and releaseProcessTree. *exec.Cmd has no
// place for this state, and the process-tree helpers must keep identical signatures on both platforms
// so execute remains platform-independent. This mirrors processJobs on Windows.
var processGroups sync.Map

// configureProcessGroup puts the command in its own process group so that all of its
// descendants can be signaled with a single call. It must be called before cmd.Start(),
// because SysProcAttr is only read when the process is forked.
//
// Signaling only the direct child is not enough. Plan instructions almost always invoke
// a run.sh script, which in turn runs an installer or package manager. Killing the shell
// can therefore leave the actual work running.
func configureProcessGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

// assignProcessTree records the process group the started child leads. It must be called after a
// successful cmd.Start().
//
// It deliberately does not consult syscall.Getpgid. Setpgid with a zero Pgid asks the kernel to place
// the child in a new group whose id equals its pid, and that is settled during the fork, so the
// SysProcAttr the child was started with is a more reliable source than a syscall against a pid that
// may already have become a zombie.
//
// It never fails: a child that did not become a group leader is recorded as such, and cancellation
// falls back to signaling the direct child.
func assignProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		// Start failed or was never called, so there is nothing to record.
		return nil
	}

	var group processGroup
	if isOwnProcessGroupLeader(cmd) {
		group = processGroup{pgid: cmd.Process.Pid, isLeader: true}
	}
	processGroups.Store(cmd, group)
	return nil
}

// isOwnProcessGroupLeader reports whether cmd was started in a process group of its own.
//
// DO NOT relax this check. It is the load-bearing guard against the agent killing itself. If
// configureProcessGroup did not take effect, the child inherits the daemon's process group, and
// kill(-pgid, SIGKILL) would then signal rancher-system-agent and every other process it started, so
// cancelling one plan could take down the node's agent. The final comparison against the agent's own
// group is redundant given the two checks above it and is kept as a cheap backstop, because the cost
// of being wrong here is unrecoverable while the cost of the check is a single syscall.
func isOwnProcessGroupLeader(cmd *exec.Cmd) bool {
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 {
		return false
	}
	return cmd.Process.Pid != syscall.Getpgrp()
}

// terminateProcessTree asks the command's whole process group to shut down cleanly.
func terminateProcessTree(cmd *exec.Cmd) error {
	return signalProcessTree(cmd, syscall.SIGTERM)
}

// killProcessTree kills the command's whole process group unconditionally.
func killProcessTree(cmd *exec.Cmd) error {
	return signalProcessTree(cmd, syscall.SIGKILL)
}

// releaseProcessTree discards the process group recorded for the command. It is safe when nothing was
// recorded and safe to call repeatedly.
func releaseProcessTree(cmd *exec.Cmd) {
	processGroups.Delete(cmd)
}

// signalProcessTree sends sig to the command's process group, falling back to the direct child
// if no process group was established for it.
//
// Deliberately not handled: Kill is a raw syscall, so unlike os.Process.Signal it provides no
// pid-reuse protection. The watchdog can be in this function while cmd.Wait() reaps the child on
// another goroutine; stop() closes done only afterward and then waits on <-finished. A pid could
// therefore be recycled in that window and receive the signal instead. Closing that race would
// require wrapping the entire pid-space operation in microseconds; the group-wide signaling that
// requires this raw syscall is considered worth the trade-off.
func signalProcessTree(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		// Start failed or was never called, so there is nothing to signal.
		return nil
	}

	group, ok := lookupProcessGroup(cmd)
	if !ok || !group.isLeader {
		return ignoreProcessGone(cmd.Process.Signal(sig))
	}
	return ignoreProcessGone(syscall.Kill(-group.pgid, sig))
}

// processTreeExited reports whether every process in the command's process group is gone, polling
// until deadline. It is only meaningful once cmd.Wait() has reaped the direct child: an unreaped
// zombie leader is still a member of its own group and would be indistinguishable from a descendant
// that survived.
//
// This confirms the process *group*, which is not the same as everything the instruction started. A
// descendant that called setsid or setpgid left the group and is invisible both to the signals sent
// to it and to this check. Confirming those would require a container-level mechanism such as a
// cgroup, which is out of scope here.
func processTreeExited(cmd *exec.Cmd, deadline time.Time) bool {
	group, ok := lookupProcessGroup(cmd)
	if !ok || !group.isLeader {
		// No process group was established, so only the direct child was ever signaled, and cmd.Wait()
		// has already reaped it. There is nothing group-wide left to confirm.
		return true
	}

	for {
		// Signal 0 runs the existence and permission checks without delivering anything, so ESRCH means
		// no process remains in the group. Any other outcome, including a permission error, is treated
		// as "something is still there" rather than assumed away.
		if errors.Is(syscall.Kill(-group.pgid, 0), syscall.ESRCH) {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		time.Sleep(min(remaining, processTreeExitPollInterval))
	}
}

func lookupProcessGroup(cmd *exec.Cmd) (processGroup, bool) {
	value, ok := processGroups.Load(cmd)
	if !ok {
		return processGroup{}, false
	}
	group, ok := value.(processGroup)
	return group, ok
}

// ignoreProcessGone treats "no such process" as success. The watchdog can race with the instruction
// exiting on its own, and an already-terminated process tree means the requested outcome has already
// been achieved rather than representing an error worth reporting.
func ignoreProcessGone(err error) error {
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
