package applyinator

import (
	"bytes"
	"context"
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
