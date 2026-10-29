package mcu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const IntentDirectory = "/run/aico-mcu-poweroff"

type shutdownIntent struct {
	BootID     string `json:"boot_id"`
	ReceivedAt string `json:"received_at"`
}
type Poweroff struct {
	mu                sync.Mutex
	ctx               context.Context
	directory, bootID string
	pending, running  bool
	execute           func(context.Context) error
}

func NewPoweroff(ctx context.Context, directory string, execute func(context.Context) error) (*Poweroff, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	p := &Poweroff{ctx: ctx, directory: directory, bootID: strings.TrimSpace(string(boot)), execute: execute}
	p.pending, err = loadShutdownIntent(directory, p.bootID)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func loadShutdownIntent(directory, bootID string) (bool, error) {
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByRoot(info) {
		return false, errors.New("unsafe shutdown intent directory")
	}
	path := filepath.Join(directory, "intent.json")
	info, err = os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ownedByRoot(info) {
		return false, errors.New("unsafe shutdown marker")
	}
	data, err := os.ReadFile(filepath.Join(directory, "intent.json"))
	if err != nil {
		return false, err
	}
	var intent shutdownIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return false, err
	}
	if intent.BootID != bootID {
		return false, errors.New("shutdown intent belongs to another boot")
	}
	return true, nil
}
func (p *Poweroff) Pending() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.pending }
func (p *Poweroff) Begin() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var setupErr error
	if !p.pending {
		setupErr = os.MkdirAll(p.directory, 0700)
		if setupErr == nil {
			var info os.FileInfo
			info, setupErr = os.Lstat(p.directory)
			if setupErr == nil && (!info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByRoot(info)) {
				setupErr = errors.New("unsafe shutdown intent directory")
			}
		}
		if setupErr == nil {
			data, _ := json.Marshal(shutdownIntent{p.bootID, time.Now().UTC().Format(time.RFC3339Nano)})
			path := filepath.Join(p.directory, "intent.json")
			var f *os.File
			f, setupErr = os.CreateTemp(p.directory, ".intent-")
			if setupErr == nil {
				name := f.Name()
				_, setupErr = f.Write(data)
				closeErr := f.Close()
				if setupErr == nil {
					setupErr = closeErr
				}
				if setupErr == nil {
					setupErr = os.Rename(name, path)
				}
				_ = os.Remove(name)
			}
		}
		p.pending = setupErr == nil
	}
	return setupErr
}

// Commit is called after the ACK has been written, or by the bounded emergency
// fallback when the UART cannot transmit it. It never waits for systemd.
func (p *Poweroff) Commit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		p.running = true
		go p.requestPoweroff()
	}
}
func (p *Poweroff) requestPoweroff() {
	ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	defer cancel()
	for {
		attempt, stop := context.WithTimeout(ctx, 2*time.Second)
		err := p.execute(attempt)
		stop()
		if err == nil {
			return
		}
		log.Printf("MCU poweroff scheduling failed: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
func SystemPoweroff(ctx context.Context) error {
	return exec.CommandContext(ctx, "/usr/bin/systemctl", "--no-block", "poweroff").Run()
}

// CheckStorageSafe is deliberately fail-closed for unknown writable filesystems.
// systemd-shutdown, not this helper, is responsible for flushing and unmounting.
func CheckStorageSafe(mounts, swaps []byte) error {
	lines := strings.Split(strings.TrimSpace(string(swaps)), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "Filename") {
		return errors.New("swap is active or its state cannot be verified")
	}
	pseudo := map[string]bool{"proc": true, "sysfs": true, "tmpfs": true, "devtmpfs": true, "devpts": true, "cgroup": true, "cgroup2": true, "securityfs": true, "debugfs": true, "tracefs": true, "pstore": true, "configfs": true, "mqueue": true, "hugetlbfs": true, "bpf": true, "fusectl": true, "ramfs": true}
	if len(strings.TrimSpace(string(mounts))) == 0 {
		return errors.New("mount state is empty")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(mounts)), "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			return errors.New("invalid mountinfo")
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(left) < 6 || len(right) < 3 {
			return errors.New("invalid mountinfo fields")
		}
		if strings.Contains(","+left[5]+",", ",rw,") && !pseudo[right[0]] {
			return fmt.Errorf("persistent filesystem is writable: %s", left[4])
		}
	}
	return nil
}
func ShutdownHook(action, port, directory string) error {
	if action != "poweroff" {
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("MCU shutdown hook requires root")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	pending, err := loadShutdownIntent(directory, strings.TrimSpace(string(boot)))
	if err != nil {
		return err
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	swaps, err := os.ReadFile("/proc/swaps")
	if err != nil {
		return err
	}
	if err := CheckStorageSafe(mounts, swaps); err != nil {
		return err
	}
	uart, err := OpenSerial(port)
	if err != nil {
		return err
	}
	defer uart.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return finishShutdown(ctx, uart, pending, func() error { return DrainSerial(uart) })
}

// Called only at systemd's final poweroff stage, after checking storage safety.
// A normal Linux poweroff has no daemon intent: answer the MCU's GET_STATE
// before sending SHUTDOWN_READY so it enters shutdown and clears its ON target.
// Read must be bounded (OpenSerial polls for 10 ms), and drain must finish TX.
func finishShutdown(ctx context.Context, uart io.ReadWriter, pending bool, drain func() error) error {
	write := func(frame Frame) error {
		wire, err := Encode(frame)
		if err != nil {
			return err
		}
		if n, err := uart.Write(wire); err != nil {
			return err
		} else if n != len(wire) {
			return io.ErrShortWrite
		}
		return drain()
	}
	ready := Frame{EVENT, 0, ShutdownReady, []byte{1, 0, 0, 0}}
	var parser Parser
	var lastReady time.Time
	var buffer [256]byte
	for ctx.Err() == nil {
		if pending && (lastReady.IsZero() || time.Since(lastReady) >= 100*time.Millisecond) {
			if err := write(ready); err != nil {
				return err
			}
			lastReady = time.Now()
		}
		n, err := uart.Read(buffer[:])
		if err != nil {
			return err
		}
		for _, frame := range parser.Feed(buffer[:n], time.Now()) {
			if frame.Type != REQ {
				continue
			}
			var payload []byte
			switch {
			case frame.Command == GetState && len(frame.Payload) == 0:
				payload = []byte{Success, 1, 1, 0, 0, ShuttingDown}
			case frame.Command == ShutdownRequest && bytes.Equal(frame.Payload, []byte{1, 0, 0, 0}):
				payload = []byte{Success}
			default:
				continue
			}
			if err := write(Frame{RESP, frame.Seq, frame.Command, payload}); err != nil {
				return err
			}
			pending = true
			lastReady = time.Time{}
		}
	}
	// Successful MCU power removal terminates this process. Keep answering
	// probes and retrying READY until then: a buffered stale probe may no longer
	// match the MCU's pending request, and V1 events have no acknowledgement.
	return fmt.Errorf("MCU power removal did not complete: %w", ctx.Err())
}
