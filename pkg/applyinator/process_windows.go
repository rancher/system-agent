//go:build windows

package applyinator

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
)

// processJob holds the per-command Windows state shared by the process-tree helpers.
type processJob struct {
	handle windows.Handle
	// assigned records whether the child was successfully added to the job. If assignment
	// failed, the job is empty, so killProcessTree must fall back to the direct child.
	assigned bool
}

// jobObjectBasicAccountingInformation mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
// golang.org/x/sys/windows exposes the information class but not the struct, and only ActiveProcesses
// is read here; the preceding fields exist so the layout matches what the kernel writes.
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// processJobs holds the Job Object created for each running command, keyed by the *exec.Cmd it
// belongs to. *exec.Cmd has no place for a platform-specific handle, and the process-tree helpers
// must keep identical signatures on both platforms so execute remains platform-independent. The
// state therefore lives here for the interval between configureProcessGroup and releaseProcessTree.
var processJobs sync.Map

// configureProcessGroup creates a Job Object for the command so that everything it spawns can be
// terminated in one call. It must be called before cmd.Start(), because the job must exist before
// the child can be assigned to it; assignProcessTree performs that assignment afterward.
//
// The job exists solely to give killProcessTree a handle through which TerminateJobObject can reach
// the entire process tree. It deliberately does not set JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
// That limit takes effect when the last handle is closed, and releaseProcessTree closes this handle
// at the end of every execute, including successful executions. Setting it would therefore cause
// successful Windows instructions to kill any background processes they leave behind, unlike their
// Unix counterparts. Orphan reaping on the success path is outside the scope of this change, and
// the resulting Windows-only behavior is not exercised by any test in this repository.
//
// Signaling only the direct child is not enough. Plan instructions almost always run a script that
// shells out to an installer or package manager, and killing the script can leave the actual work
// running.
func configureProcessGroup(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}

	processJobs.Store(cmd, processJob{handle: job})
	return nil
}

// assignProcessTree adds the started child to the Job Object created by configureProcessGroup.
// It must be called after a successful cmd.Start().
//
// Accepted caveat: os/exec exposes neither a pre-Start hook nor a handle to the child's primary
// thread, so the CREATE_SUSPENDED -> assign -> ResumeThread sequence needed to close this gap is
// unavailable. A descendant spawned between cmd.Start() and this call can therefore escape the
// job and survive cancellation. This is accepted and out of scope.
func assignProcessTree(cmd *exec.Cmd) error {
	job, ok := lookupProcessJob(cmd)
	if !ok || cmd.Process == nil {
		// configureProcessGroup failed, or Start produced no process. Either way, there is
		// nothing to assign, and cancellation falls back to the direct child.
		return nil
	}

	var assignErr error
	// WithHandle rather than OpenProcess(pid): the handle is guaranteed to refer to this process
	// for the duration of the callback, so it cannot race with pid reuse, and it requires no
	// additional access rights.
	if err := cmd.Process.WithHandle(func(handle uintptr) {
		assignErr = windows.AssignProcessToJobObject(job.handle, windows.Handle(handle))
	}); err != nil {
		return err
	}
	if assignErr != nil {
		return assignErr
	}

	// Replace the whole value rather than mutating it in place so sync.Map remains the only
	// synchronization this state requires.
	job.assigned = true
	processJobs.Store(cmd, job)
	return nil
}

// terminateProcessTree terminates the command's Job Object, as killProcessTree does.
//
// Accepted caveat: Windows has no SIGTERM, so there is no graceful signal to send to a process
// tree. Cancellation therefore terminates the tree immediately, with no grace period. This is the
// existing "Windows: direct kill" behavior, widened from the direct child to the entire tree.
// watchForTermination has a process-exit arm, so this immediate termination short-circuits the
// grace wait rather than leaving it stalled.
func terminateProcessTree(cmd *exec.Cmd) error {
	return killProcessTree(cmd)
}

// killProcessTree terminates every process in the command's Job Object.
func killProcessTree(cmd *exec.Cmd) error {
	if job, ok := lookupProcessJob(cmd); ok && job.assigned {
		return windows.TerminateJobObject(job.handle, 1)
	}

	// There is no assigned job, so terminating it would do nothing. Fall back to the direct child
	// so cancellation still terminates the command rather than becoming a no-op.
	if cmd.Process == nil {
		return nil
	}
	return ignoreProcessGone(cmd.Process.Kill())
}

// releaseProcessTree closes the command's Job Object handle and removes its state. It is safe when
// no job was recorded and safe to call repeatedly: LoadAndDelete ensures the handle is closed at
// most once.
func releaseProcessTree(cmd *exec.Cmd) {
	value, ok := processJobs.LoadAndDelete(cmd)
	if !ok {
		return
	}
	job, ok := value.(processJob)
	if !ok {
		return
	}
	if err := windows.CloseHandle(job.handle); err != nil {
		logrus.Warnf("[applyinator] error closing the job object handle: %v", err)
	}
}

// processTreeExited reports whether every process in the command's Job Object is gone, polling until
// deadline. It is only meaningful once cmd.Wait() has reaped the direct child, which is still counted
// as an active process until then.
//
// A descendant that escaped the job between cmd.Start() and assignProcessTree, the caveat documented
// on assignProcessTree, is invisible here for the same reason it is invisible to the terminate call.
func processTreeExited(cmd *exec.Cmd, deadline time.Time) bool {
	job, ok := lookupProcessJob(cmd)
	if !ok || !job.assigned {
		// There is no job to inspect, so only the direct child was ever terminated and cmd.Wait() has
		// already reaped it. There is nothing job-wide left to confirm.
		return true
	}

	for {
		active, err := activeProcessesInJob(job.handle)
		if err != nil {
			// Nothing can be confirmed either way. Report the tree as not exited so the caller records
			// TerminationIncomplete: erring towards a false alarm is safer than silently retracting a
			// warning about a process tree whose state is actually unknown.
			logrus.Warnf("[applyinator] unable to query the job object for surviving processes: %v", err)
			return false
		}
		if active == 0 {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		time.Sleep(min(remaining, processTreeExitPollInterval))
	}
}

// activeProcessesInJob returns the number of processes still running in the job.
func activeProcessesInJob(job windows.Handle) (uint32, error) {
	var info jobObjectBasicAccountingInformation
	err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	if err != nil {
		return 0, err
	}
	return info.ActiveProcesses, nil
}

func lookupProcessJob(cmd *exec.Cmd) (processJob, bool) {
	value, ok := processJobs.Load(cmd)
	if !ok {
		return processJob{}, false
	}
	job, ok := value.(processJob)
	return job, ok
}

// ignoreProcessGone treats an already-terminated process as success. The watchdog can race with
// the instruction exiting on its own, and an already-gone process tree means cancellation has
// achieved its intended outcome rather than producing an error worth reporting.
func ignoreProcessGone(err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
