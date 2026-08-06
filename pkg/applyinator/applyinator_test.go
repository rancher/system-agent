package applyinator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := NewApplyinator(t.TempDir(), false, "", "", nil)
			instruction := planapi.CommonInstruction{
				Command: "sh",
				Args:    []string{"-c", tc.script},
			}

			stdout, stderr, exitCode, err := a.execute(context.Background(), "test", t.TempDir(), instruction, tc.combinedOutput, 1)
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

	stdout, _, exitCode, err := a.execute(context.Background(), "test", executionDir, instruction, false, 5)
	if err != nil || exitCode != 0 {
		t.Fatalf("unexpected failure: exitCode=%d err=%v", exitCode, err)
	}

	got := string(stdout)
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

	stdout, _, exitCode, err := a.execute(context.Background(), "test", executionDir, instruction, false, 1)
	if err != nil || exitCode != 0 {
		t.Fatalf("unexpected failure: exitCode=%d err=%v", exitCode, err)
	}
	if !strings.Contains(string(stdout), "ran-default") {
		t.Errorf("expected default run.sh to execute, got stdout=%q", stdout)
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
			name: "unparsable last failed run time is treated as no history but Failures is still carried",
			prev: planapi.PeriodicInstructionOutput{
				LastFailedRunTime: "not-a-time",
				Failures:          5,
			},
			wantDue:      true,
			wantFailures: 5,
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
