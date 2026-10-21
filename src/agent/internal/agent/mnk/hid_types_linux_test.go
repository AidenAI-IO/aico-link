//go:build linux

package mnk

import (
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHIDDeviceWriteTimesOutWhenFDWouldBlock(t *testing.T) {
	fds := []int{0, 0}
	if err := unix.Pipe2(fds, unix.O_NONBLOCK); err != nil {
		t.Fatalf("Pipe2: %v", err)
	}
	readFile := os.NewFile(uintptr(fds[0]), "pipe-read")
	writeFile := os.NewFile(uintptr(fds[1]), "pipe-write")
	defer readFile.Close()
	defer writeFile.Close()

	buf := make([]byte, 4096)
	for {
		_, err := unix.Write(int(writeFile.Fd()), buf)
		if err == unix.EAGAIN {
			break
		}
		if err != nil {
			t.Fatalf("fill pipe: %v", err)
		}
	}

	dev := &HIDDevice{
		path:         "blocked-hid",
		file:         writeFile,
		writeTimeout: 20 * time.Millisecond,
	}
	start := time.Now()
	err := dev.Write([]byte{1})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("blocked write returned after %v, want bounded timeout", elapsed)
	}
}
