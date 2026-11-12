package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/agent/realtimevoice"
	"aico-agent/internal/agenttask"
	streamcontract "aico-agent/internal/stream"
	"nhooyr.io/websocket"
)

// loopSessionRecord is one session the wakeup loop ran.
type loopSessionRecord struct {
	route agent.RealtimeAudioRoute
}

// loopHarness runs the real realtime wakeup loop with a scripted session
// runner instead of a provider and audio devices.
type loopHarness struct {
	t        *testing.T
	server   *agent.Server
	http     *httptest.Server
	tasks    *agenttask.Manager
	sessions chan loopSessionRecord
	// script decides how each session ends. It receives the session's media
	// and returns the session error.
	mu     sync.Mutex
	script []func(realtimeSessionMedia) error
	done   chan struct{}
	signal chan os.Signal
	// wakeCallback is the loop's MCU/GPIO wakeup callback.
	wakeCallback atomic.Pointer[func()]
}

// wake simulates a wake word or button press.
func (h *loopHarness) wake() {
	h.t.Helper()
	callback := h.wakeCallback.Load()
	if callback == nil {
		h.t.Fatal("the loop installed no wakeup watcher")
	}
	(*callback)()
}

func newLoopHarness(t *testing.T, tasks *agenttask.Manager) *loopHarness {
	t.Helper()
	cfg := agent.DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	cfg.Model.Provider, cfg.Model.Model = "fake", "fake"
	cfg.InputMode, cfg.STT.Provider, cfg.TTS.Provider = "realtime", "", ""
	cfg.QuickCapture.Enabled = new(bool)
	runtime, err := agent.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	server := agent.NewServer(runtime, ":0")
	t.Cleanup(server.Close)
	h := &loopHarness{
		t: t, server: server, tasks: tasks,
		http:     httptest.NewServer(server.Handler()),
		sessions: make(chan loopSessionRecord, 8),
		done:     make(chan struct{}),
		signal:   make(chan os.Signal, 1),
	}
	t.Cleanup(h.http.Close)

	oldRunner := realtimeSessionRunner
	realtimeSessionRunner = func(_ agent.Config, _ chan os.Signal, _ *agent.Runtime, _ *agenttask.Manager, _ streamcontract.EventSink, media realtimeSessionMedia, _ ...*realtimeChatBridge) error {
		h.sessions <- loopSessionRecord{route: media.route()}
		h.mu.Lock()
		var step func(realtimeSessionMedia) error
		if len(h.script) > 0 {
			step, h.script = h.script[0], h.script[1:]
		}
		h.mu.Unlock()
		if step == nil {
			return nil
		}
		return step(media)
	}
	t.Cleanup(func() { realtimeSessionRunner = oldRunner })

	go func() {
		defer close(h.done)
		runRealtimeWakeupModeWithServer(cfg, h.signal, server, runtime, tasks, func(callback func()) (wakeupWatcher, error) {
			h.wakeCallback.Store(&callback)
			return idleWakeupWatcher{}, nil
		})
	}()
	t.Cleanup(func() {
		h.signal <- os.Interrupt
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			t.Error("wakeup loop did not stop")
		}
	})
	// The loop installs its activation handler on start.
	h.waitStatus(func(s agent.RealtimeVoiceStatus) bool { return s.Available }, "voice available")
	return h
}

func (h *loopHarness) then(steps ...func(realtimeSessionMedia) error) {
	h.mu.Lock()
	h.script = append(h.script, steps...)
	h.mu.Unlock()
}

func (h *loopHarness) status() agent.RealtimeVoiceStatus {
	resp, err := http.Get(h.http.URL + "/api/realtime/voice")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var status agent.RealtimeVoiceStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		h.t.Fatal(err)
	}
	return status
}

func (h *loopHarness) waitStatus(ok func(agent.RealtimeVoiceStatus) bool, what string) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if ok(h.status()) {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; status = %+v", what, h.status())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *loopHarness) stop() {
	resp, err := http.Post(h.http.URL+"/api/realtime/voice/stop", "application/json", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
}

// startBrowser opens the voice socket as the Web UI does, and returns a
// function that withdraws the start by disconnecting.
func (h *loopHarness) startBrowser() (withdraw func()) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.http.URL, "http")+"/api/realtime/voice/ws", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return func() { _ = conn.Close(websocket.StatusNormalClosure, "") }
}

func (h *loopHarness) nextSession(what string) loopSessionRecord {
	h.t.Helper()
	select {
	case record := <-h.sessions:
		return record
	case <-time.After(2 * time.Second):
		h.t.Fatalf("no session ran for %s", what)
		return loopSessionRecord{}
	}
}

// noSession fails if any session runs, including one that already started
// and ended before this call.
func (h *loopHarness) noSession(what string) {
	h.t.Helper()
	select {
	case record := <-h.sessions:
		h.t.Fatalf("%s opened a %s session", what, record.route)
	case <-time.After(300 * time.Millisecond):
	}
}

// A stop pressed while a device session is being rotated by the provider
// ends voice: the loop opens no successor, and the stopped session does not
// stay published and keep blocking browser starts. The stop lands after the
// session's teardown and before the loop decides whether to continue.
func TestWakeupLoopStopDuringDeviceRotationReleasesVoice(t *testing.T) {
	h := newLoopHarness(t, nil)
	h.then(func(realtimeSessionMedia) error { return realtimevoice.ErrSessionRotated })
	stopped := false
	oldHook := realtimeAfterSessionTeardown
	realtimeAfterSessionTeardown = func() {
		if !stopped {
			stopped = true
			h.stop()
		}
	}
	t.Cleanup(func() { realtimeAfterSessionTeardown = oldHook })

	h.wake()
	if got := h.nextSession("the wake activation").route; got != agent.RealtimeAudioRouteDevice {
		t.Fatalf("first session route = %s, want device", got)
	}
	h.noSession("a stop during rotation")
	h.waitStatus(func(s agent.RealtimeVoiceStatus) bool { return s.State == "idle" }, "voice idle after stop")
}

// A browser start withdrawn before the loop reaches it must not hide a task
// update queued behind it: the update still opens a device session. The
// update finishes while the withdrawn wakeup is being processed, so its
// notification arrives after the loop has already looked at the tasks.
func TestWakeupLoopWithdrawnBrowserStartKeepsTaskUpdate(t *testing.T) {
	tasks := agenttask.NewManager(staticTaskRunner{result: "done"})
	defer tasks.Close()
	h := newLoopHarness(t, tasks)

	finishTask := func() {
		if _, err := tasks.Create("background work"); err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if pending, _ := tasks.TerminalState(); len(pending) > 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Error("task did not finish")
	}
	// The browser start is withdrawn before the loop claims it; the task
	// completes just as the loop processes that withdrawn wakeup.
	// The loop can reach the hook before startBrowser returns, so hand the
	// withdraw function over through a channel.
	withdrawReady := make(chan func(), 1)
	first := true
	oldClaim := realtimeBeforeClaim
	realtimeBeforeClaim = func() {
		if !first {
			return
		}
		first = false
		(<-withdrawReady)()
		// Wait until the controller has dropped the browser start.
		deadline := time.Now().Add(2 * time.Second)
		for h.server.RealtimeVoicePending() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		finishTask()
	}
	t.Cleanup(func() { realtimeBeforeClaim = oldClaim })
	withdrawReady <- h.startBrowser()

	if got := h.nextSession("the queued task update").route; got != agent.RealtimeAudioRouteDevice {
		t.Fatalf("task update opened a %s session, want device", got)
	}
}

// idleWakeupWatcher stands in for the MCU and GPIO wakeup sources.
type idleWakeupWatcher struct{}

func (idleWakeupWatcher) Start() error { return nil }
func (idleWakeupWatcher) Stop()        {}
