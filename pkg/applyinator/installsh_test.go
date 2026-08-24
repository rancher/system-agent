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
// this package. The two write and read the same on-disk format, in two
// languages, with no shared definition — so it is pinned by test.
const installShPath = "../../install.sh"

// runInstallShFunc evaluates install.sh up to (but not including) do_install,
// which defines its helper functions without running the installer, then invokes
// the given snippet. Returns combined output.
func runInstallShFunc(t *testing.T, env []string, snippet string) (string, error) {
	t.Helper()
	if _, err := os.Stat(installShPath); err != nil {
		t.Skipf("install.sh not found at %s: %v", installShPath, err)
	}
	abs, err := filepath.Abs(installShPath)
	if err != nil {
		t.Fatal(err)
	}

	harness := fmt.Sprintf(`
set -e
eval "$(sed '/^do_install/,$d' %q)"
%s
`, abs, snippet)

	cmd := exec.Command("sh", "-c", harness)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestInstallShEnsureApplyinatorNotActiveDeadOwner: the five-minute wait was the
// user-visible cost of the leaked interlock. A dead owner must short-circuit it.
func TestInstallShEnsureApplyinatorNotActiveDeadOwner(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "interlock"), 0700); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, "interlock", "applyinator-active")
	writeOwner(t, active, deadOwner(t))

	start := time.Now()
	out, err := runInstallShFunc(t,
		[]string{"CATTLE_AGENT_VAR_DIR=" + dir},
		"ensure_applyinator_not_active")
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
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "interlock"), 0700); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, err := runInstallShFunc(t,
		[]string{"CATTLE_AGENT_VAR_DIR=" + dir},
		"ensure_applyinator_not_active")
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

// TestInstallShEnsureApplyinatorNotActiveLiveOwner: a genuinely running apply
// must still be waited on, and the wait must still terminate.
func TestInstallShEnsureApplyinatorNotActiveLiveOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~5s waiting on the live owner")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "interlock"), 0700); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, "interlock", "applyinator-active")
	writeOwner(t, active, liveOwner(t))

	// Two iterations rather than the production 60, to keep the test to one sleep.
	out, err := runInstallShFunc(t,
		[]string{"CATTLE_AGENT_VAR_DIR=" + dir},
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

// TestInstallShEnsureApplyinatorNotActiveLegacyFile: an empty file from an older
// install.sh carries no PID, so the installer cannot test liveness and must fall
// back to waiting out the timeout rather than deleting a possibly-live holder's
// interlock immediately.
func TestInstallShEnsureApplyinatorNotActiveLegacyFile(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~5s waiting out the timeout")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "interlock"), 0700); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, "interlock", "applyinator-active")
	if err := os.WriteFile(active, nil, 0600); err != nil {
		t.Fatal(err)
	}

	out, err := runInstallShFunc(t,
		[]string{"CATTLE_AGENT_VAR_DIR=" + dir},
		"APPLYINATOR_ACTIVE_WAIT_COUNT=2\nensure_applyinator_not_active")
	if err != nil {
		t.Fatalf("ensure_applyinator_not_active failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Active plan reconciliation detected") {
		t.Errorf("expected it to wait on a legacy file with no pid, got:\n%s", out)
	}
	mustNotExist(t, active, "legacy interlock after the wait timed out")
}

// TestInstallShServiceUnitClearsInterlock pins the ExecStartPre backstop. It is
// what covers a recycled PID across an agent restart, so its absence would be a
// silent regression.
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
