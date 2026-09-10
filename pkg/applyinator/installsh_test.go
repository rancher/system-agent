package applyinator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installShPath is the installer whose interlock handling must stay in step with
// this package. The agent writes applyinator-active and install.sh reads it, in
// two languages with no shared definition, so the format is pinned by test.
const installShPath = "../../install.sh"

// sedPidExpr is the exact expression install.sh uses to pull the pid back out of
// the interlock file. Tests run it rather than reimplementing it, so the contract
// is checked against the real extractor.
const sedPidExpr = `sed -n 's/^pid=//p'`

// runInstallShFunc evaluates install.sh up to (but not including) do_install,
// which defines its helper functions without running the installer, then invokes
// the given snippet. Returns combined output.
func runInstallShFunc(t *testing.T, env []string, snippet string) (string, error) {
	t.Helper()
	abs, err := filepath.Abs(installShPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("install.sh not found at %s: %v", abs, err)
	}

	harness := fmt.Sprintf("set -e\neval \"$(sed '/^do_install/,$d' %q)\"\n%s\n", abs, snippet)
	cmd := exec.Command("sh", "-c", harness)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// interlockDirWith creates a var dir containing interlock/applyinator-active with
// the given contents, and returns the var dir and the interlock file path. Pass a
// nil contents to create the directory without the file.
func interlockDirWith(t *testing.T, contents []byte) (varDir, active string) {
	t.Helper()
	varDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(varDir, "interlock"), 0700); err != nil {
		t.Fatal(err)
	}
	active = filepath.Join(varDir, "interlock", "applyinator-active")
	if contents != nil {
		if err := os.WriteFile(active, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return varDir, active
}

// stampFor renders the interlock contents the agent writes, for an arbitrary pid.
func stampFor(pid int) []byte {
	return []byte(fmt.Sprintf("pid=%d\ntime=%s\n", pid, time.Now().UTC().Format(time.UnixDate)))
}

// livePID starts a child that stays alive for the duration of the test.
func livePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("unable to start helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// deadPID starts a child, waits for it to exit, and returns its pid. The pid is
// reaped, so it is genuinely gone rather than merely improbable.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("unable to start helper process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process did not exit cleanly: %v", err)
	}
	return pid
}

func mustNotExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s: expected %s to be gone, stat err = %v", what, path, err)
	}
}

// TestActiveInterlockIsPidStamped is the agent's half of the contract: the file
// checkInterlock writes must yield this process's pid when run through the very
// sed expression install.sh uses. Without it install.sh silently falls back to
// waiting out the full timeout, which is the five-minute stall this exists to
// avoid -- and nothing would log an error.
func TestActiveInterlockIsPidStamped(t *testing.T) {
	interlockDir := t.TempDir()
	a := newTestApplyinator(t, "", false, "", interlockDir)

	cleanup, err := a.checkInterlock(time.Now())
	if err != nil {
		t.Fatalf("checkInterlock returned an unexpected error: %v", err)
	}
	defer cleanup()

	active := filepath.Join(interlockDir, applyinatorActiveInterlockFile)
	out, err := exec.Command("sh", "-c", fmt.Sprintf("%s %q | head -1", sedPidExpr, active)).Output()
	if err != nil {
		t.Fatalf("extracting the pid the way install.sh does failed: %v", err)
	}
	if got, want := strings.TrimSpace(string(out)), fmt.Sprint(os.Getpid()); got != want {
		contents, _ := os.ReadFile(active)
		t.Errorf("install.sh would read pid %q, want %q; file was:\n%s", got, want, contents)
	}
}

// TestInstallShEnsureApplyinatorNotActiveDeadOwner: the five-minute wait was the
// user-visible cost of the leaked interlock. A dead owner must short-circuit it.
func TestInstallShEnsureApplyinatorNotActiveDeadOwner(t *testing.T) {
	dir, active := interlockDirWith(t, stampFor(deadPID(t)))

	start := time.Now()
	out, err := runInstallShFunc(t, []string{"CATTLE_AGENT_VAR_DIR=" + dir}, "ensure_applyinator_not_active")
	if err != nil {
		t.Fatalf("ensure_applyinator_not_active failed: %v\n%s", err, out)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v; a dead owner must not be waited on", elapsed)
	}
	if !strings.Contains(out, "no longer running") {
		t.Errorf("expected the stale-owner message, got:\n%s", out)
	}
	mustNotExist(t, active, "stale interlock after ensure_applyinator_not_active")
}

func TestInstallShEnsureApplyinatorNotActiveNoFile(t *testing.T) {
	dir, _ := interlockDirWith(t, nil)

	start := time.Now()
	out, err := runInstallShFunc(t, []string{"CATTLE_AGENT_VAR_DIR=" + dir}, "ensure_applyinator_not_active")
	if err != nil {
		t.Fatalf("ensure_applyinator_not_active failed: %v\n%s", err, out)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v; with no interlock file it must return immediately", elapsed)
	}
	if strings.Contains(out, "Active plan reconciliation detected") {
		t.Errorf("slept despite there being no interlock file:\n%s", out)
	}
}

// TestInstallShEnsureApplyinatorNotActiveLiveOwner is the counterweight to the
// dead-owner shortcut: a genuinely running apply must still be waited on, or an
// agent upgrade would tear the agent down mid-plan.
func TestInstallShEnsureApplyinatorNotActiveLiveOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~5s waiting on the live owner")
	}
	dir, active := interlockDirWith(t, stampFor(livePID(t)))

	// Two iterations rather than the production 60, to keep the test to one sleep.
	out, err := runInstallShFunc(t, []string{"CATTLE_AGENT_VAR_DIR=" + dir},
		"APPLYINATOR_ACTIVE_WAIT_COUNT=2\nensure_applyinator_not_active")
	if err != nil {
		t.Fatalf("ensure_applyinator_not_active failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Active plan reconciliation detected") {
		t.Errorf("expected it to wait on the live owner, got:\n%s", out)
	}
	if !strings.Contains(out, "after timeout") {
		t.Errorf("expected the timeout message, got:\n%s", out)
	}
	mustNotExist(t, active, "interlock after the wait timed out")
}

// TestInstallShEnsureApplyinatorNotActiveUntestablePID covers every pid= value
// the installer cannot run a liveness check against. Each must fall through to
// the timed wait rather than take a shortcut in either direction:
//
//   - An empty file from an older install.sh, or junk, carries no pid at all.
//   - A non-numeric pid must not reach `kill -0`, which would fail and be misread
//     as "the owner is dead", deleting a possibly-live holder's interlock.
//   - A non-positive pid must not reach `kill -0` either, for the opposite reason:
//     `kill -0 0` signals the caller's whole process group and `kill -0 -1` every
//     process it may signal, so both succeed and would be misread as "alive".
func TestInstallShEnsureApplyinatorNotActiveUntestablePID(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~5s per case waiting out the timeout")
	}
	for _, tc := range []struct{ name, contents string }{
		{"empty file from a legacy touch", ""},
		{"bare timestamp from an older agent", time.Now().Format(time.UnixDate)},
		{"non-numeric pid", "pid=notanumber\ntime=x\n"},
		{"zero pid addresses the caller's process group", "pid=0\ntime=x\n"},
		{"negative pid addresses a process group", "pid=-1\ntime=x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, active := interlockDirWith(t, []byte(tc.contents))

			out, err := runInstallShFunc(t, []string{"CATTLE_AGENT_VAR_DIR=" + dir},
				"APPLYINATOR_ACTIVE_WAIT_COUNT=2\nensure_applyinator_not_active")
			if err != nil {
				t.Fatalf("ensure_applyinator_not_active failed: %v\n%s", err, out)
			}
			if strings.Contains(out, "no longer running") {
				t.Errorf("ran a liveness check against an untestable pid:\n%s", out)
			}
			if !strings.Contains(out, "Active plan reconciliation detected") {
				t.Errorf("expected it to wait out the timeout, got:\n%s", out)
			}
			mustNotExist(t, active, "interlock after the wait timed out")
		})
	}
}

// TestInstallShServiceUnitClearsInterlock pins the ExecStartPre backstop. It is
// what clears a leaked interlock before the agent starts, so its absence would be
// a silent regression.
func TestInstallShServiceUnitClearsInterlock(t *testing.T) {
	contents, err := os.ReadFile(installShPath)
	if err != nil {
		t.Skipf("install.sh not readable: %v", err)
	}
	unit := string(contents)
	if !strings.Contains(unit, "ExecStartPre=-/bin/rm -f ${CATTLE_AGENT_VAR_DIR}/interlock/applyinator-active") {
		t.Error("the generated systemd unit no longer clears applyinator-active before starting the agent")
	}
	// The leading "-" makes a missing /bin/rm non-fatal for the unit.
	if !strings.Contains(unit, "ExecStartPre=-") {
		t.Error("ExecStartPre is not prefixed with '-', so a missing /bin/rm would fail the unit")
	}
}
