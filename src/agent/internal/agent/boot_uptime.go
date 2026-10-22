package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// bootUptimePath is the kernel's monotonic since-boot clock. Overridable in tests.
var bootUptimePath = "/proc/uptime"

// bootTimelinePath is the per-boot profile written by
// aiden-boot-timeline-init.service (see
// overlay-debian/usr/lib/aiden/aiden-boot-timeline). The agent appends its own
// milestones so the whole boot lives in one file. Overridable in tests.
var bootTimelinePath = "/var/log/aiden_boot_timeline.log"

// bootTimelineArchiveStatePath holds the path of the archive the boot timeline
// wrote for this boot, published by boot_timeline_archive. Overridable in tests.
var bootTimelineArchiveStatePath = "/run/aiden_boot_timeline.archive"

// bootTimelineLockPath is shared with
// overlay-debian/usr/lib/aiden/aiden-boot-timeline.
// Holding an exclusive flock serializes Shell and Go milestone/archive writers.
var bootTimelineLockPath = "/run/aiden_boot_timeline.lock"

const bootTimelineLockTimeout = time.Second

// BootUptime reports how long the kernel has been up. The second return value
// is false when /proc/uptime is unavailable or malformed, which is the normal
// case on non-Linux hosts and in unit tests.
func BootUptime() (time.Duration, bool) {
	data, err := os.ReadFile(bootUptimePath)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

// MarkBootTimeline appends a milestone to the boot timeline recorded by rcS,
// matching the "<uptime_s> <delta_s> mark <label>" record format.
//
// Best-effort by design: the timeline is a diagnostic, so a missing file (the
// usual case once the tmpfs log is rotated, or on a dev host) is not an error
// and never affects the caller.
func MarkBootTimeline(label string) {
	uptime, ok := BootUptime()
	if !ok {
		return
	}
	withBootTimelineLock(func() {
		// Only append to an existing timeline: creating it here would produce a
		// stray file with no boot context on every agent restart.
		f, err := os.OpenFile(bootTimelinePath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(f, "%.2f 0.00 mark %s\n", uptime.Seconds(), label); err != nil {
			f.Close()
			return
		}
		// Close before refreshing so the archive copy always includes this record.
		if err := f.Close(); err != nil {
			return
		}

		refreshBootTimelineArchiveLocked()
	})
}

// withBootTimelineLock coordinates with the Shell helper through flock(2).
// Kernel locks are released automatically if either writer exits unexpectedly.
// Acquisition is bounded so a stuck diagnostic writer cannot delay agent
// startup. Failure skips the best-effort mark and leaves normal startup intact.
func withBootTimelineLock(fn func()) {
	lock, err := os.OpenFile(bootTimelineLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer lock.Close()

	deadline := time.Now().Add(bootTimelineLockTimeout)
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	fn()
}

// refreshBootTimelineArchiveLocked mirrors boot_timeline_refresh_archive in
// overlay-debian/usr/lib/aiden/aiden-boot-timeline. It replaces the archive
// from a consistent point in the append/archive sequence. The caller holds
// bootTimelineLockPath.
//
// The boot timeline archives when the systemd target finishes, but the agent is
// started by a background watchdog and can reach "listening" after that point —
// on a slow boot, or on any watchdog restart. Without this the persisted archive
// would stop at the init milestone and omit the very milestone it exists to
// record.
//
// Best-effort, like the rest of the timeline: no archive yet (the usual case
// during early boot) simply means there is nothing to refresh.
func refreshBootTimelineArchiveLocked() {
	state, err := os.ReadFile(bootTimelineArchiveStatePath)
	if err != nil {
		return
	}
	target := strings.TrimSpace(string(state))
	if target == "" {
		return
	}
	if _, err := os.Stat(target); err != nil {
		return
	}
	content, err := os.ReadFile(bootTimelinePath)
	if err != nil {
		return
	}

	// Write beside the target and rename, so a reader never sees a partial
	// archive. Same directory keeps the rename atomic.
	tmp := fmt.Sprintf("%s.tmp.%d", target, os.Getpid())
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
	}
}

// bootUptimeLogSuffix renders " uptime=12.34s" for log lines, or "" when the
// uptime is unavailable, so callers can append it unconditionally.
func bootUptimeLogSuffix() string {
	uptime, ok := BootUptime()
	if !ok {
		return ""
	}
	return fmt.Sprintf(" uptime=%.2fs", uptime.Seconds())
}
