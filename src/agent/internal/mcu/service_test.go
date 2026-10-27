package mcu

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestProtocolVectorsAndRecovery(t *testing.T) {
	for _, v := range []struct {
		f   Frame
		hex string
	}{{Frame{EVENT, 42, RVReady, []byte{1, 0, 0, 0}}, "aa5501032a060400010000001c98"}, {Frame{EVENT, 42, Wakeup, []byte{6, 0, 0, 0}}, "aa5501032a070400060000005071"}, {Frame{REQ, 42, GetBattery, nil}, "aa5501012a02000064d0"}} {
		wire, err := Encode(v.f)
		if err != nil || hex.EncodeToString(wire) != v.hex {
			t.Fatalf("wire: %x, %v", wire, err)
		}
		parser := Parser{}
		now := time.Now()
		if len(parser.Feed(wire[:3], now)) != 0 {
			t.Fatal("partial frame")
		}
		frames := parser.Feed(wire[3:], now)
		if len(frames) != 1 || !bytes.Equal(frames[0].Payload, v.f.Payload) {
			t.Fatal(frames)
		}
	}
	wire, _ := Encode(Frame{EVENT, 7, Wakeup, []byte{0x80, 0, 0, 0}})
	bad := append([]byte(nil), wire...)
	bad[len(bad)-1] ^= 1
	p := Parser{}
	frames := p.Feed(append(bad, wire...), time.Now())
	if len(frames) != 1 || frames[0].Payload[0] != 0x80 {
		t.Fatal(frames)
	}
	p.Feed(wire[:8], time.Now())
	frames = p.Feed(wire, time.Now().Add(time.Second))
	if len(frames) != 1 {
		t.Fatal("partial frame did not expire")
	}
	if CRC16([]byte("123456789")) != 0x29b1 {
		t.Fatal("CRC")
	}
}

func TestEventsPagingWakeValidationAndCancellation(t *testing.T) {
	s := NewService(nil)
	defer s.Close()
	s.events = NewEventStore(2)
	s.HandleEvent(Frame{EVENT, 1, Wakeup, []byte{6, 1, 0, 0}})
	for _, source := range []byte{3, 6, 128} {
		s.HandleEvent(Frame{EVENT, 1, Wakeup, []byte{source, 0, 0, 0}})
	}
	p, err := s.events.Page(context.Background(), "0", "", 1, 0)
	if err != nil || !p.Truncated || p.NextID != "2" || p.LastID != "3" || p.Events[0].Source != "asr" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = s.events.Page(context.Background(), p.NextID, p.Generation, 1, 0)
	if err != nil || len(p.Events) != 1 || p.Events[0].Source != "unknown" || p.Events[0].SourceCode != 128 {
		t.Fatalf("%+v %v", p, err)
	}
	p, _ = s.events.Page(context.Background(), "3", "old-generation", 50, 25000)
	if !p.ResetRequired {
		t.Fatal("restart not detected")
	}
	results := make(chan EventPage, 2)
	for range 2 {
		go func() {
			page, _ := s.events.Page(context.Background(), "3", s.events.Generation(), 50, 1000)
			results <- page
		}()
	}
	time.Sleep(10 * time.Millisecond)
	s.HandleEvent(Frame{EVENT, 2, Wakeup, []byte{4, 0, 0, 0}})
	for range 2 {
		select {
		case page := <-results:
			if len(page.Events) != 1 || page.NextID != "4" {
				t.Fatal(page)
			}
		case <-time.After(time.Second):
			t.Fatal("lost wake")
		}
	}
	if s.State().InvalidEvents != 1 {
		t.Fatal("malformed wake accepted")
	}
}

type fakeRequester struct {
	mu      sync.Mutex
	calls   int
	reply   []byte
	failure error
}

func (f *fakeRequester) Request(ctx context.Context, cmd byte, p []byte) (Frame, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return Frame{RESP, 0, cmd, append([]byte(nil), f.reply...)}, f.failure
}
func runningService(t *testing.T, f Requester) *Service {
	t.Helper()
	s := NewService(nil)
	s.link = f
	s.state = Ready
	t.Cleanup(s.Close)
	return s
}
func TestMutationIdempotencyAndBatteryUnknown(t *testing.T) {
	f := &fakeRequester{reply: []byte{0, 0, 1, 0, 10, 0, 0, 0}}
	s := runningService(t, f)
	r := Request{Op: "set_wakeup_timer", DelayS: 10, RequestID: "set-1", Generation: s.events.Generation()}
	for range 2 {
		out := s.Handle(context.Background(), r)
		if out.Status != "OK" || out.Timer.RemainingS != 10 {
			t.Fatal(out)
		}
	}
	if f.calls != 1 {
		t.Fatal("retry restarted timer")
	}
	r.DelayS = 20
	if s.Handle(context.Background(), r).Status != "CONFLICT" {
		t.Fatal("ID conflict accepted")
	}
	r.Generation = "old"
	r.RequestID = "new"
	if s.Handle(context.Background(), r).Status != "CONFLICT" {
		t.Fatal("old generation accepted")
	}
	f.reply = []byte{0, 255, 255, 255, 3, 0}
	out := s.Handle(context.Background(), Request{Op: "get_battery"})
	if out.Status != "OK" || out.Battery.Percent != nil || out.Battery.Millivolts != nil {
		t.Fatal(out)
	}
	f.reply = []byte{NotReady}
	out = s.Handle(context.Background(), Request{Op: "get_battery"})
	if out.Status != "NOT_READY" || out.Battery != nil {
		t.Fatal(out)
	}
	f.reply = []byte{Invalid}
	out = s.Handle(context.Background(), Request{Op: "get_led", LED: "white"})
	if out.Status != "UNSUPPORTED" || s.State().LEDSupport != "unsupported" {
		t.Fatal(out)
	}
	f.failure = context.DeadlineExceeded
	r.Generation = s.events.Generation()
	r.RequestID = "timeout"
	out = s.Handle(context.Background(), r)
	if !out.UnknownOutcome || out.Status != "TIMEOUT" {
		t.Fatal(out)
	}
	calls := f.calls
	_ = s.Handle(context.Background(), r)
	if f.calls != calls {
		t.Fatal("ambiguous mutation retried")
	}
}

type fakeShutdown struct {
	mu              sync.Mutex
	begins, commits int
	pending         bool
	beginErr        error
	committed       chan struct{}
}

func (f *fakeShutdown) Pending() bool { return f.pending }
func (f *fakeShutdown) Begin() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins++
	return f.beginErr
}
func (f *fakeShutdown) Commit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits++
	if f.committed != nil {
		select {
		case f.committed <- struct{}{}:
		default:
		}
	}
}

func TestSoftwarePoweroffWaitsForStateResponse(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	shutdown := &fakeShutdown{committed: make(chan struct{}, 4)}
	s := NewService(shutdown)
	defer s.Close()
	link := NewLink(a)
	link.Guard = 0
	s.Attach(link)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.Run(ctx)
	parser := Parser{}
	next := func() Frame {
		t.Helper()
		_ = b.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 128)
		for {
			n, err := b.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			if frames := parser.Feed(buf[:n], time.Now()); len(frames) > 0 {
				return frames[0]
			}
		}
	}
	if f := next(); f.Command != RVReady {
		t.Fatal(f)
	}
	state := s.State()
	for range 2 {
		result := s.Handle(ctx, Request{Op: "poweroff", Generation: state.Generation, RequestID: "software-off"})
		if result.Status != "OK" || result.State.RVState != "shutting_down" {
			t.Fatalf("poweroff result: %+v", result)
		}
	}
	shutdown.mu.Lock()
	begins, commits := shutdown.begins, shutdown.commits
	shutdown.mu.Unlock()
	if begins != 1 || commits != 0 {
		t.Fatalf("premature or duplicate shutdown: begins=%d commits=%d", begins, commits)
	}
	// A cached pre-shutdown state response must not authorize Linux poweroff.
	link.OnResponseSent(Frame{RESP, 7, GetState, []byte{Success, 1, 1, 0, 0, Ready}})
	select {
	case <-shutdown.committed:
		t.Fatal("cached READY response committed poweroff")
	default:
	}
	if result := s.Handle(ctx, Request{Op: "get_battery"}); result.Status != "NOT_READY" {
		t.Fatal(result)
	}
	wire, _ := Encode(Frame{REQ, 8, GetState, nil})
	if _, err := b.Write(wire); err != nil {
		t.Fatal(err)
	}
	if f := next(); f.Command != GetState || !bytes.Equal(f.Payload, []byte{Success, 1, 1, 0, 0, ShuttingDown}) {
		t.Fatal(f)
	}
	select {
	case <-shutdown.committed:
	case <-time.After(time.Second):
		t.Fatal("SHUTTING_DOWN response did not commit poweroff")
	}
}

func TestSoftwarePoweroffRejectsInvalidOrUnpersistedIntent(t *testing.T) {
	shutdown := &fakeShutdown{beginErr: errors.New("intent unavailable")}
	s := NewService(shutdown)
	defer s.Close()
	s.link = &fakeRequester{}
	s.state = Ready
	valid := Request{Op: "poweroff", Generation: s.events.Generation(), RequestID: "off"}
	for _, r := range []Request{
		{Op: "poweroff", Generation: "stale", RequestID: "off"},
		{Op: "poweroff", Generation: s.events.Generation()},
		{Op: "poweroff", Generation: s.events.Generation(), RequestID: "off", DelayS: 10},
	} {
		if result := s.Handle(context.Background(), r); result.Status == "OK" {
			t.Fatal("invalid poweroff accepted", r)
		}
	}
	if shutdown.begins != 0 {
		t.Fatal("invalid request created shutdown intent")
	}
	for range 2 {
		if result := s.Handle(context.Background(), valid); result.Status != "INTERNAL_ERROR" {
			t.Fatal(result)
		}
	}
	if s.State().RVState != "ready" || shutdown.begins != 1 || shutdown.commits != 0 {
		t.Fatal("persistence failure changed lifecycle or retried the mutation")
	}
	s.HandleEvent(Frame{EVENT, 1, Wakeup, []byte{6, 0, 0, 0}})
	page, err := s.events.Page(context.Background(), "0", "", 10, 0)
	if err != nil || len(page.Events) != 1 {
		t.Fatal("failed poweroff closed event delivery", err)
	}
	s.link = nil
	valid.RequestID = "disconnected"
	if result := s.Handle(context.Background(), valid); result.Status != "NOT_READY" {
		t.Fatal(result)
	}
}

func TestLinkRetriesAndShutdownIsolation(t *testing.T) {
	a, b := net.Pipe()
	link := NewLink(a)
	link.Guard = 0
	shutdown := &fakeShutdown{}
	s := NewService(shutdown)
	defer s.Close()
	s.Attach(link)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer b.Close()
	go link.Run(ctx)
	frames := make(chan Frame, 64)
	go func() {
		parser := Parser{}
		buf := make([]byte, 100)
		for {
			n, e := b.Read(buf)
			if e != nil {
				return
			}
			for _, f := range parser.Feed(buf[:n], time.Now()) {
				frames <- f
			}
		}
	}()
	next := func() Frame {
		t.Helper()
		select {
		case f := <-frames:
			return f
		case <-time.After(time.Second):
			t.Fatal("missing frame")
			return Frame{}
		}
	}
	if f := next(); f.Command != RVReady {
		t.Fatal(f)
	}
	result := make(chan error, 1)
	go func() { _, err := link.Request(ctx, GetBattery, nil); result <- err }()
	first := next()
	second := next()
	if first.Seq != second.Seq || first.Command != second.Command {
		t.Fatal("retry changed correlation")
	}
	wrong, _ := Encode(Frame{RESP, first.Seq + 1, first.Command, []byte{0, 90, 0, 16, 0, 0}})
	_, _ = b.Write(wrong)
	good, _ := Encode(Frame{RESP, first.Seq, first.Command, []byte{0, 90, 0, 16, 0, 0}})
	_, _ = b.Write(good)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	bad, _ := Encode(Frame{REQ, 8, ShutdownRequest, []byte{2, 0, 0, 0}})
	_, _ = b.Write(bad)
	if f := next(); !bytes.Equal(f.Payload, []byte{Invalid}) {
		t.Fatal(f)
	}
	shutdown.mu.Lock()
	commits := shutdown.commits
	shutdown.mu.Unlock()
	if commits != 0 {
		t.Fatal("invalid request scheduled shutdown")
	}
	request, _ := Encode(Frame{REQ, 9, ShutdownRequest, []byte{1, 0, 0, 0}})
	for range 2 {
		_, _ = b.Write(request)
		if f := next(); !bytes.Equal(f.Payload, []byte{Success}) {
			t.Fatal(f)
		}
	}
	shutdown.mu.Lock()
	begins := shutdown.begins
	shutdown.mu.Unlock()
	if begins != 1 || s.State().RVState != "shutting_down" {
		t.Fatal("shutdown repeated")
	}
	if r := s.Handle(context.Background(), Request{Op: "get_battery"}); r.Status != "NOT_READY" {
		t.Fatal(r)
	}
}

func TestMCUShutdownPersistenceFailureStillStopsLinux(t *testing.T) {
	shutdown := &fakeShutdown{beginErr: errors.New("intent unavailable"), committed: make(chan struct{}, 4)}
	s := NewService(shutdown)
	defer s.Close()
	for range 2 {
		response := s.HandleFrame(Frame{REQ, 9, ShutdownRequest, []byte{1, 0, 0, 0}})
		if !bytes.Equal(response.Payload, []byte{InternalError}) {
			t.Fatal("unpersisted shutdown acknowledged as safe", response)
		}
	}
	if s.State().RVState != "shutting_down" {
		t.Fatal("MCU's pending cutoff left application requests enabled")
	}
	select {
	case <-shutdown.committed:
	case <-time.After(time.Second):
		t.Fatal("intent failure prevented cleanup before MCU's forced cutoff")
	}
	shutdown.mu.Lock()
	defer shutdown.mu.Unlock()
	if shutdown.begins != 1 || shutdown.commits != 1 {
		t.Fatalf("duplicate request repeated shutdown: begins=%d commits=%d", shutdown.begins, shutdown.commits)
	}
}

func TestUDSLongPollDoesNotBlockQueriesOrClose(t *testing.T) {
	s := runningService(t, &fakeRequester{reply: []byte{0, 255, 255, 255, 3, 0}})
	path := filepath.Join(t.TempDir(), "mcu.sock")
	server, err := Listen(path, s)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := Client{Socket: path}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := client.EventsSince(ctx, "0", "", 50, 25000); result <- err }()
	time.Sleep(20 * time.Millisecond)
	if _, err := client.GetBattery(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancel ignored")
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not cancel")
	}
	closed := make(chan struct{})
	go func() { server.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close stuck on waiter")
	}
}
func TestStorageGate(t *testing.T) {
	mounts := []byte("1 0 1:1 / / ro,relatime - ext4 /dev/root ro\n2 1 0:1 / /run rw - tmpfs tmpfs rw\n")
	swaps := []byte("Filename Type Size Used Priority\n")
	if err := CheckStorageSafe(mounts, swaps); err != nil {
		t.Fatal(err)
	}
	if err := CheckStorageSafe(bytes.Replace(mounts, []byte("/ ro,"), []byte("/ rw,"), 1), swaps); err == nil {
		t.Fatal("writable root accepted")
	}
	if err := CheckStorageSafe(mounts, append(swaps, []byte("/userdata/swapfile file 100 0 -1\n")...)); err == nil {
		t.Fatal("active swap accepted")
	}
	if err := ShutdownHook("reboot", "/does-not-exist", "/does-not-exist"); err != nil {
		t.Fatal(err)
	}
}
func TestRequestValidation(t *testing.T) {
	for _, raw := range []string{`{"op":"get_state","raw_uart":"bad"}`, `{"op":"get_state"} {}`} {
		if _, err := DecodeRequest([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	_, _, _, err := encodeRequest(Request{Op: "set_led", LED: "red", Mode: "blink", OnMS: 1, OffMS: 100, LeaseS: 10})
	if err == nil {
		t.Fatal("short blink accepted")
	}
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatal(err)
	}
}

func TestMalformedResponsesAndPluginSnapshot(t *testing.T) {
	for _, payload := range [][]byte{
		{Invalid, 0},
		{0, 1, 0, 0, 0, 0, 0, 0, 1, 0, 0, 1},  // auto with a lease
		{0, 1, 3, 3, 1, 0, 20, 0, 1, 0, 1, 0}, // blink below 20 ms
		{0, 1, 2, 2, 0, 0, 0, 0, 1, 0, 0, 0},  // on with low output
		{0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1, 4},  // white cannot be charging indication
	} {
		s := runningService(t, &fakeRequester{reply: payload})
		if got := s.Handle(context.Background(), Request{Op: "get_led", LED: "white"}); got.Status != "PROTOCOL_ERROR" {
			t.Fatal(got)
		}
	}
	s := runningService(t, &fakeRequester{reply: []byte{0, 1, 3, 3, 200, 0, 32, 3, 2, 0, 1, 0}})
	if got := s.Handle(context.Background(), Request{Op: "get_led", LED: "white"}); got.Status != "OK" || got.LED.RemainingS != 2 {
		t.Fatal(got)
	}
	s.HandleEvent(Frame{EVENT, 12, PluginChanged, []byte{1, 0, 0, 0}})
	page, err := s.events.Page(context.Background(), "0", "", 1, 0)
	if err != nil || len(page.Events) != 1 {
		t.Fatal(err, page)
	}
	snapshot := s.State().Plugin
	if snapshot == nil || snapshot.ID != page.Events[0].ID || snapshot.ReceivedAt != page.Events[0].ReceivedAt {
		t.Fatalf("snapshot does not match event: %+v", snapshot)
	}
}
