package agent

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func withBootUptimeFile(t *testing.T, contents string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "uptime")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write uptime: %v", err)
	}
	original := bootUptimePath
	bootUptimePath = path
	t.Cleanup(func() { bootUptimePath = original })
}

func withBootTimelineFiles(t *testing.T, timeline, state string) {
	t.Helper()
	origTimeline, origState, origLock :=
		bootTimelinePath, bootTimelineArchiveStatePath, bootTimelineLockPath
	bootTimelinePath = timeline
	bootTimelineArchiveStatePath = state
	bootTimelineLockPath = filepath.Join(filepath.Dir(timeline), "timeline.lock")
	t.Cleanup(func() {
		bootTimelinePath, bootTimelineArchiveStatePath, bootTimelineLockPath =
			origTimeline, origState, origLock
	})
}

func TestBootUptimeParsesProcUptime(t *testing.T) {
	withBootUptimeFile(t, "7717.92 6832.62\n")

	uptime, ok := BootUptime()
	if !ok {
		t.Fatal("expected uptime to parse")
	}
	if got := uptime.Seconds(); got < 7717.91 || got > 7717.93 {
		t.Fatalf("uptime = %v, want ~7717.92", got)
	}
}

func TestBootUptimeRejectsUnusableInput(t *testing.T) {
	cases := map[string]string{
		"empty":     "",
		"blank":     "   \n",
		"malformed": "not-a-number 1.0\n",
		"negative":  "-5.0 1.0\n",
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			withBootUptimeFile(t, contents)
			if _, ok := BootUptime(); ok {
				t.Fatalf("expected %s input to be rejected", name)
			}
		})
	}
}

func TestBootUptimeMissingFile(t *testing.T) {
	original := bootUptimePath
	bootUptimePath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { bootUptimePath = original })

	if _, ok := BootUptime(); ok {
		t.Fatal("expected missing /proc/uptime to report not-ok")
	}
}

func TestMarkBootTimelineAppendsRecord(t *testing.T) {
	withBootUptimeFile(t, "51.18 40.00\n")

	timeline := filepath.Join(t.TempDir(), "aico_boot_timeline.log")
	if err := os.WriteFile(timeline, []byte("0.10 0.10 rcS:begin\n"), 0o644); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}
	withBootTimelineFiles(t, timeline, filepath.Join(filepath.Dir(timeline), "absent.state"))

	MarkBootTimeline("agent:listening")

	data, err := os.ReadFile(timeline)
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "0.10 0.10 rcS:begin") {
		t.Fatalf("existing records were lost: %q", got)
	}
	if !strings.Contains(got, "51.18 0.00 mark agent:listening") {
		t.Fatalf("missing milestone record: %q", got)
	}
}

// The timeline belongs to a boot. An agent restart must not resurrect it,
// otherwise the file would hold records with no boot context.
func TestMarkBootTimelineDoesNotCreateFile(t *testing.T) {
	withBootUptimeFile(t, "51.18 40.00\n")

	timeline := filepath.Join(t.TempDir(), "absent.log")
	withBootTimelineFiles(t, timeline, filepath.Join(filepath.Dir(timeline), "absent.state"))

	MarkBootTimeline("agent:listening")

	if _, err := os.Stat(timeline); !os.IsNotExist(err) {
		t.Fatalf("expected no timeline file to be created, stat err = %v", err)
	}
}

// A failed bind must not leave a readiness milestone behind: the timeline is
// used to answer "when did :8080 become reachable", so a marker for a server
// that never listened would be worse than no marker at all.
func TestStartDoesNotMarkListeningWhenBindFails(t *testing.T) {
	withBootUptimeFile(t, "15.26 5.00\n")

	timeline := filepath.Join(t.TempDir(), "aico_boot_timeline.log")
	if err := os.WriteFile(timeline, []byte("0.10 0.10 rcS:begin\n"), 0o644); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}
	withBootTimelineFiles(t, timeline, filepath.Join(filepath.Dir(timeline), "absent.state"))

	// Hold the port so the server's own bind is guaranteed to fail.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer occupied.Close()

	srv := &Server{addr: occupied.Addr().String()}
	if err := srv.Start(); err == nil {
		t.Fatal("expected Start to fail when the address is already in use")
	}

	data, err := os.ReadFile(timeline)
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	if strings.Contains(string(data), "agent:listening") {
		t.Fatalf("bind failure must not record agent:listening: %q", string(data))
	}
}

// The boot timeline archives when the systemd target finishes, but the agent is
// launched by a background watchdog and can reach "listening" afterwards. The Go
// writer must refresh the archive too, otherwise the persisted copy omits the
// milestone it exists to record.
func TestMarkBootTimelineRefreshesArchive(t *testing.T) {
	withBootUptimeFile(t, "31.40 5.00\n")
	dir := t.TempDir()

	timeline := filepath.Join(dir, "aico_boot_timeline.log")
	if err := os.WriteFile(timeline, []byte("0.10 0.10 rcS:begin\n25.52 0.03 rcS:end\n"), 0o644); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}
	archive := filepath.Join(dir, "boot-20260807-000000.log")
	if err := os.WriteFile(archive, []byte("0.10 0.10 rcS:begin\n25.52 0.03 rcS:end\n"), 0o644); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
	state := filepath.Join(dir, "archive.state")
	if err := os.WriteFile(state, []byte(archive+"\n"), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	withBootTimelineFiles(t, timeline, state)

	MarkBootTimeline("agent:listening")

	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "mark agent:listening") {
		t.Fatalf("archive was not refreshed with the late milestone: %q", got)
	}
	if !strings.Contains(got, "rcS:end") {
		t.Fatalf("refresh dropped earlier records: %q", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Fatalf("refresh left a temp file behind: %s", e.Name())
		}
	}
}

// No archive yet (the usual case for marks written during rcS) must be a
// silent no-op rather than an attempt to create one.
func TestMarkBootTimelineArchiveRefreshNoopWithoutState(t *testing.T) {
	withBootUptimeFile(t, "31.40 5.00\n")
	dir := t.TempDir()

	timeline := filepath.Join(dir, "aico_boot_timeline.log")
	if err := os.WriteFile(timeline, []byte("0.10 0.10 rcS:begin\n"), 0o644); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}
	withBootTimelineFiles(t, timeline, filepath.Join(dir, "absent.state"))

	MarkBootTimeline("agent:listening")

	data, err := os.ReadFile(timeline)
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	if !strings.Contains(string(data), "mark agent:listening") {
		t.Fatal("milestone must still be appended to the live timeline")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "boot-") {
			t.Fatalf("mark without archive state created an archive: %s", entry.Name())
		}
	}
}

// The Shell archiver holds the same flock while it snapshots the timeline and
// publishes the archive state. A Go milestone arriving in that window must wait
// and then refresh the published archive, never disappear between the two.
func TestMarkBootTimelineWaitsForArchivePublicationLock(t *testing.T) {
	withBootUptimeFile(t, "31.41 5.00\n")
	dir := t.TempDir()
	timeline := filepath.Join(dir, "aico_boot_timeline.log")
	archive := filepath.Join(dir, "boot-20260807-000000.log")
	state := filepath.Join(dir, "archive.state")
	if err := os.WriteFile(timeline, []byte("0.10 0.10 rcS:begin\n25.52 0.03 rcS:end\n"), 0o644); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}
	if err := os.WriteFile(archive, []byte("0.10 0.10 rcS:begin\n25.52 0.03 rcS:end\n"), 0o644); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
	if err := os.WriteFile(state, []byte(archive+"\n"), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	withBootTimelineFiles(t, timeline, state)

	lock, err := os.OpenFile(bootTimelineLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open timeline lock: %v", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold timeline lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}
	}()

	done := make(chan struct{})
	go func() {
		MarkBootTimeline("agent:listening")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("milestone writer ignored the archive publication lock")
	case <-time.After(100 * time.Millisecond):
	}
	data, err := os.ReadFile(timeline)
	if err != nil {
		t.Fatalf("read blocked timeline: %v", err)
	}
	if strings.Contains(string(data), "agent:listening") {
		t.Fatal("milestone was appended while archive publication held the lock")
	}

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("release timeline lock: %v", err)
	}
	locked = false
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("milestone writer did not resume after archive publication")
	}

	data, err = os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read refreshed archive: %v", err)
	}
	if !strings.Contains(string(data), "mark agent:listening") {
		t.Fatalf("archive omitted milestone written after publication: %q", data)
	}
}

// Concurrent marks used to copy the full timeline into per-process temporary
// files and race their final rename. A slower stale snapshot could then replace
// a newer one. Serializing append + refresh must preserve every completed mark.
func TestConcurrentBootTimelineMarksDoNotRegressArchive(t *testing.T) {
	withBootUptimeFile(t, "42.00 5.00\n")
	dir := t.TempDir()
	timeline := filepath.Join(dir, "aico_boot_timeline.log")
	archive := filepath.Join(dir, "boot-20260807-000000.log")
	state := filepath.Join(dir, "archive.state")
	initial := []byte("0.10 0.10 rcS:begin\n25.52 0.03 rcS:end\n")
	if err := os.WriteFile(timeline, initial, 0o644); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}
	if err := os.WriteFile(archive, initial, 0o644); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
	if err := os.WriteFile(state, []byte(archive+"\n"), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	withBootTimelineFiles(t, timeline, state)

	const marks = 16
	var wg sync.WaitGroup
	wg.Add(marks)
	for i := 0; i < marks; i++ {
		label := fmt.Sprintf("agent:concurrent-%02d", i)
		go func() {
			defer wg.Done()
			MarkBootTimeline(label)
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	got := string(data)
	for i := 0; i < marks; i++ {
		label := fmt.Sprintf("mark agent:concurrent-%02d", i)
		if !strings.Contains(got, label) {
			t.Fatalf("concurrent archive omitted %s: %q", label, got)
		}
	}
}

func TestBootUptimeLogSuffix(t *testing.T) {
	withBootUptimeFile(t, "50.70 1.00\n")
	if got := bootUptimeLogSuffix(); got != " uptime=50.70s" {
		t.Fatalf("suffix = %q", got)
	}

	original := bootUptimePath
	bootUptimePath = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { bootUptimePath = original })
	if got := bootUptimeLogSuffix(); got != "" {
		t.Fatalf("expected empty suffix when uptime unavailable, got %q", got)
	}
}
