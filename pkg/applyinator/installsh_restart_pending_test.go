package applyinator

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestInstallShWriteInterlockOwnerIsParseable is the cross-language contract
// test. install.sh writes restart-pending and the agent reads it; if the format
// drifts, parseInterlockOwner silently returns ok=false and the agent falls back
// to the legacy five-minute path — the exact behaviour the fix removes, with no
// visible error to say so.
func TestInstallShWriteInterlockOwnerIsParseable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "restart-pending")

	out, err := runInstallShFunc(t, nil,
		fmt.Sprintf("write_interlock_owner %q\necho \"SHELLPID=$$\"", target))
	if err != nil {
		t.Fatalf("write_interlock_owner failed: %v\n%s", err, out)
	}

	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("write_interlock_owner produced no file: %v\n%s", err, out)
	}

	owner, ok := parseInterlockOwner(contents)
	if !ok {
		t.Fatalf("the agent cannot parse what install.sh writes:\n%q", contents)
	}

	// The shell's PID must round-trip, so the agent can test its liveness.
	var wantPID int
	for _, line := range strings.Split(out, "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "SHELLPID="); found {
			wantPID, _ = strconv.Atoi(rest)
		}
	}
	if wantPID == 0 {
		t.Fatalf("harness did not report the shell PID:\n%s", out)
	}
	if owner.PID != wantPID {
		t.Errorf("pid = %d, want the installer's pid %d", owner.PID, wantPID)
	}

	if owner.BootID == "" && currentBootID() != "" {
		t.Error("install.sh wrote no boot id, so the agent loses its across-reboot guard")
	}
	if owner.BootID != currentBootID() {
		t.Errorf("boot id = %q, want %q", owner.BootID, currentBootID())
	}

	// A timestamp that does not parse leaves the restart-pending window with no
	// defined start.
	if owner.Written.IsZero() {
		t.Errorf("install.sh wrote a timestamp the agent cannot parse:\n%q", contents)
	} else if skew := time.Since(owner.Written); skew < -2*time.Minute || skew > 2*time.Minute {
		// Catches a zone written in a form Go resolves to the wrong offset.
		t.Errorf("timestamp is %v away from now; the zone did not round-trip:\n%q", skew, contents)
	}

	// install.sh deliberately writes no start= field; the agent must treat that
	// as "unknown" rather than as a mismatch.
	if owner.Start != 0 {
		t.Errorf("start = %d, want 0 — install.sh should not write one", owner.Start)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("permissions = %04o, want 0600", perm)
	}

	// The installer is gone by now, so the agent must consider the file stale.
	if owner.isAlive() {
		t.Error("owner reports alive after the installer shell exited")
	}
}

// TestInstallShTrapsRestartPending: every fatal between creating restart-pending
// and the final rm previously leaked it. The trap is what closes that.
func TestInstallShTrapsRestartPending(t *testing.T) {
	contents, err := os.ReadFile(installShPath)
	if err != nil {
		t.Skipf("install.sh not readable: %v", err)
	}
	script := string(contents)
	if strings.Contains(script, "touch ${CATTLE_AGENT_VAR_DIR}/interlock/restart-pending") {
		t.Error("restart-pending is still created with a bare touch, so it carries no owner")
	}
	if !strings.Contains(script, "write_interlock_owner ${CATTLE_AGENT_VAR_DIR}/interlock/restart-pending") {
		t.Error("restart-pending is not owner-stamped")
	}
	if !strings.Contains(script, "trap 'rm -f ${CATTLE_AGENT_VAR_DIR}/interlock/restart-pending' EXIT INT TERM") {
		t.Error("no EXIT/INT/TERM trap on restart-pending: a fatal between creation and the final rm leaks it")
	}
}
