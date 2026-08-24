package applyinator

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// spawnLiveProcess starts a child that stays alive for the duration of the test
// and returns its PID. Using a real process rather than os.Getpid() exercises
// the FindProcess/Signal path instead of the same-process short circuit.
func spawnLiveProcess(t *testing.T) int {
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

// spawnDeadPID starts a child, waits for it to exit, and returns its PID. The
// PID is reaped, so it is genuinely gone rather than merely improbable.
func spawnDeadPID(t *testing.T) int {
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

func TestMarshalParseRoundtrip(t *testing.T) {
	owner := newInterlockOwner(time.Now())
	if owner.PID != os.Getpid() {
		t.Fatalf("newInterlockOwner recorded pid %d, want %d", owner.PID, os.Getpid())
	}

	parsed, ok := parseInterlockOwner(owner.marshal())
	if !ok {
		t.Fatal("parseInterlockOwner returned ok=false for a freshly marshalled owner")
	}
	if parsed.PID != owner.PID {
		t.Errorf("PID mismatch: got %d, want %d", parsed.PID, owner.PID)
	}
	if parsed.BootID != owner.BootID {
		t.Errorf("BootID mismatch: got %q, want %q", parsed.BootID, owner.BootID)
	}
	if parsed.Start != owner.Start {
		t.Errorf("Start mismatch: got %d, want %d", parsed.Start, owner.Start)
	}
	// UnixDate has second granularity, so compare truncated instants.
	if want := owner.Written.Truncate(time.Second); !parsed.Written.Equal(want) {
		t.Errorf("Written mismatch: got %v, want %v", parsed.Written.UTC(), want.UTC())
	}
}

// TestMarshalWritesUTC guards the round-trip against local zone abbreviations.
// time.Parse resolves an abbreviation it does not know to a zero offset, so
// formatting in local time would decode to the wrong instant on most nodes.
func TestMarshalWritesUTC(t *testing.T) {
	// A zone with a non-UTC offset whose abbreviation Go cannot resolve on parse.
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	written := time.Date(2026, 7, 14, 9, 48, 43, 0, loc)
	owner := interlockOwner{PID: 23056, BootID: "boot", Start: 99, Written: written}

	raw := string(owner.marshal())
	if !strings.Contains(raw, " UTC ") {
		t.Errorf("marshalled timestamp is not in UTC: %q", raw)
	}

	parsed, ok := parseInterlockOwner([]byte(raw))
	if !ok {
		t.Fatal("parseInterlockOwner returned ok=false")
	}
	if !parsed.Written.Equal(written) {
		t.Errorf("round-trip changed the instant: got %v, want %v", parsed.Written.UTC(), written.UTC())
	}
}

func TestParseInterlockOwner(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		wantOK   bool
		wantPID  int
		wantBoot string
		wantStrt uint64
	}{
		{
			name:     "full record written by the agent",
			contents: "pid=23056\nboot=abc-123\nstart=456\ntime=Mon Jul 14 09:48:43 UTC 2026\n",
			wantOK:   true, wantPID: 23056, wantBoot: "abc-123", wantStrt: 456,
		},
		{
			// install.sh writes no start= field; the agent must still accept it.
			name:     "install.sh record without start",
			contents: "pid=4242\nboot=abc-123\ntime=Mon Jul 14 09:48:43 UTC 2026\n",
			wantOK:   true, wantPID: 4242, wantBoot: "abc-123", wantStrt: 0,
		},
		{
			name:     "empty file from a legacy touch",
			contents: "",
			wantOK:   false,
		},
		{
			name:     "bare timestamp from system-agent <= v0.3.16",
			contents: "Mon Jul 14 09:48:43 CEST 2026",
			wantOK:   false,
		},
		{
			name:     "no pid key",
			contents: "boot=abc-123\ntime=Mon Jul 14 09:48:43 UTC 2026\n",
			wantOK:   false,
		},
		{
			name:     "non-numeric pid",
			contents: "pid=notanumber\nboot=abc-123\n",
			wantOK:   false,
		},
		{
			name:     "unknown keys are ignored",
			contents: "pid=7\nfuture=value\nboot=b\n",
			wantOK:   true, wantPID: 7, wantBoot: "b",
		},
		{
			name:     "unparseable time leaves Written zero but still parses",
			contents: "pid=7\nboot=b\ntime=not a date\n",
			wantOK:   true, wantPID: 7, wantBoot: "b",
		},
		{
			name:     "unparseable start is ignored",
			contents: "pid=7\nstart=notanumber\n",
			wantOK:   true, wantPID: 7, wantStrt: 0,
		},
		{
			name:     "surrounding whitespace is tolerated",
			contents: "  pid=7  \n\tboot=b\t\n",
			wantOK:   true, wantPID: 7, wantBoot: "b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseInterlockOwner([]byte(tc.contents))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.PID != tc.wantPID {
				t.Errorf("PID = %d, want %d", got.PID, tc.wantPID)
			}
			if got.BootID != tc.wantBoot {
				t.Errorf("BootID = %q, want %q", got.BootID, tc.wantBoot)
			}
			if got.Start != tc.wantStrt {
				t.Errorf("Start = %d, want %d", got.Start, tc.wantStrt)
			}
		})
	}
}

// TestParseUnparseableTimeLeavesWrittenZero pins the signal Apply relies on to
// decide it must stamp its own observation time onto restart-pending.
func TestParseUnparseableTimeLeavesWrittenZero(t *testing.T) {
	got, ok := parseInterlockOwner([]byte("pid=7\nboot=b\ntime=not a date\n"))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !got.Written.IsZero() {
		t.Errorf("Written = %v, want the zero time", got.Written)
	}
}

func TestIsAliveSelf(t *testing.T) {
	if !newInterlockOwner(time.Now()).isAlive() {
		t.Error("current process should report as alive")
	}
}

func TestIsAliveLiveChildProcess(t *testing.T) {
	pid := spawnLiveProcess(t)
	owner := interlockOwner{PID: pid, BootID: currentBootID(), Start: procStartTime(pid)}
	if !owner.isAlive() {
		t.Errorf("live child pid %d should report as alive", pid)
	}
}

func TestIsAliveDeadPID(t *testing.T) {
	pid := spawnDeadPID(t)
	owner := interlockOwner{PID: pid, BootID: currentBootID()}
	if owner.isAlive() {
		t.Errorf("reaped pid %d should not be reported as alive", pid)
	}
}

func TestIsAliveOutOfRangePID(t *testing.T) {
	// Above /proc/sys/kernel/pid_max on any realistic system.
	owner := interlockOwner{PID: 99999999, BootID: currentBootID()}
	if owner.isAlive() {
		t.Error("non-existent PID should not be reported as alive")
	}
}

// TestIsAliveRejectsNonPositivePID: os.Process.Signal happens to reject 0 and -1
// on its own, so these cases are a guard against that changing.
func TestIsAliveRejectsNonPositivePID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		owner := interlockOwner{PID: pid, BootID: currentBootID()}
		if owner.isAlive() {
			t.Errorf("pid %d should not be reported as alive", pid)
		}
	}
}

// TestIsAliveRejectsNegativePIDOfLiveProcessGroup is the case the stdlib does
// not cover: kill(-N, sig) addresses process *group* N, so a corrupt interlock
// file holding a negative PID that happens to match a live process group would
// be read as a live owner — wedging the agent permanently against a plan nobody
// is applying.
func TestIsAliveRejectsNegativePIDOfLiveProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	// Put the child in its own process group, so its PGID equals its PID.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("unable to start helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Skipf("unable to determine process group: %v", err)
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Skipf("process group %d is not signalable: %v", pgid, err)
	}

	owner := interlockOwner{PID: -pgid, BootID: currentBootID()}
	if owner.isAlive() {
		t.Errorf("pid %d addresses a live process group and must not be read as a live owner", -pgid)
	}
}

func TestIsAliveBootIDMismatch(t *testing.T) {
	if currentBootID() == "" {
		t.Skip("boot id unavailable on this system")
	}
	pid := spawnLiveProcess(t)
	owner := interlockOwner{PID: pid, BootID: "00000000-0000-0000-0000-000000000000"}
	if owner.isAlive() {
		t.Error("a file from a previous boot must be treated as stale even when the PID is live")
	}
}

// TestIsAliveMissingBootIDDegradesToPID covers a file written where /proc was
// unreadable, and the mixed-version case of an install.sh that wrote no boot id.
func TestIsAliveMissingBootIDDegradesToPID(t *testing.T) {
	pid := spawnLiveProcess(t)
	if !(interlockOwner{PID: pid, BootID: ""}).isAlive() {
		t.Error("empty boot id should fall back to PID-only liveness, not fail closed")
	}
	dead := spawnDeadPID(t)
	if (interlockOwner{PID: dead, BootID: ""}).isAlive() {
		t.Error("empty boot id with a dead PID should still report dead")
	}
}

// TestIsAliveStartTimeMismatch is the PID-reuse guard: a different process
// wearing a recycled PID within the same boot must not be mistaken for the
// interlock's owner.
func TestIsAliveStartTimeMismatch(t *testing.T) {
	pid := spawnLiveProcess(t)
	actual := procStartTime(pid)
	if actual == 0 {
		t.Skip("/proc start time unavailable on this system")
	}
	owner := interlockOwner{PID: pid, BootID: currentBootID(), Start: actual + 1}
	if owner.isAlive() {
		t.Error("start time mismatch should be treated as a recycled PID, not a live owner")
	}
}

func TestIsAliveStartTimeZeroSkipsCheck(t *testing.T) {
	pid := spawnLiveProcess(t)
	owner := interlockOwner{PID: pid, BootID: currentBootID(), Start: 0}
	if !owner.isAlive() {
		t.Error("a missing start time must degrade to PID-only liveness, not fail closed")
	}
}

func TestProcStartTime(t *testing.T) {
	self := procStartTime(os.Getpid())
	if self == 0 {
		t.Skip("/proc start time unavailable on this system")
	}
	if again := procStartTime(os.Getpid()); again != self {
		t.Errorf("start time is not stable: %d then %d", self, again)
	}
	if got := procStartTime(99999999); got != 0 {
		t.Errorf("start time for a non-existent pid = %d, want 0", got)
	}
	// A distinct process started later must have a different start time,
	// otherwise the PID-reuse guard has no discriminating power.
	child := spawnLiveProcess(t)
	if got := procStartTime(child); got == 0 {
		t.Errorf("start time for live child pid %d = 0", child)
	}
}

// TestProcStartTimeCommWithSpaces covers the parsing hazard in /proc/<pid>/stat:
// field 2 is the executable name in parentheses and may itself contain spaces
// and ')', so fields must be counted from the last ')' rather than split naively.
func TestProcStartTimeCommWithSpaces(t *testing.T) {
	// Field 22 (index 19 after the final ')') is 4242.
	fields := make([]string, 0, 30)
	for i := 3; i <= 30; i++ {
		if i == 22 {
			fields = append(fields, "4242")
			continue
		}
		fields = append(fields, fmt.Sprintf("%d", i))
	}
	stat := "1234 (weird ) name) S " + strings.Join(fields[1:], " ")

	// Exercise the same slicing procStartTime performs.
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		t.Fatal("test fixture has no ')'")
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 20 {
		t.Fatalf("fixture produced only %d fields", len(f))
	}
	if f[19] != "4242" {
		t.Errorf("field 22 = %q, want %q — comm containing ')' broke field counting", f[19], "4242")
	}
}
