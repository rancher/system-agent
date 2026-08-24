package applyinator

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// interlockOwner identifies the process that created an interlock file, so a
// later process can distinguish a live holder from one that was killed.
type interlockOwner struct {
	PID     int
	BootID  string
	Written time.Time
	Start   uint64
}

func newInterlockOwner(now time.Time) interlockOwner {
	pid := os.Getpid()
	return interlockOwner{PID: pid, BootID: currentBootID(), Start: procStartTime(pid), Written: now}
}

// procStartTime returns field 22 of /proc/<pid>/stat — the process start time in
// clock ticks since boot. Zero means unavailable, in which case callers fall
// back to PID-only liveness.
func procStartTime(pid int) uint64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0
	}
	n, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func currentBootID() string {
	b, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "" // non-Linux or restricted /proc: degrade to PID-only liveness
	}
	return strings.TrimSpace(string(b))
}

func (o interlockOwner) marshal() []byte {
	return []byte(fmt.Sprintf("pid=%d\nboot=%s\nstart=%d\ntime=%s\n",
		o.PID, o.BootID, o.Start, o.Written.UTC().Format(time.UnixDate)))
}

// parseInterlockOwner reports ok=false for any file it does not recognise —
// including the empty file written by `touch` in an older install.sh and the
// bare timestamp written by system-agent <= v0.3.16. Callers must keep the
// legacy path for those.
func parseInterlockOwner(contents []byte) (interlockOwner, bool) {
	var o interlockOwner
	var sawPID bool
	s := bufio.NewScanner(strings.NewReader(string(contents)))
	for s.Scan() {
		k, v, found := strings.Cut(strings.TrimSpace(s.Text()), "=")
		if !found {
			continue
		}
		switch k {
		case "pid":
			n, err := strconv.Atoi(v)
			if err != nil {
				return interlockOwner{}, false
			}
			o.PID, sawPID = n, true
		case "boot":
			o.BootID = v
		case "start":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				o.Start = n
			}
		case "time":
			if t, err := time.Parse(time.UnixDate, v); err == nil {
				o.Written = t
			}
		}
	}
	return o, sawPID
}

// isAlive reports whether the writing process is still running. A boot ID that
// differs from the current one means the file predates a reboot, so the PID —
// which recycles across reboots — must not be trusted.
func (o interlockOwner) isAlive() bool {
	if cur := currentBootID(); cur != "" && o.BootID != "" && cur != o.BootID {
		return false
	}
	if o.PID <= 0 {
		return false
	}
	if o.PID == os.Getpid() {
		return true
	}
	p, err := os.FindProcess(o.PID)
	if err != nil {
		return false
	}
	if p.Signal(syscall.Signal(0)) != nil {
		return false
	}
	// PIDs recycle within a single boot. If we recorded the owner's start time,
	// a mismatch means a different process is wearing the same PID.
	if o.Start != 0 {
		if cur := procStartTime(o.PID); cur != 0 && cur != o.Start {
			return false
		}
	}
	return true
}
