//go:build linux

package mcu

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
