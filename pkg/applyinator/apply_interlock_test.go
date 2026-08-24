package applyinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	planapi "github.com/rancher/rancher/pkg/plan"
)

// testEnv bundles an Applyinator with the paths the interlock tests assert on.
type testEnv struct {
	a *Applyinator
	// active and restartPending are the two interlock files.
	active         string
	restartPending string
	// appliedPlanDir receives a "<datecode>-applied.plan" file once Apply gets
	// past the interlock gate. Unlike executing an instruction, writing it does
	// not require root, so it is the signal these tests use for "Apply
	// proceeded".
	appliedPlanDir string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	interlockDir := t.TempDir()
	appliedPlanDir := t.TempDir()
	return &testEnv{
		a:              NewApplyinator(t.TempDir(), false, appliedPlanDir, interlockDir, nil),
		active:         filepath.Join(interlockDir, applyinatorActiveInterlockFile),
		restartPending: filepath.Join(interlockDir, restartPendingInterlockFile),
		appliedPlanDir: appliedPlanDir,
	}
}

// apply runs a minimal plan. One-time instructions are deliberately not run:
// executing one chowns the working directory to root, which unprivileged test
// runners cannot do, and the interlock gate gets no say in what runs afterwards.
func (e *testEnv) apply(t *testing.T) (ApplyOutput, error) {
	t.Helper()
	return e.a.Apply(context.Background(), ApplyInput{
		CalculatedPlan: CalculatedPlan{Checksum: "testchecksum"},
	})
}

func (e *testEnv) planWasApplied(t *testing.T) bool {
	t.Helper()
	entries, err := os.ReadDir(e.appliedPlanDir)
	if err != nil {
		t.Fatalf("unable to read applied plan dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), appliedPlanFileSuffix) {
			return true
		}
	}
	return false
}

// assertProceeded asserts Apply got past the interlock gate.
func (e *testEnv) assertProceeded(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Apply returned an unexpected error: %v", err)
	}
	if !e.planWasApplied(t) {
		t.Error("Apply returned nil but never reached the apply stage")
	}
}

// assertBlocked asserts Apply refused, with an error mentioning want, and never
// reached the apply stage.
func (e *testEnv) assertBlocked(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("Apply returned nil error, want it to be blocked by an interlock")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to contain %q", err, want)
	}
	if e.planWasApplied(t) {
		t.Error("Apply reached the apply stage despite being blocked")
	}
}

func writeOwner(t *testing.T, path string, o interlockOwner) {
	t.Helper()
	if err := os.WriteFile(path, o.marshal(), 0600); err != nil {
		t.Fatalf("unable to write interlock file %s: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s: expected %s to be gone, stat err = %v", what, path, err)
	}
}

func mustExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s: expected %s to still exist, stat err = %v", what, path, err)
	}
}

func liveOwner(t *testing.T) interlockOwner {
	t.Helper()
	pid := spawnLiveProcess(t)
	return interlockOwner{PID: pid, BootID: currentBootID(), Start: procStartTime(pid), Written: time.Now()}
}

func deadOwner(t *testing.T) interlockOwner {
	t.Helper()
	return interlockOwner{PID: spawnDeadPID(t), BootID: currentBootID(), Written: time.Now()}
}

// TestApplyRemovesStaleActiveInterlock is the direct regression test for the
// wrong-path stat: before the fix Apply looked for a bare "applyinator-active"
// relative to the working directory (systemd supplies "/"), so a leaked file was
// never cleared and every subsequent install.sh burned five minutes on it.
func TestApplyRemovesStaleActiveInterlock(t *testing.T) {
	e := newTestEnv(t)
	writeOwner(t, e.active, deadOwner(t))

	_, err := e.apply(t)
	e.assertProceeded(t, err)
	mustNotExist(t, e.active, "stale interlock from a dead owner")
}

// TestApplyRefusesWhenActiveInterlockOwnerAlive is the counterweight: the new
// liveness check must not become a licence to trample a genuinely concurrent
// apply. A false negative here means two agents applying plans at once.
func TestApplyRefusesWhenActiveInterlockOwnerAlive(t *testing.T) {
	e := newTestEnv(t)
	writeOwner(t, e.active, liveOwner(t))

	_, err := e.apply(t)
	e.assertBlocked(t, err, "refusing to apply concurrently")
	mustExist(t, e.active, "live holder's interlock must not be deleted")
}

// TestApplyRemovesActiveInterlockAfterReboot: a PID may well be live after a
// reboot, belonging to something else entirely.
func TestApplyRemovesActiveInterlockAfterReboot(t *testing.T) {
	if currentBootID() == "" {
		t.Skip("boot id unavailable on this system")
	}
	e := newTestEnv(t)
	owner := liveOwner(t)
	owner.BootID = "00000000-0000-0000-0000-000000000000"
	writeOwner(t, e.active, owner)

	_, err := e.apply(t)
	e.assertProceeded(t, err)
	mustNotExist(t, e.active, "interlock from a previous boot")
}

// TestApplyRemovesActiveInterlockOnPIDReuse covers a recycled PID within a single
// boot, which the boot id cannot catch.
func TestApplyRemovesActiveInterlockOnPIDReuse(t *testing.T) {
	e := newTestEnv(t)
	owner := liveOwner(t)
	if owner.Start == 0 {
		t.Skip("/proc start time unavailable on this system")
	}
	owner.Start++ // a different process wearing this PID
	writeOwner(t, e.active, owner)

	_, err := e.apply(t)
	e.assertProceeded(t, err)
	mustNotExist(t, e.active, "interlock whose owner's start time does not match")
}

// TestApplyRemovesLegacyActiveInterlock covers a new binary meeting a file
// written by an older install.sh (bare touch) or an older agent (bare timestamp).
func TestApplyRemovesLegacyActiveInterlock(t *testing.T) {
	for _, tc := range []struct{ name, contents string }{
		{"empty file from a legacy touch", ""},
		{"bare timestamp from an older agent", time.Now().Format(time.UnixDate)},
		{"unrecognised junk", "garbage\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			if err := os.WriteFile(e.active, []byte(tc.contents), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := e.apply(t)
			e.assertProceeded(t, err)
			mustNotExist(t, e.active, "legacy interlock")
		})
	}
}

// TestApplyClearsItsOwnInterlock: the deferred cleanup on the happy path.
func TestApplyClearsItsOwnInterlock(t *testing.T) {
	e := newTestEnv(t)
	_, err := e.apply(t)
	e.assertProceeded(t, err)
	mustNotExist(t, e.active, "interlock after a completed apply")
}

// TestApplyWritesOwnerStampedInterlockWhileRunning proves the file exists while
// the plan is running and carries this process's identity. It needs a plan that
// actually executes, and executing an instruction chowns the working directory
// to root.
func TestApplyWritesOwnerStampedInterlockWhileRunning(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("executing an instruction chowns the working directory to root; run as root to cover this")
	}
	e := newTestEnv(t)
	captured := filepath.Join(t.TempDir(), "captured")

	out, err := e.a.Apply(context.Background(), ApplyInput{
		CalculatedPlan: CalculatedPlan{
			Checksum: "testchecksum",
			Plan: planapi.Plan{
				OneTimeInstructions: []planapi.OneTimeInstruction{{
					CommonInstruction: planapi.CommonInstruction{
						Name:    "capture",
						Command: "/bin/sh",
						Args:    []string{"-c", "cat " + e.active + " > " + captured},
					},
				}},
			},
		},
		RunOneTimeInstructions: true,
	})
	if err != nil {
		t.Fatalf("Apply returned an unexpected error: %v", err)
	}
	if !out.OneTimeApplySucceeded {
		t.Fatal("plan did not run, so the interlock was never captured")
	}

	contents, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("interlock file did not exist while the plan was running: %v", err)
	}
	owner, ok := parseInterlockOwner(contents)
	if !ok {
		t.Fatalf("interlock written during apply is not owner-stamped: %q", contents)
	}
	if owner.PID != os.Getpid() {
		t.Errorf("interlock pid = %d, want this process %d", owner.PID, os.Getpid())
	}
	if owner.BootID != currentBootID() {
		t.Errorf("interlock boot id = %q, want %q", owner.BootID, currentBootID())
	}
	if want := procStartTime(os.Getpid()); want != 0 && owner.Start != want {
		t.Errorf("interlock start = %d, want %d", owner.Start, want)
	}
	mustNotExist(t, e.active, "interlock after a completed apply")
}
