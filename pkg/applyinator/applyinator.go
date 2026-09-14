package applyinator

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/system-agent/pkg/image"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// Applyinator coordinates plan application and execution.
// It holds configuration and resources used during an apply.
type Applyinator struct {
	mu              *sync.Mutex
	workDir         string
	preserveWorkDir bool
	appliedPlanDir  string
	interlockDir    string
	imageUtil       *image.Utility
}

// CalculatedPlan holds a Plan and its checksum, and is passed into Applyinator.
type CalculatedPlan struct {
	Plan     planapi.Plan
	Checksum string
}

const appliedPlanFileSuffix = "-applied.plan"
const applyinatorDateCodeLayout = "20060102-150405"
const defaultCommand = "/run.sh"
const cattleAgentExecutionPwdEnvKey = "CATTLE_AGENT_EXECUTION_PWD"
const cattleAgentAttemptKey = "CATTLE_AGENT_ATTEMPT_NUMBER"
const planRetentionPolicyCount = 64
const restartPendingInterlockFile = "restart-pending"
const applyinatorActiveInterlockFile = "applyinator-active"
const restartPendingTimeout = 5 * time.Minute // Wait a maximum of 5 minutes before force-applying a plan if a restart is pending.
const deleteFileAction = "delete"

const defaultEffectivePeriod = 600 // 10 minutes
const defaultFailureCooldown = 6

// instructionTerminationGrace is the time allowed for a canceled instruction's process tree to
// exit after a graceful termination signal before it is forcefully killed.
//
// It is a variable rather than a constant so tests can shorten the interval and exercise the
// escalation path without waiting the full default duration.
var instructionTerminationGrace = 10 * time.Second

// processTreeExitTimeout bounds the final confirmation that a terminated instruction's process tree
// is really gone. It is spent only after the tree has already been signaled and killed, so reaching
// it means the processes are not responding to SIGKILL rather than that they need more time.
//
// A variable for the same reason as instructionTerminationGrace.
var processTreeExitTimeout = 5 * time.Second

// processTreeExitPollInterval is how often processTreeExited re-checks whether a signaled process
// tree has gone away.
var processTreeExitPollInterval = 50 * time.Millisecond

func NewApplyinator(workDir string, preserveWorkDir bool, appliedPlanDir, interlockDir string, imageUtil *image.Utility) *Applyinator {
	return &Applyinator{
		mu:              &sync.Mutex{},
		workDir:         workDir,
		preserveWorkDir: preserveWorkDir,
		appliedPlanDir:  appliedPlanDir,
		interlockDir:    interlockDir,
		imageUtil:       imageUtil,
	}
}

func CalculatePlan(rawPlan []byte) (CalculatedPlan, error) {
	p, err := planapi.Parse(rawPlan)
	if err != nil {
		return CalculatedPlan{}, err
	}
	return CalculatedPlan{
		Plan:     p,
		Checksum: planapi.Checksum(rawPlan),
	}, nil
}

// Interruption reports why an apply operation stopped before completing normally.
type Interruption string

const (
	// InterruptionNone means the apply was not interrupted.
	InterruptionNone Interruption = ""
	// InterruptionPaused means the apply stopped at an instruction boundary and may be resumed.
	InterruptionPaused Interruption = "paused"
	// InterruptionCanceled means the apply was abandoned; the in-flight instruction was signaled.
	InterruptionCanceled Interruption = "canceled"
)

// checkInterruption reports any interruption that is already pending. It never blocks, and a nil
// channel is never ready. Cancellation takes precedence over pause.
func checkInterruption(cancel, pause <-chan struct{}) Interruption {
	// When both channels are ready, select chooses between them pseudo-randomly, so cancellation
	// must be checked separately to guarantee its precedence over pause.
	select {
	case <-cancel:
		return InterruptionCanceled
	default:
	}

	select {
	case <-pause:
		return InterruptionPaused
	default:
	}

	return InterruptionNone
}

type ApplyOutput struct {
	OneTimeOutput          []byte
	OneTimeApplySucceeded  bool
	PeriodicOutput         []byte
	PeriodicApplySucceeded bool
	// Interruption is InterruptionNone unless the apply stopped early.
	Interruption Interruption
	// CompletedOneTimeInstructions is the absolute number of one-time instructions completed
	// across the entire plan, not the number completed by this apply. This allows successive
	// pause/resume cycles to preserve and build on the same checkpoint.
	CompletedOneTimeInstructions int
	// TerminationIncomplete reports that an instruction was terminated because the apply was
	// interrupted, but processes from its process tree were still running once the agent gave up on
	// them. The apply is over either way; what this says is that the node is not necessarily quiescent,
	// so work started by the abandoned plan may still be mutating it.
	//
	// It can only be a lower bound. Descendants that left the instruction's process group cannot be
	// signaled or observed, so false means "nothing unterminated was detected" rather than "nothing
	// survived".
	TerminationIncomplete bool
}

type ApplyInput struct {
	CalculatedPlan             CalculatedPlan
	RunOneTimeInstructions     bool
	OneTimeInstructionAttempts int
	ReconcileFiles             bool
	ExistingOneTimeOutput      []byte
	ExistingPeriodicOutput     []byte
	// Cancel, when closed, cancels the in-flight instruction's context and prevents any further
	// instruction from starting. A nil channel is never ready, so the zero value means "never canceled".
	Cancel <-chan struct{}
	// Pause, when closed, stops the apply at the next instruction boundary. It never interrupts a
	// running instruction, ensuring that every instruction counted by ApplyOutput.CompletedOneTimeInstructions
	// has completed successfully. A nil channel is never ready.
	Pause <-chan struct{}
	// ResumeFromOneTimeInstruction is the index of the first one-time instruction to execute.
	// Instructions before this index are treated as already completed and are not re-run. Zero,
	// the zero value, starts execution from the beginning.
	ResumeFromOneTimeInstruction int
}

// Apply reconciles the local system to input.CalculatedPlan.
// It honors the interlock and archives the plan.
// It reconciles files and optionally runs one-time instructions.
// It runs due periodic instructions.
// It returns gzip+JSON encoded one-time and periodic outputs and their success flags.
// ApplyOutput.OneTimeApplySucceeded is false when RunOneTimeInstructions is false.
//
// Stop Apply with input.Cancel or input.Pause.
// Cancel stops the context of the running instruction and prevent all further instructions.
// Pause lets the current instruction finish and prevent the next instruction.
func (a *Applyinator) Apply(ctx context.Context, input ApplyInput) (ApplyOutput, error) {
	logrus.Debugf("[applyinator] applying plan with checksum %s", input.CalculatedPlan.Checksum)
	output := ApplyOutput{
		OneTimeOutput:                input.ExistingOneTimeOutput,
		PeriodicOutput:               input.ExistingPeriodicOutput,
		CompletedOneTimeInstructions: input.ResumeFromOneTimeInstruction,
	}
	if interruption := checkInterruption(input.Cancel, input.Pause); interruption != InterruptionNone {
		logrus.Infof("[applyinator] not applying plan with checksum %s: %s before the apply started", input.CalculatedPlan.Checksum, interruption)
		output.Interruption = interruption
		return output, nil
	}

	logrus.Tracef("[applyinator] applying plan - attempting to get lock")
	a.mu.Lock()
	logrus.Tracef("[applyinator] applying plan - lock achieved")
	defer a.mu.Unlock()

	// Recheck: the check above can pass and then this call can block behind another local/remote
	// apply holding the lock. An interruption requested while queued behind that lock would
	// otherwise go unnoticed until the first instruction boundary, letting the archive and file
	// reconciliation below run despite an already-pending cancel or pause.
	if interruption := checkInterruption(input.Cancel, input.Pause); interruption != InterruptionNone {
		logrus.Infof("[applyinator] not applying plan with checksum %s: %s while waiting for the lock", input.CalculatedPlan.Checksum, interruption)
		output.Interruption = interruption
		return output, nil
	}

	now := time.Now()
	nowString := now.Format(applyinatorDateCodeLayout)

	cleanupInterlock, err := a.checkInterlock(now)
	if err != nil {
		return output, err
	}
	defer cleanupInterlock()

	// execCtx is passed to instruction execution and is canceled when input.Cancel closes, allowing
	// cancellation to interrupt a running instruction rather than waiting for the next boundary.
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	if input.Cancel != nil {
		go func() {
			select {
			case <-input.Cancel:
				cancelExec()
			case <-execCtx.Done():
				// Apply returned (or ctx was canceled); exit so this goroutine cannot leak.
			}
		}()
	}

	executionDir := filepath.Join(a.workDir, nowString)
	logrus.Tracef("[applyinator] applying calculated node plan contents %v", input.CalculatedPlan.Checksum)
	logrus.Tracef("[applyinator] using %s as execution directory", executionDir)
	if a.appliedPlanDir != "" {
		logrus.Debugf("[applyinator] writing applied calculated plan contents to historical plan directory %s", a.appliedPlanDir)
		if err := os.MkdirAll(a.appliedPlanDir, 0700); err != nil {
			logrus.Errorf("[applyinator] error creating applied plan directory: %v", err)
		}
		if err := a.writePlanToDisk(now, &input.CalculatedPlan); err != nil {
			logrus.Errorf("[applyinator] error writing applied plan to disk: %v", err)
		}
		if err := a.appliedPlanRetentionPolicy(planRetentionPolicyCount); err != nil {
			logrus.Errorf("[applyinator] error while applying plan retention policy: %v", err)
		}
	}

	if input.ReconcileFiles {
		if err := reconcileFiles(input.CalculatedPlan.Plan.Files); err != nil {
			return output, err
		}
	}

	if !a.preserveWorkDir {
		logrus.Debugf("[applyinator] cleaning working directory before applying %s", a.workDir)
		if err := os.RemoveAll(a.workDir); err != nil {
			return output, err
		}
	}

	if input.RunOneTimeInstructions {
		oneTime, err := a.runOneTimeInstructions(execCtx, executionDir, input.CalculatedPlan, input.ExistingOneTimeOutput, input.OneTimeInstructionAttempts, input.ResumeFromOneTimeInstruction, input.Cancel, input.Pause)
		if err != nil {
			return output, err
		}
		output.OneTimeOutput = oneTime.Output
		output.OneTimeApplySucceeded = oneTime.Succeeded
		output.CompletedOneTimeInstructions = oneTime.Completed
		output.TerminationIncomplete = oneTime.TerminationIncomplete
		if oneTime.Interruption != InterruptionNone {
			// An interrupt suppresses periodic instructions as well. Once the operator has stopped the apply,
			// no additional work should start after the one-time instructions are abandoned.
			output.Interruption = oneTime.Interruption
			return output, nil
		}
	}

	// Reaching this point with RunOneTimeInstructions means oneTime.Interruption was InterruptionNone
	// (an interrupted one-time pass already returned above), so OneTimeApplySucceeded can only be false here
	// because of a genuine failure. Periodic instructions still run regardless, but the outcome below must not
	// let an interruption observed during that periodic pass retroactively relabel the failure.
	oneTimeGenuinelyFailed := input.RunOneTimeInstructions && !output.OneTimeApplySucceeded

	periodic, err := a.runPeriodicInstructions(execCtx, executionDir, input.CalculatedPlan, input.ExistingPeriodicOutput, input.RunOneTimeInstructions, now, input.Cancel, input.Pause)
	if err != nil {
		return output, err
	}
	output.PeriodicOutput = periodic.Output
	output.PeriodicApplySucceeded = periodic.Succeeded
	// Never clear a report the one-time pass already made: the two passes terminate different
	// instructions, and only one of them needs to have left something behind.
	output.TerminationIncomplete = output.TerminationIncomplete || periodic.TerminationIncomplete
	// Periodic instructions have no checkpoint, so their interruption is only observable here. Skipped
	// when the one-time pass genuinely failed: that failure is already authoritative, and a cancel or
	// pause merely coinciding with the periodic pass that follows it must not report an interruption
	// instead of the failure that actually stopped the plan.
	if output.Interruption == InterruptionNone && !oneTimeGenuinelyFailed {
		output.Interruption = checkInterruption(input.Cancel, input.Pause)
	}

	return output, nil
}

// parseUnixTimeOrZero parses s using time.UnixDate.
// It returns ok false when s is empty or unparsable.
// Callers treat that as no recorded time, not an error.
func parseUnixTimeOrZero(label, s string) (t time.Time, ok bool) {
	if s == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.UnixDate, s)
	if err != nil {
		logrus.Errorf("[applyinator] error parsing %s %q: %v", label, s, err)
		return time.Time{}, false
	}
	return parsed, true
}

// decodeGzipJSON gunzips data and unmarshals into out.
// It returns nil when data is empty.
func decodeGzipJSON(data []byte, out any) error {
	if len(data) == 0 {
		return nil
	}
	objectBuffer, err := generateByteBufferFromBytes(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(objectBuffer.Bytes(), out)
}

// encodeGzipJSON marshals v to JSON and gzips the result.
func encodeGzipJSON(v any) ([]byte, error) {
	marshalled, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return gzipByteSlice(marshalled)
}

// periodicInstructionDue determines if a periodic instruction should run now.
// It uses the previously recorded output for the instruction.
// It treats an unset or unparsable last-successful timestamp as no history (always due).
// When forced is true, bypass the period and the failure cooldown.
func periodicInstructionDue(now time.Time, prev planapi.PeriodicInstructionOutput, periodSeconds int, forced bool) (due bool, failures int) {
	if t, ok := parseUnixTimeOrZero("last successful run time", prev.LastSuccessfulRunTime); ok {
		effectivePeriod := periodSeconds
		if effectivePeriod == 0 {
			effectivePeriod = defaultEffectivePeriod
		}
		if now.Before(t.Add(time.Second*time.Duration(effectivePeriod))) && !forced {
			logrus.Debugf("[applyinator] not running periodic instruction as period duration has not elapsed since last successful run")
			return false, failures
		}
	}

	if prev.LastFailedRunTime != "" {
		if t, ok := parseUnixTimeOrZero("last failed run time", prev.LastFailedRunTime); ok {
			failures = prev.Failures
			failureCooldown := failures
			if failureCooldown > defaultFailureCooldown {
				failureCooldown = defaultFailureCooldown
			} else if failureCooldown == 0 {
				failureCooldown = 1
			}
			if now.Before(t.Add(time.Second*time.Duration(30*failureCooldown))) && !forced {
				logrus.Debugf("[applyinator] not running periodic instruction as failure cooldown has not elapsed since last failed run")
				return false, failures
			}
		}
	}

	return true, failures
}

// reconcileFiles applies a plan's Files.
// It writes regular files, creates directories, and deletes marked paths.
func reconcileFiles(files []planapi.File) error {
	for _, file := range files {
		if file.Action == deleteFileAction {
			if err := removeFile(file); err != nil {
				return err
			}
		} else if file.Directory {
			logrus.Debugf("[applyinator] creating directory %s", file.Path)
			if err := createDirectory(file); err != nil {
				return err
			}
		} else {
			logrus.Debugf("[applyinator] writing file %s", file.Path)
			if err := writeBase64ContentToFile(file); err != nil {
				return err
			}
		}
	}
	return nil
}

// instructionExecutionDir returns the per-instruction execution directory and log prefix.
// The values derive from the plan checksum and the instruction index.
func instructionExecutionDir(baseDir, checksum string, index int) (dir, prefix string) {
	prefix = checksum + "_" + strconv.Itoa(index)
	return filepath.Join(baseDir, prefix), prefix
}

// oneTimeResult is the outcome of one pass over a plan's one-time instructions.
type oneTimeResult struct {
	Output    []byte
	Succeeded bool
	// Completed is the absolute number of one-time instructions completed so far: the index of
	// the last instruction that returned, plus one.
	Completed int
	// Interruption is InterruptionNone unless execution stopped before all instructions completed.
	Interruption Interruption
	// TerminationIncomplete mirrors ApplyOutput.TerminationIncomplete for this pass.
	TerminationIncomplete bool
}

// periodicResult is the outcome of one pass over a plan's periodic instructions.
type periodicResult struct {
	Output    []byte
	Succeeded bool
	// TerminationIncomplete mirrors ApplyOutput.TerminationIncomplete for this pass.
	TerminationIncomplete bool
}

// runOneTimeInstructions executes one-time instructions in order, starting at resumeFrom. It stops
// at the first failure or when cancel or pause is pending at an instruction boundary, and returns
// the updated gzip+JSON saved-output map together with the resume checkpoint. Instructions before
// resumeFrom are treated as already completed and are not re-run.
func (a *Applyinator) runOneTimeInstructions(ctx context.Context, executionDir string, cp CalculatedPlan, existingOutput []byte,
	attempts, resumeFrom int, cancel, pause <-chan struct{}) (oneTimeResult, error) {
	logrus.Infof("[applyinator] applying one-time instructions for plan with checksum %s starting at instruction %d", cp.Checksum, resumeFrom)
	executionOutputs := map[string][]byte{}
	if err := decodeGzipJSON(existingOutput, &executionOutputs); err != nil {
		return oneTimeResult{}, err
	}

	if resumeFrom < 0 {
		logrus.Warnf("[applyinator] negative resume index %d for plan %s, starting from the first instruction", resumeFrom, cp.Checksum)
		resumeFrom = 0
	}

	result := oneTimeResult{Succeeded: true, Completed: min(resumeFrom, len(cp.Plan.OneTimeInstructions))}
	for index := resumeFrom; index < len(cp.Plan.OneTimeInstructions); index++ {
		if interruption := checkInterruption(cancel, pause); interruption != InterruptionNone {
			logrus.Infof("[applyinator] plan %s %s before instruction %d; not executing it", cp.Checksum, interruption, index)
			result.Interruption = interruption
			break
		}

		instruction := cp.Plan.OneTimeInstructions[index]
		logrus.Debugf("[applyinator] executing instruction %d attempt %d for plan %s", index, attempts, cp.Checksum)
		instructionDir, prefix := instructionExecutionDir(executionDir, cp.Checksum, index)
		executed, err := a.execute(ctx, prefix, instructionDir, instruction.CommonInstruction, true, attempts)
		if executed.TerminationIncomplete {
			result.TerminationIncomplete = true
		}
		failed := err != nil || executed.ExitCode != 0
		if failed {
			logrus.Errorf("[applyinator] error executing instruction %d: %v", index, err)
			result.Succeeded = false
		}
		// Save output even when the instruction fails or is killed, so any output it produced is preserved.
		if instruction.Name == "" && instruction.SaveOutput {
			logrus.Errorf("[applyinator] instruction does not have a name set, cannot save output data")
		} else if instruction.SaveOutput {
			executionOutputs[instruction.Name] = executed.Stdout
		}
		// Stop after the first failed instruction; subsequent instructions must not execute.
		if failed {
			// A canceled instruction may fail because its context was killed. Re-check for a
			// cancellation specifically, so it is reported as an interruption rather than a plan
			// failure. Pause is deliberately excluded from this check: it never interrupts a running
			// instruction, so a failure observed with only pause pending is still a genuine
			// instruction failure. Passing a nil pause channel here, rather than reusing
			// checkInterruption(cancel, pause), is what keeps that failure from being reported as
			// InterruptionPaused merely because the operator happened to pause around the same time.
			result.Interruption = checkInterruption(cancel, nil)
			// The failed instruction did not complete, so do not advance the checkpoint.
			break
		}
		result.Completed = index + 1
	}

	output, err := encodeGzipJSON(executionOutputs)
	if err != nil {
		return oneTimeResult{}, err
	}
	result.Output = output
	return result, nil
}

// runPeriodicInstructions executes each due periodic instruction.
// It returns the updated gzip+JSON encoded periodic-output map and a success flag.
// Set ranOneTime to force every instruction to run regardless of period and cooldown.
//
// Periodic instructions have no resume checkpoint. If cancel or pause becomes pending, execution
// stops before the next instruction, and the caller re-checks the interruption channels to determine
// whether the apply was interrupted.
func (a *Applyinator) runPeriodicInstructions(ctx context.Context, executionDir string, cp CalculatedPlan, existingOutput []byte,
	ranOneTime bool, now time.Time, cancel, pause <-chan struct{}) (periodicResult, error) {
	nowUnixTimeString := now.Format(time.UnixDate)

	periodicOutputs := map[string]planapi.PeriodicInstructionOutput{}
	if err := decodeGzipJSON(existingOutput, &periodicOutputs); err != nil {
		return periodicResult{}, err
	}

	result := periodicResult{Succeeded: true}
	for index, instruction := range cp.Plan.PeriodicInstructions {
		if interruption := checkInterruption(cancel, pause); interruption != InterruptionNone {
			logrus.Infof("[applyinator] plan %s %s before periodic instruction %d; not executing it", cp.Checksum, interruption, index)
			break
		}

		if instruction.Name == "" {
			logrus.Errorf("[applyinator] periodic instruction %d did not have name, unable to run", index)
			continue
		}

		prev := periodicOutputs[instruction.Name]
		due, failures := periodicInstructionDue(now, prev, instruction.PeriodSeconds, ranOneTime)
		if !due {
			logrus.Debugf("[applyinator] not running periodic instruction %s; not yet due", instruction.Name)
			continue
		}

		previousRunTime := ""
		if _, ok := parseUnixTimeOrZero("last successful run time", prev.LastSuccessfulRunTime); ok {
			previousRunTime = prev.LastSuccessfulRunTime
		}

		logrus.Debugf("[applyinator] executing periodic instruction %d for plan %s", index, cp.Checksum)
		instructionDir, prefix := instructionExecutionDir(executionDir, cp.Checksum, index)
		executed, err := a.execute(ctx, prefix, instructionDir, instruction.CommonInstruction, false, failures+1)
		if executed.TerminationIncomplete {
			result.TerminationIncomplete = true
		}
		if err != nil || executed.ExitCode != 0 {
			result.Succeeded = false
		}

		lsrt := nowUnixTimeString
		lastFailureTime := ""
		if executed.ExitCode != 0 {
			lsrt = previousRunTime
			lastFailureTime = nowUnixTimeString
			failures++
		} else {
			// reset last failure time and failure count
			failures = 0
		}
		stderr := executed.Stderr
		if !instruction.SaveStderrOutput {
			stderr = []byte{}
		}
		periodicOutputs[instruction.Name] = planapi.PeriodicInstructionOutput{
			Name:                  instruction.Name,
			Stdout:                executed.Stdout,
			Stderr:                stderr,
			ExitCode:              executed.ExitCode,
			LastSuccessfulRunTime: lsrt,
			LastFailedRunTime:     lastFailureTime,
			Failures:              failures,
		}
		if !result.Succeeded {
			break
		}
	}

	output, err := encodeGzipJSON(periodicOutputs)
	if err != nil {
		return periodicResult{}, err
	}
	result.Output = output
	return result, nil
}

// checkInterlock enforces the interlock directory protocol used by install.sh during agent upgrade.
// A restart-pending file blocks applies for restartPendingTimeout, then it is removed and ignored.
// On success return a cleanup func. The caller must defer that func to remove applyinator-active file.
func (a *Applyinator) checkInterlock(now time.Time) (func(), error) {
	noop := func() {}
	if a.interlockDir == "" {
		return noop, nil
	}

	nowUnixTimeString := now.Format(time.UnixDate)
	restartPendingInterlockFilePath := filepath.Join(a.interlockDir, restartPendingInterlockFile)
	applyinatorActiveInterlockFilePath := filepath.Join(a.interlockDir, applyinatorActiveInterlockFile)

	// First off, remove check and remove the active interlock as the applyinator is not actually active
	if _, err := os.Stat(applyinatorActiveInterlockFilePath); err == nil {
		if err := os.Remove(applyinatorActiveInterlockFilePath); err != nil {
			logrus.Errorf("[applyinator] unable to remove applyinator active interlock file %s: %v", applyinatorActiveInterlockFilePath, err)
		}
	}

	if _, err := os.Stat(restartPendingInterlockFilePath); err == nil {
		// check the restart pending interlock file to see if we've passed our threshold for blocking
		fileContents, err := os.ReadFile(restartPendingInterlockFilePath)
		if err != nil {
			return noop, fmt.Errorf("unable to read restart pending interlock file %s: %w", restartPendingInterlockFilePath, err)
		}
		// Parse the time out of the file and determine if we have passed our time threshold
		t, err := time.Parse(time.UnixDate, string(fileContents))
		if err != nil {
			// If we are unable to parse the first observed time out of the file, write "now" as the first observed time of the file.
			if err := os.WriteFile(restartPendingInterlockFilePath, []byte(nowUnixTimeString), 0600); err != nil {
				return noop, fmt.Errorf("unable to write first-observed time to restart pending interlock file %s: %w", restartPendingInterlockFilePath, err)
			}
			return noop, fmt.Errorf("restart is pending for system-agent, waiting %s until ignoring pending restart", restartPendingTimeout.String())
		}
		if now.Before(t.Add(restartPendingTimeout)) {
			return noop, fmt.Errorf("restart is pending for system-agent, waiting %s until ignoring pending restart", t.Add(restartPendingTimeout).Sub(now).String())
		}
		// remove the restart pending file
		if err := os.Remove(restartPendingInterlockFilePath); err != nil {
			logrus.Errorf("[applyinator] error encountered while removing restart pending interlock file %s: %v", restartPendingInterlockFilePath, err)
		}
	}

	// At this point, there is no restart-pending and we can continue with applyinator reconciliation, so create the applyinator-active file
	if err := os.WriteFile(applyinatorActiveInterlockFilePath, []byte(nowUnixTimeString), 0600); err != nil {
		logrus.Errorf("[applyinator] unable to write applyinator active interlock file %s: %v", applyinatorActiveInterlockFilePath, err)
	}

	return func() {
		// Remove the Applyinator Active Interlock File
		if err := os.Remove(applyinatorActiveInterlockFilePath); err != nil {
			logrus.Errorf("[applyinator] unable to remove applyinator active interlock file %s: %v", applyinatorActiveInterlockFilePath, err)
		}
	}, nil
}

func gzipByteSlice(input []byte) ([]byte, error) {
	var gzOutput bytes.Buffer

	gzWriter := gzip.NewWriter(&gzOutput)

	if _, err := gzWriter.Write(input); err != nil {
		logrus.Errorf("[applyinator] error writing gzipped byte slice: %v", err)
	}

	if err := gzWriter.Close(); err != nil {
		return []byte{}, err
	}
	return gzOutput.Bytes(), nil
}

func generateByteBufferFromBytes(input []byte) (*bytes.Buffer, error) {
	buffer := bytes.NewBuffer(input)
	gzReader, err := gzip.NewReader(buffer)
	if err != nil {
		return nil, err
	}
	defer gzReader.Close()

	var objectBuffer bytes.Buffer
	_, err = io.Copy(&objectBuffer, gzReader)
	if err != nil {
		return nil, err
	}
	return &objectBuffer, nil
}

func (a *Applyinator) appliedPlanRetentionPolicy(retention int) error {
	planFiles, err := a.getAppliedPlanFiles()
	if err != nil {
		return err
	}

	if len(planFiles) <= retention {
		return nil
	}

	sort.Slice(planFiles, func(i, j int) bool {
		return planFiles[i].Name() < planFiles[j].Name()
	})

	delCount := len(planFiles) - retention
	for _, df := range planFiles[:delCount] {
		historicalPlanFile := filepath.Join(a.appliedPlanDir, df.Name())
		logrus.Infof("[applyinator] removing historical applied plan (retention policy count: %d) %s", retention, historicalPlanFile)
		if err := os.Remove(historicalPlanFile); err != nil {
			return err
		}
	}
	return nil
}

func (a *Applyinator) getAppliedPlanFiles() ([]os.DirEntry, error) {
	var planFiles []os.DirEntry
	dirListedPlanFiles, err := os.ReadDir(a.appliedPlanDir)
	if err != nil {
		return nil, err
	}

	for _, f := range dirListedPlanFiles {
		if strings.HasSuffix(f.Name(), appliedPlanFileSuffix) && !f.IsDir() {
			planFiles = append(planFiles, f)
		}
	}
	return planFiles, nil
}

func (a *Applyinator) writePlanToDisk(now time.Time, plan *CalculatedPlan) error {
	planFiles, err := a.getAppliedPlanFiles()
	if err != nil {
		return err
	}

	file := now.Format(applyinatorDateCodeLayout) + appliedPlanFileSuffix
	anpString, err := json.Marshal(plan)
	if err != nil {
		return err
	}

	if len(planFiles) != 0 {
		sort.Slice(planFiles, func(i, j int) bool {
			return planFiles[i].Name() > planFiles[j].Name()
		})
		existingFileContent, err := os.ReadFile(filepath.Join(a.appliedPlanDir, planFiles[0].Name()))
		if err != nil {
			return err
		}
		if bytes.Equal(existingFileContent, anpString) {
			logrus.Debugf("[applyinator] not writing applied plan to file %s as the last file written (%s) had identical contents", file, planFiles[0].Name())
			return nil
		}
	}

	return writeContentToFile(filepath.Join(a.appliedPlanDir, file), os.Getuid(), os.Getgid(), 0600, anpString)
}

// executeResult is the outcome of running a single instruction's command.
type executeResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// TerminationIncomplete mirrors ApplyOutput.TerminationIncomplete for this instruction.
	TerminationIncomplete bool
}

// execute stages the instruction's execution directory, runs its command, and returns an
// executeResult alongside any wait error.
//
// The command runs in its own process group on Unix or Job Object on Windows. A watchdog monitors
// ctx and, when canceled, signals the entire process tree to terminate. If the tree does not exit
// within instructionTerminationGrace, it is killed. Signaling the whole tree prevents child
// processes such as installers or package managers launched by a shell script from surviving
// cancellation. Killing is a request rather than an outcome, so the tree is then confirmed to be
// empty; executeResult.TerminationIncomplete reports a tree that could not be confirmed gone.
func (a *Applyinator) execute(ctx context.Context, prefix, executionDir string, instruction planapi.CommonInstruction, combinedOutput bool, attempt int) (executeResult, error) {
	if instruction.Image == "" {
		logrus.Infof("[applyinator] no image provided, creating empty working directory %s", executionDir)
		// UID/GID -1 means "don't change ownership" (a no-op chown). Without this, the directory
		// defaults to UID/GID 0 (root) — harmless in production, where the agent always runs as
		// root, but it makes this code unusable from a non-root test process (os.Chown to a
		// different owner than the caller returns "operation not permitted").
		if err := createDirectory(planapi.File{Directory: true, Path: executionDir, UID: -1, GID: -1}); err != nil {
			logrus.Errorf("[applyinator] error while creating empty working directory: %v", err)
			return executeResult{ExitCode: -1}, err
		}
	} else {
		logrus.Infof("[applyinator] extracting image %s to directory %s", instruction.Image, executionDir)
		if err := a.imageUtil.Stage(executionDir, instruction.Image); err != nil {
			logrus.Errorf("[applyinator] error while staging: %v", err)
			return executeResult{ExitCode: -1}, err
		}
	}

	command := instruction.Command

	if command == "" {
		logrus.Debugf("[applyinator] command was not specified, defaulting to %s%s", executionDir, defaultCommand)
		command = executionDir + defaultCommand
	}

	cmd := exec.Command(command, instruction.Args...)
	logrus.Infof("[applyinator] running command: %s %v", instruction.Command, instruction.Args)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, instruction.Env...)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", cattleAgentExecutionPwdEnvKey, executionDir))
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", cattleAgentAttemptKey, attempt))
	cmd.Env = append(cmd.Env, "PATH="+os.Getenv("PATH")+":"+executionDir)
	cmd.Dir = executionDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logrus.Errorf("[applyinator] error setting up stdout pipe: %v", err)
		return executeResult{ExitCode: -1}, err
	}
	defer stdout.Close()

	stderr, err := cmd.StderrPipe()
	if err != nil {
		logrus.Errorf("[applyinator] error setting up stderr pipe: %v", err)
		return executeResult{ExitCode: -1}, err
	}
	defer stderr.Close()

	// SysProcAttr is read when the process starts. If process-group setup fails here, continue
	// without it so execution still proceeds; cancellation will then signal only the direct child.
	if err := configureProcessGroup(cmd); err != nil {
		logrus.Errorf("[applyinator] error configuring the process group for %s: %v; cancelling it will only signal its direct child", command, err)
	}

	var (
		eg           = errgroup.Group{}
		stdoutBuffer bytes.Buffer
		stderrBuffer bytes.Buffer
	)

	stdoutTarget := &stdoutBuffer
	stderrTarget := &stderrBuffer
	stdoutLock := &sync.Mutex{}
	stderrLock := stdoutLock

	if combinedOutput {
		// Share one buffer (and therefore the one lock already assigned above) so stdout and
		// stderr genuinely interleave into a single combined result. Previously this assigned
		// stderrBuffer = stdoutBuffer, which copies an empty bytes.Buffer by value: the two
		// goroutines below still wrote into two independent buffers, so combinedOutput silently
		// did nothing, and one-time instructions (which call execute with combinedOutput=true and
		// only keep the first return value) never captured stderr in SaveOutput results.
		stderrTarget = stdoutTarget
	} else {
		stderrLock = &sync.Mutex{}
	}

	eg.Go(func() error {
		return streamLogs("["+prefix+":stdout]", stdoutTarget, stdout, stdoutLock)
	})
	eg.Go(func() error {
		return streamLogs("["+prefix+":stderr]", stderrTarget, stderr, stderrLock)
	})

	if err := cmd.Start(); err != nil {
		// The watchdog has not started yet, so release any process-tree handle created during setup.
		// releaseProcessTree is idempotent and does nothing if no handle was recorded.
		releaseProcessTree(cmd)
		return executeResult{ExitCode: -1}, err
	}

	// Windows assigns the process to its Job Object only after the process starts. If assignment
	// fails, continue with direct-child cancellation as the fallback.
	if err := assignProcessTree(cmd); err != nil {
		logrus.Errorf("[applyinator] error assigning %s to its process tree: %v; cancelling it will only signal its direct child", command, err)
	}

	stop := watchForTermination(ctx, cmd, stdout, stderr)
	defer stop()

	// Wait for I/O to complete before calling cmd.Wait() because cmd.Wait() will close the I/O pipes.
	_ = eg.Wait()
	exitCode := 0
	waitErr := cmd.Wait()
	if waitErr != nil {
		// A non-ExitError wait failure (the process never produced an exit status) must not be
		// reported as exit code 0: runPeriodicInstructions branches on the exit code rather than
		// the error, and would otherwise persist a failed run as a success.
		exitCode = -1
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		}
	}
	// Stop the watchdog here rather than leaving it to the deferred call. stop() decides whether the
	// process tree really went away, and that question can only be answered once cmd.Wait() has reaped
	// the direct child, because an unreaped zombie is still a member of its own process group.
	terminationIncomplete := stop()

	logrus.Infof("[applyinator] command %s %v finished with err: %v and exit code: %d", instruction.Command, instruction.Args, waitErr, exitCode)
	return executeResult{
		Stdout:                stdoutTarget.Bytes(),
		Stderr:                stderrTarget.Bytes(),
		ExitCode:              exitCode,
		TerminationIncomplete: terminationIncomplete,
	}, waitErr
}

// watchForTermination monitors ctx and terminates cmd's process tree when cancellation occurs.
// It first sends a graceful termination signal, then force-kills the tree if it has not exited
// within instructionTerminationGrace. Pipes are closed only after forced termination so that
// streamLogs cannot remain blocked on descendants that inherited the pipe handles.
//
// The returned function stops the watchdog, releases any platform-specific process-tree handles, and
// reports whether processes from the tree were still running once the agent gave up on them. It is
// always false when the instruction was never terminated. Callers must defer it, and should call it
// explicitly after cmd.Wait() when they care about the return value: the tree cannot be confirmed
// empty until the direct child has been reaped.
func watchForTermination(ctx context.Context, cmd *exec.Cmd, pipes ...io.Closer) func() bool {
	// done is closed by the returned stop function; finished is closed when the watchdog exits.
	// Waiting for finished ensures all termination work is complete before process-tree handles are
	// released.
	done := make(chan struct{})
	finished := make(chan struct{})
	// terminated is written by the watchdog goroutine before it closes finished and is read only after
	// a receive from finished has succeeded. The channel close provides the happens-before edge, so no
	// additional synchronization is needed. incomplete is only ever touched inside once.Do.
	terminated := false
	incomplete := false

	// Read here rather than in the watchdog so both it and the stop function report the same pid without
	// racing cmd.Wait(). Callers invoke this after a successful cmd.Start(), so Process is set; the guard
	// only keeps the log honest if that ever stops being true.
	pid := -1
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	go func() {
		defer close(finished)

		select {
		case <-done:
			// The instruction completed normally; there is nothing to terminate.
			return
		case <-ctx.Done():
		}
		terminated = true

		logrus.Infof("[applyinator] apply was canceled, terminating the process tree of pid %d", pid)
		if err := terminateProcessTree(cmd); err != nil {
			logrus.Warnf("[applyinator] error terminating the process tree of pid %d: %v", pid, err)
		}

		// Wait for either the process to finish or the grace period to expire. execute closes done after
		// cmd.Wait() returns, so a process that exits promptly avoids waiting out the full grace period.
		// This also handles platforms where terminateProcessTree kills the tree immediately, such as
		// Windows, where no graceful signal is available.
		graceDeadline := time.Now().Add(instructionTerminationGrace)
		select {
		case <-done:
			// The direct child is gone: either it took the hint, or terminateProcessTree killed it
			// outright. That is not the same as the tree being gone, and because done is only closed
			// after cmd.Wait() has reaped the child, whatever the group still contains is a survivor
			// rather than an unreaped zombie. Give the rest of the tree the remainder of the grace
			// period before escalating.
			if processTreeExited(cmd, graceDeadline) {
				return
			}
			logrus.Warnf("[applyinator] pid %d exited but processes from its process tree are still running", pid)
		case <-time.After(instructionTerminationGrace):
		}

		logrus.Warnf("[applyinator] process tree of pid %d did not exit within %s of being asked, killing it", pid, instructionTerminationGrace)
		if err := killProcessTree(cmd); err != nil {
			logrus.Warnf("[applyinator] error killing the process tree of pid %d: %v", pid, err)
		}

		// execute waits for both output streams to reach EOF before calling cmd.Wait(). A descendant
		// that inherited a pipe can keep the streams open even after the main process is killed, so close
		// the pipes explicitly to unblock streamLogs. Only do this on the forced-kill path, so output from
		// a gracefully exiting instruction is not truncated. Ignore close errors because cmd.Wait() may
		// close the same descriptors afterward.
		for _, pipe := range pipes {
			_ = pipe.Close()
		}
	}()

	var once sync.Once
	return func() bool {
		once.Do(func() {
			close(done)
			<-finished
			if terminated {
				// The tree was signaled and, if it did not comply, killed. Confirm it is actually gone
				// before the caller reports the instruction as terminated. This is deliberately the last
				// thing done before the handles are released: on Unix the process group cannot be
				// distinguished from its own unreaped leader, so this is only meaningful after the caller
				// has run cmd.Wait().
				incomplete = !processTreeExited(cmd, time.Now().Add(processTreeExitTimeout))
				if incomplete {
					logrus.Warnf("[applyinator] processes from the terminated process tree of pid %d were still running %s after it was killed; "+
						"the node may still be modified by the abandoned instruction", pid, processTreeExitTimeout)
				}
			}
			releaseProcessTree(cmd)
		})
		return incomplete
	}
}

// streamLogs reads lines from reader and appends them to outputBuffer.
// Log each line with prefix. Protect writes with lock.
func streamLogs(prefix string, outputBuffer *bytes.Buffer, reader io.Reader, lock *sync.Mutex) error {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		logrus.Infof("%s: %s", prefix, scanner.Text())
		lock.Lock()
		outputBuffer.Write(append(scanner.Bytes(), []byte("\n")...))
		lock.Unlock()
	}
	return nil
}
