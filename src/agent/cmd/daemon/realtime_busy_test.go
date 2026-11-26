package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/agent/agentpath"
	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/realtimevoice"
	streamcontract "aico-agent/internal/stream"
)

// Exercise the real daemon select loop, including its audio traffic and chat
// admission. The base provider deliberately has no ResponseInterrupter.
type busyTestSession struct {
	events                   chan realtimevoice.Event
	created                  chan struct{}
	closed                   chan struct{}
	toolResults              chan struct{}
	interrupted              chan struct{}
	blockCreate              bool
	explicitToolContinuation bool
}

func (s *busyTestSession) Info() realtimevoice.SessionInfo {
	return realtimevoice.SessionInfo{InputSampleRate: 16000, OutputSampleRate: 16000,
		Capabilities: realtimevoice.Capabilities{EmitsSpeechEvents: true, ExplicitToolContinuation: s.explicitToolContinuation}}
}
func (s *busyTestSession) Events() <-chan realtimevoice.Event      { return s.events }
func (s *busyTestSession) Errors() <-chan error                    { return nil }
func (s *busyTestSession) Done() <-chan struct{}                   { return s.closed }
func (s *busyTestSession) SendAudio(context.Context, []byte) error { return nil }
func (s *busyTestSession) SendText(context.Context, string) error  { return nil }
func (s *busyTestSession) SendToolResult(context.Context, string, string) error {
	s.toolResults <- struct{}{}
	return nil
}
func (s *busyTestSession) CreateResponse(ctx context.Context) error {
	s.created <- struct{}{}
	if s.blockCreate {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (s *busyTestSession) Close() error { close(s.closed); return nil }

type busyInterruptSession struct {
	*busyTestSession
	interruptErr error
	ackTimeout   time.Duration
}

func (s *busyInterruptSession) Interrupt(context.Context, realtimevoice.ResponseInterruption) error {
	s.interrupted <- struct{}{}
	return s.interruptErr
}

func (s *busyInterruptSession) InterruptionAckTimeout() time.Duration { return s.ackTimeout }

type busyTestProvider struct{ session realtimevoice.Session }

func (p busyTestProvider) Open(context.Context, realtimevoice.SessionConfig) (realtimevoice.Session, error) {
	return p.session, nil
}

func busyTestAudio(t *testing.T) string {
	t.Helper()
	// macOS Unix socket paths cannot accommodate the usual long t.TempDir path.
	dir, err := os.MkdirTemp("/tmp", "busy-audio-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "audio.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				for {
					var prefix [12]byte
					if _, err := io.ReadFull(conn, prefix[:]); err != nil {
						return
					}
					header := make([]byte, binary.LittleEndian.Uint32(prefix[:4]))
					if _, err := io.ReadFull(conn, header); err != nil {
						return
					}
					if _, err := io.CopyN(io.Discard, conn, int64(binary.LittleEndian.Uint64(prefix[4:]))); err != nil {
						return
					}
					var req struct {
						Op string `json:"op"`
					}
					if json.Unmarshal(header, &req) != nil {
						return
					}
					response := []byte(`{"status":"OK","session_id":"1"}`)
					var pcm []byte
					if req.Op == "read_record_chunk" {
						time.Sleep(5 * time.Millisecond)
						pcm = make([]byte, 320) // Continuous mic traffic must not reset the watchdog.
					}
					binary.LittleEndian.PutUint32(prefix[:4], uint32(len(response)))
					binary.LittleEndian.PutUint64(prefix[4:], uint64(len(pcm)))
					if _, err := conn.Write(append(append(prefix[:], response...), pcm...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return path
}

func startBusyTestSession(t *testing.T, timeout time.Duration, bridges ...*realtimeChatBridge) (*busyTestSession, *realtimeChatBridge, <-chan error) {
	return startBusyTestSessionWithBlockedCreate(t, timeout, false, bridges...)
}

func startBusyTestSessionWithBlockedCreate(t *testing.T, timeout time.Duration, blockCreate bool, bridges ...*realtimeChatBridge) (*busyTestSession, *realtimeChatBridge, <-chan error) {
	return startBusyTestSessionConfigured(t, timeout, blockCreate, nil, 0, false, bridges...)
}

// busyTestSemanticSink, when set before a session starts, receives that
// session's semantic events.
var busyTestSemanticSink streamcontract.EventSink

// busyTestConfigDir is the config directory of the last started session.
var busyTestConfigDir string

func startBusyTestSessionConfigured(t *testing.T, timeout time.Duration, blockCreate bool, interruptErr *error, ackTimeout time.Duration, explicitTools bool, bridges ...*realtimeChatBridge) (*busyTestSession, *realtimeChatBridge, <-chan error) {
	t.Helper()
	s := &busyTestSession{events: make(chan realtimevoice.Event, 32), created: make(chan struct{}, 32), closed: make(chan struct{}), toolResults: make(chan struct{}, 32)}
	s.blockCreate = blockCreate
	s.explicitToolContinuation = explicitTools
	s.interrupted = make(chan struct{}, 8)
	var raw realtimevoice.Session = s
	if interruptErr != nil {
		raw = &busyInterruptSession{busyTestSession: s, interruptErr: *interruptErr, ackTimeout: ackTimeout}
	}
	registry := realtimevoice.NewProviderRegistry()
	registry.Register("gemini", func(realtimevoice.ProviderConfig) realtimevoice.Provider { return busyTestProvider{raw} })
	busyTestConfigDir = t.TempDir()
	cfg := agent.Config{ConfigDir: busyTestConfigDir, Locale: "zh-CN", VoiceModel: agent.VoiceModelConfig{Provider: "gemini"},
		Audio: agent.AudioConfig{Backend: "audio_service", Socket: busyTestAudio(t), SampleRate: 16000, Channels: 1, BitWidth: 16}}
	bridge := newRealtimeChatBridge()
	if len(bridges) > 0 {
		bridge = bridges[0]
	}
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	sink := busyTestSemanticSink
	go func() {
		done <- runRealtimeSessionWithRegistryAndSink(cfg, signals, nil, nil, registry, sink, timeout, nil, bridge)
	}()
	t.Cleanup(func() {
		signals <- os.Interrupt
		select {
		case <-s.closed:
		case <-time.After(2 * time.Second):
			t.Error("session did not close")
		}
	})
	return s, bridge, done
}

func TestRealtimeBusyCancelClosesUninterruptibleSession(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, 60*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := bridge.Handle(ctx, agent.RealtimeChatRequest{RequestID: "cancel-me", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("session startup: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("response not requested")
	}
	cancel()
	select {
	case <-s.closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled Gemini-like request still owns the session; following requests can remain busy")
	}
	drainBusyEvents(t, events)
	bridge.mu.RLock()
	active := bridge.active
	bridge.mu.RUnlock()
	if active {
		t.Fatal("closed session still admits commands")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("session exit = %v, want cancellation", err)
	}
	// The next request uses a fresh provider session through the SAME bridge.
	next, _, nextDone := startBusyTestSession(t, time.Second, bridge)
	nextEvents := busyTestRequest(t, next, bridge, nextDone, "after-cancel")
	next.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, Status: "completed"}
	requireBusyEvent(t, nextEvents, agent.RealtimeChatEventDone)
}

func TestRealtimeBusyNoOutputTimesOutDespiteMicTraffic(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, 100*time.Millisecond)
	events, err := bridge.Handle(context.Background(), agent.RealtimeChatRequest{RequestID: "stalled", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("session startup: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("response not requested")
	}
	requireBusyTimeout(t, events)
	select {
	case <-s.closed:
	case <-time.After(time.Second):
		t.Fatal("timed-out session not closed")
	}
}

func TestRealtimeBusyCancelDuringCreateClosesSession(t *testing.T) {
	s, bridge, done := startBusyTestSessionWithBlockedCreate(t, time.Second, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := bridge.Handle(ctx, agent.RealtimeChatRequest{RequestID: "cancel-create", Message: "hello"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("startup: %v", err)
	case <-time.After(time.Second):
		t.Fatal("create not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("exit=%v, want cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("partially sent response retained its session after cancellation")
	}
}

// Shared assertion used by the timeout tests below.
func requireBusyTimeout(t *testing.T, events <-chan agent.RealtimeChatEvent) {
	t.Helper()
	select {
	case event := <-events:
		if event.Type != agent.RealtimeChatEventError || !strings.Contains(event.Error, "no progress") {
			t.Fatalf("event = %+v, want explicit no-progress timeout", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled response never timed out")
	}
}

func busyTestRequest(t *testing.T, s *busyTestSession, bridge *realtimeChatBridge, done <-chan error, id string) <-chan agent.RealtimeChatEvent {
	t.Helper()
	events, err := bridge.Handle(context.Background(), agent.RealtimeChatRequest{RequestID: id, Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("session exited before requesting response: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("response not requested")
	}
	return events
}

func requireBusyEvent(t *testing.T, events <-chan agent.RealtimeChatEvent, kind string) {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok || event.Type != kind {
			t.Fatalf("event=%+v open=%t, want %s", event, ok, kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("missing %s", kind)
	}
}

func TestRealtimeBusyStreamingRenewsDeadlineAndCompletionDisarmsIt(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, 250*time.Millisecond)
	events := busyTestRequest(t, s, bridge, done, "streaming")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "r1"}
	// Total response time exceeds the timeout, but each real delta renews it.
	for range 5 {
		time.Sleep(80 * time.Millisecond)
		s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, Role: "assistant", TextSource: "text", Text: "word", ResponseID: "r1"}
		requireBusyEvent(t, events, agent.RealtimeChatEventDelta)
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "r1", Status: "completed"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)
	select {
	case err := <-done:
		t.Fatalf("idle conversation closed after completion: %v", err)
	case <-time.After(350 * time.Millisecond):
	}
	next := busyTestRequest(t, s, bridge, done, "next-turn")
	// Late output from the retired response must not complete the new request.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "r1", Status: "completed"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "r2"}
	requireBusyTimeout(t, next)
}

func TestRealtimeBusyNoiseDoesNotKeepStalledResponseAlive(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, 150*time.Millisecond)
	events := busyTestRequest(t, s, bridge, done, "noise")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "current"}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-s.closed:
				return
			case <-ticker.C:
				for _, event := range []realtimevoice.Event{
					{Kind: realtimevoice.EventReady},
					{Kind: realtimevoice.EventUsage},
					{Kind: realtimevoice.EventTranscriptDelta, Role: "user", Text: "mic"},
					{Kind: realtimevoice.EventAudio, ResponseID: "stale", PCM: []byte{0, 0}},
					{Kind: realtimevoice.EventTranscriptDelta, Role: "assistant", ResponseID: "current", Text: ""},
				} {
					select {
					case s.events <- event:
					case <-stop:
						return
					case <-s.closed:
						return
					}
				}
			}
		}
	}()
	requireBusyTimeout(t, events)
}

func TestRealtimeBusyAudioAndToolProgressRenewDeadline(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, 300*time.Millisecond)
	events := busyTestRequest(t, s, bridge, done, "audio-tool")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "r"}
	time.Sleep(180 * time.Millisecond)
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventAudio, ResponseID: "r", PCM: make([]byte, 320)}
	time.Sleep(180 * time.Millisecond)
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "r", CallID: "tool", Name: "unknown_tool", Arguments: "{}"}
	select {
	case <-s.toolResults:
	case err := <-done:
		t.Fatalf("session exited before tool result: %v", err)
	case <-time.After(time.Second):
		t.Fatal("tool result not sent")
	}
	time.Sleep(180 * time.Millisecond)
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "r", Status: "completed"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)
}

func TestRealtimeBusySpeechStoppedReleasesAdmission(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, 100*time.Millisecond)
	events := busyTestRequest(t, s, bridge, done, "setup")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, Status: "completed"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStarted}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStopped}
	// The tool result acknowledges processing of all preceding provider events.
	// Queue the next chat only after the stop has reached the daemon loop.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, CallID: "stop-barrier", Name: "unknown_tool", Arguments: "{}"}
	select {
	case <-s.toolResults:
	case <-time.After(time.Second):
		t.Fatal("speech stop processing was not acknowledged")
	}
	next := busyTestRequest(t, s, bridge, done, "after-speech-stopped")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, Status: "completed"}
	requireBusyEvent(t, next, agent.RealtimeChatEventDone)
}

func TestRealtimeBusyQueuedChatReleasesWhenSpeechStopsAfterResponseTerminal(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, time.Second)

	// Put an otherwise idle provider response into the farewell state so the
	// next WebUI request is parked while the response is still active.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "farewell"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "farewell", CallID: "farewell", Name: "end_conversation", Arguments: "{}"}
	select {
	case <-s.toolResults:
	case err := <-done:
		t.Fatalf("session exited before farewell tool result: %v", err)
	case <-time.After(time.Second):
		t.Fatal("farewell tool result was not sent")
	}
	// The second tool result is a barrier after sleep.request() has run.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "farewell", CallID: "barrier", Name: "unknown_tool", Arguments: "{}"}
	select {
	case <-s.toolResults:
	case err := <-done:
		t.Fatalf("session exited before farewell barrier: %v", err)
	case <-time.After(time.Second):
		t.Fatal("farewell barrier was not processed")
	}

	events, err := bridge.Handle(context.Background(), agent.RealtimeChatRequest{RequestID: "queued-after-speech", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for realtimeChatPending(bridge, nil) {
		if time.Now().After(deadline) {
			t.Fatal("queued chat command was not admitted to the realtime loop")
		}
		time.Sleep(time.Millisecond)
	}

	// Providers can acknowledge the response before they report that the
	// interrupting speech stopped. The queued request must be retried from the
	// latter event instead of remaining busy forever.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStarted}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "farewell", Status: "completed"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStopped}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("session exited before queued response: %v", err)
	case <-time.After(time.Second):
		t.Fatal("queued chat was not started after speech stopped")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, Status: "completed"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)
}

func TestRealtimeBusyGeminiInterruptionTerminalReleasesChat(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, time.Second)
	events := busyTestRequest(t, s, bridge, done, "interrupted")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventInterruption, At: "assistant"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseCancelled, Status: "cancelled"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)
	next := busyTestRequest(t, s, bridge, done, "after-interruption")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, Status: "completed"}
	requireBusyEvent(t, next, agent.RealtimeChatEventDone)
}

func TestRealtimeTurnStateInterruptedAnonymousTerminalPreservesNewInput(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("")
	state.responseInterrupted()
	if state.canInjectResponse() {
		t.Fatal("interrupted response admitted another request before terminal acknowledgement")
	}
	state.userTranscriptObserved()
	if !state.responseFinished("") {
		t.Fatal("Gemini turn_complete was rejected after interrupted")
	}
	if !state.inputTurnPending {
		t.Fatal("old terminal consumed the interrupting user's new input")
	}
	if !state.responseStarted("") || !state.responseFinished("") || !state.canInjectResponse() {
		t.Fatal("new turn did not finish cleanly")
	}
	if state.responseFinished("") {
		t.Fatal("duplicate terminal was accepted")
	}
}

func TestRealtimeBusySupportedCancelWaitsForAckAndKeepsSession(t *testing.T) {
	var interruptErr error
	s, bridge, done := startBusyTestSessionConfigured(t, time.Second, false, &interruptErr, 0, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := bridge.Handle(ctx, agent.RealtimeChatRequest{RequestID: "cancel", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "old"}
	cancel()
	select {
	case <-s.interrupted:
	case <-time.After(time.Second):
		t.Fatal("provider interrupt not called")
	}
	drainBusyEvents(t, events)
	probe, err := bridge.Handle(t.Context(), agent.RealtimeChatRequest{RequestID: "before-ack", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	requireBusyEvent(t, probe, agent.RealtimeChatEventError)
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventInterruption, ResponseID: "old"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseCancelled, ResponseID: "old", Status: "cancelled"}
	// Observe processing via a subsequent queued event, not a timing sleep.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, Role: "assistant", ResponseID: "old", Text: "late"}
	deadline := time.After(time.Second)
	for {
		reply, err := bridge.Handle(t.Context(), agent.RealtimeChatRequest{RequestID: "after-ack", Message: "hello"})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.created:
			s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, Status: "completed"}
			requireBusyEvent(t, reply, agent.RealtimeChatEventDone)
			select {
			case <-s.closed:
				t.Fatal("supported cancellation closed session")
			default:
			}
			return
		case e := <-reply:
			if e.Type != agent.RealtimeChatEventError || e.Error != "realtime response is busy" {
				t.Fatalf("unexpected event=%+v", e)
			}
		case <-deadline:
			t.Fatal("terminal acknowledgement did not release admission")
		}
	}
}

func TestRealtimeBusyCancelFailureClosesSession(t *testing.T) {
	failure := errors.New("interrupt write failed")
	s, bridge, done := startBusyTestSessionConfigured(t, time.Second, false, &failure, 0, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.Handle(ctx, agent.RealtimeChatRequest{RequestID: "cancel-failure", Message: "hello"})
	select {
	case <-s.created:
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("exit=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed cancel kept session")
	}
}

func TestRealtimeBusyCancelMissingAckUsesProviderTimeout(t *testing.T) {
	var interruptErr error
	s, bridge, done := startBusyTestSessionConfigured(t, time.Second, false, &interruptErr, 100*time.Millisecond, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.Handle(ctx, agent.RealtimeChatRequest{RequestID: "no-ack", Message: "hello"})
	select {
	case <-s.created:
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
	cancel()
	select {
	case <-s.interrupted:
	case <-time.After(time.Second):
		t.Fatal("no cancel")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "acknowledgement timed out") {
			t.Fatalf("exit=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing acknowledgement never timed out")
	}
}

func TestRealtimeBusyCancelMissingAckUsesWatchdogWithoutProviderTimeout(t *testing.T) {
	var interruptErr error
	s, bridge, done := startBusyTestSessionConfigured(t, 100*time.Millisecond, false, &interruptErr, 0, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := bridge.Handle(ctx, agent.RealtimeChatRequest{RequestID: "no-provider-timeout", Message: "hello"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.created:
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
	cancel()
	select {
	case <-s.interrupted:
	case <-time.After(time.Second):
		t.Fatal("no cancel")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no progress") {
			t.Fatalf("exit=%v, want watchdog timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal-pending cancellation disarmed watchdog")
	}
}

func drainBusyEvents(t *testing.T, events <-chan agent.RealtimeChatEvent) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-timer.C:
			t.Fatal("chat events did not close")
		}
	}
}

func TestRealtimeBusyInterruptedBeforeStartStillTimesOut(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, 100*time.Millisecond)
	events := busyTestRequest(t, s, bridge, done, "pre-start-interrupt")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventInterruption}
	requireBusyTimeout(t, events)
}

func TestRealtimeRequestedInterruptionRemainsBusyWithoutChat(t *testing.T) {
	state := realtimeTurnState{}
	state.responseRequested()
	state.responseInterrupted()
	if !state.responseTerminalPending || state.canInjectResponse() {
		t.Fatal("pre-start interrupted request lost terminal ownership")
	}
	if !state.responseFinished("") || !state.canInjectResponse() {
		t.Fatal("terminal did not release interrupted request")
	}
}

func TestRealtimeBusyInterruptionClearsExplicitToolContinuation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	oldStarter := realtimeToolCallStarter
	realtimeToolCallStarter = func(ctx context.Context, _ realtimeVoiceToolExecutor, _ realtimevoice.Event, _ chan<- realtimeToolResult, done func()) {
		defer done()
		close(started)
		go func() {
			<-ctx.Done()
			close(canceled)
		}()
	}
	defer func() { realtimeToolCallStarter = oldStarter }()

	s, bridge, done := startBusyTestSessionConfigured(t, time.Second, false, nil, 0, true)
	events := busyTestRequest(t, s, bridge, done, "interrupted-tool")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "r"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "r", CallID: "tool", Name: "unknown_tool", Arguments: "{}"}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	// Providers may omit the response ID on interruption; that still cancels
	// every foreground tool owned by the one active response.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventInterruption}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("interruption did not cancel the foreground tool")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "r", Status: "completed"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)
}

func TestRealtimeBusyToolCancellationPreservesParallelCall(t *testing.T) {
	type heldTool struct {
		ctx     context.Context
		call    realtimevoice.Event
		results chan<- realtimeToolResult
	}
	started := make(chan heldTool, 2)
	oldStarter := realtimeToolCallStarter
	realtimeToolCallStarter = func(ctx context.Context, _ realtimeVoiceToolExecutor, call realtimevoice.Event, results chan<- realtimeToolResult, done func()) {
		defer done()
		started <- heldTool{ctx, call, results}
	}
	defer func() { realtimeToolCallStarter = oldStarter }()
	s, bridge, done := startBusyTestSession(t, time.Second)
	events := busyTestRequest(t, s, bridge, done, "partial-tool-cancel")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted}
	for _, id := range []string{"a", "b"} {
		s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, CallID: id, Name: "unknown_tool", Arguments: "{}"}
	}
	tools := make(map[string]heldTool)
	for range 2 {
		select {
		case tool := <-started:
			tools[tool.call.CallID] = tool
		case <-time.After(time.Second):
			t.Fatal("parallel tools did not start")
		}
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCallCancelled, CallID: "a"}
	select {
	case <-tools["a"].ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("selected tool was not canceled")
	}
	if err := tools["b"].ctx.Err(); err != nil {
		t.Fatalf("unselected tool was canceled: %v", err)
	}
	// Queue a racing result from the canceled tool before the valid result.
	for _, id := range []string{"a", "b"} {
		tool := tools[id]
		tool.results <- realtimeToolResult{call: tool.call, output: "{}"}
	}
	select {
	case <-s.toolResults:
	case <-time.After(time.Second):
		t.Fatal("remaining tool result was not sent")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, Role: "assistant", TextSource: "text", Text: "finished"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDelta)
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, Status: "completed"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)
	select {
	case <-s.toolResults:
		t.Fatal("canceled tool result was sent")
	default:
	}
}

func TestRealtimeBusyToolCancellationAfterResponseDone(t *testing.T) {
	started := make(chan context.Context, 1)
	oldStarter := realtimeToolCallStarter
	realtimeToolCallStarter = func(ctx context.Context, _ realtimeVoiceToolExecutor, _ realtimevoice.Event, _ chan<- realtimeToolResult, done func()) {
		defer done()
		started <- ctx
	}
	defer func() { realtimeToolCallStarter = oldStarter }()

	s, bridge, done := startBusyTestSessionConfigured(t, time.Second, false, nil, 0, true)
	events := busyTestRequest(t, s, bridge, done, "terminal-before-tool-cancel")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "r1"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "r1", CallID: "tool", Name: "unknown_tool", Arguments: "{}"}
	var toolCtx context.Context
	select {
	case toolCtx = <-started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "r1", Status: "completed"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCallCancelled, ResponseID: "r1", CallID: "tool"}
	select {
	case <-toolCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("tool was not canceled after its response ended")
	}
	select {
	case <-s.created:
	case <-time.After(time.Second):
		t.Fatal("tool cancellation did not continue the completed response")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "r2"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "r2", Status: "completed"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)
}

func TestRealtimeBusyLateToolCancellationDoesNotCancelReusedCallID(t *testing.T) {
	type startedTool struct {
		responseID string
		ctx        context.Context
	}
	started := make(chan startedTool, 2)
	oldStarter := realtimeToolCallStarter
	realtimeToolCallStarter = func(ctx context.Context, _ realtimeVoiceToolExecutor, call realtimevoice.Event, _ chan<- realtimeToolResult, done func()) {
		defer done()
		started <- startedTool{responseID: call.ResponseID, ctx: ctx}
	}
	defer func() { realtimeToolCallStarter = oldStarter }()

	s, bridge, done := startBusyTestSession(t, time.Second)
	first := busyTestRequest(t, s, bridge, done, "first-reused-call")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "r1"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "r1", CallID: "tool", Name: "unknown_tool", Arguments: "{}"}
	select {
	case tool := <-started:
		if tool.responseID != "r1" {
			t.Fatalf("first tool response = %q, want r1", tool.responseID)
		}
	case <-time.After(time.Second):
		t.Fatal("first tool did not start")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "r1", Status: "completed"}
	requireBusyEvent(t, first, agent.RealtimeChatEventDone)

	second := busyTestRequest(t, s, bridge, done, "second-reused-call")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "r2"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "r2", CallID: "tool", Name: "unknown_tool", Arguments: "{}"}
	var secondTool context.Context
	select {
	case tool := <-started:
		if tool.responseID != "r2" {
			t.Fatalf("second tool response = %q, want r2", tool.responseID)
		}
		secondTool = tool.ctx
	case <-time.After(time.Second):
		t.Fatal("second tool did not start")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCallCancelled, ResponseID: "r1", CallID: "tool"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, Role: "assistant", TextSource: "text", ResponseID: "r2", Text: "active"}
	requireBusyEvent(t, second, agent.RealtimeChatEventDelta)
	if err := secondTool.Err(); err != nil {
		t.Fatalf("late r1 cancellation stopped r2 tool: %v", err)
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "r2", Status: "completed"}
	requireBusyEvent(t, second, agent.RealtimeChatEventDone)
}

// A Web UI chat reply is shown from the request's own stream. Its semantic
// events must carry the request ID so observers recognise the reply as that
// request's instead of rendering it a second time under the response ID.
func TestRealtimeChatResponseEventsCarryRequestID(t *testing.T) {
	sink := &lockedSemanticSink{}
	busyTestSemanticSink = sink
	t.Cleanup(func() { busyTestSemanticSink = nil })
	s, bridge, done := startBusyTestSession(t, time.Second)

	events, err := bridge.Handle(context.Background(), agent.RealtimeChatRequest{RequestID: "web-chat-1", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("session exited before the chat response: %v", err)
	case <-time.After(time.Second):
		t.Fatal("chat response was not requested")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "resp-1"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, ResponseID: "resp-1", Role: "assistant", Text: "pong"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDelta)
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "resp-1", Status: "completed"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)

	var replies int
	for _, event := range sink.snapshot() {
		if event.ResponseID != "resp-1" {
			continue
		}
		replies++
		if event.TurnID != "web-chat-1" {
			t.Fatalf("%s turn_id = %q, want the chat request ID", event.Kind, event.TurnID)
		}
	}
	if replies == 0 {
		t.Fatal("no semantic events were published for the chat response")
	}
}

type lockedSemanticSink struct {
	mu     sync.Mutex
	events []streamcontract.Event
}

func (s *lockedSemanticSink) Emit(_ context.Context, event streamcontract.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *lockedSemanticSink) snapshot() []streamcontract.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]streamcontract.Event(nil), s.events...)
}

// A response the session asked for can lose a race with one the provider
// started for speech it just detected; the provider refuses the request. That
// is not a session failure: the session keeps running, asks again once the
// provider's response is over, and the chat is answered by that retried
// response, not by the provider's own.
func TestRealtimeRejectedResponseIsRetriedAfterProviderResponse(t *testing.T) {
	s, bridge, done := startBusyTestSession(t, time.Second)
	events := busyTestRequest(t, s, bridge, done, "chat-1")
	// The provider answers its own turn instead and refuses ours.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "provider-resp"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseRejected, Error: errors.New("Cannot create response while another response is in progress.")}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, Role: "assistant", TextSource: "text", Text: "not for the chat", ResponseID: "provider-resp"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "provider-resp", Status: "completed"}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("session exited instead of retrying: %v", err)
	case event := <-events:
		t.Fatalf("the provider's own response reached the chat: %+v", event)
	case <-time.After(time.Second):
		t.Fatal("the refused response was not requested again")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "retried"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, Role: "assistant", TextSource: "text", Text: "hello back", ResponseID: "retried"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDelta)
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "retried", Status: "completed"}
	select {
	case event := <-events:
		if event.Type != agent.RealtimeChatEventDone || event.Response != "hello back" {
			t.Fatalf("chat completion = %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the chat did not complete with its retried response")
	}
}

// Cancelling the chat whose request was refused drops the retry: nothing is
// requested on its behalf after the user stopped it.
func TestRealtimeRejectedResponseRetryEndsWithItsChat(t *testing.T) {
	var interruptErr error
	s, bridge, done := startBusyTestSessionConfigured(t, time.Second, false, &interruptErr, time.Second, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := bridge.Handle(ctx, agent.RealtimeChatRequest{RequestID: "chat-1", Message: "hello"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.created:
	case <-time.After(time.Second):
		t.Fatal("response was not requested")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "provider-resp"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseRejected, Error: errors.New("Cannot create response while another response is in progress.")}
	cancel()
	select {
	case <-s.interrupted:
	case <-time.After(time.Second):
		t.Fatal("the chat was not cancelled")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "provider-resp", Status: "cancelled"}
	select {
	case <-s.created:
		t.Fatal("a cancelled chat's refused response was requested again")
	case err := <-done:
		t.Fatalf("session exited: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// A tool continuation the session asks for after a chat was served belongs to
// no chat. If the provider refuses it, the session still asks again; the
// finished chat is not mistaken for a cancelled one that is owed nothing.
func TestRealtimeRejectedContinuationAfterServedChatIsRetried(t *testing.T) {
	s, bridge, done := startBusyTestSessionConfigured(t, time.Second, false, nil, 0, true)
	events := busyTestRequest(t, s, bridge, done, "served")
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, Status: "completed"}
	requireBusyEvent(t, events, agent.RealtimeChatEventDone)

	// A voice turn calls a tool; its result asks for a continuation.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "voice"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "voice", CallID: "tool", Name: "unknown_tool", Arguments: "{}"}
	select {
	case <-s.toolResults:
	case <-time.After(time.Second):
		t.Fatal("tool result not sent")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "voice", Status: "completed"}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("session exited: %v", err)
	case <-time.After(time.Second):
		t.Fatal("tool continuation not requested")
	}
	// The provider started its own response first and refuses ours.
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "provider-resp"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseRejected, Error: errors.New("Cannot create response while another response is in progress.")}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "provider-resp", Status: "completed"}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("session exited instead of retrying: %v", err)
	case <-time.After(time.Second):
		t.Fatal("the refused continuation was not requested again")
	}
}

// A response the session requested and the user then spoke over is cancelled
// at the provider, and a user turn the provider left unanswered gets an
// explicit response instead of ending the session.
func TestRealtimeSupersededRequestedResponseIsCancelled(t *testing.T) {
	var noErr error
	s, bridge, done := startBusyTestSessionConfigured(t, 60*time.Second, false, &noErr, time.Second, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := bridge.Handle(ctx, agent.RealtimeChatRequest{RequestID: "typed", Message: "hello"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("session ended: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("response not requested")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStarted, ItemID: "speech"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "superseded"}
	select {
	case <-s.interrupted:
	case err := <-done:
		t.Fatalf("session ended: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("superseded response was left running at the provider")
	}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStopped, ItemID: "speech"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTurnResponseMissed}
	select {
	case <-s.created:
	case err := <-done:
		t.Fatalf("unanswered turn ended the session: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("unanswered user turn did not get a response")
	}
}

// Providers send a reply's final transcript while its audio still plays.
// Clients reload history on it, so it is published only once the response
// ends and the reply is saved, followed by the terminal event.
func TestRealtimeReplyFinalWaitsForTheSavedReply(t *testing.T) {
	sink := &lockedSemanticSink{}
	busyTestSemanticSink = sink
	t.Cleanup(func() { busyTestSemanticSink = nil })
	s, _, _ := startBusyTestSession(t, time.Second)

	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStarted, ItemID: "speech"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStopped, ItemID: "speech"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "reply"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, ResponseID: "reply", Role: "assistant", Text: "it is sunny"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptFinal, ResponseID: "reply", Role: "assistant", Text: "it is sunny"}
	time.Sleep(150 * time.Millisecond)
	for _, event := range sink.snapshot() {
		if event.Kind == streamcontract.EventTextFinal && event.ResponseID == "reply" {
			t.Fatal("final transcript published while the reply was still playing")
		}
	}

	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "reply", Status: "completed"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		finalAt, terminalAt := -1, -1
		for i, event := range sink.snapshot() {
			if event.ResponseID != "reply" {
				continue
			}
			switch event.Kind {
			case streamcontract.EventTextFinal:
				finalAt = i
			case streamcontract.EventResponseTerminal:
				terminalAt = i
			}
		}
		if finalAt >= 0 && terminalAt >= 0 {
			if finalAt > terminalAt {
				t.Fatal("final transcript published after the terminal event")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("final=%d terminal=%d, want both after the response ended", finalAt, terminalAt)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Replies are saved even when they do not end normally: what was said before
// a tool call, and a reply the user cut off, which is marked as interrupted.
func TestRealtimeSavesToolAndInterruptedReplies(t *testing.T) {
	s, _, _ := startBusyTestSession(t, 60*time.Second)
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStarted, ItemID: "speech-1"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStopped, ItemID: "speech-1"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "tool-reply"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, ResponseID: "tool-reply", Role: "assistant", Text: "let me check"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "tool-reply", CallID: "call-1", Name: realtimeCurrentTimeTool, Arguments: `{}`}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseDone, ResponseID: "tool-reply", Status: "completed"}
	select {
	case <-s.toolResults:
	case <-time.After(2 * time.Second):
		t.Fatal("tool result was not sent")
	}

	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "long-reply"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptDelta, ResponseID: "long-reply", Role: "assistant", Text: "here is a long answer"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStarted, ItemID: "speech-2"}
	s.events <- realtimevoice.Event{Kind: realtimevoice.EventResponseCancelled, ResponseID: "long-reply", Status: "cancelled"}

	deadline := time.Now().Add(2 * time.Second)
	for {
		var saved []string
		if manager, err := contextmanager.LoadContextManagerFromCurrentSession(agentpath.UserContextManagerSessionFolder(busyTestConfigDir)); err == nil {
			for _, message := range manager.MessageListDump().Messages {
				if message.Role == messages.MessageRoleAssistant {
					saved = append(saved, message.Content)
				}
			}
		}
		if len(saved) == 2 {
			if saved[0] != "let me check" || saved[1] != "here is a long answer（已中断）" {
				t.Fatalf("saved replies = %q", saved)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("saved replies = %q, want the tool reply and the interrupted one", saved)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
