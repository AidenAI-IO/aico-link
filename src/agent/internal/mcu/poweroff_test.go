package mcu

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// Like the Linux serial port, Read returns periodically even without input.
type shutdownTestPort struct {
	*channelPort
	readErr, writeErr error
	shortWrite        bool
}

func (p *shutdownTestPort) Read(b []byte) (int, error) {
	if p.readErr != nil {
		return 0, p.readErr
	}
	select {
	case wire := <-p.rx:
		return copy(b, wire), nil
	case <-p.done:
		return 0, io.EOF
	case <-time.After(time.Millisecond):
		return 0, nil
	}
}

func (p *shutdownTestPort) Write(b []byte) (int, error) {
	if p.writeErr != nil {
		return 0, p.writeErr
	}
	if p.shortWrite {
		return len(b) - 1, nil
	}
	return p.channelPort.Write(b)
}

func assertShutdownReady(t *testing.T, f Frame) {
	t.Helper()
	if f.Type != EVENT || f.Command != ShutdownReady || !bytes.Equal(f.Payload, []byte{1, 0, 0, 0}) {
		t.Fatalf("expected SHUTDOWN_READY, got %+v", f)
	}
}

func TestFinalShutdownNormalPoweroff(t *testing.T) {
	p := &shutdownTestPort{channelPort: newChannelPort()}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- finishShutdown(ctx, p, false, func() error { return nil }) }()
	// Unsolicited responses, events and malformed requests cannot authorize
	// READY or start the MCU's forced cutoff timer.
	for _, f := range []Frame{
		{RESP, 1, GetState, []byte{Success}},
		{EVENT, 2, GetState, nil},
		{REQ, 3, GetState, []byte{0}},
		{REQ, 4, ShutdownRequest, []byte{1, 0, 0, 1}},
		{REQ, 5, GetBattery, nil},
	} {
		p.send(f)
	}
	select {
	case wire := <-p.tx:
		t.Fatalf("shutdown transmitted before a valid MCU request: %x", wire)
	case <-time.After(30 * time.Millisecond):
	}
	// The first query could be stale UART input. The hook must stay available
	// for a fresh probe with a different sequence number.
	for _, seq := range []byte{42, 43} {
		p.send(Frame{REQ, seq, GetState, nil})
		for {
			f := p.receive(t)
			if f.Type == EVENT {
				assertShutdownReady(t, f)
				continue
			}
			if f.Type != RESP || f.Seq != seq || f.Command != GetState ||
				!bytes.Equal(f.Payload, []byte{Success, 1, 1, 0, 0, ShuttingDown}) {
				t.Fatalf("incorrect shutdown state response: %+v", f)
			}
			break
		}
		assertShutdownReady(t, p.receive(t))
	}
	// Completion events have no acknowledgement, so retry a lost READY.
	assertShutdownReady(t, p.receive(t))
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("helper did not stop at its bound: %v", err)
	}
}

func TestFinalShutdownExistingIntentAndMCURequest(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "MCU request during final shutdown", true: "existing intent"}[pending], func(t *testing.T) {
			p := &shutdownTestPort{channelPort: newChannelPort()}
			defer p.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- finishShutdown(ctx, p, pending, func() error { return nil }) }()
			if !pending {
				p.send(Frame{REQ, 7, ShutdownRequest, []byte{1, 0, 0, 0}})
				f := p.receive(t)
				if f.Type != RESP || f.Seq != 7 || f.Command != ShutdownRequest || !bytes.Equal(f.Payload, []byte{Success}) {
					t.Fatalf("incorrect shutdown acknowledgement: %+v", f)
				}
			}
			assertShutdownReady(t, p.receive(t))
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestFinalShutdownFailures(t *testing.T) {
	failed := errors.New("UART failure")
	for _, tc := range []struct {
		name              string
		readErr, writeErr error
		shortWrite        bool
		drainErr, want    error
	}{
		{name: "read", readErr: failed, want: failed},
		{name: "write", writeErr: failed, want: failed},
		{name: "short write", shortWrite: true, want: io.ErrShortWrite},
		{name: "drain", drainErr: failed, want: failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &shutdownTestPort{channelPort: newChannelPort(), readErr: tc.readErr, writeErr: tc.writeErr, shortWrite: tc.shortWrite}
			defer p.Close()
			p.send(Frame{REQ, 42, GetState, nil})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := finishShutdown(ctx, p, false, func() error { return tc.drainErr })
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			for len(p.tx) > 0 {
				if f := p.receive(t); f.Command == ShutdownReady {
					t.Fatal("READY sent despite failed state response")
				}
			}
		})
	}
}

func TestFinalShutdownMissingMCU(t *testing.T) {
	p := &shutdownTestPort{channelPort: newChannelPort()}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := finishShutdown(ctx, p, false, func() error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || len(p.tx) != 0 {
		t.Fatalf("missing MCU must time out without sending READY: %v", err)
	}
}

func TestShutdownHookIgnoresOtherActions(t *testing.T) {
	for _, action := range []string{"reboot", "halt", "kexec", ""} {
		if err := ShutdownHook(action, "/does-not-exist", "/does-not-exist"); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
}
