package applyinator

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// TestApplyClearsRestartPendingFromDeadInstaller: install.sh writes this file and
// removes it on the happy path. If it is killed in between, the agent previously
// served the full five-minute timeout — and restarted that clock on every agent
// restart, so it never expired.
func TestApplyClearsRestartPendingFromDeadInstaller(t *testing.T) {
	e := newTestEnv(t)
	writeOwner(t, e.restartPending, deadOwner(t))

	_, err := e.apply(t)
	e.assertProceeded(t, err)
	mustNotExist(t, e.restartPending, "restart-pending from a dead installer")
}

// TestApplyHonoursRestartPendingFromLiveInstaller: the interlock exists to stop
// the agent applying a plan against a binary that is mid-replacement.
func TestApplyHonoursRestartPendingFromLiveInstaller(t *testing.T) {
	e := newTestEnv(t)
	writeOwner(t, e.restartPending, liveOwner(t))

	_, err := e.apply(t)
	e.assertBlocked(t, err, "restart is pending")
	mustExist(t, e.restartPending, "live installer's restart-pending must not be deleted")
}

// TestApplyForcesThroughExpiredRestartPending keeps the existing safety valve:
// even a live installer cannot block applies forever.
func TestApplyForcesThroughExpiredRestartPending(t *testing.T) {
	e := newTestEnv(t)
	owner := liveOwner(t)
	owner.Written = time.Now().Add(-2 * restartPendingTimeout)
	writeOwner(t, e.restartPending, owner)

	_, err := e.apply(t)
	e.assertProceeded(t, err)
	mustNotExist(t, e.restartPending, "expired restart-pending")
}

// TestApplyStampsRestartPendingWithoutTimestamp: an owner-stamped file whose
// time= is missing or unparseable has no defined window start. The agent must
// stamp its own observation time and block — not force through, which would
// defeat the interlock while the installer is still live.
func TestApplyStampsRestartPendingWithoutTimestamp(t *testing.T) {
	e := newTestEnv(t)
	pid := spawnLiveProcess(t)
	record := "pid=" + strconv.Itoa(pid) + "\nboot=" + currentBootID() + "\ntime=not a date\n"
	if err := os.WriteFile(e.restartPending, []byte(record), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := e.apply(t)
	e.assertBlocked(t, err, "restart is pending")
	mustExist(t, e.restartPending, "restart-pending must be kept and stamped")

	contents, err := os.ReadFile(e.restartPending)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := parseInterlockOwner(contents)
	if !ok {
		t.Fatalf("stamped file no longer parses: %q", contents)
	}
	if owner.Written.IsZero() {
		t.Error("Written is still zero: the observation time was not stamped")
	}
	if owner.PID != pid {
		t.Errorf("stamping clobbered the owner: pid = %d, want %d", owner.PID, pid)
	}

	// The stamped window must be honoured on the next pass, not restarted.
	e2 := &testEnv{a: e.a, active: e.active, restartPending: e.restartPending, appliedPlanDir: e.appliedPlanDir}
	_, err = e2.apply(t)
	e2.assertBlocked(t, err, "restart is pending")
}

// TestApplyLegacyRestartPending covers an old install.sh against a new binary:
// the bare `touch` and bare-timestamp behaviour must be unchanged.
func TestApplyLegacyRestartPending(t *testing.T) {
	t.Run("empty file blocks and is stamped with the observation time", func(t *testing.T) {
		e := newTestEnv(t)
		if err := os.WriteFile(e.restartPending, nil, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := e.apply(t)
		e.assertBlocked(t, err, "restart is pending")

		contents, err := os.ReadFile(e.restartPending)
		if err != nil {
			t.Fatal(err)
		}
		if _, parseErr := time.Parse(time.UnixDate, string(contents)); parseErr != nil {
			t.Errorf("legacy file was not stamped with a parseable time: %q", contents)
		}
	})

	t.Run("recent bare timestamp blocks", func(t *testing.T) {
		e := newTestEnv(t)
		if err := os.WriteFile(e.restartPending, []byte(time.Now().Format(time.UnixDate)), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := e.apply(t)
		e.assertBlocked(t, err, "restart is pending")
		mustExist(t, e.restartPending, "recent legacy restart-pending")
	})

	t.Run("expired bare timestamp is cleared", func(t *testing.T) {
		e := newTestEnv(t)
		stamp := time.Now().Add(-2 * restartPendingTimeout).Format(time.UnixDate)
		if err := os.WriteFile(e.restartPending, []byte(stamp), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := e.apply(t)
		e.assertProceeded(t, err)
		mustNotExist(t, e.restartPending, "expired legacy restart-pending")
	})
}

// TestApplyAcceptsInstallShFormat pins the cross-language contract: the exact
// three-line record install.sh's write_interlock_owner emits, with no start=
// field, must be understood by the agent. If the two drift, the restart-pending
// fix silently reverts to the legacy five-minute path.
func TestApplyAcceptsInstallShFormat(t *testing.T) {
	e := newTestEnv(t)
	record := "pid=" + strconv.Itoa(spawnDeadPID(t)) +
		"\nboot=" + currentBootID() +
		"\ntime=" + time.Now().UTC().Format(time.UnixDate) + "\n"
	if err := os.WriteFile(e.restartPending, []byte(record), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := e.apply(t)
	e.assertProceeded(t, err)
	mustNotExist(t, e.restartPending, "install.sh-format restart-pending from a dead installer")
}
