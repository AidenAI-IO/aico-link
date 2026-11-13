package main

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/agent/tts"
)

// fakeRouteAudio is an audio endpoint the fake dialog never opens.
type fakeRouteAudio agent.RealtimeAudioRoute

func (r fakeRouteAudio) Route() agent.RealtimeAudioRoute      { return agent.RealtimeAudioRoute(r) }
func (fakeRouteAudio) Recording() agent.AudioRecordingBackend { return nil }
func (fakeRouteAudio) Playback() tts.AudioServiceBackend      { return nil }
func (fakeRouteAudio) Close(string)                           {}

// fakeClassicVoice stands in for the agent server's voice controller. A
// browser start queues browser and wakes the loop.
type fakeClassicVoice struct {
	mu       sync.Mutex
	activate func()
	browser  *agent.RealtimeVoiceSession
	claims   []agent.RealtimeAudioRoute
	ended    chan *agent.RealtimeVoiceSession
}

func (f *fakeClassicVoice) SetRealtimeVoiceActivation(activate func()) {
	f.mu.Lock()
	f.activate = activate
	f.mu.Unlock()
}

func (f *fakeClassicVoice) ClaimRealtimeVoiceSession(_ agent.Config, requested agent.RealtimeAudioRoute) *agent.RealtimeVoiceSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	session := f.browser
	f.browser = nil
	if session == nil && requested == agent.RealtimeAudioRouteDevice {
		session = agent.NewRealtimeVoiceSession(fakeRouteAudio(agent.RealtimeAudioRouteDevice))
	}
	if session != nil {
		f.claims = append(f.claims, session.Route())
	}
	return session
}

func (f *fakeClassicVoice) EndRealtimeVoiceSession(session *agent.RealtimeVoiceSession, _ string) {
	f.ended <- session
}

func (f *fakeClassicVoice) startBrowser(session *agent.RealtimeVoiceSession) {
	f.mu.Lock()
	f.browser = session
	activate := f.activate
	f.mu.Unlock()
	activate()
}

// A Web UI voice start opens a Classic conversation on the browser route
// with no listen timeout, ignores wake words while it runs, and ends when
// the user presses End.
func TestClassicBrowserVoiceStartRunsUntilEnded(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	watcher := &fakeWakeupWatcher{}
	dialog := &fakeAudioDialog{frameSamples: 2, repeatEmpty: true, readDelay: time.Millisecond}
	voice := &fakeClassicVoice{ended: make(chan *agent.RealtimeVoiceSession, 2)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWakeupModeWithVoice(agent.Config{InputMode: "stt", VoiceFollowupTimeoutMs: 5, VoiceFirstTurnTimeoutMs: 5}, dialog, nil, voice, sigChan, func(callback func()) (wakeupWatcher, error) {
			watcher.callback = callback
			return watcher, nil
		})
	}()
	waitFor(t, "the loop to accept browser starts", func() bool {
		voice.mu.Lock()
		defer voice.mu.Unlock()
		return voice.activate != nil
	})

	browser := agent.NewRealtimeVoiceSession(fakeRouteAudio(agent.RealtimeAudioRouteWeb))
	voice.startBrowser(browser)
	waitFor(t, "the browser conversation to open", func() bool { return dialog.VoiceRoute() == agent.RealtimeAudioRouteWeb })

	// Well past the 5 ms listen timeouts, a browser conversation is still open
	// and a wake word neither ends it nor opens the board.
	watcher.callback()
	time.Sleep(50 * time.Millisecond)
	if dialog.VoiceRoute() != agent.RealtimeAudioRouteWeb {
		t.Fatal("the browser conversation closed before the user ended it")
	}

	browser.RequestStop()
	select {
	case ended := <-voice.ended:
		if ended != browser {
			t.Fatal("a different conversation ended")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("End did not close the browser conversation")
	}
	if dialog.VoiceRoute() != "" {
		t.Fatal("the dialog still serves the ended conversation")
	}
	time.Sleep(20 * time.Millisecond)
	voice.mu.Lock()
	claims := append([]agent.RealtimeAudioRoute(nil), voice.claims...)
	voice.mu.Unlock()
	if len(claims) != 1 || claims[0] != agent.RealtimeAudioRouteWeb {
		t.Fatalf("claims = %v, want only the browser conversation", claims)
	}

	sigChan <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not stop")
	}
}

// A browser start withdrawn before the loop reaches it opens nothing, and in
// particular not the board microphone.
func TestClassicWithdrawnBrowserStartOpensNothing(t *testing.T) {
	dialog := &fakeAudioDialog{frameSamples: 2}
	voice := &fakeClassicVoice{ended: make(chan *agent.RealtimeVoiceSession, 1)}
	var browserActive atomic.Bool
	if exit := runClassicConversation(agent.Config{}, dialog, nil, voice, make(chan os.Signal, 1), make(chan voiceEvent, 1), "", &browserActive, nil); exit {
		t.Fatal("runClassicConversation returned exit=true")
	}
	if dialog.starts != 0 || len(voice.claims) != 0 {
		t.Fatalf("recording starts = %d, claims = %v; want nothing opened", dialog.starts, voice.claims)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Muting the speaker while a Classic reply plays stops the queued reply and
// goes back to listening; it does not end the conversation.
func TestClassicSpeakerMuteStopsQueuedReply(t *testing.T) {
	voice := agent.NewRealtimeVoiceSession(fakeRouteAudio(agent.RealtimeAudioRouteWeb))
	speaking := make(chan struct{})
	var interrupted atomic.Int32
	dialog := &fakeAudioDialog{
		frameSamples:    2,
		interruptOutput: func() { interrupted.Add(1) },
	}
	dialog.speak = func(ctx context.Context) error {
		close(speaking)
		<-ctx.Done()
		return ctx.Err()
	}
	dialog.SetVoiceSession(voice)
	done := make(chan voiceTurnResult, 1)
	go func() {
		done <- runVoiceTurnWithInputContext(agent.Config{}, dialog, nil, agent.TurnInput{InputText: "voice"}, nil,
			make(chan os.Signal, 1), make(chan voiceEvent, 1), agent.VoiceTurnContext{}, true)
	}()
	<-speaking
	muted := true
	voice.SetMuted(nil, &muted)
	select {
	case result := <-done:
		if result.ended || result.exit || result.interrupted {
			t.Fatalf("result = %+v, want the conversation to continue", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("speaker mute did not stop the queued reply")
	}
	if interrupted.Load() == 0 {
		t.Fatal("speaker mute did not interrupt playback")
	}
}
