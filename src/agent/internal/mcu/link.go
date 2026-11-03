package mcu

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrTransmitBusy means that zero bytes were accepted; replay is safe.
var ErrTransmitBusy = errors.New("UART transmit queue is busy")

type linkResult struct {
	frame Frame
	err   error
}
type linkRequest struct {
	ctx     context.Context
	cmd     byte
	payload []byte
	result  chan linkResult
}
type cachedReply struct {
	request, response              Frame
	created, retryUntil, nextRetry time.Time
	retries                        int
	pending                        bool
}

// Link owns all UART writes and the sequence/cache state. Port writes must be
// bounded (the Linux port uses a 20 ms deadline); reads unblock on Close.
type Link struct {
	port           io.ReadWriteCloser
	requests       chan linkRequest
	done           chan struct{}
	closeOnce      sync.Once
	Handle         func(Frame) Frame
	OnEvent        func(Frame)
	OnRX           func()
	OnResponseSent func(Frame)
	OnReady        func() bool
	AllowRequest   func(byte) bool
	Guard          time.Duration
}

func NewLink(port io.ReadWriteCloser) *Link {
	return &Link{port: port, requests: make(chan linkRequest, 32), done: make(chan struct{}), Guard: 2 * time.Second}
}
func (l *Link) Close() { l.closeOnce.Do(func() { close(l.done); _ = l.port.Close() }) }
func (l *Link) Request(ctx context.Context, cmd byte, payload []byte) (Frame, error) {
	r := linkRequest{ctx, cmd, append([]byte(nil), payload...), make(chan linkResult, 1)}
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-l.done:
		return Frame{}, errors.New("MCU link disconnected")
	default:
	}
	select {
	case l.requests <- r:
	default:
		return Frame{}, &APIError{Status: "BUSY", Message: "MCU request queue is full"}
	}
	select {
	case result := <-r.result:
		return result.frame, result.err
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-l.done:
		return Frame{}, errors.New("MCU link disconnected")
	}
}

func (l *Link) write(f Frame) error {
	wire, err := Encode(f)
	if err != nil {
		return err
	}
	n, err := l.port.Write(wire)
	if err == nil && n != len(wire) {
		err = io.ErrShortWrite
	}
	return err
}

func (l *Link) Run(ctx context.Context) error {
	defer l.Close()
	rx := make(chan Frame, 64)
	readErrors := make(chan error, 1)
	go func() {
		parser := Parser{}
		buffer := make([]byte, 256)
		for {
			n, err := l.port.Read(buffer)
			if err != nil {
				select {
				case readErrors <- err:
				default:
				}
				return
			}
			for _, f := range parser.Feed(buffer[:n], time.Now()) {
				select {
				case rx <- f:
				case <-l.done:
					return
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	started := time.Now()
	readySent := false
	var seqByte [1]byte
	_, _ = rand.Read(seqByte[:])
	next := seqByte[0]
	var used [256]time.Time
	cache := make([]cachedReply, 0, 16)
	var current *linkRequest
	var outgoing Frame
	var began, lastAttempt time.Time
	attempts := 0
	finish := func(f Frame, e error) {
		if current != nil {
			current.result <- linkResult{f, e}
			current = nil
		}
	}
	defer func() { finish(Frame{}, errors.New("MCU link disconnected")) }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	handle := func(f Frame) error {
		now := time.Now()
		if l.OnRX != nil {
			l.OnRX()
		}
		switch f.Type {
		case EVENT:
			if l.OnEvent != nil {
				l.OnEvent(f)
			}
		case RESP:
			if current != nil && f.Seq == outgoing.Seq && f.Command == outgoing.Command {
				finish(f, nil)
			}
		case REQ:
			live := cache[:0]
			for _, c := range cache {
				if now.Sub(c.created) < 2*time.Second {
					live = append(live, c)
				}
			}
			cache = live
			for i := range cache {
				c := &cache[i]
				if c.request.Seq != f.Seq || c.request.Command != f.Command {
					continue
				}
				if !bytes.Equal(c.request.Payload, f.Payload) {
					err := l.write(Frame{RESP, f.Seq, f.Command, []byte{Invalid}})
					if errors.Is(err, ErrTransmitBusy) {
						return nil
					}
					return err
				}
				c.pending = true
				c.retries = 0
				c.retryUntil = now.Add(100 * time.Millisecond)
				c.nextRetry = now
				return nil
			}
			if len(cache) == 16 {
				err := l.write(Frame{RESP, f.Seq, f.Command, []byte{Busy}})
				if errors.Is(err, ErrTransmitBusy) {
					return nil
				}
				return err
			}
			cache = append(cache, cachedReply{request: f, created: now})
			response := Frame{RESP, f.Seq, f.Command, []byte{Invalid}}
			if l.Handle != nil {
				response = l.Handle(f)
			}
			response.Type = RESP
			response.Seq = f.Seq
			response.Command = f.Command
			if _, err := Encode(response); err != nil {
				response.Payload = []byte{InternalError}
			}
			c := &cache[len(cache)-1]
			c.response = response
			c.pending = true
			c.retryUntil = now.Add(100 * time.Millisecond)
			c.nextRetry = now
		}
		return nil
	}
	for {
		// Prioritize already received frames over starting an application request.
		for range 8 {
			select {
			case f := <-rx:
				if err := handle(f); err != nil {
					return err
				}
			default:
			}
		}
		now := time.Now()
		for i := range cache {
			now = time.Now()
			c := &cache[i]
			if !c.pending || now.Before(c.nextRetry) {
				continue
			}
			if !now.Before(c.retryUntil) || c.retries >= 20 {
				c.pending = false
				continue
			}
			err := l.write(c.response)
			if err != nil && !errors.Is(err, ErrTransmitBusy) {
				return err
			}
			c.pending = err != nil
			c.retries++
			c.nextRetry = now.Add(5 * time.Millisecond)
			if err == nil && l.OnResponseSent != nil {
				l.OnResponseSent(c.response)
			}
		}
		if !readySent && now.Sub(started) >= l.Guard {
			if l.OnReady == nil || l.OnReady() {
				err := l.write(Frame{EVENT, next, RVReady, []byte{1, 0, 0, 0}})
				if err != nil && !errors.Is(err, ErrTransmitBusy) {
					return err
				}
				if err == nil {
					next++
					readySent = true
				}
			} else {
				readySent = true
			}
		}
		if current != nil {
			if l.AllowRequest != nil && !l.AllowRequest(outgoing.Command) {
				finish(Frame{}, &APIError{Status: "NOT_READY", Message: "MCU service is stopping"})
			} else if current.ctx.Err() != nil {
				finish(Frame{}, current.ctx.Err())
			} else if now.Sub(began) >= 400*time.Millisecond {
				finish(Frame{}, context.DeadlineExceeded)
			} else if attempts < 4 && now.Sub(lastAttempt) >= 100*time.Millisecond {
				attempts++
				lastAttempt = now
				if err := l.write(outgoing); err != nil && !errors.Is(err, ErrTransmitBusy) {
					return err
				}
			}
		}
		if current == nil && readySent {
			select {
			case r := <-l.requests:
				if l.AllowRequest != nil && !l.AllowRequest(r.cmd) {
					r.result <- linkResult{err: &APIError{Status: "NOT_READY", Message: "MCU service is stopping"}}
					break
				}
				if r.ctx.Err() != nil {
					r.result <- linkResult{err: r.ctx.Err()}
					break
				}
				found := false
				for range 256 {
					if used[next].IsZero() || now.Sub(used[next]) >= 2400*time.Millisecond {
						found = true
						break
					}
					next++
				}
				if !found {
					r.result <- linkResult{err: &APIError{Status: "BUSY", Message: "MCU sequence space is quarantined"}}
					break
				}
				current = &r
				outgoing = Frame{REQ, next, r.cmd, r.payload}
				used[next] = now
				next++
				began = now
				lastAttempt = now
				attempts = 1
				if err := l.write(outgoing); err != nil && !errors.Is(err, ErrTransmitBusy) {
					return err
				}
			default:
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.done:
			return nil
		case err := <-readErrors:
			return err
		case f := <-rx:
			if err := handle(f); err != nil {
				return err
			}
		case <-ticker.C:
		}
	}
}
