package realtimevoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type jsonWebSocketTransport struct {
	conn      *websocket.Conn
	label     string
	events    chan Event
	errs      chan error
	done      chan struct{}
	writeGate chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newJSONWebSocketTransport(conn *websocket.Conn, label string, eventBuffer int) *jsonWebSocketTransport {
	transport := &jsonWebSocketTransport{
		conn:      conn,
		label:     label,
		events:    make(chan Event, buffer(eventBuffer)),
		errs:      make(chan error, 1),
		done:      make(chan struct{}),
		writeGate: make(chan struct{}, 1),
	}
	transport.writeGate <- struct{}{}
	return transport
}

func (t *jsonWebSocketTransport) start(translate func([]byte) []Event) {
	go t.readLoop(translate)
}

func (t *jsonWebSocketTransport) Events() <-chan Event  { return t.events }
func (t *jsonWebSocketTransport) Errors() <-chan error  { return t.errs }
func (t *jsonWebSocketTransport) Done() <-chan struct{} { return t.done }

func (t *jsonWebSocketTransport) writeJSON(ctx context.Context, value any) error {
	if ctx == nil {
		return fmt.Errorf("%s: nil context", t.label)
	}
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s: marshal event: %w", t.label, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return fmt.Errorf("%s: session is closed", t.label)
	case <-t.writeGate:
	}
	defer func() { t.writeGate <- struct{}{} }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return fmt.Errorf("%s: session is closed", t.label)
	default:
	}
	// A websocket write is a blocking syscall. Closing the connection from a
	// cancellation callback is the only reliable way to interrupt it; the
	// resulting transport shutdown also prevents a partially written request
	// from leaving the session reusable.
	stopCancel := context.AfterFunc(ctx, func() { _ = t.conn.Close() })
	defer stopCancel()
	deadline := time.Now().Add(10 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = t.conn.SetWriteDeadline(deadline)
	defer t.conn.SetWriteDeadline(time.Time{})
	if err := t.conn.WriteMessage(websocket.TextMessage, body); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%s websocket write: %w", t.label, err)
	}
	return nil
}

func (t *jsonWebSocketTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.done)
		t.closeErr = t.conn.Close()
	})
	return t.closeErr
}

func (t *jsonWebSocketTransport) readLoop(translate func([]byte) []Event) {
	defer close(t.events)
	defer close(t.errs)
	defer t.closeOnce.Do(func() { close(t.done); _ = t.conn.Close() })
	for {
		_, body, err := t.conn.ReadMessage()
		if err != nil {
			if !normalClose(err) && !errors.Is(err, context.Canceled) {
				select {
				case t.errs <- classifyTransportError(err):
				default:
				}
			}
			return
		}
		for _, event := range translate(body) {
			select {
			case t.events <- event:
			case <-t.done:
				return
			}
		}
	}
}
