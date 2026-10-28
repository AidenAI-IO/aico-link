package mcu

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"time"
)

const DefaultSocket = "/run/mcu_service/mcu_service.sock"

type APIError struct {
	Status         string `json:"status"`
	Message        string `json:"error"`
	MCUStatus      *byte  `json:"mcu_status,omitempty"`
	UnknownOutcome bool   `json:"unknown_outcome,omitempty"`
}

func (e *APIError) Error() string { return e.Status + ": " + e.Message }
func invalid(message string) *APIError {
	return &APIError{Status: "INVALID_ARGUMENT", Message: message}
}

type Request struct {
	Op         string `json:"op"`
	Since      string `json:"since,omitempty"`
	Generation string `json:"generation,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	WaitMS     int    `json:"wait_ms,omitempty"`
	DelayS     int64  `json:"delay_s,omitempty"`
	LED        string `json:"led,omitempty"`
	Mode       string `json:"mode,omitempty"`
	OnMS       int    `json:"on_ms,omitempty"`
	OffMS      int    `json:"off_ms,omitempty"`
	LeaseS     int    `json:"lease_s,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
}
type State struct {
	RVState          string         `json:"rv_state"`
	RVServiceVersion string         `json:"rv_service_version"`
	ProtocolVersion  int            `json:"protocol_version"`
	Generation       string         `json:"generation"`
	UARTConnected    bool           `json:"uart_connected"`
	LastRXAt         string         `json:"last_rx_at,omitempty"`
	LEDSupport       string         `json:"led_support"`
	Plugin           *HardwareEvent `json:"plugin,omitempty"`
	InvalidEvents    uint64         `json:"invalid_events"`
}
type Battery struct {
	Percent       *byte   `json:"percent"`
	Millivolts    *uint16 `json:"millivolts"`
	ChargeState   string  `json:"charge_state"`
	RawPercent    byte    `json:"raw_percent"`
	RawMillivolts uint16  `json:"raw_millivolts"`
	ReceivedAt    string  `json:"received_at"`
}
type Timer struct {
	TimerID    byte   `json:"timer_id"`
	Active     bool   `json:"active"`
	RemainingS uint32 `json:"remaining_s"`
}
type LEDState struct {
	LED            string `json:"led"`
	RequestedMode  string `json:"requested_mode"`
	EffectiveMode  string `json:"effective_mode"`
	OnMS           uint16 `json:"on_ms"`
	OffMS          uint16 `json:"off_ms"`
	RemainingS     uint16 `json:"remaining_s"`
	OutputLevel    bool   `json:"output_level"`
	OverrideReason string `json:"override_reason"`
}
type Response struct {
	Status         string    `json:"status"`
	Error          string    `json:"error,omitempty"`
	MCUStatus      *byte     `json:"mcu_status,omitempty"`
	UnknownOutcome bool      `json:"unknown_outcome,omitempty"`
	State          *State    `json:"state,omitempty"`
	Battery        *Battery  `json:"battery,omitempty"`
	Timer          *Timer    `json:"timer,omitempty"`
	LED            *LEDState `json:"led,omitempty"`
	*EventPage
}
type Requester interface {
	Request(context.Context, byte, []byte) (Frame, error)
}
type ShutdownCoordinator interface {
	Pending() bool
	Begin() error
	Commit()
}
type mutation struct {
	signature string
	created   time.Time
	done      chan struct{}
	result    Response
}
type Service struct {
	mu                    sync.Mutex
	suspendMu             sync.Mutex
	ctx                   context.Context
	cancel                context.CancelFunc
	events                *EventStore
	link                  Requester
	state, shutdownStatus byte
	lastRX                time.Time
	ledSupport            string
	plugin                *HardwareEvent
	invalidEvents         uint64
	wakeVersion           uint64
	shutdown              ShutdownCoordinator
	mutations             map[string]*mutation
}

func NewService(shutdown ShutdownCoordinator) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{ctx: ctx, cancel: cancel, events: NewEventStore(512), state: Booting, ledSupport: "unknown", shutdown: shutdown, mutations: make(map[string]*mutation)}
	if shutdown != nil && shutdown.Pending() {
		s.state = ShuttingDown
		s.events.Close()
	}
	return s
}
func (s *Service) Close() { s.cancel(); s.events.Close() }
func (s *Service) Attach(l *Link) {
	s.mu.Lock()
	s.link = l
	if s.state != ShuttingDown {
		s.state = Booting
	}
	s.ledSupport = "unknown"
	s.mu.Unlock()
	l.Handle = s.HandleFrame
	l.OnEvent = s.HandleEvent
	l.OnResponseSent = func(f Frame) {
		reportedShutdown := f.Command == GetState && len(f.Payload) == 6 &&
			f.Payload[0] == Success && f.Payload[5] == ShuttingDown
		if (f.Command == ShutdownRequest || reportedShutdown) && s.shutdown != nil && s.State().RVState == "shutting_down" {
			if reportedShutdown {
				log.Printf("MCU software shutdown: SHUTTING_DOWN response transmitted")
			}
			s.shutdown.Commit()
		}
	}
	l.OnRX = func() { s.mu.Lock(); s.lastRX = time.Now(); s.mu.Unlock() }
	l.AllowRequest = func(cmd byte) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.state == Ready || (s.state == Sleeping && cmd == SuspendResume)
	}
	l.OnReady = func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.state == ShuttingDown {
			return false
		}
		s.state = Ready
		return true
	}
}
func (s *Service) Detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.link = nil
	if s.state != ShuttingDown {
		s.state = Booting
	}
}
func (s *Service) HandleFrame(f Frame) Frame {
	payload := []byte{Invalid}
	if f.Command == GetState && len(f.Payload) == 0 {
		s.mu.Lock()
		payload = []byte{Success, 1, 1, 0, 0, s.state}
		s.mu.Unlock()
	}
	if f.Command == ShutdownRequest && bytes.Equal(f.Payload, []byte{1, 0, 0, 0}) {
		s.mu.Lock()
		already := s.state == ShuttingDown
		s.state = ShuttingDown
		s.mu.Unlock()
		s.events.Close()
		if !already {
			status := byte(Success)
			if s.shutdown == nil {
				status = InternalError
			} else if err := s.shutdown.Begin(); err != nil {
				status = InternalError
				log.Printf("MCU shutdown intent failed: %v; proceeding with Linux cleanup without SHUTDOWN_READY", err)
			}
			s.mu.Lock()
			s.shutdownStatus = status
			s.mu.Unlock()
			if s.shutdown != nil {
				// MCU's forced cutoff is already running. Stop Linux even if
				// intent persistence failed; the final hook then withholds READY.
				time.AfterFunc(100*time.Millisecond, s.shutdown.Commit)
			}
		}
		s.mu.Lock()
		payload = []byte{s.shutdownStatus}
		s.mu.Unlock()
	}
	return Frame{RESP, f.Seq, f.Command, payload}
}
func (s *Service) HandleEvent(f Frame) {
	log.Printf("MCU EVENT cmd=0x%02x seq=%d payload=%x", f.Command, f.Seq, f.Payload)
	if f.Command != Wakeup && f.Command != PluginChanged {
		return
	}
	p := f.Payload
	valid := len(p) == 4
	if valid {
		if f.Command == Wakeup {
			valid = p[1] == 0 && p[2] == 0 && p[3] == 0
		} else {
			valid = p[0] <= 1 && p[1] <= 1 && p[0]+p[1] == 1 && p[2] == 0 && p[3] == 0
		}
	}
	if !valid {
		log.Printf("MCU invalid event payload cmd=0x%02x", f.Command)
		s.mu.Lock()
		s.invalidEvents++
		s.mu.Unlock()
		return
	}
	e := HardwareEvent{UARTSeq: f.Seq}
	if f.Command == Wakeup {
		s.mu.Lock()
		s.wakeVersion++
		if s.state == Sleeping {
			s.state = Ready
		}
		s.mu.Unlock()
		e.Type = "wakeup"
		e.SourceCode = p[0]
		e.Source = "unknown"
		if p[0] >= 1 && p[0] <= 6 {
			e.Source = []string{"external_power", "power_switch", "touch_key", "rtc_timer", "other", "asr"}[p[0]-1]
		}
	} else {
		e.Type = "rk628_plugin_det_changed"
		inserted := p[0] == 1
		raw := p[1]
		e.Inserted = &inserted
		e.RawLevel = &raw
	}
	published := s.events.Append(e)
	if published.ID == "" {
		// Shutdown closed event publication; retain the last published plug
		// snapshot rather than silently replacing it with a rejected event.
		return
	}
	if published.Type == "rk628_plugin_det_changed" {
		s.mu.Lock()
		s.plugin = &published
		s.mu.Unlock()
	}
}
func (s *Service) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := map[byte]string{Booting: "booting", Ready: "ready", ShuttingDown: "shutting_down", Sleeping: "sleeping"}
	r := State{RVState: names[s.state], RVServiceVersion: "1.0.0", ProtocolVersion: 1, Generation: s.events.Generation(), UARTConnected: s.link != nil, LEDSupport: s.ledSupport, Plugin: s.plugin, InvalidEvents: s.invalidEvents}
	if !s.lastRX.IsZero() {
		r.LastRXAt = s.lastRX.UTC().Format(time.RFC3339Nano)
	}
	return r
}
func errorResponse(err error) Response {
	var api *APIError
	if errors.As(err, &api) {
		return Response{Status: api.Status, Error: api.Message, MCUStatus: api.MCUStatus, UnknownOutcome: api.UnknownOutcome}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Response{Status: "TIMEOUT", Error: "MCU operation timed out"}
	}
	if errors.Is(err, context.Canceled) {
		return Response{Status: "NOT_READY", Error: "request canceled"}
	}
	return Response{Status: "SERVICE_UNAVAILABLE", Error: err.Error()}
}

func (s *Service) Handle(ctx context.Context, r Request) Response {
	if r.Op == "get_state" {
		state := s.State()
		return Response{Status: "OK", State: &state}
	}
	if r.Op == "events_since" {
		page, err := s.events.Page(ctx, r.Since, r.Generation, r.Limit, r.WaitMS)
		if err != nil {
			return errorResponse(err)
		}
		return Response{Status: "OK", EventPage: &page}
	}
	if r.Op == "suspend_prepare" || r.Op == "suspend_resume" {
		return s.suspendOperation(ctx, r.Op)
	}
	cmd, payload, mutating, err := encodeRequest(r)
	if err != nil {
		return errorResponse(err)
	}
	if !mutating {
		return s.execute(ctx, cmd, payload, false)
	}
	if len(r.RequestID) < 1 || len(r.RequestID) > 128 {
		return errorResponse(invalid("request_id must contain 1..128 characters"))
	}
	if r.Generation != s.events.Generation() {
		return errorResponse(&APIError{Status: "CONFLICT", Message: "service generation changed; query state before mutating"})
	}
	signature := r.Op + ":" + strconv.Itoa(int(cmd)) + ":" + fmt.Sprintf("%x", payload)
	now := time.Now()
	s.mu.Lock()
	for id, m := range s.mutations {
		select {
		case <-m.done:
			if now.Sub(m.created) >= 10*time.Minute {
				delete(s.mutations, id)
			}
		default:
		}
	}
	m, exists := s.mutations[r.RequestID]
	if exists && m.signature != signature {
		s.mu.Unlock()
		return errorResponse(&APIError{Status: "CONFLICT", Message: "request_id was already used with different arguments"})
	}
	if !exists {
		if len(s.mutations) >= 512 {
			s.mu.Unlock()
			return errorResponse(&APIError{Status: "BUSY", Message: "mutation cache is full"})
		}
		m = &mutation{signature: signature, created: now, done: make(chan struct{})}
		s.mutations[r.RequestID] = m
	}
	s.mu.Unlock()
	if !exists {
		go func() {
			opCtx, cancel := context.WithTimeout(s.ctx, time.Second)
			defer cancel()
			if r.Op == "poweroff" {
				m.result = s.softwarePoweroff()
			} else {
				m.result = s.execute(opCtx, cmd, payload, true)
			}
			close(m.done)
		}()
	}
	select {
	case <-m.done:
		return m.result
	case <-ctx.Done():
		return errorResponse(ctx.Err())
	}
}

func encodeRequest(r Request) (byte, []byte, bool, error) {
	switch r.Op {
	case "suspend_prepare":
		return SuspendPrepare, nil, true, nil
	case "suspend_resume":
		return SuspendResume, nil, true, nil
	case "poweroff":
		if r.Since != "" || r.Limit != 0 || r.WaitMS != 0 || r.DelayS != 0 ||
			r.LED != "" || r.Mode != "" || r.OnMS != 0 || r.OffMS != 0 || r.LeaseS != 0 {
			return 0, nil, false, invalid("poweroff accepts only generation and request_id")
		}
		// This is a local lifecycle operation, not a new MCU UART command.
		return 0, nil, true, nil
	case "get_battery":
		return GetBattery, nil, false, nil
	case "get_wakeup_timer":
		return GetTimer, []byte{0}, false, nil
	case "cancel_wakeup_timer":
		return CancelTimer, []byte{0}, true, nil
	case "set_wakeup_timer":
		if r.DelayS < 1 || r.DelayS > 604800 {
			return 0, nil, false, invalid("delay_s must be 1..604800")
		}
		p := []byte{0, 1, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(p[2:], uint32(r.DelayS))
		return SetTimer, p, true, nil
	case "get_led", "set_led":
		var id byte
		switch r.LED {
		case "red":
			id = 0
		case "white":
			id = 1
		default:
			return 0, nil, false, invalid("led must be red or white")
		}
		if r.Op == "get_led" {
			return GetLED, []byte{id}, false, nil
		}
		modes := map[string]byte{"auto": 0, "off": 1, "on": 2, "blink": 3}
		mode, ok := modes[r.Mode]
		if !ok {
			return 0, nil, false, invalid("invalid LED mode")
		}
		valid := r.OnMS == 0 && r.OffMS == 0 && r.LeaseS == 0
		if mode > 0 {
			valid = r.LeaseS >= 1 && r.LeaseS <= 3600
			if mode == 3 {
				valid = valid && r.OnMS >= 20 && r.OnMS <= 60000 && r.OffMS >= 20 && r.OffMS <= 60000
			} else {
				valid = valid && r.OnMS == 0 && r.OffMS == 0
			}
		}
		if !valid {
			return 0, nil, false, invalid("invalid LED timing or lease")
		}
		p := []byte{id, mode, 0, 0, 0, 0, 0, 0}
		binary.LittleEndian.PutUint16(p[2:], uint16(r.OnMS))
		binary.LittleEndian.PutUint16(p[4:], uint16(r.OffMS))
		binary.LittleEndian.PutUint16(p[6:], uint16(r.LeaseS))
		return SetLED, p, true, nil
	default:
		return 0, nil, false, invalid("unknown MCU operation")
	}
}

func (s *Service) suspendOperation(ctx context.Context, op string) Response {
	s.suspendMu.Lock()
	defer s.suspendMu.Unlock()
	s.mu.Lock()
	link, state, wakeVersion := s.link, s.state, s.wakeVersion
	s.mu.Unlock()
	if link == nil || (op == "suspend_prepare" && state != Ready) ||
		(op == "suspend_resume" && state != Ready && state != Sleeping) {
		return errorResponse(&APIError{Status: "NOT_READY", Message: "MCU link is not ready for suspend"})
	}
	cmd := SuspendPrepare
	if op == "suspend_resume" {
		cmd = SuspendResume
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	f, err := link.Request(requestCtx, cmd, nil)
	if err != nil {
		r := errorResponse(err)
		r.UnknownOutcome = true
		return r
	}
	if len(f.Payload) != 1 || f.Payload[0] > Timeout {
		return errorResponse(&APIError{Status: "PROTOCOL_ERROR", Message: "invalid suspend response", UnknownOutcome: true})
	}
	if f.Payload[0] != Success {
		status := f.Payload[0]
		names := []string{"OK", "BUSY", "INVALID_ARGUMENT", "NOT_READY", "DENIED", "INTERNAL_ERROR", "TIMEOUT"}
		return errorResponse(&APIError{Status: names[status], Message: "MCU rejected suspend command", MCUStatus: &status})
	}
	s.mu.Lock()
	if op == "suspend_prepare" {
		if s.state != Ready || s.wakeVersion != wakeVersion {
			s.mu.Unlock()
			// A wake crossed the grant. The caller must not enter Linux suspend.
			rollbackCtx, rollbackCancel := context.WithTimeout(s.ctx, time.Second)
			defer rollbackCancel()
			_, _ = link.Request(rollbackCtx, SuspendResume, nil)
			return errorResponse(&APIError{Status: "BUSY", Message: "wake or shutdown arrived during suspend preparation"})
		}
		s.state = Sleeping
	} else if s.state == Sleeping {
		s.state = Ready
	}
	s.mu.Unlock()
	stateResult := s.State()
	return Response{Status: "OK", State: &stateResult}
}

func (s *Service) softwarePoweroff() Response {
	s.mu.Lock()
	if s.state != Ready || s.link == nil || s.shutdown == nil {
		s.mu.Unlock()
		return errorResponse(&APIError{Status: "NOT_READY", Message: "software poweroff requires a ready MCU service and UART"})
	}
	// Serialize intent creation with lifecycle transitions. Failed persistence
	// must leave an independently requested software shutdown unstarted.
	if err := s.shutdown.Begin(); err != nil {
		s.mu.Unlock()
		return errorResponse(&APIError{Status: "INTERNAL_ERROR", Message: "cannot persist software shutdown intent: " + err.Error()})
	}
	s.state = ShuttingDown
	s.shutdownStatus = Success
	s.mu.Unlock()
	s.events.Close()
	log.Printf("MCU software shutdown: intent recorded; waiting for GET_STATE")
	// The MCU probes every five seconds while READY. Start Linux poweroff only
	// after writing its SHUTTING_DOWN response. If the link fails, still stop
	// Linux safely; power removal and subsequent wake are then unconfirmed.
	go func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-s.ctx.Done():
		case <-timer.C:
			s.shutdown.Commit()
		}
	}()
	state := s.State()
	return Response{Status: "OK", State: &state}
}

func (s *Service) execute(ctx context.Context, cmd byte, p []byte, mutating bool) Response {
	s.mu.Lock()
	link := s.link
	state := s.state
	s.mu.Unlock()
	if state != Ready {
		return errorResponse(&APIError{Status: "NOT_READY", Message: "MCU service is not ready"})
	}
	if link == nil {
		return errorResponse(&APIError{Status: "SERVICE_UNAVAILABLE", Message: "UART is disconnected"})
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if cmd == SetLED {
		// A read-only probe distinguishes an older MCU from invalid SET data.
		probe := s.execute(ctx, GetLED, p[:1], false)
		if probe.Status != "OK" {
			return probe
		}
	}
	f, err := link.Request(ctx, cmd, p)
	if err != nil {
		r := errorResponse(err)
		r.UnknownOutcome = mutating && r.Status != "BUSY"
		return r
	}
	data := f.Payload
	bad := func() Response {
		return errorResponse(&APIError{Status: "PROTOCOL_ERROR", Message: "invalid MCU response payload", UnknownOutcome: mutating})
	}
	if len(data) == 0 || (data[0] != Success && len(data) != 1) {
		return bad()
	}
	if data[0] != Success {
		status := data[0]
		names := []string{"OK", "BUSY", "INVALID_ARGUMENT", "NOT_READY", "DENIED", "INTERNAL_ERROR", "TIMEOUT"}
		name := "PROTOCOL_ERROR"
		if status <= Timeout {
			name = names[status]
		}
		if cmd == GetLED && status == Invalid {
			name = "UNSUPPORTED"
			s.mu.Lock()
			s.ledSupport = "unsupported"
			s.mu.Unlock()
		}
		return errorResponse(&APIError{Status: name, Message: "MCU rejected command", MCUStatus: &status})
	}
	result := Response{Status: "OK"}
	switch cmd {
	case GetBattery:
		if len(data) != 6 || (data[1] > 100 && data[1] != 255) || data[4] > 3 || data[5] != 0 {
			return bad()
		}
		mv := binary.LittleEndian.Uint16(data[2:4])
		b := &Battery{RawPercent: data[1], RawMillivolts: mv, ChargeState: []string{"not_charging", "charging", "full", "absent_or_unknown"}[data[4]], ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if data[1] != 255 {
			percent := data[1]
			b.Percent = &percent
		}
		if mv != 65535 {
			b.Millivolts = &mv
		}
		result.Battery = b
	case GetTimer, SetTimer, CancelTimer:
		if len(data) != 8 || data[1] != 0 || data[2] > 1 || data[3] != 0 {
			return bad()
		}
		remaining := binary.LittleEndian.Uint32(data[4:])
		if remaining > 604800 || (data[2] == 0 && remaining != 0) || (cmd == CancelTimer && (data[2] != 0 || remaining != 0)) {
			return bad()
		}
		result.Timer = &Timer{0, data[2] == 1, remaining}
	case GetLED, SetLED:
		if len(data) != 12 || data[1] != p[0] || data[2] > 3 || data[3] > 3 || data[10] > 1 || data[11] > 5 {
			return bad()
		}
		on, off, remaining := binary.LittleEndian.Uint16(data[4:]), binary.LittleEndian.Uint16(data[6:]), binary.LittleEndian.Uint16(data[8:])
		if (data[2] == 0 && (on != 0 || off != 0 || remaining != 0)) ||
			(data[2] != 0 && (remaining == 0 || remaining > 3600)) ||
			(data[2] != 3 && (on != 0 || off != 0)) ||
			(data[2] == 3 && (on < 20 || on > 60000 || off < 20 || off > 60000)) ||
			(data[11] == 0 && (data[2] == 0 || data[3] != data[2])) ||
			(data[11] != 0 && data[3] != 0) ||
			((data[11] == 1 || data[11] == 5) && data[2] != 0) ||
			((data[11] == 3 || data[11] == 4) && data[1] != 0) ||
			(data[3] == 1 && data[10] != 0) || (data[3] == 2 && data[10] != 1) {
			return bad()
		}
		modes := []string{"auto", "off", "on", "blink"}
		result.LED = &LEDState{[]string{"red", "white"}[data[1]], modes[data[2]], modes[data[3]], binary.LittleEndian.Uint16(data[4:]), binary.LittleEndian.Uint16(data[6:]), binary.LittleEndian.Uint16(data[8:]), data[10] == 1, []string{"application", "auto", "boot", "low_battery", "charging", "shutdown_or_off"}[data[11]]}
		s.mu.Lock()
		s.ledSupport = "supported"
		s.mu.Unlock()
	}
	return result
}

func DecodeRequest(data []byte) (Request, error) {
	var r Request
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, invalid(err.Error())
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return r, invalid("expected one JSON request")
	}
	return r, nil
}
