package mcu

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func readMessage(r io.Reader, limit uint32) ([]byte, error) {
	var prefix [12]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(prefix[:4])
	if n == 0 || n > limit || binary.LittleEndian.Uint64(prefix[4:]) != 0 {
		return nil, errors.New("invalid MCU UDS envelope")
	}
	data := make([]byte, n)
	_, err := io.ReadFull(r, data)
	return data, err
}
func writeMessage(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > 256*1024 {
		return errors.New("MCU UDS response too large")
	}
	wire := make([]byte, 12+len(data))
	binary.LittleEndian.PutUint32(wire, uint32(len(data)))
	copy(wire[12:], data)
	for len(wire) > 0 {
		n, err := w.Write(wire)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		wire = wire[n:]
	}
	return nil
}

// A connection carries one operation. Dedicated wait connections make client
// disconnect observable without consuming a subsequent pipelined request.
type UDSServer struct {
	listener    *net.UnixListener
	service     *Service
	mu          sync.Mutex
	connections map[net.Conn]context.CancelFunc
	closed      bool
	wg          sync.WaitGroup
	path        string
}

func Listen(path string, s *Service) (*UDSServer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing to replace a non-socket path")
		}
		if c, err := net.DialTimeout("unix", path, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return nil, errors.New("MCU socket already in use")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0660); err != nil {
		_ = l.Close()
		return nil, err
	}
	u := &UDSServer{listener: l, service: s, connections: make(map[net.Conn]context.CancelFunc), path: path}
	u.wg.Add(1)
	go u.accept()
	return u, nil
}
func (u *UDSServer) accept() {
	defer u.wg.Done()
	for {
		c, err := u.listener.AcceptUnix()
		if err != nil {
			return
		}
		u.mu.Lock()
		if u.closed || len(u.connections) >= 64 {
			u.mu.Unlock()
			_ = c.Close()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		u.connections[c] = cancel
		u.wg.Add(1)
		u.mu.Unlock()
		go u.serve(ctx, c)
	}
}
func (u *UDSServer) serve(ctx context.Context, c net.Conn) {
	defer u.wg.Done()
	defer func() { _ = c.Close(); u.mu.Lock(); u.connections[c](); delete(u.connections, c); u.mu.Unlock() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	data, err := readMessage(c, 16*1024)
	if err != nil {
		return
	}
	r, err := DecodeRequest(data)
	if err != nil {
		_ = writeMessage(c, errorResponse(err))
		return
	}
	wait := r.WaitMS
	if wait < 0 || wait > 25000 {
		wait = 0
	}
	_ = c.SetDeadline(time.Now().Add(time.Duration(wait)*time.Millisecond + 5*time.Second))
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { var extra [1]byte; _, _ = c.Read(extra[:]); cancel() }()
	response := u.service.Handle(requestCtx, r)
	if requestCtx.Err() == nil {
		_ = writeMessage(c, response)
	}
}
func (u *UDSServer) Close() {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.closed = true
	_ = u.listener.Close()
	for c, cancel := range u.connections {
		cancel()
		_ = c.Close()
	}
	u.mu.Unlock()
	u.wg.Wait()
}

type Client struct{ Socket string }

func (c Client) Do(ctx context.Context, r Request) (Response, error) {
	path := c.Socket
	if path == "" {
		path = DefaultSocket
	}
	wait := r.WaitMS
	if wait < 0 || wait > 25000 {
		wait = 0
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(wait)*time.Millisecond+5*time.Second)
	defer cancel()
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if err := writeMessage(conn, r); err != nil {
		return Response{}, err
	}
	// Once a complete mutation has been sent, losing its response cannot prove
	// that it failed. Keep this uncertainty across the HTTP adapter as well.
	_, _, mutating, _ := encodeRequest(r)
	responseError := func(status, message string) (Response, error) {
		api := &APIError{Status: status, Message: message, UnknownOutcome: mutating}
		return errorResponse(api), api
	}
	data, err := readMessage(conn, 256*1024)
	if err != nil {
		var timeout net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
			return responseError("TIMEOUT", "MCU service response timed out")
		}
		return responseError("SERVICE_UNAVAILABLE", "MCU service response unavailable")
	}
	var response Response
	if err := json.Unmarshal(data, &response); err != nil || response.Status == "" {
		return responseError("PROTOCOL_ERROR", "invalid MCU service response")
	}
	if response.Status != "OK" {
		return response, &APIError{Status: response.Status, Message: response.Error, MCUStatus: response.MCUStatus, UnknownOutcome: response.UnknownOutcome}
	}
	return response, nil
}
func (c Client) GetState(ctx context.Context) (*State, error) {
	r, e := c.Do(ctx, Request{Op: "get_state"})
	return r.State, e
}
func (c Client) Poweroff(ctx context.Context, generation, id string) (*State, error) {
	r, e := c.Do(ctx, Request{Op: "poweroff", Generation: generation, RequestID: id})
	return r.State, e
}
func (c Client) GetBattery(ctx context.Context) (*Battery, error) {
	r, e := c.Do(ctx, Request{Op: "get_battery"})
	return r.Battery, e
}
func (c Client) GetWakeupTimer(ctx context.Context) (*Timer, error) {
	r, e := c.Do(ctx, Request{Op: "get_wakeup_timer"})
	return r.Timer, e
}
func (c Client) SetWakeupTimer(ctx context.Context, seconds int64, generation, id string) (*Timer, error) {
	r, e := c.Do(ctx, Request{Op: "set_wakeup_timer", DelayS: seconds, Generation: generation, RequestID: id})
	return r.Timer, e
}
func (c Client) CancelWakeupTimer(ctx context.Context, generation, id string) (*Timer, error) {
	r, e := c.Do(ctx, Request{Op: "cancel_wakeup_timer", Generation: generation, RequestID: id})
	return r.Timer, e
}
func (c Client) EventsSince(ctx context.Context, since, generation string, limit, waitMS int) (*EventPage, error) {
	r, e := c.Do(ctx, Request{Op: "events_since", Since: since, Generation: generation, Limit: limit, WaitMS: waitMS})
	return r.EventPage, e
}
func (c Client) GetLED(ctx context.Context, led string) (*LEDState, error) {
	r, e := c.Do(ctx, Request{Op: "get_led", LED: led})
	return r.LED, e
}
func (c Client) SetLED(ctx context.Context, r Request) (*LEDState, error) {
	r.Op = "set_led"
	response, e := c.Do(ctx, r)
	return response.LED, e
}
