// Command benchmark-mcu emulates MCU firmware for benchmark daemon containers.
//
// Only the UART peer is simulated. The real mcu.Service, link protocol and UDS
// server run unchanged, so the agent's schedule tool, routines manager and MCU
// client exercise the same code paths as on a device. Wake timers count down in
// real time and fire an RTC wakeup event; hardware this emulator does not have
// (LEDs, suspend, poweroff) is reported as unsupported or refused.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"log"
	"net"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"aico-agent/internal/mcu"
)

func main() {
	socket := flag.String("socket", "/tmp/aico-mcu/mcu_service.sock", "MCU service socket to serve")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *socket); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

// run serves the socket only after the emulated link is ready, so clients never
// observe a booting service.
func run(ctx context.Context, socket string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rv, board := net.Pipe()
	// No shutdown coordinator: a container must never be powered off by the agent.
	service := mcu.NewService(nil)
	defer service.Close()
	link := mcu.NewLink(rv)
	// The hardware guard covers a real MCU boot race that does not exist here.
	link.Guard = 0
	service.Attach(link)
	fw := &firmware{port: board, now: time.Now}
	go fw.serve(ctx)
	go func() { _ = link.Run(ctx); cancel() }()
	deadline := time.Now().Add(5 * time.Second)
	for service.State().RVState != "ready" {
		if time.Now().After(deadline) {
			return errors.New("emulated MCU link did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	server, err := mcu.Listen(socket, service)
	if err != nil {
		return err
	}
	defer server.Close()
	log.Printf("benchmark MCU emulator serving %s", socket)
	<-ctx.Done()
	return ctx.Err()
}

type firmware struct {
	port net.Conn
	now  func() time.Time
	mu   sync.Mutex // guards the timer state and event sequence; net.Pipe serializes writes
	due  time.Time  // zero while the wake timer is inactive
	seq  byte
}

func (f *firmware) serve(ctx context.Context) {
	go f.fireTimer(ctx)
	parser := mcu.Parser{}
	buffer := make([]byte, 256)
	for {
		n, err := f.port.Read(buffer)
		if err != nil {
			return
		}
		for _, frame := range parser.Feed(buffer[:n], f.now()) {
			if frame.Type == mcu.REQ {
				f.write(mcu.Frame{Type: mcu.RESP, Seq: frame.Seq, Command: frame.Command, Payload: f.respond(frame)})
			}
		}
	}
}

func (f *firmware) respond(frame mcu.Frame) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch frame.Command {
	case mcu.GetTimer:
		return f.timerPayload()
	case mcu.SetTimer:
		if len(frame.Payload) != 6 || frame.Payload[0] != 0 || frame.Payload[1] != 1 {
			return []byte{mcu.Invalid}
		}
		delay := binary.LittleEndian.Uint32(frame.Payload[2:])
		if delay < 1 || delay > 604800 {
			return []byte{mcu.Invalid}
		}
		f.due = f.now().Add(time.Duration(delay) * time.Second)
		return f.timerPayload()
	case mcu.CancelTimer:
		f.due = time.Time{}
		return f.timerPayload()
	case mcu.GetBattery:
		// A steady, not-charging battery: percent, millivolts, charge state, reserved.
		return []byte{mcu.Success, 80, 0x6e, 0x0f, 0, 0}
	case mcu.SuspendPrepare, mcu.SuspendResume:
		return []byte{mcu.Denied}
	default:
		// Includes LED commands: the service reports this MCU as LED-unsupported.
		return []byte{mcu.Invalid}
	}
}

// timerPayload must be called with f.mu held.
func (f *firmware) timerPayload() []byte {
	payload := []byte{mcu.Success, 0, 0, 0, 0, 0, 0, 0}
	if !f.due.IsZero() {
		remaining := (f.due.Sub(f.now()) + time.Second - 1) / time.Second
		payload[2] = 1
		binary.LittleEndian.PutUint32(payload[4:], uint32(max(remaining, 1)))
	}
	return payload
}

func (f *firmware) fireTimer(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		f.mu.Lock()
		fired := !f.due.IsZero() && !f.now().Before(f.due)
		if fired {
			f.due = time.Time{}
		}
		f.mu.Unlock()
		if fired {
			// Wakeup source 4 is the RTC timer, as reported by device firmware.
			f.write(mcu.Frame{Type: mcu.EVENT, Command: mcu.Wakeup, Payload: []byte{4, 0, 0, 0}})
		}
	}
}

func (f *firmware) write(frame mcu.Frame) {
	f.mu.Lock()
	if frame.Type == mcu.EVENT {
		frame.Seq = f.seq
		f.seq++
	}
	f.mu.Unlock()
	wire, err := mcu.Encode(frame)
	if err != nil {
		log.Printf("benchmark MCU emulator: %v", err)
		return
	}
	_, _ = f.port.Write(wire)
}
