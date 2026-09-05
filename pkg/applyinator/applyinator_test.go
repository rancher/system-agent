package applyinator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
)

func TestGzipByteSliceRoundTrip(t *testing.T) {
	t.Parallel()

	input := []byte(`{"hello":"world"}`)

	gzipped, err := gzipByteSlice(input)
	if err != nil {
		t.Fatalf("gzipByteSlice returned error: %v", err)
	}

	buf, err := generateByteBufferFromBytes(gzipped)
	if err != nil {
		t.Fatalf("generateByteBufferFromBytes returned error: %v", err)
	}

	if !bytes.Equal(buf.Bytes(), input) {
		t.Errorf("expected round-tripped bytes %q, got %q", input, buf.Bytes())
	}
}

func TestGenerateByteBufferFromBytesInvalidGzip(t *testing.T) {
	t.Parallel()

	if _, err := generateByteBufferFromBytes([]byte("not gzip data")); err == nil {
		t.Error("expected error decoding non-gzip data, got nil")
	}
}

func TestStreamLogs(t *testing.T) {
	t.Parallel()

	reader := strings.NewReader("line one\nline two\n")
	var outputBuffer bytes.Buffer
	lock := &sync.Mutex{}

	if err := streamLogs("[test]", &outputBuffer, reader, lock); err != nil {
		t.Fatalf("streamLogs returned error: %v", err)
	}

	expected := "line one\nline two\n"
	if outputBuffer.String() != expected {
		t.Errorf("expected buffer %q, got %q", expected, outputBuffer.String())
	}
}

func newTestApplyinator(t *testing.T, workDir string, preserveWorkDir bool, appliedPlanDir, interlockDir string) *Applyinator {
	t.Helper()
	if workDir == "" {
		workDir = t.TempDir()
	}
	return NewApplyinator(workDir, preserveWorkDir, appliedPlanDir, interlockDir, nil)
}

func TestGetAppliedPlanFiles(t *testing.T) {
	t.Parallel()

	appliedPlanDir := t.TempDir()
	for _, name := range []string{"20260101-000000-applied.plan", "20260102-000000-applied.plan", "not-a-plan.txt"} {
		if err := os.WriteFile(filepath.Join(appliedPlanDir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(appliedPlanDir, "20260103-000000-applied.plan-dir"), 0700); err != nil {
		t.Fatal(err)
	}

	a := newTestApplyinator(t, "", false, appliedPlanDir, "")
	files, err := a.getAppliedPlanFiles()
	if err != nil {
		t.Fatalf("getAppliedPlanFiles returned error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 applied plan files, got %d: %v", len(files), files)
	}
}

func TestAppliedPlanRetentionPolicy(t *testing.T) {
	t.Parallel()

	appliedPlanDir := t.TempDir()
	names := []string{
		"20260101-000000-applied.plan",
		"20260102-000000-applied.plan",
		"20260103-000000-applied.plan",
		"20260104-000000-applied.plan",
		"20260105-000000-applied.plan",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(appliedPlanDir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	a := newTestApplyinator(t, "", false, appliedPlanDir, "")
	if err := a.appliedPlanRetentionPolicy(3); err != nil {
		t.Fatalf("appliedPlanRetentionPolicy returned error: %v", err)
	}

	remaining, err := a.getAppliedPlanFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 {
		t.Fatalf("expected 3 files to remain, got %d", len(remaining))
	}
	for _, deleted := range names[:2] {
		if _, err := os.Stat(filepath.Join(appliedPlanDir, deleted)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted, stat err: %v", deleted, err)
		}
	}
	for _, kept := range names[2:] {
		if _, err := os.Stat(filepath.Join(appliedPlanDir, kept)); err != nil {
			t.Errorf("expected %s to survive, stat err: %v", kept, err)
		}
	}
}

// TestExecuteCapturesStdoutStderrAndExitCode is also the happy-path guard for the termination
// watchdog execute arms on every command: a command that is never canceled must have its output
// captured in full and its exit code reported unchanged, so the watchdog neither truncates output
// by closing the pipes early nor interferes with exit reporting.
func TestExecuteCapturesStdoutStderrAndExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	testCases := []struct {
		name           string
		combinedOutput bool
		script         string
		wantExitCode   int
	}{
		{
			name:           "separate streams, success",
			combinedOutput: false,
			script:         "echo out-line; echo err-line 1>&2; exit 0",
			wantExitCode:   0,
		},
		{
			name:           "separate streams, failure",
			combinedOutput: false,
			script:         "echo out-line; echo err-line 1>&2; exit 7",
			wantExitCode:   7,
		},
		{
			name:           "combined streams merge stdout and stderr",
			combinedOutput: true,
			script:         "echo out-line; echo err-line 1>&2; exit 0",
			wantExitCode:   0,
		},
		{
			// A signal-terminated process never produces an exit status. It must surface as -1,
			// not 0, or runPeriodicInstructions (which branches on the exit code rather than the
			// error) would persist the failed run as a success.
			name:           "signal-terminated process reports exit code -1",
			combinedOutput: false,
			script:         "echo out-line; echo err-line 1>&2; kill -9 $$",
			wantExitCode:   -1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := NewApplyinator(t.TempDir(), false, "", "", nil)
			instruction := planapi.CommonInstruction{
				Command: "sh",
				Args:    []string{"-c", tc.script},
			}

			result, err := a.execute(context.Background(), "test", t.TempDir(), instruction, tc.combinedOutput, 1)
			stdout, stderr, exitCode := result.Stdout, result.Stderr, result.ExitCode
			if exitCode != tc.wantExitCode {
				t.Errorf("expected exit code %d, got %d (err: %v)", tc.wantExitCode, exitCode, err)
			}
			if tc.wantExitCode == 0 && err != nil {
				t.Errorf("expected no error on success, got %v", err)
			}
			if tc.wantExitCode != 0 && err == nil {
				t.Error("expected the wait error to be surfaced on a non-zero exit, got nil")
			}

			if tc.combinedOutput {
				if !strings.Contains(string(stdout), "out-line") || !strings.Contains(string(stdout), "err-line") {
					t.Errorf("expected combined output to contain both streams, got stdout=%q stderr=%q", stdout, stderr)
				}
				if !bytes.Equal(stdout, stderr) {
					t.Errorf("expected combined mode to return identical stdout/stderr, got stdout=%q stderr=%q", stdout, stderr)
				}
			} else {
				if !strings.Contains(string(stdout), "out-line") {
					t.Errorf("expected stdout to contain %q, got %q", "out-line", stdout)
				}
				if !strings.Contains(string(stderr), "err-line") {
					t.Errorf("expected stderr to contain %q, got %q", "err-line", stderr)
				}
			}
		})
	}
}

func TestExecuteInjectsEnvironmentVariables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	a := NewApplyinator(t.TempDir(), false, "", "", nil)
	executionDir := t.TempDir()
	instruction := planapi.CommonInstruction{
		Command: "sh",
		Args:    []string{"-c", `echo "pwd=$CATTLE_AGENT_EXECUTION_PWD attempt=$CATTLE_AGENT_ATTEMPT_NUMBER foo=$FOO"`},
		Env:     []string{"FOO=bar"},
	}

	result, err := a.execute(context.Background(), "test", executionDir, instruction, false, 5)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("unexpected failure: exitCode=%d err=%v", result.ExitCode, err)
	}

	got := string(result.Stdout)
	for _, want := range []string{"pwd=" + executionDir, "attempt=5", "foo=bar"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected output to contain %q, got %q", want, got)
		}
	}
}

func TestExecuteDefaultsToRunShInExecutionDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	executionDir := t.TempDir()
	if err := os.WriteFile(executionDir+"/run.sh", []byte("#!/bin/sh\necho ran-default\n"), 0700); err != nil {
		t.Fatal(err)
	}

	a := NewApplyinator(t.TempDir(), false, "", "", nil)
	instruction := planapi.CommonInstruction{} // no Command set

	result, err := a.execute(context.Background(), "test", executionDir, instruction, false, 1)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("unexpected failure: exitCode=%d err=%v", result.ExitCode, err)
	}
	if !strings.Contains(string(result.Stdout), "ran-default") {
		t.Errorf("expected default run.sh to execute, got stdout=%q", result.Stdout)
	}
}

func TestWritePlanToDisk(t *testing.T) {
	t.Parallel()

	appliedPlanDir := t.TempDir()
	a := newTestApplyinator(t, "", false, appliedPlanDir, "")

	planA := &CalculatedPlan{Plan: planapi.Plan{}, Checksum: "checksum-a"}
	if err := a.writePlanToDisk(time.Now(), planA); err != nil {
		t.Fatalf("first write returned error: %v", err)
	}
	first, err := a.getAppliedPlanFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("expected 1 file after first write, got %d", len(first))
	}

	// Writing the same content again (even at a slightly later time) should not create a new file.
	if err := a.writePlanToDisk(time.Now().Add(time.Second), planA); err != nil {
		t.Fatalf("second write (identical content) returned error: %v", err)
	}
	second, err := a.getAppliedPlanFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("expected identical-content write to be a no-op, got %d files", len(second))
	}

	planB := &CalculatedPlan{Plan: planapi.Plan{}, Checksum: "checksum-b"}
	if err := a.writePlanToDisk(time.Now().Add(2*time.Second), planB); err != nil {
		t.Fatalf("third write (different content) returned error: %v", err)
	}
	third, err := a.getAppliedPlanFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 2 {
		t.Fatalf("expected different-content write to create a new file, got %d files", len(third))
	}
}

func decodeOneTimeOutputs(t *testing.T, gz []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if len(gz) == 0 {
		return out
	}
	buf, err := generateByteBufferFromBytes(gz)
	if err != nil {
		t.Fatalf("failed to gunzip one-time outputs: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("failed to unmarshal one-time outputs: %v", err)
	}
	return out
}

func TestApplyOneTimeAndPeriodicSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	a := newTestApplyinator(t, "", false, "", "")
	plan := planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{
				CommonInstruction: planapi.CommonInstruction{
					Name:    "onetime",
					Command: "sh",
					Args:    []string{"-c", "echo onetime-ok"},
				},
				SaveOutput: true,
			},
		},
		PeriodicInstructions: []planapi.PeriodicInstruction{
			{
				CommonInstruction: planapi.CommonInstruction{
					Name:    "periodic",
					Command: "sh",
					Args:    []string{"-c", "echo periodic-ok"},
				},
			},
		},
	}

	output, err := a.Apply(context.Background(), ApplyInput{
		CalculatedPlan:             CalculatedPlan{Plan: plan, Checksum: "checksum1"},
		RunOneTimeInstructions:     true,
		OneTimeInstructionAttempts: 1,
		ReconcileFiles:             true,
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if !output.OneTimeApplySucceeded {
		t.Error("expected one-time instructions to succeed")
	}
	if !output.PeriodicApplySucceeded {
		t.Error("expected periodic instructions to succeed")
	}

	outputs := decodeOneTimeOutputs(t, output.OneTimeOutput)
	if !strings.Contains(string(outputs["onetime"]), "onetime-ok") {
		t.Errorf("expected onetime output to contain %q, got %q", "onetime-ok", outputs["onetime"])
	}
}

func TestApplyOneTimeFailureStopsSubsequentInstructions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	workDir := t.TempDir()
	markerPath := filepath.Join(workDir, "should-not-exist")
	a := newTestApplyinator(t, workDir, false, "", "")
	plan := planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{
				CommonInstruction: planapi.CommonInstruction{
					Name:    "failing",
					Command: "sh",
					Args:    []string{"-c", "exit 1"},
				},
			},
			{
				CommonInstruction: planapi.CommonInstruction{
					Name:    "should-not-run",
					Command: "sh",
					Args:    []string{"-c", "touch " + markerPath},
				},
			},
		},
	}

	output, err := a.Apply(context.Background(), ApplyInput{
		CalculatedPlan:             CalculatedPlan{Plan: plan, Checksum: "checksum2"},
		RunOneTimeInstructions:     true,
		OneTimeInstructionAttempts: 1,
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if output.OneTimeApplySucceeded {
		t.Error("expected one-time instructions to fail")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("expected second instruction to never run, but marker file exists (stat err: %v)", err)
	}
}

func TestApplyReconcileFilesFalseSkipsWrites(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	filesDir := t.TempDir()
	filePath := filepath.Join(filesDir, "config.txt")

	a := newTestApplyinator(t, workDir, false, "", "")
	plan := planapi.Plan{
		Files: []planapi.File{
			{
				Path:    filePath,
				Content: base64.StdEncoding.EncodeToString([]byte("hello")),
				UID:     -1,
				GID:     -1,
			},
		},
	}

	if _, err := a.Apply(context.Background(), ApplyInput{
		CalculatedPlan: CalculatedPlan{Plan: plan, Checksum: "checksum3"},
		ReconcileFiles: false,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Errorf("expected file to not be written when ReconcileFiles is false (stat err: %v)", err)
	}
}

func TestApplyPreserveWorkDirectory(t *testing.T) {
	testCases := []struct {
		name            string
		preserveWorkDir bool
		expectSurvives  bool
	}{
		{name: "wipes work dir by default", preserveWorkDir: false, expectSurvives: false},
		{name: "preserves work dir when configured", preserveWorkDir: true, expectSurvives: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			workDir := t.TempDir()
			markerPath := filepath.Join(workDir, "pre-existing-marker")
			if err := os.WriteFile(markerPath, []byte("marker"), 0600); err != nil {
				t.Fatal(err)
			}

			a := newTestApplyinator(t, workDir, tc.preserveWorkDir, "", "")
			if _, err := a.Apply(context.Background(), ApplyInput{
				CalculatedPlan: CalculatedPlan{Plan: planapi.Plan{}, Checksum: "checksum4"},
			}); err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}

			_, statErr := os.Stat(markerPath)
			survived := statErr == nil
			if survived != tc.expectSurvives {
				t.Errorf("expected marker survival = %v, got %v (stat err: %v)", tc.expectSurvives, survived, statErr)
			}
		})
	}
}

func TestApplyWritesAppliedPlanToDisk(t *testing.T) {
	t.Parallel()

	appliedPlanDir := t.TempDir()
	a := newTestApplyinator(t, "", false, appliedPlanDir, "")

	if _, err := a.Apply(context.Background(), ApplyInput{
		CalculatedPlan: CalculatedPlan{Plan: planapi.Plan{}, Checksum: "checksum5"},
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	entries, err := os.ReadDir(appliedPlanDir)
	if err != nil {
		t.Fatalf("failed to read applied plan dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), appliedPlanFileSuffix) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a %s file in %s, found entries: %v", appliedPlanFileSuffix, appliedPlanDir, entries)
	}
}

func TestCheckInterlock(t *testing.T) {
	t.Run("no interlock directory configured", func(t *testing.T) {
		t.Parallel()
		a := newTestApplyinator(t, "", false, "", "")
		cleanup, err := a.checkInterlock(time.Now())
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		cleanup()
	})

	t.Run("no existing interlock files", func(t *testing.T) {
		t.Parallel()
		interlockDir := t.TempDir()
		a := newTestApplyinator(t, "", false, "", interlockDir)

		cleanup, err := a.checkInterlock(time.Now())
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		activePath := filepath.Join(interlockDir, applyinatorActiveInterlockFile)
		if _, err := os.Stat(activePath); err != nil {
			t.Fatalf("expected active interlock file to exist: %v", err)
		}
		cleanup()
		if _, err := os.Stat(activePath); !os.IsNotExist(err) {
			t.Fatalf("expected active interlock file to be removed after cleanup, stat err: %v", err)
		}
	})

	t.Run("restart pending with unparsable timestamp blocks and seeds first-observed time", func(t *testing.T) {
		t.Parallel()
		interlockDir := t.TempDir()
		restartPendingPath := filepath.Join(interlockDir, restartPendingInterlockFile)
		if err := os.WriteFile(restartPendingPath, []byte("not-a-timestamp"), 0600); err != nil {
			t.Fatal(err)
		}
		a := newTestApplyinator(t, "", false, "", interlockDir)

		_, err := a.checkInterlock(time.Now())
		if err == nil {
			t.Fatal("expected error while restart is pending, got nil")
		}
		contents, err := os.ReadFile(restartPendingPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := time.Parse(time.UnixDate, string(contents)); err != nil {
			t.Fatalf("expected file to be rewritten with a parsable first-observed time, got %q", contents)
		}
	})

	t.Run("restart pending within timeout blocks", func(t *testing.T) {
		t.Parallel()
		interlockDir := t.TempDir()
		now := time.Now()
		restartPendingPath := filepath.Join(interlockDir, restartPendingInterlockFile)
		if err := os.WriteFile(restartPendingPath, []byte(now.Add(-1*time.Minute).Format(time.UnixDate)), 0600); err != nil {
			t.Fatal(err)
		}
		a := newTestApplyinator(t, "", false, "", interlockDir)

		_, err := a.checkInterlock(now)
		if err == nil {
			t.Fatal("expected error while restart pending timeout has not elapsed, got nil")
		}
		if _, err := os.Stat(restartPendingPath); err != nil {
			t.Fatalf("expected restart pending file to remain, stat err: %v", err)
		}
	})

	t.Run("restart pending past timeout is cleared and apply proceeds", func(t *testing.T) {
		t.Parallel()
		interlockDir := t.TempDir()
		now := time.Now()
		restartPendingPath := filepath.Join(interlockDir, restartPendingInterlockFile)
		if err := os.WriteFile(restartPendingPath, []byte(now.Add(-6*time.Minute).Format(time.UnixDate)), 0600); err != nil {
			t.Fatal(err)
		}
		a := newTestApplyinator(t, "", false, "", interlockDir)

		cleanup, err := a.checkInterlock(now)
		if err != nil {
			t.Fatalf("expected no error once restart pending timeout has elapsed, got %v", err)
		}
		if _, err := os.Stat(restartPendingPath); !os.IsNotExist(err) {
			t.Fatalf("expected restart pending file to be removed, stat err: %v", err)
		}
		cleanup()
	})
}

func TestReconcileFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "written.txt")
	dirPath := filepath.Join(dir, "created-dir")
	deletedPath := filepath.Join(dir, "to-delete.txt")
	if err := os.WriteFile(deletedPath, []byte("gone soon"), 0600); err != nil {
		t.Fatal(err)
	}

	files := []planapi.File{
		{
			Path:    filePath,
			Content: base64.StdEncoding.EncodeToString([]byte("hello")),
			UID:     -1,
			GID:     -1,
		},
		{
			Path:      dirPath,
			Directory: true,
			UID:       -1,
			GID:       -1,
		},
		{
			Path:   deletedPath,
			Action: deleteFileAction,
		},
	}

	if err := reconcileFiles(files); err != nil {
		t.Fatalf("reconcileFiles returned error: %v", err)
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("expected file to be written: %v", err)
	}
	if string(content) != "hello" {
		t.Errorf("expected file content %q, got %q", "hello", content)
	}

	if info, err := os.Stat(dirPath); err != nil || !info.IsDir() {
		t.Errorf("expected directory to be created at %s: %v", dirPath, err)
	}

	if _, err := os.Stat(deletedPath); !os.IsNotExist(err) {
		t.Errorf("expected %s to be deleted, stat err: %v", deletedPath, err)
	}
}

func TestRunOneTimeInstructionsStopsAtFirstFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	a := NewApplyinator(t.TempDir(), false, "", "", nil)
	executionDir := t.TempDir()
	cp := CalculatedPlan{
		Checksum: "checksum-onetime",
		Plan: planapi.Plan{
			OneTimeInstructions: []planapi.OneTimeInstruction{
				{
					CommonInstruction: planapi.CommonInstruction{Name: "ok", Command: "sh", Args: []string{"-c", "echo ok-output"}},
					SaveOutput:        true,
				},
				{
					CommonInstruction: planapi.CommonInstruction{Name: "fails", Command: "sh", Args: []string{"-c", "exit 1"}},
				},
				{
					CommonInstruction: planapi.CommonInstruction{Name: "never-runs", Command: "sh", Args: []string{"-c", "echo should-not-appear"}},
					SaveOutput:        true,
				},
			},
		},
	}

	result, err := a.runOneTimeInstructions(context.Background(), executionDir, cp, nil, 1, 0, nil, nil)
	if err != nil {
		t.Fatalf("runOneTimeInstructions returned error: %v", err)
	}
	if result.Succeeded {
		t.Error("expected succeeded=false because the second instruction failed")
	}
	if result.Completed != 1 {
		t.Errorf("expected Completed=1 (only the first instruction ran to completion), got %d", result.Completed)
	}
	if result.Interruption != InterruptionNone {
		t.Errorf("expected a plain failure to report no interruption, got %q", result.Interruption)
	}

	outputs := decodeOneTimeOutputs(t, result.Output)
	if !strings.Contains(string(outputs["ok"]), "ok-output") {
		t.Errorf("expected saved output for %q to contain %q, got %q", "ok", "ok-output", outputs["ok"])
	}
	if _, ran := outputs["never-runs"]; ran {
		t.Error("expected the third instruction to never run")
	}
}

func TestPeriodicInstructionDue(t *testing.T) {
	t.Parallel()

	now := time.Now()

	testCases := []struct {
		name          string
		prev          planapi.PeriodicInstructionOutput
		periodSeconds int
		forced        bool
		wantDue       bool
		wantFailures  int
	}{
		{
			name:    "never run before is always due",
			prev:    planapi.PeriodicInstructionOutput{},
			wantDue: true,
		},
		{
			name: "period not yet elapsed is not due",
			prev: planapi.PeriodicInstructionOutput{
				LastSuccessfulRunTime: now.Add(-30 * time.Second).Format(time.UnixDate),
			},
			periodSeconds: 600,
			wantDue:       false,
		},
		{
			name: "period elapsed is due",
			prev: planapi.PeriodicInstructionOutput{
				LastSuccessfulRunTime: now.Add(-700 * time.Second).Format(time.UnixDate),
			},
			periodSeconds: 600,
			wantDue:       true,
		},
		{
			name: "zero PeriodSeconds defaults to 600",
			prev: planapi.PeriodicInstructionOutput{
				LastSuccessfulRunTime: now.Add(-30 * time.Second).Format(time.UnixDate),
			},
			periodSeconds: 0,
			wantDue:       false,
		},
		{
			name: "forced bypasses an unelapsed period",
			prev: planapi.PeriodicInstructionOutput{
				LastSuccessfulRunTime: now.Add(-30 * time.Second).Format(time.UnixDate),
			},
			periodSeconds: 600,
			forced:        true,
			wantDue:       true,
		},
		{
			name: "failure cooldown not yet elapsed is not due",
			prev: planapi.PeriodicInstructionOutput{
				LastFailedRunTime: now.Add(-5 * time.Second).Format(time.UnixDate),
				Failures:          1,
			},
			wantDue:      false,
			wantFailures: 1,
		},
		{
			name: "failure cooldown elapsed is due",
			prev: planapi.PeriodicInstructionOutput{
				LastFailedRunTime: now.Add(-31 * time.Second).Format(time.UnixDate),
				Failures:          1,
			},
			wantDue:      true,
			wantFailures: 1,
		},
		{
			name: "failure cooldown caps at 6 failures worth (180s)",
			prev: planapi.PeriodicInstructionOutput{
				LastFailedRunTime: now.Add(-181 * time.Second).Format(time.UnixDate),
				Failures:          50,
			},
			wantDue:      true,
			wantFailures: 50,
		},
		{
			name: "failure cooldown still capped at 180s when not yet elapsed",
			prev: planapi.PeriodicInstructionOutput{
				LastFailedRunTime: now.Add(-179 * time.Second).Format(time.UnixDate),
				Failures:          50,
			},
			wantDue:      false,
			wantFailures: 50,
		},
		{
			name: "forced bypasses failure cooldown",
			prev: planapi.PeriodicInstructionOutput{
				LastFailedRunTime: now.Add(-5 * time.Second).Format(time.UnixDate),
				Failures:          1,
			},
			forced:       true,
			wantDue:      true,
			wantFailures: 1,
		},
		{
			name: "unparsable last successful run time is treated as no history",
			prev: planapi.PeriodicInstructionOutput{
				LastSuccessfulRunTime: "not-a-time",
			},
			periodSeconds: 600,
			wantDue:       true,
		},
		{
			name: "failure cooldown defaults to 1s when Failures is zero but LastFailedRunTime is set",
			prev: planapi.PeriodicInstructionOutput{
				LastFailedRunTime: now.Add(-5 * time.Second).Format(time.UnixDate),
				Failures:          0,
			},
			wantDue:      false,
			wantFailures: 0,
		},
		{
			name: "failures stays zero when LastFailedRunTime is empty even if Failures is set",
			prev: planapi.PeriodicInstructionOutput{
				LastFailedRunTime: "",
				Failures:          3,
			},
			wantDue:      true,
			wantFailures: 0,
		},
		{
			name: "unparsable last failed run time is treated as no history, including failures resetting to zero",
			prev: planapi.PeriodicInstructionOutput{
				LastFailedRunTime: "not-a-time",
				Failures:          5,
			},
			wantDue:      true,
			wantFailures: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			due, failures := periodicInstructionDue(now, tc.prev, tc.periodSeconds, tc.forced)
			if due != tc.wantDue {
				t.Errorf("expected due=%v, got %v", tc.wantDue, due)
			}
			if failures != tc.wantFailures {
				t.Errorf("expected failures=%d, got %d", tc.wantFailures, failures)
			}
		})
	}
}

func encodeExistingPeriodicOutput(t *testing.T, outputs map[string]planapi.PeriodicInstructionOutput) []byte {
	t.Helper()
	marshalled, err := json.Marshal(outputs)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzipByteSlice(marshalled)
	if err != nil {
		t.Fatal(err)
	}
	return gz
}

func decodePeriodicOutputs(t *testing.T, gz []byte) map[string]planapi.PeriodicInstructionOutput {
	t.Helper()
	out := map[string]planapi.PeriodicInstructionOutput{}
	if len(gz) == 0 {
		return out
	}
	buf, err := generateByteBufferFromBytes(gz)
	if err != nil {
		t.Fatalf("failed to gunzip periodic outputs: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("failed to unmarshal periodic outputs: %v", err)
	}
	return out
}

func TestRunPeriodicInstructionsSkipsWhenNotDue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	a := NewApplyinator(t.TempDir(), false, "", "", nil)
	executionDir := t.TempDir()
	now := time.Now()
	cp := CalculatedPlan{
		Checksum: "checksum-periodic",
		Plan: planapi.Plan{
			PeriodicInstructions: []planapi.PeriodicInstruction{
				{
					CommonInstruction: planapi.CommonInstruction{Name: "steady", Command: "sh", Args: []string{"-c", "echo ran"}},
					PeriodSeconds:     600,
				},
			},
		},
	}

	existing := encodeExistingPeriodicOutput(t, map[string]planapi.PeriodicInstructionOutput{
		"steady": {Name: "steady", LastSuccessfulRunTime: now.Add(-1 * time.Second).Format(time.UnixDate)},
	})

	periodic, err := a.runPeriodicInstructions(context.Background(), executionDir, cp, existing, false, now, nil, nil)
	if err != nil {
		t.Fatalf("runPeriodicInstructions returned error: %v", err)
	}
	output, succeeded := periodic.Output, periodic.Succeeded
	if !succeeded {
		t.Error("expected succeeded=true when nothing ran")
	}

	outputs := decodePeriodicOutputs(t, output)
	if got := outputs["steady"].LastSuccessfulRunTime; got != now.Add(-1*time.Second).Format(time.UnixDate) {
		t.Errorf("expected last successful run time to be untouched, got %q", got)
	}
}

func TestRunPeriodicInstructionsRunsWhenDue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	a := NewApplyinator(t.TempDir(), false, "", "", nil)
	executionDir := t.TempDir()
	cp := CalculatedPlan{
		Checksum: "checksum-periodic-2",
		Plan: planapi.Plan{
			PeriodicInstructions: []planapi.PeriodicInstruction{
				{
					CommonInstruction: planapi.CommonInstruction{Name: "first-run", Command: "sh", Args: []string{"-c", "echo hello-periodic"}},
				},
			},
		},
	}

	periodic, err := a.runPeriodicInstructions(context.Background(), executionDir, cp, nil, false, time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("runPeriodicInstructions returned error: %v", err)
	}
	output, succeeded := periodic.Output, periodic.Succeeded
	if !succeeded {
		t.Error("expected succeeded=true")
	}

	outputs := decodePeriodicOutputs(t, output)
	got := outputs["first-run"]
	if !strings.Contains(string(got.Stdout), "hello-periodic") {
		t.Errorf("expected stdout to contain %q, got %q", "hello-periodic", got.Stdout)
	}
	if got.LastSuccessfulRunTime == "" {
		t.Error("expected LastSuccessfulRunTime to be set after a successful run")
	}
}

func TestResolvePermissions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		perm     string
		def      os.FileMode
		expected os.FileMode
		wantErr  bool
	}{
		{name: "empty uses default", perm: "", def: 0644, expected: 0644},
		{name: "valid octal", perm: "0755", def: 0644, expected: 0755},
		{name: "invalid octal", perm: "not-octal", def: 0644, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolvePermissions(tc.perm, tc.def)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.expected {
				t.Errorf("expected %v, got %v", tc.expected, got)
			}
		})
	}
}

func TestInstructionExecutionDir(t *testing.T) {
	t.Parallel()

	dir, prefix := instructionExecutionDir("/work/20260101-000000", "abc123", 2)
	if want := "abc123_2"; prefix != want {
		t.Errorf("expected prefix %q, got %q", want, prefix)
	}
	if want := filepath.Join("/work/20260101-000000", "abc123_2"); dir != want {
		t.Errorf("expected dir %q, got %q", want, dir)
	}
}

func TestRunPeriodicInstructionsRecordsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	testCases := []struct {
		name         string
		script       string
		wantExitCode int
	}{
		{
			name:         "non-zero exit status",
			script:       "exit 3",
			wantExitCode: 3,
		},
		{
			// Regression guard: a signal-terminated instruction has no exit status. If execute()
			// reported it as 0, the run below would be persisted as a success -- clearing
			// LastFailedRunTime and resetting Failures -- while succeeded=false.
			name:         "signal-terminated instruction",
			script:       "kill -9 $$",
			wantExitCode: -1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := NewApplyinator(t.TempDir(), false, "", "", nil)
			executionDir := t.TempDir()
			now := time.Now()
			previousSuccess := now.Add(-1000 * time.Second).Format(time.UnixDate)
			cp := CalculatedPlan{
				Checksum: "checksum-periodic-fail",
				Plan: planapi.Plan{
					PeriodicInstructions: []planapi.PeriodicInstruction{
						{
							CommonInstruction: planapi.CommonInstruction{Name: "flaky", Command: "sh", Args: []string{"-c", tc.script}},
							PeriodSeconds:     600,
						},
						{
							CommonInstruction: planapi.CommonInstruction{Name: "should-not-run", Command: "sh", Args: []string{"-c", "exit 0"}},
						},
					},
				},
			}
			existing := encodeExistingPeriodicOutput(t, map[string]planapi.PeriodicInstructionOutput{
				"flaky": {Name: "flaky", LastSuccessfulRunTime: previousSuccess},
			})

			periodic, err := a.runPeriodicInstructions(context.Background(), executionDir, cp, existing, false, now, nil, nil)
			if err != nil {
				t.Fatalf("runPeriodicInstructions returned error: %v", err)
			}
			output, succeeded := periodic.Output, periodic.Succeeded
			if succeeded {
				t.Error("expected succeeded=false because the instruction failed")
			}

			outputs := decodePeriodicOutputs(t, output)
			got := outputs["flaky"]
			if got.ExitCode != tc.wantExitCode {
				t.Errorf("expected ExitCode %d, got %d", tc.wantExitCode, got.ExitCode)
			}
			if got.Failures != 1 {
				t.Errorf("expected Failures to increment to 1, got %d", got.Failures)
			}
			if got.LastSuccessfulRunTime != previousSuccess {
				t.Errorf("expected LastSuccessfulRunTime to carry forward the prior success time %q, got %q", previousSuccess, got.LastSuccessfulRunTime)
			}
			if got.LastFailedRunTime == "" {
				t.Error("expected LastFailedRunTime to be set")
			}
			if _, ran := outputs["should-not-run"]; ran {
				t.Error("expected the second instruction to never run after the first failed")
			}
		})
	}
}

func TestApplyBlockedByInterlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	interlockDir := t.TempDir()
	now := time.Now()
	restartPendingPath := filepath.Join(interlockDir, restartPendingInterlockFile)
	if err := os.WriteFile(restartPendingPath, []byte(now.Add(-1*time.Minute).Format(time.UnixDate)), 0600); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	markerPath := filepath.Join(workDir, "should-not-exist")
	a := newTestApplyinator(t, workDir, false, "", interlockDir)
	plan := planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{
				CommonInstruction: planapi.CommonInstruction{
					Name:    "should-not-run",
					Command: "sh",
					Args:    []string{"-c", "touch " + markerPath},
				},
			},
		},
	}

	_, err := a.Apply(context.Background(), ApplyInput{
		CalculatedPlan:             CalculatedPlan{Plan: plan, Checksum: "checksum-interlock"},
		RunOneTimeInstructions:     true,
		OneTimeInstructionAttempts: 1,
	})
	if err == nil {
		t.Fatal("expected Apply to return an error while the interlock blocks")
	}
	if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
		t.Errorf("expected instruction to never run while blocked, but marker file exists (stat err: %v)", statErr)
	}
	activePath := filepath.Join(interlockDir, applyinatorActiveInterlockFile)
	if _, statErr := os.Stat(activePath); !os.IsNotExist(statErr) {
		t.Errorf("expected applyinator-active file to not be created when blocked, stat err: %v", statErr)
	}
}

// signalState describes how a Cancel/Pause channel is constructed for a test case.
type signalState int

const (
	signalNil signalState = iota
	signalOpen
	signalClosed
)

func newSignal(s signalState) <-chan struct{} {
	if s == signalNil {
		return nil
	}
	ch := make(chan struct{})
	if s == signalClosed {
		close(ch)
	}
	return ch
}

func TestCheckInterruptionPrecedence(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		cancel signalState
		pause  signalState
		want   Interruption
	}{
		{name: "nil channels are never ready", cancel: signalNil, pause: signalNil, want: InterruptionNone},
		{name: "closed cancel reports a cancellation", cancel: signalClosed, pause: signalNil, want: InterruptionCanceled},
		{name: "closed pause reports a pause", cancel: signalNil, pause: signalClosed, want: InterruptionPaused},
		{name: "cancel wins over a simultaneously closed pause", cancel: signalClosed, pause: signalClosed, want: InterruptionCanceled},
		{name: "open channels are not ready", cancel: signalOpen, pause: signalOpen, want: InterruptionNone},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cancel, pause := newSignal(tc.cancel), newSignal(tc.pause)
			// Repeated because a select over two ready channels picks pseudo-randomly: a single
			// observation would not prove that cancel deterministically wins over pause.
			for i := range 200 {
				if got := checkInterruption(cancel, pause); got != tc.want {
					t.Fatalf("iteration %d: expected %q, got %q", i, tc.want, got)
				}
			}
		})
	}
}

// waitForPath polls until path exists, failing the test if it has not appeared before the deadline.
func waitForPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s to exist", timeout, path)
}

// waitForGlob polls until at least one path matches pattern, failing the test if none does before
// the deadline. Used to observe an instruction starting: execute() creates the instruction's
// execution directory immediately before launching the command.
func waitForGlob(t *testing.T, pattern string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("bad glob pattern %q: %v", pattern, err)
		}
		if len(matches) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for a path matching %s", timeout, pattern)
}

func assertPathAbsent(t *testing.T, path, reason string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to not exist (%s), stat err: %v", path, reason, err)
	}
}

// touchCommand returns a command that creates sentinel and returns immediately.
func touchCommand(name, sentinel string) planapi.CommonInstruction {
	return planapi.CommonInstruction{Name: name, Command: "sh", Args: []string{"-c", "touch " + sentinel}}
}

// gatedTouchCommand returns a command that creates sentinel and then blocks until gate exists,
// giving a test a deterministic window during which the instruction is still running.
func gatedTouchCommand(name, sentinel, gate string) planapi.CommonInstruction {
	script := "touch " + sentinel + "; while [ ! -e " + gate + " ]; do sleep 0.02; done"
	return planapi.CommonInstruction{Name: name, Command: "sh", Args: []string{"-c", script}}
}

func touchInstruction(name, sentinel string) planapi.OneTimeInstruction {
	return planapi.OneTimeInstruction{CommonInstruction: touchCommand(name, sentinel)}
}

func gatedTouchInstruction(name, sentinel, gate string) planapi.OneTimeInstruction {
	return planapi.OneTimeInstruction{CommonInstruction: gatedTouchCommand(name, sentinel, gate)}
}

type applyResult struct {
	output ApplyOutput
	err    error
}

// applyAsync runs Apply on its own goroutine so the test can drive Cancel/Pause while it is in flight.
func applyAsync(a *Applyinator, input ApplyInput) <-chan applyResult {
	results := make(chan applyResult, 1)
	go func() {
		output, err := a.Apply(context.Background(), input)
		results <- applyResult{output: output, err: err}
	}()
	return results
}

func awaitApply(t *testing.T, results <-chan applyResult, timeout time.Duration) ApplyOutput {
	t.Helper()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("Apply returned error: %v", result.err)
		}
		return result.output
	case <-time.After(timeout):
		t.Fatalf("Apply did not return within %s", timeout)
		return ApplyOutput{}
	}
}

func TestApplyPreClosedCancelShortCircuitsBeforeTheLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	sentinel := filepath.Join(t.TempDir(), "instruction-ran")
	a := newTestApplyinator(t, "", false, "", "")
	// Holding the apply lock is what makes this test pin the short-circuit above a.mu.Lock():
	// Apply cannot reach any other code path while the lock is held.
	a.mu.Lock()
	defer a.mu.Unlock()

	cancel := make(chan struct{})
	close(cancel)
	plan := planapi.Plan{OneTimeInstructions: []planapi.OneTimeInstruction{touchInstruction("should-not-run", sentinel)}}

	output := awaitApply(t, applyAsync(a, ApplyInput{
		CalculatedPlan:               CalculatedPlan{Plan: plan, Checksum: "checksum-cancel-prelock"},
		RunOneTimeInstructions:       true,
		OneTimeInstructionAttempts:   1,
		ReconcileFiles:               true,
		Cancel:                       cancel,
		ResumeFromOneTimeInstruction: 3,
	}), 5*time.Second)

	if output.Interruption != InterruptionCanceled {
		t.Errorf("expected %q, got %q", InterruptionCanceled, output.Interruption)
	}
	if output.CompletedOneTimeInstructions != 3 {
		t.Errorf("expected the incoming checkpoint (3) to be reported unchanged, got %d", output.CompletedOneTimeInstructions)
	}
	assertPathAbsent(t, sentinel, "no instruction may run once the apply is already canceled")
}

func TestApplyRechecksInterruptionAfterAcquiringTheLock(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	appliedPlanDir := t.TempDir()
	filesDir := t.TempDir()
	filePath := filepath.Join(filesDir, "config.txt")

	a := newTestApplyinator(t, workDir, false, appliedPlanDir, "")
	// Hold the lock to simulate another local/remote apply already in flight. The queued Apply
	// call below passes its pre-lock interruption check (Cancel is not yet closed) and then blocks
	// on a.mu.Lock() until this test releases it.
	a.mu.Lock()

	cancel := make(chan struct{})
	plan := planapi.Plan{
		Files: []planapi.File{
			{
				Path:    filePath,
				Content: base64.StdEncoding.EncodeToString([]byte("hello")),
				UID:     -1,
				GID:     -1,
			},
		},
	}

	results := applyAsync(a, ApplyInput{
		CalculatedPlan: CalculatedPlan{Plan: plan, Checksum: "checksum-cancel-queued"},
		ReconcileFiles: true,
		Cancel:         cancel,
	})

	// Give the goroutine time to clear the pre-lock check and start waiting on a.mu.Lock().
	time.Sleep(100 * time.Millisecond)
	close(cancel)
	a.mu.Unlock()

	output := awaitApply(t, results, 5*time.Second)

	if output.Interruption != InterruptionCanceled {
		t.Errorf("expected %q, got %q", InterruptionCanceled, output.Interruption)
	}
	assertPathAbsent(t, filePath, "no file may be reconciled once the queued apply observes an already-pending cancel after the lock")
	archived, err := a.getAppliedPlanFiles()
	if err != nil {
		t.Fatalf("getAppliedPlanFiles returned error: %v", err)
	}
	if len(archived) != 0 {
		t.Errorf("expected no plan to be archived once the queued apply observes an already-pending cancel after the lock, got %d", len(archived))
	}
}

func TestApplyPauseStopsBeforeTheNextInstruction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	dir := t.TempDir()
	firstSentinel := filepath.Join(dir, "first-ran")
	secondSentinel := filepath.Join(dir, "second-ran")
	periodicSentinel := filepath.Join(dir, "periodic-ran")
	gate := filepath.Join(dir, "gate")

	a := newTestApplyinator(t, "", false, "", "")
	plan := planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			gatedTouchInstruction("first", firstSentinel, gate),
			touchInstruction("second", secondSentinel),
		},
		// Present so this test also pins the rule that an interrupted one-time set suppresses the
		// periodic instructions: running them would execute work the operator asked to stop.
		PeriodicInstructions: []planapi.PeriodicInstruction{
			{CommonInstruction: touchCommand("periodic", periodicSentinel)},
		},
	}

	pause := make(chan struct{})
	results := applyAsync(a, ApplyInput{
		CalculatedPlan:             CalculatedPlan{Plan: plan, Checksum: "checksum-pause"},
		RunOneTimeInstructions:     true,
		OneTimeInstructionAttempts: 1,
		Pause:                      pause,
	})

	// Pause while instruction 0 is still running: it must be allowed to finish, and instruction 1
	// must never start.
	waitForPath(t, firstSentinel, 30*time.Second)
	close(pause)
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}

	output := awaitApply(t, results, 30*time.Second)
	if output.Interruption != InterruptionPaused {
		t.Errorf("expected %q, got %q", InterruptionPaused, output.Interruption)
	}
	if output.CompletedOneTimeInstructions != 1 {
		t.Errorf("expected CompletedOneTimeInstructions=1, got %d", output.CompletedOneTimeInstructions)
	}
	if !output.OneTimeApplySucceeded {
		t.Error("expected a pause with no failure to still report OneTimeApplySucceeded=true")
	}
	assertPathAbsent(t, secondSentinel, "a pause stops before the next instruction")
	assertPathAbsent(t, periodicSentinel, "an interrupted one-time set skips the periodic instructions entirely")
	// The sentinel above cannot distinguish "Apply returned before calling runPeriodicInstructions"
	// from "runPeriodicInstructions ran and broke at its own boundary check", because the pause is
	// still pending either way. These two assertions can: a periodic pass that runs -- even a
	// no-op one -- replaces PeriodicOutput with an encoded empty map and sets PeriodicApplySucceeded.
	// An abandoned one-time set must leave the caller's recorded periodic state untouched instead.
	if output.PeriodicOutput != nil {
		t.Errorf("expected PeriodicOutput to be left as the caller passed it (nil), got %d bytes", len(output.PeriodicOutput))
	}
	if output.PeriodicApplySucceeded {
		t.Error("expected PeriodicApplySucceeded=false: no periodic pass may run once the one-time set is interrupted")
	}
}

func TestApplyResumeFromSkipsAlreadyCompletedInstructions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	dir := t.TempDir()
	firstSentinel := filepath.Join(dir, "first-ran")
	secondSentinel := filepath.Join(dir, "second-ran")

	a := newTestApplyinator(t, "", false, "", "")
	plan := planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			touchInstruction("first", firstSentinel),
			touchInstruction("second", secondSentinel),
		},
	}

	output, err := a.Apply(context.Background(), ApplyInput{
		CalculatedPlan:               CalculatedPlan{Plan: plan, Checksum: "checksum-resume"},
		RunOneTimeInstructions:       true,
		OneTimeInstructionAttempts:   1,
		ResumeFromOneTimeInstruction: 1,
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if output.Interruption != InterruptionNone {
		t.Errorf("expected no interruption, got %q", output.Interruption)
	}
	if output.CompletedOneTimeInstructions != 2 {
		t.Errorf("expected CompletedOneTimeInstructions=2, got %d", output.CompletedOneTimeInstructions)
	}
	assertPathAbsent(t, firstSentinel, "instructions below the resume index are treated as complete")
	waitForPath(t, secondSentinel, time.Second)
}

func TestApplyCompletedOneTimeInstructionsIsAbsoluteAcrossResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	dir := t.TempDir()
	sentinel := func(i int) string { return filepath.Join(dir, "ran-"+strconv.Itoa(i)) }
	firstGate := filepath.Join(dir, "gate-1")
	secondGate := filepath.Join(dir, "gate-2")

	a := newTestApplyinator(t, "", false, "", "")
	plan := planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			touchInstruction("zero", sentinel(0)),
			gatedTouchInstruction("one", sentinel(1), firstGate),
			gatedTouchInstruction("two", sentinel(2), secondGate),
			touchInstruction("three", sentinel(3)),
		},
	}
	cp := CalculatedPlan{Plan: plan, Checksum: "checksum-absolute-checkpoint"}

	// First cycle: pause while instruction 1 runs, so it completes and instruction 2 never starts.
	pause := make(chan struct{})
	results := applyAsync(a, ApplyInput{
		CalculatedPlan:             cp,
		RunOneTimeInstructions:     true,
		OneTimeInstructionAttempts: 1,
		Pause:                      pause,
	})
	waitForPath(t, sentinel(1), 30*time.Second)
	close(pause)
	if err := os.WriteFile(firstGate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	first := awaitApply(t, results, 30*time.Second)
	if first.Interruption != InterruptionPaused {
		t.Fatalf("expected the first cycle to report %q, got %q", InterruptionPaused, first.Interruption)
	}
	if first.CompletedOneTimeInstructions != 2 {
		t.Fatalf("expected the first cycle checkpoint to be 2, got %d", first.CompletedOneTimeInstructions)
	}
	assertPathAbsent(t, sentinel(2), "the first cycle paused before instruction 2")

	// Second cycle: resume at the reported checkpoint and pause again one instruction later. The
	// checkpoint must compose (3), not restart from this cycle's own count (1).
	resumePause := make(chan struct{})
	results = applyAsync(a, ApplyInput{
		CalculatedPlan:               cp,
		RunOneTimeInstructions:       true,
		OneTimeInstructionAttempts:   1,
		Pause:                        resumePause,
		ResumeFromOneTimeInstruction: first.CompletedOneTimeInstructions,
	})
	waitForPath(t, sentinel(2), 30*time.Second)
	close(resumePause)
	if err := os.WriteFile(secondGate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	second := awaitApply(t, results, 30*time.Second)

	if second.Interruption != InterruptionPaused {
		t.Errorf("expected the second cycle to report %q, got %q", InterruptionPaused, second.Interruption)
	}
	if second.CompletedOneTimeInstructions != 3 {
		t.Errorf("expected the resumed checkpoint to be absolute (3), got %d", second.CompletedOneTimeInstructions)
	}
	assertPathAbsent(t, sentinel(3), "the second cycle paused before instruction 3")
}

func TestApplyCancelDuringLongRunningInstructionReturnsPromptly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	const checksum = "checksum-cancel-inflight"
	workDir := t.TempDir()
	a := newTestApplyinator(t, workDir, false, "", "")
	plan := planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "sleeper", Command: "sh", Args: []string{"-c", "sleep 60"}}},
		},
	}

	cancel := make(chan struct{})
	started := time.Now()
	results := applyAsync(a, ApplyInput{
		CalculatedPlan:             CalculatedPlan{Plan: plan, Checksum: checksum},
		RunOneTimeInstructions:     true,
		OneTimeInstructionAttempts: 1,
		Cancel:                     cancel,
	})

	// execute() creates the instruction's execution directory immediately before launching the
	// command, so its appearance means the sleep is about to be (or already is) in flight.
	waitForGlob(t, filepath.Join(workDir, "*", checksum+"_0"), 30*time.Second)
	close(cancel)

	output := awaitApply(t, results, 10*time.Second)
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("expected Apply to return well inside the 60s sleep, took %s", elapsed)
	}
	if output.Interruption != InterruptionCanceled {
		t.Errorf("expected %q, got %q", InterruptionCanceled, output.Interruption)
	}
	if output.CompletedOneTimeInstructions != 0 {
		t.Errorf("expected the killed instruction to not advance the checkpoint, got %d", output.CompletedOneTimeInstructions)
	}
	// A killed instruction is still a failed instruction, so OneTimeApplySucceeded is false here.
	// "A cancel-induced kill must not be reported as a plan failure" is therefore an obligation on
	// the caller: it has to test Interruption BEFORE OneTimeApplySucceeded, or it will record a
	// canceled plan as failed. Pinned so the downstream reconcile task can rely on this ordering.
	if output.OneTimeApplySucceeded {
		t.Error("expected OneTimeApplySucceeded=false for a cancel-killed instruction; callers must check Interruption first")
	}
}

func TestApplyFailureIsNotReportedAsAnInterruption(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	a := newTestApplyinator(t, "", false, "", "")
	plan := planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "fails", Command: "sh", Args: []string{"-c", "exit 1"}}},
		},
	}

	output, err := a.Apply(context.Background(), ApplyInput{
		CalculatedPlan:             CalculatedPlan{Plan: plan, Checksum: "checksum-failure-not-interruption"},
		RunOneTimeInstructions:     true,
		OneTimeInstructionAttempts: 1,
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if output.OneTimeApplySucceeded {
		t.Error("expected OneTimeApplySucceeded=false")
	}
	if output.Interruption != InterruptionNone {
		t.Errorf("expected a plain failure to report no interruption, got %q", output.Interruption)
	}
	if output.CompletedOneTimeInstructions != 0 {
		t.Errorf("expected a failed instruction to not advance the checkpoint, got %d", output.CompletedOneTimeInstructions)
	}
}

// TestRunOneTimeInstructionsPauseDuringAGenuineFailureIsNotReportedAsPaused is a regression test:
// pause never interrupts a running instruction, so a failure the instruction was always going to
// produce must remain a genuine failure even if the operator's pause happens to land while it is
// still running. The instruction sleeps briefly so there is a real window in which the pause
// channel closes mid-execution, rather than before the boundary check that starts it.
func TestRunOneTimeInstructionsPauseDuringAGenuineFailureIsNotReportedAsPaused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	a := NewApplyinator(t.TempDir(), false, "", "", nil)
	cp := CalculatedPlan{
		Checksum: "checksum-pause-during-genuine-failure",
		Plan: planapi.Plan{
			OneTimeInstructions: []planapi.OneTimeInstruction{
				{CommonInstruction: planapi.CommonInstruction{Name: "fails-slowly", Command: "sh", Args: []string{"-c", "sleep 0.2; exit 1"}}},
			},
		},
	}

	pause := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(pause)
	}()

	result, err := a.runOneTimeInstructions(context.Background(), t.TempDir(), cp, nil, 1, 0, nil, pause)
	if err != nil {
		t.Fatalf("runOneTimeInstructions returned error: %v", err)
	}
	if result.Succeeded {
		t.Error("expected Succeeded=false")
	}
	if result.Interruption != InterruptionNone {
		t.Errorf("expected the genuine failure to remain authoritative even though pause became pending mid-execution, got %q",
			result.Interruption)
	}
}

// TestApplyPauseDuringPeriodicPassAfterAGenuineOneTimeFailureIsNotReportedAsPaused is a regression
// test for Apply's own final interruption check. Periodic instructions run regardless of whether
// the one-time pass failed, and a pause that happens to land during that periodic pass must not
// retroactively relabel an already-genuine one-time failure as an interruption.
func TestApplyPauseDuringPeriodicPassAfterAGenuineOneTimeFailureIsNotReportedAsPaused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	dir := t.TempDir()
	periodicRan := filepath.Join(dir, "periodic-ran")
	a := newTestApplyinator(t, "", false, "", "")
	plan := planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "fails", Command: "sh", Args: []string{"-c", "exit 1"}}},
		},
		PeriodicInstructions: []planapi.PeriodicInstruction{
			{CommonInstruction: planapi.CommonInstruction{Name: "slow-periodic", Command: "sh", Args: []string{"-c", "sleep 0.2; touch " + periodicRan}}},
		},
	}

	pause := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(pause)
	}()

	output, err := a.Apply(context.Background(), ApplyInput{
		CalculatedPlan:             CalculatedPlan{Plan: plan, Checksum: "checksum-pause-after-onetime-failure"},
		RunOneTimeInstructions:     true,
		OneTimeInstructionAttempts: 1,
		Pause:                      pause,
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if output.OneTimeApplySucceeded {
		t.Error("expected OneTimeApplySucceeded=false")
	}
	// Apply is synchronous, so by the time it returns the periodic instruction has already run;
	// this confirms the pause genuinely landed during that pass rather than before it started.
	if _, statErr := os.Stat(periodicRan); statErr != nil {
		t.Fatalf("expected the periodic instruction to have run, sentinel missing: %v", statErr)
	}
	if output.Interruption != InterruptionNone {
		t.Errorf("expected the genuine one-time failure to remain authoritative even though pause became pending during "+
			"the periodic pass that follows it, got %q", output.Interruption)
	}
}

func TestRunOneTimeInstructionsOutOfRangeResumeIndex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	testCases := []struct {
		name         string
		resumeFrom   int
		wantExecuted bool
	}{
		{name: "resume exactly at the instruction count runs nothing", resumeFrom: 2, wantExecuted: false},
		{name: "resume past the last instruction runs nothing", resumeFrom: 5, wantExecuted: false},
		{name: "negative resume index starts from the first instruction", resumeFrom: -1, wantExecuted: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			first, second := filepath.Join(dir, "first-ran"), filepath.Join(dir, "second-ran")
			a := NewApplyinator(t.TempDir(), false, "", "", nil)
			cp := CalculatedPlan{
				Checksum: "checksum-resume-range",
				Plan: planapi.Plan{
					OneTimeInstructions: []planapi.OneTimeInstruction{
						touchInstruction("first", first),
						touchInstruction("second", second),
					},
				},
			}

			result, err := a.runOneTimeInstructions(context.Background(), t.TempDir(), cp, nil, 1, tc.resumeFrom, nil, nil)
			if err != nil {
				t.Fatalf("runOneTimeInstructions returned error: %v", err)
			}
			if !result.Succeeded {
				t.Error("expected Succeeded=true")
			}
			if result.Interruption != InterruptionNone {
				t.Errorf("expected no interruption, got %q", result.Interruption)
			}
			// Either nothing ran or everything ran, so the checkpoint is the full instruction count both ways.
			if result.Completed != 2 {
				t.Errorf("expected Completed=2, got %d", result.Completed)
			}
			if !tc.wantExecuted {
				assertPathAbsent(t, first, "an out-of-range resume index must not execute anything")
				assertPathAbsent(t, second, "an out-of-range resume index must not execute anything")
				return
			}
			waitForPath(t, first, time.Second)
			waitForPath(t, second, time.Second)
		})
	}
}

func TestRunPeriodicInstructionsStopsWhenInterrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	testCases := []struct {
		name   string
		cancel signalState
		pause  signalState
	}{
		{name: "a pending pause stops before the next instruction", cancel: signalNil, pause: signalClosed},
		{name: "a pending cancel stops before the next instruction", cancel: signalClosed, pause: signalNil},
		{name: "open channels do not stop anything", cancel: signalOpen, pause: signalOpen},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			first, second := filepath.Join(dir, "first-ran"), filepath.Join(dir, "second-ran")
			a := NewApplyinator(t.TempDir(), false, "", "", nil)
			cp := CalculatedPlan{
				Checksum: "checksum-periodic-interrupted",
				Plan: planapi.Plan{
					PeriodicInstructions: []planapi.PeriodicInstruction{
						{CommonInstruction: touchCommand("first", first)},
						{CommonInstruction: touchCommand("second", second)},
					},
				},
			}

			periodic, err := a.runPeriodicInstructions(context.Background(), t.TempDir(), cp, nil, false, time.Now(),
				newSignal(tc.cancel), newSignal(tc.pause))
			if err != nil {
				t.Fatalf("runPeriodicInstructions returned error: %v", err)
			}
			output, succeeded := periodic.Output, periodic.Succeeded
			if !succeeded {
				t.Error("expected succeeded=true: an interruption is not a failure, and nothing that ran failed")
			}

			outputs := decodePeriodicOutputs(t, output)
			interrupted := tc.cancel == signalClosed || tc.pause == signalClosed
			if !interrupted {
				waitForPath(t, first, time.Second)
				waitForPath(t, second, time.Second)
				if len(outputs) != 2 {
					t.Errorf("expected both instructions to be recorded, got %v", outputs)
				}
				return
			}
			assertPathAbsent(t, first, "an interruption pending at entry stops before the first periodic instruction")
			assertPathAbsent(t, second, "an interruption pending at entry stops before the first periodic instruction")
			if len(outputs) != 0 {
				t.Errorf("expected no periodic instruction to be recorded, got %v", outputs)
			}
		})
	}
}

func TestApplyPauseDuringPeriodicInstructionsIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	dir := t.TempDir()
	firstSentinel := filepath.Join(dir, "first-ran")
	secondSentinel := filepath.Join(dir, "second-ran")
	gate := filepath.Join(dir, "gate")

	a := newTestApplyinator(t, "", false, "", "")
	// RunOneTimeInstructions is false, so the interruption below can only reach ApplyOutput through
	// Apply's re-check after runPeriodicInstructions returns: periodic instructions have no
	// checkpoint and their runner does not report an interruption itself.
	plan := planapi.Plan{
		PeriodicInstructions: []planapi.PeriodicInstruction{
			{CommonInstruction: gatedTouchCommand("first", firstSentinel, gate)},
			{CommonInstruction: touchCommand("second", secondSentinel)},
		},
	}

	pause := make(chan struct{})
	results := applyAsync(a, ApplyInput{
		CalculatedPlan:               CalculatedPlan{Plan: plan, Checksum: "checksum-periodic-pause"},
		RunOneTimeInstructions:       false,
		Pause:                        pause,
		ResumeFromOneTimeInstruction: 2,
	})

	waitForPath(t, firstSentinel, 30*time.Second)
	close(pause)
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}

	output := awaitApply(t, results, 30*time.Second)
	if output.Interruption != InterruptionPaused {
		t.Errorf("expected %q, got %q", InterruptionPaused, output.Interruption)
	}
	if !output.PeriodicApplySucceeded {
		t.Error("expected a pause with no failure to still report PeriodicApplySucceeded=true")
	}
	if output.CompletedOneTimeInstructions != 2 {
		t.Errorf("expected the one-time checkpoint to be reported unchanged (2), got %d", output.CompletedOneTimeInstructions)
	}
	assertPathAbsent(t, secondSentinel, "a pause stops before the next periodic instruction")
}

// assertFileStopsGrowing samples path's size and fails as soon as it grows during window. Used to
// prove a backgrounded descendant of a canceled instruction is really dead rather than orphaned:
// a fixed wait followed by a single comparison would prove the same thing, but this reports the
// failure the moment it happens instead of always burning the whole window.
func assertFileStopsGrowing(t *testing.T, path string, window time.Duration) {
	t.Helper()
	initial, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		current, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if current.Size() != initial.Size() {
			t.Fatalf("expected %s to stop growing once the instruction was canceled, but it grew from %d to %d bytes: "+
				"a descendant of the canceled instruction is still running", path, initial.Size(), current.Size())
		}
	}
}

// trappingCommand returns a command that installs a SIGTERM trap, announces itself by creating
// started, and then idles. On SIGTERM it creates marker and exits 143 (128+SIGTERM), the exit
// status a shell reports for a terminated process. The idle loop sleeps in short bursts because a
// POSIX shell defers trap handlers until the current foreground command finishes.
func trappingCommand(name, marker, started string) planapi.CommonInstruction {
	script := "trap 'touch " + marker + "; exit 143' TERM; touch " + started + "; while true; do sleep 0.05; done"
	return planapi.CommonInstruction{Name: name, Command: "sh", Args: []string{"-c", script}}
}

// backgroundedWriterCommand returns a command whose *grandchild* appends to sentinel forever while
// the direct child sleeps for a minute. Cancelling it exercises two mechanisms at once: the signal
// has to reach the whole process tree (the grandchild is not the process *exec.Cmd knows about),
// and the grandchild inherits the stdout/stderr pipes, so execute's eg.Wait() cannot return while
// it is alive.
//
// started is created by the grandchild rather than by the direct child, and only after its first
// append, so waiting on it proves the grandchild is genuinely running and sentinel already exists.
func backgroundedWriterCommand(name, sentinel, started string) planapi.CommonInstruction {
	script := "sh -c 'while true; do echo x >> " + sentinel + "; touch " + started + "; sleep 0.05; done' & sleep 60"
	return planapi.CommonInstruction{Name: name, Command: "sh", Args: []string{"-c", script}}
}

// cancelDuringInstruction runs a single-instruction plan, waits for started to appear, closes
// Cancel, and returns the ApplyOutput. Shared by the process-tree termination tests below.
func cancelDuringInstruction(t *testing.T, checksum string, instruction planapi.CommonInstruction, started string) ApplyOutput {
	t.Helper()

	a := newTestApplyinator(t, "", false, "", "")
	plan := planapi.Plan{OneTimeInstructions: []planapi.OneTimeInstruction{{CommonInstruction: instruction}}}

	cancel := make(chan struct{})
	results := applyAsync(a, ApplyInput{
		CalculatedPlan:             CalculatedPlan{Plan: plan, Checksum: checksum},
		RunOneTimeInstructions:     true,
		OneTimeInstructionAttempts: 1,
		Cancel:                     cancel,
	})

	waitForPath(t, started, 30*time.Second)
	close(cancel)

	// This deadline is the real assertion that the apply does not hang: the instructions used here
	// outlive it by design (sleep 60), so awaitApply fails the test if termination did not work.
	// 20s rather than a tight bound because the watchdog's escalation path can legitimately take
	// instructionTerminationGrace before the kill, and this must not be flaky on a loaded machine.
	return awaitApply(t, results, 20*time.Second)
}

func TestApplyCancelSendsSIGTERMBeforeKilling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	// Signal files live outside the work directory because Apply wipes the work directory.
	dir := t.TempDir()
	marker := filepath.Join(dir, "term-received")
	started := filepath.Join(dir, "started")

	output := cancelDuringInstruction(t, "checksum-cancel-sigterm", trappingCommand("trapper", marker, started), started)

	// The trap has run by the time Apply returns, but poll anyway so this never depends on the
	// exact interleaving of the shell's exit and cmd.Wait() returning.
	waitForPath(t, marker, 5*time.Second)

	if output.Interruption != InterruptionCanceled {
		t.Errorf("expected %q, got %q", InterruptionCanceled, output.Interruption)
	}
}

func TestApplyCancelKillsTheInstructionsGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	t.Parallel()

	dir := t.TempDir()
	sentinel := filepath.Join(dir, "grandchild-writes")
	started := filepath.Join(dir, "started")

	instruction := backgroundedWriterCommand("backgrounder", sentinel, started)
	output := cancelDuringInstruction(t, "checksum-cancel-grandchild", instruction, started)

	if output.Interruption != InterruptionCanceled {
		t.Errorf("expected %q, got %q", InterruptionCanceled, output.Interruption)
	}
	// The grandchild appends every 50ms, so a full second of a static file size means it is gone
	// rather than merely descheduled.
	assertFileStopsGrowing(t, sentinel, time.Second)
	if output.TerminationIncomplete {
		t.Error("expected the process tree to be confirmed gone, got TerminationIncomplete=true")
	}
}

// TestApplyCancelKillsAGrandchildThatIgnoresSIGTERM drives the escalation path all the way through
// Apply: the grandchild ignores the graceful signal, so only the SIGKILL that follows the grace
// period can stop it.
//
// What it covers depends on the platform, so do not read a pass on Linux as proof of more than it is.
// By the time the watchdog escalates, the direct child has already died from the SIGTERM but
// cmd.Wait() has not reaped it, because it is blocked behind output pipes the grandchild still holds,
// leaving a zombie group leader. On darwin getpgid reports ESRCH for that zombie, so before the group
// id was captured at start the kill degraded into a signal aimed at the already-dead direct child and
// this test failed with the grandchild still writing. Linux answers getpgid for a zombie, so it passed
// there either way, and on the deployment platforms this is coverage of the escalation path rather
// than a reproduction of a bug they had.
func TestApplyCancelKillsAGrandchildThatIgnoresSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	// Not parallel: withTerminationGrace writes a package-level var. Shortened so the test does not
	// wait out the full production grace period before the kill.
	withTerminationGrace(t, 500*time.Millisecond)

	dir := t.TempDir()
	sentinel := filepath.Join(dir, "grandchild-writes")
	started := filepath.Join(dir, "started")

	// `trap "" TERM` makes SIGTERM ignored rather than merely handled, so no amount of waiting will
	// end this grandchild. The direct child sleeps in the foreground, as in backgroundedWriterCommand.
	script := `sh -c 'trap "" TERM; while true; do echo x >> ` + sentinel + `; touch ` + started + `; sleep 0.05; done' & sleep 60`
	instruction := planapi.CommonInstruction{Name: "term-ignorer", Command: "sh", Args: []string{"-c", script}}

	output := cancelDuringInstruction(t, "checksum-cancel-sigterm-ignored", instruction, started)

	if output.Interruption != InterruptionCanceled {
		t.Errorf("expected %q, got %q", InterruptionCanceled, output.Interruption)
	}
	assertFileStopsGrowing(t, sentinel, time.Second)
	if output.TerminationIncomplete {
		t.Error("expected the killed process tree to be confirmed gone, got TerminationIncomplete=true")
	}
}

// recordingCloser stands in for one of execute's stdout/stderr pipes and records whether
// watchForTermination closed it. Closing the pipes is the watchdog's escalation-only behaviour, so
// the call count is the only observable difference between the graceful and the escalated path.
type recordingCloser struct {
	mu      sync.Mutex
	closes  int
	closedA time.Time
}

func (r *recordingCloser) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes++
	if r.closes == 1 {
		r.closedA = time.Now()
	}
	return nil
}

func (r *recordingCloser) state() (int, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closes, r.closedA
}

// startWatchedCommand launches a shell that installs trapAction as its SIGTERM handler and then
// idles, under the same process-group setup execute uses, and arms a watchdog on it. It returns the
// cancel func, the recorded pipe, a channel carrying cmd.Wait()'s result, and the watchdog's stop
// func.
//
// It does not return until the trap is demonstrably installed. cmd.Start() returns as soon as
// fork/exec succeeds, well before sh has parsed the trap, so a test that canceled immediately
// would signal a shell still running the *default* SIGTERM disposition: it would die instantly and
// every case would silently observe the graceful path, whichever handler it asked for.
func startWatchedCommand(t *testing.T, trapAction string) (context.CancelFunc, *recordingCloser, <-chan error, func() bool) {
	t.Helper()

	installed := filepath.Join(t.TempDir(), "trap-installed")
	script := "trap " + trapAction + " TERM; touch " + installed + "; while true; do sleep 0.02; done"

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

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	waitForPath(t, installed, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	pipe := &recordingCloser{}
	stop := watchForTermination(ctx, cmd, pipe)
	t.Cleanup(func() {
		cancel()
		stop()
	})
	return cancel, pipe, waited, stop
}

// withTerminationGrace shortens the watchdog's escalation grace for the duration of the test and
// restores it afterwards.
//
// The t.Setenv call sets nothing anybody reads: it is a tripwire. t.Setenv panics if the test, or
// any ancestor of it, has called t.Parallel() — which is exactly the condition under which writing
// this package-level var from a test becomes a data race with every other watchdog the package
// arms. CI runs no -race job, so this panic is the only thing that would catch a future parallel
// test reaching for this helper.
//
// Call it before anything whose cleanup reads the var — the watchdog's own cancel()/stop() — so
// LIFO restores the original only after those have finished.
func withTerminationGrace(t *testing.T, d time.Duration) {
	t.Helper()
	t.Setenv("APPLYINATOR_GRACE_GUARD", "1")

	original := instructionTerminationGrace
	instructionTerminationGrace = d
	t.Cleanup(func() { instructionTerminationGrace = original })
}

// withProcessTreeExitTimeout bounds the watchdog's final exit confirmation for the duration of the
// test and restores it afterwards. Same t.Setenv tripwire, for the same reason, as
// withTerminationGrace.
func withProcessTreeExitTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	t.Setenv("APPLYINATOR_EXIT_TIMEOUT_GUARD", "1")

	original := processTreeExitTimeout
	processTreeExitTimeout = d
	t.Cleanup(func() { processTreeExitTimeout = original })
}

// TestWatchForTerminationClosesThePipesOnlyWhenItEscalates pins both halves of the watchdog's pipe
// handling, which is the one part of it a canceled Apply cannot observe: on Unix the graceful
// SIGTERM already reaches the whole process group, so the pipes reach EOF on their own and closing
// them explicitly makes no observable difference at the Apply level.
//
// The two halves matter for different reasons. Closing on escalation is what stops a surviving
// descendant that inherited the write ends from blocking execute's eg.Wait() forever. NOT closing
// on the graceful path is what stops a well-behaved instruction's final output being truncated.
//
// Not parallel: it rewrites the package-level instructionTerminationGrace, and Go runs every
// non-parallel test body to completion before resuming any parallel one, so this cannot race with
// the watchdogs the other tests arm. withTerminationGrace enforces that rather than merely
// documenting it.
func TestWatchForTerminationClosesThePipesOnlyWhenItEscalates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}

	testCases := []struct {
		name string
		// trapAction is the command's response to SIGTERM, and is what selects the path under test.
		trapAction string
		grace      time.Duration
		wantClosed bool
		reason     string
	}{
		{
			name:       "graceful exit leaves the pipes open",
			trapAction: `'exit 0'`,
			grace:      5 * time.Second,
			wantClosed: false,
			reason:     "a well-behaved instruction's final output must not be truncated",
		},
		{
			name:       "ignoring SIGTERM escalates to a kill and closes the pipes",
			trapAction: `''`,
			grace:      200 * time.Millisecond,
			wantClosed: true,
			reason:     "a descendant holding the write ends would block execute's eg.Wait() forever",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Registered before startWatchedCommand, whose cleanup cancels the watchdog and waits
			// for it: LIFO means the grace is restored only once nothing is still reading it.
			withTerminationGrace(t, tc.grace)

			cancel, pipe, waited, stop := startWatchedCommand(t, tc.trapAction)

			canceledAt := time.Now()
			cancel()

			// The command idles forever on its own, so this only returns because the watchdog
			// terminated or killed it.
			select {
			case <-waited:
			case <-time.After(30 * time.Second):
				t.Fatal("the watchdog neither terminated nor killed the command")
			}

			// stop() waits for the watchdog goroutine to finish, and the pipe closes are the last
			// thing it does, so no polling is needed: the count is final once stop() returns.
			stop()

			closes, closedAt := pipe.state()
			if tc.wantClosed && closes == 0 {
				t.Fatalf("expected the pipes to be closed when the watchdog escalated to a kill: %s", tc.reason)
			}
			if !tc.wantClosed && closes != 0 {
				t.Fatalf("expected the pipes to be left open on the graceful path, got %d Close calls: %s", closes, tc.reason)
			}
			if !tc.wantClosed {
				return
			}
			if elapsed := closedAt.Sub(canceledAt); elapsed < tc.grace {
				t.Errorf("expected the pipes to be closed only after the %s grace elapsed, closed after %s", tc.grace, elapsed)
			}
		})
	}
}

func TestWatchForTerminationStopIsIdempotent(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		alreadyCanceled bool
	}{
		{name: "context still open", alreadyCanceled: false},
		{name: "context already canceled", alreadyCanceled: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.alreadyCanceled {
				cancel()
			}

			// Deliberately never started: every platform helper has to tolerate a nil Process
			// rather than panicking a root daemon.
			stop := watchForTermination(ctx, exec.Command("true"))

			returned := make(chan struct{})
			go func() {
				defer close(returned)
				stop()
				stop()
			}()

			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Fatal("stop() did not return: it must be idempotent and must not block on the termination grace period")
			}
		})
	}
}
