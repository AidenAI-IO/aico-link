//go:build linux

package mcu

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUDSPoweroffPeerCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assertDenied := func(client Client) {
		t.Helper()
		state, err := client.GetState(ctx)
		if err != nil {
			t.Fatal("ordinary queries must remain available", err)
		}
		response, err := client.Do(ctx, Request{Op: "poweroff", Generation: state.Generation, RequestID: "peer-test"})
		if err == nil || response.Status != "DENIED" || response.UnknownOutcome {
			t.Fatalf("unprivileged poweroff: %+v %v", response, err)
		}
	}
	if socket := os.Getenv("AICO_MCU_PEER_TEST_SOCKET"); socket != "" {
		if os.Geteuid() == 0 {
			t.Fatal("test client did not drop root privileges")
		}
		assertDenied(Client{Socket: socket})
		return
	}
	dir, err := os.MkdirTemp("/tmp", "mcu-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	shutdown := &fakeShutdown{}
	s := NewService(shutdown)
	defer s.Close()
	s.link = &fakeRequester{}
	s.state = Ready
	socket := filepath.Join(dir, "mcu.sock")
	server, err := Listen(socket, s)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := Client{Socket: socket}
	if os.Geteuid() != 0 {
		assertDenied(client)
	} else {
		// Give the child access to the socket and executable so rejection proves
		// peer authentication rather than a filesystem permission failure.
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(socket, 0666); err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		binary, err := os.ReadFile(executable)
		if err != nil {
			t.Fatal(err)
		}
		childPath := filepath.Join(dir, "peer.test")
		if err := os.WriteFile(childPath, binary, 0755); err != nil {
			t.Fatal(err)
		}
		child := exec.CommandContext(ctx, childPath, "-test.run=^TestUDSPoweroffPeerCredentials$", "-test.v")
		child.Env = append(os.Environ(), "AICO_MCU_PEER_TEST_SOCKET="+socket)
		child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("unprivileged client: %v\n%s", err, output)
		}
	}
	shutdown.mu.Lock()
	begins, commits := shutdown.begins, shutdown.commits
	shutdown.mu.Unlock()
	if begins != 0 || commits != 0 || s.State().RVState != "ready" {
		t.Fatal("denied poweroff changed lifecycle")
	}
	if os.Geteuid() == 0 {
		for range 2 {
			state, err := client.Poweroff(ctx, s.events.Generation(), "peer-test")
			if err != nil || state.RVState != "shutting_down" {
				t.Fatalf("root poweroff: %+v %v", state, err)
			}
		}
		shutdown.mu.Lock()
		defer shutdown.mu.Unlock()
		if shutdown.begins != 1 || shutdown.commits != 0 {
			t.Fatal("root poweroff did not preserve lifecycle ordering and idempotency")
		}
	}
}

func TestLinuxSerialPTY(t *testing.T) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "ptmx")
	defer master.Close()
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/dev/pts/%d", n)
	port, err := OpenSerial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	if second, err := OpenSerial(path); err == nil {
		second.Close()
		t.Fatal("second UART owner accepted")
	}
	termios, err := unix.IoctlGetTermios(port.(*serialPort).fd, unix.TCGETS)
	if err != nil || termios.Cflag&unix.CBAUD != unix.B115200 || termios.Lflag != 0 || termios.Oflag != 0 {
		t.Fatalf("not raw 115200: %+v %v", termios, err)
	}
	wire, _ := Encode(Frame{EVENT, 42, Wakeup, []byte{6, 0, 0, 0}})
	if _, err := master.Write(wire); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(wire))
	if _, err := io.ReadFull(port, got); err != nil || !bytes.Equal(got, wire) {
		t.Fatalf("RX %x %v", got, err)
	}
	if _, err := port.Write(wire); err != nil {
		t.Fatal(err)
	}
	_ = master.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(master, got); err != nil || !bytes.Equal(got, wire) {
		t.Fatalf("TX %x %v", got, err)
	}
	if err := DrainSerial(port); err != nil {
		t.Fatal(err)
	}
	port.Close()
	if _, err := port.Read(got); err == nil {
		t.Fatal("closed serial read accepted")
	}
}

func TestPoweroffIntentResumeAndFailure(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root ownership contract")
	}
	dir := filepath.Join(t.TempDir(), "intent")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executed := make(chan struct{}, 2)
	execute := func(context.Context) error { executed <- struct{}{}; return nil }
	p, err := NewPoweroff(ctx, dir, execute)
	if err != nil || p.Pending() {
		t.Fatalf("initial intent %v", err)
	}
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	if !p.Pending() {
		t.Fatal("intent was not recorded")
	}
	select {
	case <-executed:
		t.Fatal("poweroff preceded response commit")
	default:
	}
	resumed, err := NewPoweroff(ctx, dir, execute)
	if err != nil || !resumed.Pending() {
		t.Fatalf("resume: %v", err)
	}
	s := NewService(resumed)
	defer s.Close()
	if s.State().RVState != "shutting_down" {
		t.Fatal("pending intent became ready")
	}
	resumed.Commit()
	resumed.Commit()
	select {
	case <-executed:
	case <-time.After(time.Second):
		t.Fatal("poweroff not resumed")
	}
	select {
	case <-executed:
		t.Fatal("duplicate poweroff")
	default:
	}
	if err := os.Chmod(filepath.Join(dir, "intent.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPoweroff(ctx, dir, execute); err == nil {
		t.Fatal("unsafe marker accepted")
	}
	broken := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(broken, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	p = &Poweroff{ctx: ctx, directory: filepath.Join(broken, "intent"), execute: execute}
	if err := p.Begin(); err == nil || p.Pending() {
		t.Fatal("failed persistence acknowledged")
	}
	// Even without a marker, schedule ordinary Linux poweroff to reduce writes
	// before MCU's forced deadline. The final hook will withhold READY.
	p.Commit()
	select {
	case <-executed:
	case <-time.After(time.Second):
		t.Fatal("emergency poweroff not scheduled")
	}
}
