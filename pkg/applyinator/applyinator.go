package applyinator

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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

type Applyinator struct {
	mu              *sync.Mutex
	workDir         string
	preserveWorkDir bool
	appliedPlanDir  string
	interlockDir    string
	imageUtil       *image.Utility
}

// CalculatedPlan is passed into Applyinator and is a Plan with checksum calculated
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

type ApplyOutput struct {
	OneTimeOutput          []byte
	OneTimeApplySucceeded  bool
	PeriodicOutput         []byte
	PeriodicApplySucceeded bool
}

type ApplyInput struct {
	CalculatedPlan             CalculatedPlan
	RunOneTimeInstructions     bool
	OneTimeInstructionAttempts int
	ReconcileFiles             bool
	ExistingOneTimeOutput      []byte
	ExistingPeriodicOutput     []byte
}

// periodicInstructionDue decides whether a periodic instruction should run now, given the
// previously recorded output for that instruction. An unset or unparsable last-run timestamp is
// treated as "no history" (always due). forced (the one-time instructions ran this cycle) bypasses
// both the period and the failure cooldown.
func periodicInstructionDue(now time.Time, prev planapi.PeriodicInstructionOutput, periodSeconds int, forced bool) (due bool, failures int) {
	if prev.LastSuccessfulRunTime != "" {
		t, err := time.Parse(time.UnixDate, prev.LastSuccessfulRunTime)
		if err != nil {
			logrus.Errorf("error encountered during parsing of last successful run time: %v", err)
		} else {
			effectivePeriod := periodSeconds
			if effectivePeriod == 0 {
				effectivePeriod = 600
			}
			if now.Before(t.Add(time.Second*time.Duration(effectivePeriod))) && !forced {
				return false, failures
			}
		}
	}

	if prev.LastFailedRunTime != "" {
		t, err := time.Parse(time.UnixDate, prev.LastFailedRunTime)
		if err != nil {
			logrus.Errorf("error encountered during parsing of last failed time: %+v", err)
		} else {
			failures = prev.Failures
			failureCooldown := failures
			if failureCooldown > 6 {
				failureCooldown = 6
			} else if failureCooldown == 0 {
				failureCooldown = 1
			}
			if now.Before(t.Add(time.Second*time.Duration(30*failureCooldown))) && !forced {
				return false, failures
			}
		}
	}

	return true, failures
}

// reconcileFiles applies a plan's Files: writing regular files, creating directories, and
// deleting anything marked with the delete action.
func reconcileFiles(files []planapi.File) error {
	for _, file := range files {
		if file.Action == deleteFileAction {
			if err := removeFile(file); err != nil {
				return err
			}
		} else if file.Directory {
			logrus.Debugf("[Applyinator] Creating directory %s", file.Path)
			if err := createDirectory(file); err != nil {
				return err
			}
		} else {
			logrus.Debugf("[Applyinator] Writing file %s", file.Path)
			if err := writeBase64ContentToFile(file); err != nil {
				return err
			}
		}
	}
	return nil
}

// runOneTimeInstructions executes a plan's one-time instructions in order, stopping at the first
// failure, and returns the updated (gzip+JSON encoded) saved-output map.
func (a *Applyinator) runOneTimeInstructions(ctx context.Context, executionDir string, cp CalculatedPlan, existingOutput []byte, attempts int) ([]byte, bool, error) {
	logrus.Infof("[Applyinator] Applying one-time instructions for plan with checksum %s", cp.Checksum)
	executionOutputs := map[string][]byte{}
	if len(existingOutput) > 0 {
		objectBuffer, err := generateByteBufferFromBytes(existingOutput)
		if err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal(objectBuffer.Bytes(), &executionOutputs); err != nil {
			return nil, false, err
		}
	}

	oneTimeApplySucceeded := true
	for index, instruction := range cp.Plan.OneTimeInstructions {
		logrus.Debugf("[Applyinator] Executing instruction %d attempt %d for plan %s", index, attempts, cp.Checksum)
		prefix := cp.Checksum + "_" + strconv.Itoa(index)
		instructionDir := filepath.Join(executionDir, prefix)
		executeOutput, _, exitCode, err := a.execute(ctx, prefix, instructionDir, instruction.CommonInstruction, true, attempts)
		if err != nil || exitCode != 0 {
			logrus.Errorf("error executing instruction %d: %v", index, err)
			oneTimeApplySucceeded = false
		}
		if instruction.Name == "" && instruction.SaveOutput {
			logrus.Errorf("instruction does not have a name set, cannot save output data")
		} else if instruction.SaveOutput {
			executionOutputs[instruction.Name] = executeOutput
		}
		if !oneTimeApplySucceeded {
			break
		}
	}

	marshalled, err := json.Marshal(executionOutputs)
	if err != nil {
		return nil, false, err
	}
	output, err := gzipByteSlice(marshalled)
	if err != nil {
		return nil, false, err
	}
	return output, oneTimeApplySucceeded, nil
}

// runPeriodicInstructions executes each due periodic instruction and returns the updated
// (gzip+JSON encoded) periodic-output map. ranOneTime forces every instruction to run regardless
// of its period/failure cooldown, matching the one-time-instructions-just-ran semantics.
func (a *Applyinator) runPeriodicInstructions(ctx context.Context, executionDir string, cp CalculatedPlan, existingOutput []byte, ranOneTime bool, now time.Time) ([]byte, bool, error) {
	nowUnixTimeString := now.Format(time.UnixDate)

	periodicOutputs := map[string]planapi.PeriodicInstructionOutput{}
	if len(existingOutput) > 0 {
		objectBuffer, err := generateByteBufferFromBytes(existingOutput)
		if err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal(objectBuffer.Bytes(), &periodicOutputs); err != nil {
			return nil, false, err
		}
	}

	periodicApplySucceeded := true
	for index, instruction := range cp.Plan.PeriodicInstructions {
		if instruction.Name == "" {
			logrus.Errorf("periodic instruction %d did not have name, unable to run", index)
			continue
		}

		prev := periodicOutputs[instruction.Name]
		due, failures := periodicInstructionDue(now, prev, instruction.PeriodSeconds, ranOneTime)
		if !due {
			logrus.Debugf("[Applyinator] Not running periodic instruction %s; not yet due", instruction.Name)
			continue
		}

		previousRunTime := ""
		if prev.LastSuccessfulRunTime != "" {
			if _, err := time.Parse(time.UnixDate, prev.LastSuccessfulRunTime); err == nil {
				previousRunTime = prev.LastSuccessfulRunTime
			}
		}

		logrus.Debugf("[Applyinator] Executing periodic instruction %d for plan %s", index, cp.Checksum)
		prefix := cp.Checksum + "_" + strconv.Itoa(index)
		instructionDir := filepath.Join(executionDir, prefix)
		stdout, stderr, exitCode, err := a.execute(ctx, prefix, instructionDir, instruction.CommonInstruction, false, failures+1)
		if err != nil || exitCode != 0 {
			periodicApplySucceeded = false
		}

		lsrt := nowUnixTimeString
		lastFailureTime := ""
		if exitCode != 0 {
			lsrt = previousRunTime
			lastFailureTime = nowUnixTimeString
			failures++
		} else {
			failures = 0
		}
		if !instruction.SaveStderrOutput {
			stderr = []byte{}
		}
		periodicOutputs[instruction.Name] = planapi.PeriodicInstructionOutput{
			Name:                  instruction.Name,
			Stdout:                stdout,
			Stderr:                stderr,
			ExitCode:              exitCode,
			LastSuccessfulRunTime: lsrt,
			LastFailedRunTime:     lastFailureTime,
			Failures:              failures,
		}
		if !periodicApplySucceeded {
			break
		}
	}

	marshalled, err := json.Marshal(periodicOutputs)
	if err != nil {
		return nil, false, err
	}
	output, err := gzipByteSlice(marshalled)
	if err != nil {
		return nil, false, err
	}
	return output, periodicApplySucceeded, nil
}

// checkInterlock enforces the interlock directory protocol used by install.sh during an agent
// upgrade: a restart-pending file blocks applying for up to restartPendingTimeout, after which it
// is ignored and removed. On success it returns a cleanup func that must be deferred by the
// caller to remove the applyinator-active file once the apply completes.
func (a *Applyinator) checkInterlock(now time.Time) (func(), error) {
	noop := func() {}
	if a.interlockDir == "" {
		return noop, nil
	}

	nowUnixTimeString := now.Format(time.UnixDate)
	restartPendingInterlockFilePath := filepath.Join(a.interlockDir, restartPendingInterlockFile)
	applyinatorActiveInterlockFilePath := filepath.Join(a.interlockDir, applyinatorActiveInterlockFile)

	// NOTE: this checks/removes a bare relative filename, not applyinatorActiveInterlockFilePath —
	// a pre-existing bug preserved intentionally. See the "Risks / edge cases" section of
	// docs/superpowers/specs/2026-08-05-applyinator-refactor-design.md.
	if _, err := os.Stat(applyinatorActiveInterlockFile); err == nil {
		if err := os.Remove(applyinatorActiveInterlockFile); err != nil {
			logrus.Errorf("unable to remove applyinator active interlock file %s: %v", applyinatorActiveInterlockFilePath, err)
		}
	}

	if _, err := os.Stat(restartPendingInterlockFilePath); err == nil {
		fileContents, err := os.ReadFile(restartPendingInterlockFilePath)
		if err != nil {
			return noop, fmt.Errorf("unable to read restart pending interlock file %s: %w", restartPendingInterlockFilePath, err)
		}
		t, err := time.Parse(time.UnixDate, string(fileContents))
		if err != nil {
			if err := os.WriteFile(restartPendingInterlockFilePath, []byte(nowUnixTimeString), 0600); err != nil {
				return noop, fmt.Errorf("unable to write first-observed time to restart pending interlock file %s: %w", restartPendingInterlockFilePath, err)
			}
			return noop, fmt.Errorf("restart is pending for system-agent, waiting %s until ignoring pending restart", restartPendingTimeout.String())
		}
		if now.Before(t.Add(restartPendingTimeout)) {
			return noop, fmt.Errorf("restart is pending for system-agent, waiting %s until ignoring pending restart", t.Add(restartPendingTimeout).Sub(now).String())
		}
		if err := os.Remove(restartPendingInterlockFilePath); err != nil {
			logrus.Errorf("error encountered while removing restart pending interlock file %s: %v", restartPendingInterlockFilePath, err)
		}
	}

	if err := os.WriteFile(applyinatorActiveInterlockFilePath, []byte(nowUnixTimeString), 0600); err != nil {
		logrus.Errorf("unable to write applyinator active interlock file %s: %v", applyinatorActiveInterlockFilePath, err)
	}

	return func() {
		if err := os.Remove(applyinatorActiveInterlockFilePath); err != nil {
			logrus.Errorf("unable to remove applyinator active interlock file %s: %v", applyinatorActiveInterlockFilePath, err)
		}
	}, nil
}

// Apply accepts a context, calculated plan, a bool to indicate whether to run the onetime instructions, the existing onetimeinstruction output, and an input byte slice which is a base64+gzip json-marshalled map of PeriodicInstructionOutput
// entries where the key is the PeriodicInstructionOutput.Name. It outputs a revised versions of the existing outputs, and if specified, runs the one time instructions. Notably, ApplyOutput.OneTimeApplySucceeded will be false if ApplyInput.RunOneTimeInstructions is false
func (a *Applyinator) Apply(ctx context.Context, input ApplyInput) (ApplyOutput, error) {
	logrus.Debugf("[Applyinator] Applying plan with checksum %s", input.CalculatedPlan.Checksum)
	logrus.Tracef("[Applyinator] Applying plan - attempting to get lock")
	output := ApplyOutput{
		OneTimeOutput:  input.ExistingOneTimeOutput,
		PeriodicOutput: input.ExistingPeriodicOutput,
	}
	a.mu.Lock()
	logrus.Tracef("[Applyinator] Applying plan - lock achieved")
	defer a.mu.Unlock()
	now := time.Now()
	nowString := now.Format(applyinatorDateCodeLayout)

	cleanupInterlock, err := a.checkInterlock(now)
	if err != nil {
		return output, err
	}
	defer cleanupInterlock()

	executionDir := filepath.Join(a.workDir, nowString)
	logrus.Tracef("[Applyinator] Applying calculated node plan contents %v", input.CalculatedPlan.Checksum)
	logrus.Tracef("[Applyinator] Using %s as execution directory", executionDir)
	if a.appliedPlanDir != "" {
		logrus.Debugf("[Applyinator] Writing applied calculated plan contents to historical plan directory %s", a.appliedPlanDir)
		if err := os.MkdirAll(a.appliedPlanDir, 0700); err != nil {
			logrus.Errorf("error creating applied plan directory: %v", err)
		}
		if err := a.writePlanToDisk(now, &input.CalculatedPlan); err != nil {
			logrus.Errorf("error writing applied plan to disk: %v", err)
		}
		if err := a.appliedPlanRetentionPolicy(planRetentionPolicyCount); err != nil {
			logrus.Errorf("error while applying plan retention policy: %v", err)
		}
	}

	if input.ReconcileFiles {
		if err := reconcileFiles(input.CalculatedPlan.Plan.Files); err != nil {
			return output, err
		}
	}

	if !a.preserveWorkDir {
		logrus.Debugf("[Applyinator] Cleaning working directory before applying %s", a.workDir)
		if err := os.RemoveAll(a.workDir); err != nil {
			return output, err
		}
	}

	if input.RunOneTimeInstructions {
		oneTimeOutput, oneTimeSucceeded, err := a.runOneTimeInstructions(ctx, executionDir, input.CalculatedPlan, input.ExistingOneTimeOutput, input.OneTimeInstructionAttempts)
		if err != nil {
			return output, err
		}
		output.OneTimeOutput = oneTimeOutput
		output.OneTimeApplySucceeded = oneTimeSucceeded
	}

	periodicOutput, periodicSucceeded, err := a.runPeriodicInstructions(ctx, executionDir, input.CalculatedPlan, input.ExistingPeriodicOutput, input.RunOneTimeInstructions, now)
	if err != nil {
		return output, err
	}
	output.PeriodicOutput = periodicOutput
	output.PeriodicApplySucceeded = periodicSucceeded

	return output, nil
}

func gzipByteSlice(input []byte) ([]byte, error) {
	var gzOutput bytes.Buffer

	gzWriter := gzip.NewWriter(&gzOutput)

	if _, err := gzWriter.Write(input); err != nil {
		logrus.Errorf("error writing gzipped byte slice: %v", err)
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
		logrus.Infof("[Applyinator] Removing historical applied plan (retention policy count: %d) %s", retention, historicalPlanFile)
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
			logrus.Debugf("[Applyinator] Not writing applied plan to file %s as the last file written (%s) had identical contents", file, planFiles[0].Name())
			return nil
		}
	}

	return writeContentToFile(filepath.Join(a.appliedPlanDir, file), os.Getuid(), os.Getgid(), 0600, anpString)
}

func (a *Applyinator) execute(ctx context.Context, prefix, executionDir string, instruction planapi.CommonInstruction, combinedOutput bool, attempt int) ([]byte, []byte, int, error) {
	if instruction.Image == "" {
		logrus.Infof("[Applyinator] No image provided, creating empty working directory %s", executionDir)
		// UID/GID -1 means "don't change ownership" (a no-op chown). Without this, the directory
		// defaults to UID/GID 0 (root) — harmless in production, where the agent always runs as
		// root, but it makes this code unusable from a non-root test process (os.Chown to a
		// different owner than the caller returns "operation not permitted").
		if err := createDirectory(planapi.File{Directory: true, Path: executionDir, UID: -1, GID: -1}); err != nil {
			logrus.Errorf("error while creating empty working directory: %v", err)
			return nil, nil, -1, err
		}
	} else {
		logrus.Infof("[Applyinator] Extracting image %s to directory %s", instruction.Image, executionDir)
		if err := a.imageUtil.Stage(executionDir, instruction.Image); err != nil {
			logrus.Errorf("error while staging: %v", err)
			return nil, nil, -1, err
		}
	}

	command := instruction.Command

	if command == "" {
		logrus.Debugf("[Applyinator] Command was not specified, defaulting to %s%s", executionDir, defaultCommand)
		command = executionDir + defaultCommand
	}

	cmd := exec.CommandContext(ctx, command, instruction.Args...)
	logrus.Infof("[Applyinator] Running command: %s %v", instruction.Command, instruction.Args)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, instruction.Env...)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", cattleAgentExecutionPwdEnvKey, executionDir))
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", cattleAgentAttemptKey, attempt))
	cmd.Env = append(cmd.Env, "PATH="+os.Getenv("PATH")+":"+executionDir)
	cmd.Dir = executionDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logrus.Errorf("error setting up stdout pipe: %v", err)
		return nil, nil, -1, err
	}
	defer stdout.Close()

	stderr, err := cmd.StderrPipe()
	if err != nil {
		logrus.Errorf("error setting up stderr pipe: %v", err)
		return nil, nil, -1, err
	}
	defer stderr.Close()

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
		return nil, nil, -1, err
	}

	// Wait for I/O to complete before calling cmd.Wait() because cmd.Wait() will close the I/O pipes.
	_ = eg.Wait()
	exitCode := 0
	waitErr := cmd.Wait()
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	logrus.Infof("[Applyinator] Command %s %v finished with err: %v and exit code: %d", instruction.Command, instruction.Args, waitErr, exitCode)
	return stdoutBuffer.Bytes(), stderrTarget.Bytes(), exitCode, waitErr
}

// streamLogs accepts a prefix, outputBuffer, reader, and buffer lock and will scan input from the reader and write it
// to the output buffer while also logging anything that comes from the reader with the prefix.
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
