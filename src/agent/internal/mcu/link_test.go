package mcu

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type channelPort struct {
	rx, tx chan []byte
	done   chan struct{}
	once   sync.Once
	busy   atomic.Int32
}

func newChannelPort() *channelPort {
	return &channelPort{rx: make(chan []byte, 64), tx: make(chan []byte, 64), done: make(chan struct{})}
}
func (p *channelPort) Read(b []byte) (int, error) {
	select {
	case wire := <-p.rx:
		return copy(b, wire), nil
	case <-p.done:
		return 0, io.EOF
	}
}
func (p *channelPort) Write(b []byte) (int, error) {
	if p.busy.Add(-1) >= 0 {
		return 0, ErrTransmitBusy
	}
	select {
	case p.tx <- append([]byte(nil), b...):
		return len(b), nil
	case <-p.done:
		return 0, io.ErrClosedPipe
	}
}
func (p *channelPort) Close() error { p.once.Do(func() { close(p.done) }); return nil }
func (p *channelPort) send(f Frame) { wire, _ := Encode(f); p.rx <- wire }
func (p *channelPort) receive(t *testing.T) Frame {
	t.Helper()
	select {
	case wire := <-p.tx:
		parser := Parser{}
		frames := parser.Feed(wire, time.Now())
		if len(frames) != 1 {
			t.Fatalf("invalid output: %x", wire)
		}
		return frames[0]
	case <-time.After(time.Second):
		t.Fatal("no UART output")
		return Frame{}
	}
}

func TestLinkReceiveCacheAndBackpressure(t *testing.T) {
	p := newChannelPort()
	p.busy.Store(2)
	l := NewLink(p)
	l.Guard = time.Hour
	defer l.Close()
	var handled atomic.Int32
	l.Handle = func(f Frame) Frame { handled.Add(1); return Frame{Payload: []byte{Success}} }
	go l.Run(context.Background())
	request := Frame{REQ, 0, ShutdownRequest, []byte{1, 0, 0, 0}}
	p.send(request)
	if f := p.receive(t); f.Payload[0] != Success {
		t.Fatal(f)
	}
	p.send(request)
	if f := p.receive(t); f.Payload[0] != Success {
		t.Fatal(f)
	}
	request.Payload = []byte{2, 0, 0, 0}
	p.send(request)
	if f := p.receive(t); f.Payload[0] != Invalid {
		t.Fatal(f)
	}
	if handled.Load() != 1 {
		t.Fatal("duplicate executed a side effect")
	}
	for seq := byte(1); seq < 16; seq++ {
		p.send(Frame{REQ, seq, GetState, nil})
		if f := p.receive(t); f.Payload[0] != Success {
			t.Fatal(f)
		}
	}
	p.send(Frame{REQ, 16, GetState, nil})
	if f := p.receive(t); f.Payload[0] != Busy {
		t.Fatal(f)
	}
	if handled.Load() != 16 {
		t.Fatal("full cache dispatched a request")
	}
}

func TestLinkStartupGuardStillAnswersMCU(t *testing.T) {
	p := newChannelPort()
	l := NewLink(p)
	l.Guard = 150 * time.Millisecond
	defer l.Close()
	l.Handle = func(f Frame) Frame { return Frame{Payload: []byte{Success}} }
	started := time.Now()
	go l.Run(context.Background())
	p.send(Frame{REQ, 42, GetState, nil})
	if f := p.receive(t); f.Type != RESP || f.Seq != 42 {
		t.Fatal(f)
	}
	if f := p.receive(t); f.Command != RVReady || time.Since(started) < l.Guard {
		t.Fatal("early or absent RV_READY", f)
	}
}

func TestLinkShutdownStopsPendingAndQueuedCommands(t *testing.T) {
	p := newChannelPort()
	l := NewLink(p)
	l.Guard = 0
	s := NewService(&fakeShutdown{})
	defer s.Close()
	defer l.Close()
	s.Attach(l)
	go l.Run(context.Background())
	if f := p.receive(t); f.Command != RVReady {
		t.Fatal(f)
	}
	results := make(chan error, 2)
	go func() { _, err := l.Request(context.Background(), SetTimer, []byte{0, 1, 10, 0, 0, 0}); results <- err }()
	if f := p.receive(t); f.Command != SetTimer {
		t.Fatal(f)
	}
	go func() { _, err := l.Request(context.Background(), CancelTimer, []byte{0}); results <- err }()
	p.send(Frame{REQ, 99, ShutdownRequest, []byte{1, 0, 0, 0}})
	if f := p.receive(t); f.Command != ShutdownRequest || f.Payload[0] != Success {
		t.Fatal(f)
	}
	for range 2 {
		select {
		case err := <-results:
			var api *APIError
			if !errors.As(err, &api) || api.Status != "NOT_READY" {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("application request left pending")
		}
	}
	select {
	case wire := <-p.tx:
		t.Fatalf("application traffic after shutdown: %x", wire)
	case <-time.After(150 * time.Millisecond):
	}
}
