package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/agent/realtimevoice"
	"aico-agent/internal/agenttask"

	langtools "github.com/tmc/langchaingo/tools"
)

type fakeRealtimePlaybackAudio struct {
	nextSessionID uint64
	starts        int
	writes        []fakeRealtimePlaybackWrite
	stops         []uint64
	failWriteAt   int
	writeErr      error
}

type fakeRealtimePlaybackWrite struct {
	sessionID uint64
	data      []byte
	final     bool
}

type fakeRealtimeResponseInterrupter struct {
	calls int
	last  realtimevoice.ResponseInterruption
}

func (f *fakeRealtimeResponseInterrupter) Interrupt(_ context.Context, interruption realtimevoice.ResponseInterruption) error {
	f.calls++
	f.last = interruption
	return nil
}

func (f *fakeRealtimePlaybackAudio) StartPlayback(agent.AudioFormat) (*agent.PlaybackStartResult, error) {
	f.starts++
	f.nextSessionID++
	return &agent.PlaybackStartResult{SessionID: f.nextSessionID}, nil
}

func (f *fakeRealtimePlaybackAudio) WritePlayChunk(sessionID uint64, data []byte, final bool) error {
	if f.failWriteAt == len(f.writes)+1 {
		return f.writeErr
	}
	f.writes = append(f.writes, fakeRealtimePlaybackWrite{
		sessionID: sessionID,
		data:      append([]byte(nil), data...),
		final:     final,
	})
	return nil
}

func (f *fakeRealtimePlaybackAudio) StopPlayback(sessionID uint64) error {
	f.stops = append(f.stops, sessionID)
	return nil
}

func TestRealtimePlaybackSplitsLargeAudioDelta(t *testing.T) {
	audio := &fakeRealtimePlaybackAudio{}
	playback := realtimePlaybackState{}
	format := agent.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16}
	pcm := make([]byte, 9*realtimePlaybackChunkBytes+77)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	if err := playback.appendItem(audio, format, "item_1", pcm); err != nil {
		t.Fatal(err)
	}
	var written []byte
	for i, write := range audio.writes {
		if write.sessionID != 1 || write.final || len(write.data) > realtimePlaybackChunkBytes {
			t.Fatalf("write %d = %+v, want bounded non-final PCM for session 1", i, write)
		}
		written = append(written, write.data...)
	}
	if len(audio.writes) != 10 {
		t.Fatalf("writes = %d, want 10", len(audio.writes))
	}
	if len(audio.writes[9].data) != 77 || !bytes.Equal(written, pcm) {
		t.Fatalf("writes = %d, tail = %d, PCM preserved = %t", len(audio.writes), len(audio.writes[9].data), bytes.Equal(written, pcm))
	}
	if playback.submittedBytes != int64(len(pcm)) {
		t.Fatalf("submitted bytes = %d, want %d", playback.submittedBytes, len(pcm))
	}
}

func TestRealtimePlaybackChunkWriteFailureTracksAcceptedAudio(t *testing.T) {
	writeErr := errors.New("audio write failed")
	audio := &fakeRealtimePlaybackAudio{failWriteAt: 2, writeErr: writeErr}
	playback := realtimePlaybackState{}
	format := agent.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16}
	err := playback.appendItem(audio, format, "item_1", make([]byte, 3*realtimePlaybackChunkBytes))
	if !errors.Is(err, writeErr) {
		t.Fatalf("append error = %v, want %v", err, writeErr)
	}
	if len(audio.writes) != 1 || playback.submittedBytes != realtimePlaybackChunkBytes || playback.itemID != "item_1" {
		t.Fatalf("accepted writes = %d, submitted bytes = %d, item = %q", len(audio.writes), playback.submittedBytes, playback.itemID)
	}
}

func TestRealtimePlaybackStaysOpenAcrossResponsesAndInterrupts(t *testing.T) {
	audio := &fakeRealtimePlaybackAudio{}
	playback := realtimePlaybackState{}
	format := agent.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16}

	if err := playback.open(audio, format); err != nil {
		t.Fatal(err)
	}
	if err := playback.append(audio, format, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := playback.beginResponse(audio, format); err != nil {
		t.Fatal(err)
	}
	if audio.starts != 1 {
		t.Fatalf("playback starts = %d, want one persistent session", audio.starts)
	}

	if err := playback.interrupt(audio, format); err != nil {
		t.Fatal(err)
	}

	if len(audio.stops) != 1 || audio.stops[0] != 1 {
		t.Fatalf("stopped sessions = %v, want [1]", audio.stops)
	}
	if err := playback.append(audio, format, []byte{3, 4}); err != nil {
		t.Fatal(err)
	}
	if audio.starts != 2 || len(audio.writes) != 1 {
		t.Fatalf("after stale delta: starts=%d writes=%d, want starts=2 writes=1", audio.starts, len(audio.writes))
	}

	if err := playback.beginResponse(audio, format); err != nil {
		t.Fatal(err)
	}
	if err := playback.append(audio, format, []byte{5, 6}); err != nil {
		t.Fatal(err)
	}
	if audio.starts != 2 {
		t.Fatalf("playback starts = %d, want one restart after interruption", audio.starts)
	}
	for _, write := range audio.writes {
		if write.final {
			t.Fatalf("persistent realtime playback sent final write: %#v", write)
		}
	}
}

func TestRealtimeLocalPlaybackFinalizesEachResponse(t *testing.T) {
	audio := &fakeRealtimePlaybackAudio{}
	playback := realtimePlaybackState{finalizeResponses: true}
	format := agent.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}

	if err := playback.open(audio, format); err != nil {
		t.Fatal(err)
	}
	if err := playback.append(audio, format, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := playback.finishResponse(audio); err != nil {
		t.Fatal(err)
	}
	if len(audio.writes) != 2 || !audio.writes[1].final {
		t.Fatalf("writes = %#v, want PCM followed by a final write", audio.writes)
	}
	if err := playback.beginResponse(audio, format); err != nil {
		t.Fatal(err)
	}
	if len(audio.stops) != 1 || audio.starts != 2 {
		t.Fatalf("after next response: starts=%d stops=%v, want starts=2 stops=[1]", audio.starts, audio.stops)
	}
}

func TestEnsureImplicitRealtimeResponseReopensPlaybackAfterInterruption(t *testing.T) {
	audio := &fakeRealtimePlaybackAudio{}
	playback := realtimePlaybackState{}
	format := agent.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16}
	if err := playback.open(audio, format); err != nil {
		t.Fatal(err)
	}
	if err := playback.interrupt(audio, format); err != nil {
		t.Fatal(err)
	}
	active := false
	assistantPersisted := true
	responseText := strings.Builder{}
	responseText.WriteString("stale")
	responseTranscript := strings.Builder{}
	responseTranscript.WriteString("stale")
	if err := ensureImplicitRealtimeResponse(
		&active, &assistantPersisted, &responseText, &responseTranscript,
		&playback, audio, format,
	); err != nil {
		t.Fatal(err)
	}
	if !active || assistantPersisted || playback.suppressDeltas {
		t.Fatalf("response state = active:%t persisted:%t suppress:%t", active, assistantPersisted, playback.suppressDeltas)
	}
	if responseText.Len() != 0 || responseTranscript.Len() != 0 {
		t.Fatalf("implicit response did not reset buffers: text=%q transcript=%q", responseText.String(), responseTranscript.String())
	}
	if err := playback.append(audio, format, []byte{5, 6}); err != nil {
		t.Fatal(err)
	}
	if len(audio.writes) != 1 || string(audio.writes[0].data) != string([]byte{5, 6}) {
		t.Fatalf("writes = %#v, want reopened response audio", audio.writes)
	}
}

func TestRecordRealtimeFinalTranscriptPopulatesChatFallback(t *testing.T) {
	var responseTranscript strings.Builder
	var chatTranscript strings.Builder
	var chatText strings.Builder
	if !recordRealtimeFinalTranscript("final only", &responseTranscript, &chatText, &chatTranscript) {
		t.Fatal("final transcript was not recorded for chat fallback")
	}
	if responseTranscript.String() != "final only" || chatTranscript.String() != "final only" {
		t.Fatalf("transcripts = response:%q chat:%q", responseTranscript.String(), chatTranscript.String())
	}
}

func TestRealtimeSessionTerminationPreservesBufferedError(t *testing.T) {
	want := errors.New("transport failed")
	errs := make(chan error, 1)
	errs <- want
	close(errs)
	if got := realtimeSessionTerminationError(errs); !errors.Is(got, want) {
		t.Fatalf("termination error = %v, want %v", got, want)
	}
}

func TestRealtimeSessionEventClosureMarksBufferedProviderError(t *testing.T) {
	want := errors.New("transport failed")
	errs := make(chan error, 1)
	errs <- want
	close(errs)

	got := realtimeSessionEventClosureError(errs)
	if !errors.Is(got, want) {
		t.Fatalf("event closure error = %v, want %v", got, want)
	}
	if !shouldAnnounceRealtimeSessionFailure(got) {
		t.Fatal("buffered provider error was not eligible for failure announcement")
	}
}

func TestRealtimeSessionEventClosurePreservesRotation(t *testing.T) {
	rotated := fmt.Errorf("%w: provider budget exhausted", realtimevoice.ErrSessionRotated)
	errs := make(chan error, 1)
	errs <- rotated
	close(errs)

	got := realtimeSessionEventClosureError(errs)
	if !errors.Is(got, realtimevoice.ErrSessionRotated) {
		t.Fatalf("rotation sentinel lost on event closure: %v", got)
	}
}

func TestRealtimeFailureAnnouncementOnlyAcceptsProviderFailures(t *testing.T) {
	original := errors.New("websocket closed")
	providerErr := markRealtimeProviderFailure(original)
	if !shouldAnnounceRealtimeSessionFailure(providerErr) {
		t.Fatal("provider failure was not eligible for announcement")
	}
	if shouldAnnounceRealtimeSessionFailure(errors.New("audio backend failed")) {
		t.Fatal("local failure was eligible for announcement")
	}
	if !errors.Is(providerErr, original) {
		t.Fatal("provider failure did not unwrap to the original error")
	}
}

func TestRealtimeTeardownPreservesWakeupDuringFailureAnnouncement(t *testing.T) {
	for _, tc := range []struct {
		name                string
		announcementPending bool
		activate            bool
		wantWakeup          bool
	}{
		{name: "failure announcement startup", announcementPending: true, activate: true, wantWakeup: true},
		{name: "failure announcement without activation", announcementPending: true},
		{name: "normal teardown clears stale activation", activate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan struct{}, 1)
			if tc.activate {
				signalWakeupEvent(events)
			}
			drainRealtimeWakeups(events, tc.announcementPending)
			select {
			case <-events:
				if !tc.wantWakeup {
					t.Fatal("teardown retained a stale activation")
				}
			default:
				if tc.wantWakeup {
					t.Fatal("teardown lost the activation needed to cancel failure speech and reconnect")
				}
			}
			// Later activations must still reach the normal event-loop path.
			signalWakeupEvent(events)
			select {
			case <-events:
			default:
				t.Fatal("activation after teardown was lost")
			}
		})
	}
}

func TestInterruptRealtimeResponseSkipsIdleResponse(t *testing.T) {
	interrupter := &fakeRealtimeResponseInterrupter{}
	position := realtimevoice.ResponseInterruption{ItemID: "item_1", AudioEndMS: 250}
	if err := interruptRealtimeResponse(context.Background(), false, interrupter, position); err != nil {
		t.Fatal(err)
	}
	if interrupter.calls != 0 {
		t.Fatalf("idle response interrupts = %d, want 0", interrupter.calls)
	}
	if err := interruptRealtimeResponse(context.Background(), true, interrupter, position); err != nil {
		t.Fatal(err)
	}
	if interrupter.calls != 1 {
		t.Fatalf("active response interrupts = %d, want 1", interrupter.calls)
	}
	if interrupter.last != position {
		t.Fatalf("interrupt position = %+v, want %+v", interrupter.last, position)
	}
}

func TestRealtimeTurnStateReleasesAdmissionAfterSpeechStopped(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("response-old")
	state.speechStarted()
	state.speechStopped("")

	if !state.responseFinished("response-old") {
		t.Fatal("old response terminal event was not accepted")
	}
	if !state.canInjectResponse() {
		t.Fatal("task response remained blocked after speech_stopped")
	}

	state.responseStarted("response-new")
	if state.inputTurnPending {
		t.Fatal("new response did not consume the pending user turn")
	}
	if !state.responseFinished("response-new") || !state.canInjectResponse() {
		t.Fatal("turn state did not become idle after the new response completed")
	}
}

func TestRealtimeTurnStateSuppressesBargedInOutputAfterSpeechStopped(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("old")
	state.speechStarted()
	state.speechStopped("")
	if state.acceptsResponseEvent("old") {
		t.Fatal("stopped speech restored interrupted output")
	}
	state.responseStarted("new")
	if state.acceptsResponseEvent("old") || !state.acceptsResponseEvent("new") {
		t.Fatal("new response did not preserve old output suppression")
	}
	state.responseFinished("new")
	if state.acceptsResponseEvent("old") {
		t.Fatal("old output became eligible after the new response finished")
	}
}

func TestRealtimeTurnStateSuppressesInterruptedAnonymousOutputAfterTerminalPending(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("")
	state.responseInterrupted()
	state.speechStopped("")
	if state.acceptsResponseEvent("") {
		t.Fatal("anonymous interrupted output was accepted after speech stopped")
	}
}

func TestRealtimeTurnStateRejectsStaleResponseDone(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("response-new")
	if state.responseFinished("response-old") {
		t.Fatal("stale response terminal event was accepted")
	}
	if !state.responseActive || state.responseID != "response-new" {
		t.Fatalf("stale terminal event changed active response state: %+v", state)
	}
}

func TestRealtimeTurnStateDoesNotConsumeNewTurnForLateResponseCreated(t *testing.T) {
	state := realtimeTurnState{}
	state.responseRequested()
	state.speechStarted()
	state.speechStopped("")
	if state.responseStarted("response-old") {
		t.Fatal("late response.created was accepted as the current turn")
	}

	if state.inputTurnPending {
		t.Fatalf("speech_stopped left a pending input turn: %+v", state)
	}
}

func TestRealtimeTurnStateKeepsTranscriptFromInterruptedTurn(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("response-old")
	state.speechStarted()
	state.speechStopped("")
	state.userTranscriptObserved()

	if state.inputTurnPending || state.inputTurnSequence != 1 {
		t.Fatalf("user transcript was lost while old response was active: %+v", state)
	}
}

func TestRealtimeTurnStateIgnoresLateTranscriptForActiveResponse(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("response-1")
	if state.userTranscriptObserved() {
		t.Fatal("late transcript refinement was classified as a new user turn")
	}
	if state.inputTurnPending || state.inputTurnSequence != 0 {
		t.Fatalf("late transcript opened a new input turn: %+v", state)
	}
}

func TestRealtimeTurnStateRejectsStaleResponseOutput(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("response-old")
	state.speechStarted()
	state.speechStopped("")
	state.responseRequested()

	if state.acceptsResponseEvent("response-old") {
		t.Fatal("retired response output was accepted after a new request")
	}
	if !state.acceptsResponseEvent("response-new") {
		t.Fatal("current response output was rejected before response.created")
	}
}

func TestRealtimeTurnStateSuppressesCurrentResponseAfterBargeIn(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("response-1")
	state.speechStarted()
	if state.acceptsResponseEvent("response-1") {
		t.Fatal("response output was accepted after a new input turn started")
	}
	state.speechStopped("turn_invalid")
	if !state.acceptsResponseEvent("response-1") {
		t.Fatal("response output was not restored after turn_invalid")
	}
}

func TestRealtimeTurnStateRejectsAnonymousStaleResponseUntilNewTranscript(t *testing.T) {
	state := realtimeTurnState{}
	state.responseRequested()
	state.speechStarted()
	state.speechStopped("")
	if state.responseStarted("") {
		t.Fatal("anonymous stale response.created was accepted")
	}
	if state.acceptsResponseEvent("") || state.responseFinished("") {
		t.Fatal("anonymous stale response output was accepted")
	}

	state.userTranscriptObserved()
	if !state.responseStarted("") {
		t.Fatal("new anonymous response was rejected after its transcript arrived")
	}
}

func TestRealtimeTurnStateRejectsDuplicateResponseCreated(t *testing.T) {
	state := realtimeTurnState{}
	if !state.responseStarted("response-1") {
		t.Fatal("initial response.created was rejected")
	}
	state.speechStarted()
	state.speechStopped("")
	if state.responseStarted("response-1") {
		t.Fatal("duplicate response.created consumed the new input turn")
	}
	if state.inputTurnPending {
		t.Fatal("speech_stopped left the new input turn pending")
	}
}

// A repeated utterance boundary must not reopen the input turn once
// response.created has bound the response ID. If it does, the response's own
// tool calls are discarded as stale and inputTurnPending stays set, which
// permanently blocks voice notification and background task injection. The
// adapters no longer emit a second boundary, and speechStopped clears
// inputTurnPending outright; this pins both properties down together.
func TestRealtimeTurnStateDuplicateSpeechStopKeepsResponseUsable(t *testing.T) {
	state := realtimeTurnState{}
	state.speechStarted()
	state.speechStopped("")
	if !state.responseStarted("response-1") {
		t.Fatal("response.created was rejected")
	}
	state.speechStopped("")
	if !state.acceptsResponseEvent("response-1") {
		t.Fatalf("active response's own output was discarded as stale: %+v", state)
	}
	if !state.responseFinished("response-1") {
		t.Fatal("terminal event for the active response was rejected")
	}
	if !state.canInjectResponse() {
		t.Fatalf("voice notification injection stayed blocked after the turn: %+v", state)
	}
}

func TestRealtimeTurnStateLocalSpeechStopDoesNotReopenConsumedTurn(t *testing.T) {
	state := realtimeTurnState{}
	state.speechStarted()
	state.responseStarted("")
	state.localSpeechStopped()
	if state.inputTurnPending || state.inputSpeechActive {
		t.Fatalf("local speech stop reopened consumed turn: %+v", state)
	}
}

func TestRealtimeAdmissionSpeechWaitsForPendingTextResponse(t *testing.T) {
	state := realtimeTurnState{responseRequestPending: true}
	endpoint := newRealtimeClientTurnEndpoint(800)
	if !endpoint.Observe(loudPCMFrame(), time.Now()) || !endpoint.speechActive {
		t.Fatal("test endpoint did not detect its loud frame")
	}
	endpoint.Reset()
	if shouldTrackRealtimeAdmissionSpeech(&state, false, realtimevoice.Capabilities{}) {
		endpoint.Observe(loudPCMFrame(), time.Now())
	}
	if endpoint.speechActive {
		t.Fatal("test endpoint should not be consulted while a text response is pending")
	}
	if shouldTrackRealtimeAdmissionSpeech(&realtimeTurnState{}, true, realtimevoice.Capabilities{}) {
		t.Fatal("local admission gate tracked microphone audio while a chat command was queued")
	}
	if !shouldTrackRealtimeAdmissionSpeech(&realtimeTurnState{}, false, realtimevoice.Capabilities{}) {
		t.Fatal("local admission gate did not track audio for an idle realtime session")
	}

	state = realtimeTurnState{}
	state.responseRequested()
	if shouldTrackRealtimeAdmissionSpeech(&state, false, realtimevoice.Capabilities{}) {
		t.Fatal("local admission gate tracked microphone audio after startChat requested a response")
	}
	if !state.responseStarted("") {
		t.Fatal("explicit text response was incorrectly treated as stale")
	}
}

func TestRealtimeChatAdmissionQueuesDuringServerAuthoritativeLocalSpeech(t *testing.T) {
	state := realtimeTurnState{}
	endpoint := newRealtimeClientTurnEndpoint(800)
	capabilities := realtimevoice.Capabilities{ServerAuthoritativeTurnDetection: true}
	if !shouldTrackRealtimeAdmissionSpeech(&state, false, capabilities) {
		t.Fatal("server-authoritative turn detection disabled the transient admission guard")
	}
	now := time.Now()
	started, stopped := observeRealtimeAdmissionSpeech(endpoint, &state, capabilities, loudPCMFrame(), now, true)
	if !started || stopped || !endpoint.speechActive {
		t.Fatalf("loud frame did not start transient speech: started=%t stopped=%t endpoint=%+v", started, stopped, endpoint)
	}
	if state.inputSpeechActive || state.inputTurnPending || !state.canInjectResponse() {
		t.Fatalf("transient speech claimed a provider-owned turn: %+v", state)
	}
	admission := (realtimeSessionAdmissionState{turnBlocked: !state.canInjectResponse(), localSpeechActive: endpoint.speechActive}).admission()
	if admission != realtimeChatQueueAfterResponse {
		t.Fatalf("speech-time admission=%v, want queue without busy", admission)
	}

	started, stopped = observeRealtimeAdmissionSpeech(endpoint, &state, capabilities, nil, now.Add(endpoint.silenceDuration), true)
	if started || !stopped || endpoint.speechActive {
		t.Fatalf("silence did not clear transient speech: started=%t stopped=%t endpoint=%+v", started, stopped, endpoint)
	}
	if state.inputSpeechActive || state.inputTurnPending || !state.canInjectResponse() {
		t.Fatalf("cleared transient speech left a pending turn: %+v", state)
	}

	admission = (realtimeSessionAdmissionState{turnBlocked: !state.canInjectResponse(), localSpeechActive: endpoint.speechActive}).admission()
	if admission != realtimeChatStart {
		t.Fatalf("cleared transient speech blocked WebUI text: state=%+v admission=%v", state, admission)
	}
}

func TestRealtimeAdmissionSpeechPreservesLegacyPendingTurn(t *testing.T) {
	state := realtimeTurnState{}
	endpoint := newRealtimeClientTurnEndpoint(800)
	now := time.Now()
	started, stopped := observeRealtimeAdmissionSpeech(endpoint, &state, realtimevoice.Capabilities{}, loudPCMFrame(), now, true)
	if !started || stopped || !state.inputSpeechActive || !state.inputTurnPending {
		t.Fatalf("legacy speech start was not promoted: started=%t stopped=%t state=%+v", started, stopped, state)
	}
	_, stopped = observeRealtimeAdmissionSpeech(endpoint, &state, realtimevoice.Capabilities{}, nil, now.Add(endpoint.silenceDuration), true)
	if !stopped || state.inputSpeechActive || !state.inputTurnPending {
		t.Fatalf("legacy speech stop changed pending-turn behavior: stopped=%t state=%+v", stopped, state)
	}
}

func TestRealtimeAdmissionSpeechLeavesClientEndpointForCommitTimer(t *testing.T) {
	state := realtimeTurnState{}
	endpoint := newRealtimeClientTurnEndpoint(800)
	now := time.Now()
	observeRealtimeAdmissionSpeech(endpoint, &state, realtimevoice.Capabilities{}, loudPCMFrame(), now, false)
	_, stopped := observeRealtimeAdmissionSpeech(endpoint, &state, realtimevoice.Capabilities{}, nil, now.Add(endpoint.silenceDuration), false)
	if stopped || !endpoint.speechActive || !state.inputSpeechActive || !state.inputTurnPending {
		t.Fatalf("client endpoint was cleared before its commit timer: stopped=%t endpoint=%+v state=%+v", stopped, endpoint, state)
	}
}

func TestRealtimeAdmissionSpeechDefersToAuthoritativeInterruptionDuringResponse(t *testing.T) {
	state := realtimeTurnState{}
	capabilities := realtimevoice.Capabilities{ServerAuthoritativeInterruption: true}
	if !shouldTrackRealtimeAdmissionSpeech(&state, false, capabilities) {
		t.Fatal("interruption authority disabled the local admission gate while idle")
	}
	if !state.responseStarted("") {
		t.Fatal("anonymous Gemini response did not start")
	}
	if shouldTrackRealtimeAdmissionSpeech(&state, false, capabilities) {
		t.Fatal("local admission VAD tracked speaker echo during an authoritative provider response")
	}
	if !state.acceptsResponseEvent("") {
		t.Fatalf("active anonymous response was marked stale: %+v", state)
	}
	if !shouldTrackRealtimeAdmissionSpeech(&state, false, realtimevoice.Capabilities{}) {
		t.Fatal("legacy provider behavior changed without the authoritative capability")
	}
}

func TestRealtimeGeminiTextResponseKeepsNextChatAdmissible(t *testing.T) {
	state := realtimeTurnState{}
	capabilities := realtimevoice.Capabilities{ServerAuthoritativeInterruption: true}
	state.responseRequested()
	if !state.responseStarted("") {
		t.Fatal("anonymous Gemini text response did not start")
	}

	endpoint := newRealtimeClientTurnEndpoint(800)
	if shouldTrackRealtimeAdmissionSpeech(&state, false, capabilities) {
		observeRealtimeAdmissionSpeech(endpoint, &state, capabilities, loudPCMFrame(), time.Now(), true)
	}
	if !state.acceptsResponseEvent("") {
		t.Fatalf("local playback echo marked the anonymous response stale: %+v", state)
	}
	if !state.responseFinished("") {
		t.Fatal("anonymous Gemini text response did not finish")
	}
	if !state.canInjectResponse() {
		t.Fatalf("completed text response left input admission blocked: %+v", state)
	}
}

func TestRealtimeChatPendingCoversQueuedCommand(t *testing.T) {
	bridge := newRealtimeChatBridge()
	if realtimeChatPending(bridge, nil) {
		t.Fatal("idle bridge reported a pending chat request")
	}

	bridge.commands <- realtimeChatCommand{}
	if !realtimeChatPending(bridge, nil) {
		t.Fatal("unread bridge command was not reported as pending")
	}

	// The event loop selects the command off the bridge and parks it in
	// queuedChat until the active response finishes. The request is still
	// pending during that window even though the bridge is now empty.
	command := <-bridge.commands
	if realtimeChatPending(bridge, nil) {
		t.Fatal("drained bridge reported a pending chat request")
	}
	if !realtimeChatPending(bridge, &command) {
		t.Fatal("queued chat command was not reported as pending")
	}
	if shouldTrackRealtimeAdmissionSpeech(&realtimeTurnState{}, realtimeChatPending(bridge, &command), realtimevoice.Capabilities{}) {
		t.Fatal("local admission gate tracked microphone audio while a chat command was parked in queuedChat")
	}
}

func loudPCMFrame() []byte {
	frame := make([]byte, 320)
	for i := 0; i+1 < len(frame); i += 2 {
		binary.LittleEndian.PutUint16(frame[i:i+2], 2000)
	}
	return frame
}

func TestRealtimeTurnStateLocalSpeechStopKeepsUnansweredTurnPending(t *testing.T) {
	state := realtimeTurnState{}
	state.speechStarted()
	state.localSpeechStopped()
	if state.canInjectResponse() || !state.inputTurnPending {
		t.Fatalf("local speech stop admitted unanswered turn: %+v", state)
	}
}

func TestRealtimeTurnStateTracksTranscriptOnlyUserTurn(t *testing.T) {
	state := realtimeTurnState{}
	state.userTranscriptObserved()
	if state.canInjectResponse() {
		t.Fatal("task response was admitted while a transcript-only user turn was pending")
	}
	if !state.responseStarted("") {
		t.Fatal("current transcript-only response was rejected")
	}
	if state.inputTurnPending {
		t.Fatal("response start did not consume transcript-only user turn")
	}
}

func TestRealtimeTurnStateCompletesTranscriptOnlyTurnWithoutResponse(t *testing.T) {
	state := realtimeTurnState{}
	state.userTranscriptObserved()
	if !state.responseFinished("") || state.inputTurnPending {
		t.Fatalf("transcript-only turn did not complete cleanly: %+v", state)
	}
}

func TestRealtimeTurnStateAcceptsInterruptedResponseTerminalForCleanup(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("response-old")
	state.responseInterrupted()

	if !state.responseFinished("response-old") {
		t.Fatal("terminal event for the interrupted response was not accepted for cleanup")
	}
	if state.responseActive || state.responseID != "" {
		t.Fatalf("interrupted response terminal did not leave the state idle: %+v", state)
	}
}

func TestRealtimeTurnStateRejectsInterruptedResponseTerminalAfterNewRequest(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("response-old")
	state.responseInterrupted()
	state.responseRequested()

	if state.responseFinished("response-old") {
		t.Fatal("terminal event from the interrupted response was accepted")
	}
	if !state.responseActive {
		t.Fatalf("stale terminal event cleared the new requested response: %+v", state)
	}
}

func TestRealtimeTurnStateRejectsCompletedResponseDuplicateAfterNewRequest(t *testing.T) {
	state := realtimeTurnState{}
	state.responseStarted("response-old")
	if !state.responseFinished("response-old") {
		t.Fatal("initial response terminal event was not accepted")
	}
	state.responseRequested()

	if state.responseFinished("response-old") {
		t.Fatal("duplicate terminal event from the completed response was accepted")
	}
	if !state.responseActive {
		t.Fatalf("duplicate terminal event cleared the new requested response: %+v", state)
	}
}

func TestRealtimeTurnStateLeavesInvalidTurnIdle(t *testing.T) {
	state := realtimeTurnState{}
	state.speechStarted()
	state.speechStopped("turn_invalid")
	if !state.canInjectResponse() {
		t.Fatal("invalid speech turn left response admission blocked")
	}
}

func TestRealtimePlaybackCapsInterruptionAtEstimatedPlayedAudio(t *testing.T) {
	audio := &fakeRealtimePlaybackAudio{}
	format := agent.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16}
	now := time.Unix(100, 0)
	playback := realtimePlaybackState{now: func() time.Time { return now }}
	if err := playback.appendItem(audio, format, "item_1", make([]byte, 12000)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(100 * time.Millisecond)
	got := playback.responseInterruption(format)
	want := (realtimevoice.ResponseInterruption{ItemID: "item_1", AudioEndMS: 100})
	if got != want {
		t.Fatalf("interruption = %+v, want %+v", got, want)
	}

	// Wall-clock playback cannot exceed the PCM duration already accepted by the
	// asynchronous audio queue.
	now = now.Add(time.Second)
	got = playback.responseInterruption(format)
	want.AudioEndMS = 250
	if got != want {
		t.Fatalf("capped interruption = %+v, want %+v", got, want)
	}
}

func TestRealtimePlaybackResumesResponseAfterInvalidTurn(t *testing.T) {
	audio := &fakeRealtimePlaybackAudio{}
	playback := realtimePlaybackState{}
	state := realtimeTurnState{}
	format := agent.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16}
	state.responseStarted("response-1")
	if err := playback.beginResponse(audio, format); err != nil {
		t.Fatal(err)
	}
	if err := playback.append(audio, format, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := playback.interrupt(audio, format); err != nil {
		t.Fatal(err)
	}
	state.speechStarted()
	state.speechStopped("turn_invalid")
	if err := restoreRealtimePlaybackAfterInvalidTurn(&state, &playback, audio, format); err != nil {
		t.Fatal(err)
	}
	if playback.suppressDeltas {
		t.Fatal("invalid turn left response audio suppressed")
	}
	if err := playback.append(audio, format, []byte{3, 4}); err != nil {
		t.Fatal(err)
	}
	if len(audio.writes) != 2 || string(audio.writes[1].data) != string([]byte{3, 4}) {
		t.Fatalf("writes = %#v, want resumed response audio", audio.writes)
	}
}

func TestRealtimePlaybackOutputFormatUsesConfiguredDeviceFormat(t *testing.T) {
	format := realtimePlaybackOutputFormat(agent.Config{Audio: agent.AudioConfig{
		SampleRate: 16000,
		Channels:   1,
		BitWidth:   16,
	}})
	if format.SampleRate != 16000 || format.Channels != 1 || format.BitWidth != 16 {
		t.Fatalf("format = %+v, want pcm/16000/mono/16", format)
	}
}

func TestRealtimeProviderAudioFormatPrefersNegotiatedFormat(t *testing.T) {
	got := realtimeProviderAudioFormat(realtimevoice.AudioFormat{
		Encoding: "pcm_s16le", SampleRate: 24000, Channels: 2, BitDepth: 24,
	}, 16000, 8000)
	if got.SampleRate != 24000 || got.Channels != 2 || got.BitWidth != 24 {
		t.Fatalf("format = %+v, want negotiated pcm/24000/stereo/24", got)
	}
}

func TestRealtimeProviderAudioFormatFallsBackToLegacyRate(t *testing.T) {
	got := realtimeProviderAudioFormat(realtimevoice.AudioFormat{}, 16000, 24000)
	if got.SampleRate != 16000 || got.Channels != 1 || got.BitWidth != 16 {
		t.Fatalf("format = %+v, want legacy pcm/16000/mono/16", got)
	}
}

type blockingRealtimeTool struct {
	started chan struct{}
	release chan struct{}
}

func (t *blockingRealtimeTool) Name() string        { return realtimeRecallTool }
func (t *blockingRealtimeTool) Description() string { return "blocking recall" }
func (t *blockingRealtimeTool) Call(ctx context.Context, _ string) (string, error) {
	close(t.started)
	select {
	case <-t.release:
		return `{"status":"ok"}`, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestStartRealtimeToolCallDoesNotBlockEventLoop(t *testing.T) {
	tool := &blockingRealtimeTool{started: make(chan struct{}), release: make(chan struct{})}
	results := make(chan realtimeToolResult, 1)
	call := realtimevoice.Event{Kind: realtimevoice.EventToolCall, ResponseID: "resp-1", CallID: "call-1", Name: realtimeRecallTool, Arguments: `{}`}

	executor := realtimeVoiceToolExecutor{delegated: map[string]langtools.Tool{realtimeRecallTool: tool}}
	startRealtimeToolCall(context.Background(), executor, call, results)
	select {
	case <-tool.started:
	case <-time.After(time.Second):
		t.Fatal("tool call did not start")
	}
	select {
	case <-results:
		t.Fatal("tool call completed before release")
	default:
	}
	close(tool.release)
	select {
	case result := <-results:
		if result.call.CallID != call.CallID || result.output != `{"status":"ok"}` {
			t.Fatalf("tool result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for asynchronous tool result")
	}
}

func TestRealtimeToolTrackerWaitsForAllResultsAfterResponseDone(t *testing.T) {
	tracker := newRealtimeToolTracker()
	tracker.start("resp-1")
	tracker.start("resp-1")
	if hasTools, continueNow := tracker.done("resp-1"); !hasTools || continueNow {
		t.Fatalf("done() = (%t, %t), want tools with deferred continuation", hasTools, continueNow)
	}
	if tracker.complete("resp-1") {
		t.Fatal("first tool result continued before all results completed")
	}
	if !tracker.complete("resp-1") {
		t.Fatal("last tool result did not release deferred continuation")
	}
}

func TestRealtimeToolTrackerContinuesWhenResultsPrecedeResponseDone(t *testing.T) {
	tracker := newRealtimeToolTracker()
	tracker.start("resp-1")
	if tracker.complete("resp-1") {
		t.Fatal("tool result continued before response.done")
	}
	if hasTools, continueNow := tracker.done("resp-1"); !hasTools || !continueNow {
		t.Fatalf("done() = (%t, %t), want immediate continuation", hasTools, continueNow)
	}
}

func TestRealtimeChatAdmissionQueuesTextWhileFarewellResponseIsActive(t *testing.T) {
	state := realtimeSessionAdmissionState{responseActive: true, turnBlocked: true, standbyPending: true}
	if got := state.admission(); got != realtimeChatQueueAfterResponse {
		t.Fatalf("admission() = %v, want queue after farewell response", got)
	}

	state.standbyPending = false
	if got := state.admission(); got != realtimeChatRejectBusy {
		t.Fatalf("admission without standby request = %v, want busy", got)
	}
}

func TestRealtimeChatAdmissionRejectsProviderOwnedSpeechInsteadOfQueuing(t *testing.T) {
	state := realtimeSessionAdmissionState{turnBlocked: true, localSpeechActive: true}
	if got := state.admission(); got != realtimeChatRejectBusy {
		t.Fatalf("admission() = %v, want busy for a provider-owned pending turn", got)
	}
}

func TestRealtimeChatAdmissionStartsTextDuringFarewellDrain(t *testing.T) {
	state := realtimeSessionAdmissionState{standbyPending: true}
	if got := state.admission(); got != realtimeChatStart {
		t.Fatalf("admission() = %v, want immediate start", got)
	}
}

func TestRealtimeSessionAdmissionGatesBackgroundResponses(t *testing.T) {
	ready := realtimeSessionAdmissionState{
		supportsText:     true,
		runtimeAvailable: true,
		taskUpdatesReady: true,
		pendingTaskCount: 1,
	}
	if !ready.canInjectTaskUpdates() || !ready.canInjectVoiceNotification() {
		t.Fatal("idle session did not admit background responses")
	}

	blocked := []struct {
		name  string
		apply func(*realtimeSessionAdmissionState)
	}{
		{"turn", func(s *realtimeSessionAdmissionState) { s.turnBlocked = true }},
		{"speech", func(s *realtimeSessionAdmissionState) { s.localSpeechActive = true }},
		{"notification", func(s *realtimeSessionAdmissionState) { s.notificationActive = true }},
		{"notification drain", func(s *realtimeSessionAdmissionState) { s.notificationDraining = true }},
		{"chat", func(s *realtimeSessionAdmissionState) { s.activeChat = true }},
		{"standby", func(s *realtimeSessionAdmissionState) { s.standbyPending = true }},
		{"chat bridge", func(s *realtimeSessionAdmissionState) { s.bridgePending = true }},
		{"text unsupported", func(s *realtimeSessionAdmissionState) { s.supportsText = false }},
	}
	for _, test := range blocked {
		t.Run(test.name, func(t *testing.T) {
			state := ready
			test.apply(&state)
			if state.canInjectTaskUpdates() || state.canInjectVoiceNotification() {
				t.Fatalf("background response admitted with %s active", test.name)
			}
		})
	}

	withoutTasks := ready
	withoutTasks.pendingTaskCount = 0
	if withoutTasks.canInjectTaskUpdates() || !withoutTasks.canInjectVoiceNotification() {
		t.Fatal("task availability affected notification admission")
	}
	withoutRuntime := ready
	withoutRuntime.runtimeAvailable = false
	if !withoutRuntime.canInjectTaskUpdates() || withoutRuntime.canInjectVoiceNotification() {
		t.Fatal("runtime availability affected task admission")
	}
}

func TestRealtimeSleepStateDefersStandbyUntilFarewellDrains(t *testing.T) {
	var sleep realtimeSleepState
	if sleep.pending() || sleep.shouldDrain() {
		t.Fatal("fresh sleep state already requested standby")
	}
	sleep.request()
	if !sleep.pending() || !sleep.shouldDrain() {
		t.Fatal("standby request did not become drainable")
	}

	drain := make(chan error, 1)
	sleep.startDrain(drain, func() {})
	if sleep.shouldDrain() || sleep.draining() == nil {
		t.Fatal("standby drain did not start")
	}
	drain <- nil
	select {
	case <-sleep.draining():
	case <-time.After(time.Second):
		t.Fatal("drain completion was not observable")
	}
}

func TestRealtimeSleepStateAbandonCancelsPendingStandby(t *testing.T) {
	var sleep realtimeSleepState
	canceled := false
	sleep.request()
	sleep.startDrain(make(chan error, 1), func() { canceled = true })
	if !sleep.abandon() {
		t.Fatal("abandon() = false with a pending request")
	}
	if !canceled || sleep.pending() || sleep.draining() != nil {
		t.Fatalf("abandoned standby state = pending:%t drain:%v canceled:%t", sleep.pending(), sleep.draining(), canceled)
	}
}

func TestRealtimeFarewellDrainYieldsToReadySpeechEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := make(chan realtimevoice.Event, 1)
	events, reengagement := relayRealtimeSessionEvents(ctx, source)
	source <- realtimevoice.Event{Kind: realtimevoice.EventSpeechStarted}

	deadline := time.Now().Add(time.Second)
	for len(events) == 0 || len(reengagement) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("speech event was not relayed with its re-engagement marker")
		}
		time.Sleep(time.Millisecond)
	}

	var sleep realtimeSleepState
	sleep.request()
	drain := make(chan error, 1)
	drain <- nil
	select {
	case <-drain:
		if !abandonSleepForPendingRealtimeReengagement(&sleep, reengagement) {
			t.Fatal("ready speech event did not override farewell drain completion")
		}
	default:
		t.Fatal("drain completion was not ready")
	}
	if sleep.pending() {
		t.Fatal("standby remained pending after user re-engagement")
	}
	select {
	case event := <-events:
		if event.Kind != realtimevoice.EventSpeechStarted {
			t.Fatalf("relayed event = %s, want speech_started", event.Kind)
		}
	default:
		t.Fatal("speech event was consumed while prioritizing re-engagement")
	}
}

func TestRealtimeFarewellDrainYieldsToProviderUserTranscript(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := make(chan realtimevoice.Event, 1)
	events, reengagement := relayRealtimeSessionEvents(ctx, source)
	source <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptFinal, Role: "user", Text: "hello"}

	deadline := time.Now().Add(time.Second)
	for len(events) == 0 || len(reengagement) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("provider user transcript was not relayed with its re-engagement marker")
		}
		time.Sleep(time.Millisecond)
	}

	var sleep realtimeSleepState
	sleep.request()
	if !abandonSleepForPendingRealtimeReengagement(&sleep, reengagement) {
		t.Fatal("provider user transcript did not override farewell standby")
	}
	if sleep.pending() {
		t.Fatal("standby remained pending after provider user transcript")
	}
	select {
	case event := <-events:
		if event.Kind != realtimevoice.EventTranscriptFinal || event.Role != "user" {
			t.Fatalf("relayed event = %+v, want user transcript", event)
		}
	default:
		t.Fatal("user transcript was consumed while prioritizing re-engagement")
	}
}

func TestRealtimeFarewellDrainIgnoresTranscriptRefinement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := make(chan realtimevoice.Event, 2)
	events, reengagement := relayRealtimeSessionEvents(ctx, source)
	source <- realtimevoice.Event{Kind: realtimevoice.EventResponseStarted, ResponseID: "farewell"}
	source <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptFinal, Role: "user", Text: "goodbye"}

	deadline := time.Now().Add(time.Second)
	for len(events) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("farewell response events were not relayed")
		}
		time.Sleep(time.Millisecond)
	}
	if consumeRealtimeReengagement(reengagement) {
		t.Fatal("late farewell transcript was marked as user re-engagement")
	}

	state := realtimeTurnState{}
	if !state.responseStarted("farewell") {
		t.Fatal("farewell response did not start")
	}
	<-events
	transcript := <-events
	if transcript.Kind != realtimevoice.EventTranscriptFinal || state.userTranscriptObserved() {
		t.Fatal("late farewell transcript was classified as a new user turn")
	}
}

func TestRealtimeChatBridgeInactiveRequestQueuesAndWakesSession(t *testing.T) {
	wakeup := make(chan struct{}, 1)
	bridge := newRealtimeChatBridge(func() { wakeup <- struct{}{} })
	events, err := bridge.Handle(context.Background(), agent.RealtimeChatRequest{RequestID: "req-1", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-wakeup:
	case <-time.After(time.Second):
		t.Fatal("inactive request did not activate realtime session")
	}
	select {
	case command := <-bridge.commands:
		if command.request.Message != "hello" || command.request.RequestID != "req-1" {
			t.Fatalf("queued request = %+v", command.request)
		}
		if !sendRealtimeChatEvent(command, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventDone, Response: "hi"}) {
			t.Fatal("failed to deliver bridge response")
		}
		close(command.events)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for queued request")
	}
	select {
	case event := <-events:
		if event.Type != agent.RealtimeChatEventDone || event.Response != "hi" {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bridge response")
	}
}

func TestRealtimeChatBridgeActiveRequestDoesNotWakeSessionAgain(t *testing.T) {
	wakeups := 0
	bridge := newRealtimeChatBridge(func() { wakeups++ })

	bridge.activate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := bridge.Handle(ctx, agent.RealtimeChatRequest{RequestID: "req-1", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case command := <-bridge.commands:
		if command.request.Message != "hello" || command.request.RequestID != "req-1" {
			t.Fatalf("queued request = %+v", command.request)
		}
		if !sendRealtimeChatEvent(command, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventDone, Response: "hi"}) {
			t.Fatal("failed to deliver bridge response")
		}
		close(command.events)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for queued request")
	}
	select {
	case event := <-events:
		if event.Type != agent.RealtimeChatEventDone || event.Response != "hi" {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bridge response")
	}
	bridge.deactivate()
	if wakeups != 0 {
		t.Fatalf("active request triggered %d extra wakeups", wakeups)
	}
}

func TestRealtimeRotationSurvivesSessionErrorPath(t *testing.T) {
	// Rotation is distinguished with errors.Is in the wakeup loop, so the
	// sentinel has to reach it intact through the session error channel. A plain
	// error must stay non-rotation so real failures still drop to idle.
	rotated := fmt.Errorf("%w: gemini live ends this session in 50s", realtimevoice.ErrSessionRotated)
	errs := make(chan error, 1)
	errs <- rotated
	close(errs)
	got := realtimeSessionTerminationError(errs)
	if !errors.Is(got, realtimevoice.ErrSessionRotated) {
		t.Fatalf("rotation sentinel lost on the termination path: %v", got)
	}
	if !strings.Contains(got.Error(), "50s") {
		t.Fatalf("announced budget lost: %v", got)
	}

	plain := make(chan error, 1)
	plain <- errors.New("transport failed")
	close(plain)
	if errors.Is(realtimeSessionTerminationError(plain), realtimevoice.ErrSessionRotated) {
		t.Fatal("a plain transport error must not be treated as rotation")
	}
	if shouldFailQueuedRealtimeChat(rotated) {
		t.Fatal("queued chat must survive a scheduled session rotation")
	}
	if !shouldFailQueuedRealtimeChat(errors.New("transport failed")) {
		t.Fatal("queued chat must fail when a session ends with a real error")
	}
}

func TestFailActiveRealtimeChatReportsSessionError(t *testing.T) {
	command := realtimeChatCommand{
		ctx:    context.Background(),
		events: make(chan agent.RealtimeChatEvent, 1),
	}
	failActiveRealtimeChat(&command, errors.New("provider response failed"))
	event, ok := <-command.events
	if !ok {
		t.Fatal("active chat closed without an error event")
	}
	if event.Type != agent.RealtimeChatEventError || event.Error != "provider response failed" {
		t.Fatalf("active chat event = %+v", event)
	}
	if _, ok := <-command.events; ok {
		t.Fatal("active chat event channel remains open")
	}
}

func TestTaskWakeStateActivatesOncePerTerminalBatch(t *testing.T) {
	var state taskWakeState
	if state.shouldActivate(0) {
		t.Fatal("empty terminal sequence activated standby")
	}
	if !state.shouldActivate(1) {
		t.Fatal("first terminal update did not activate standby")
	}
	if state.shouldActivate(1) {
		t.Fatal("duplicate terminal sequence activated standby twice")
	}
	state.observeSession(2)
	if state.shouldActivate(2) {
		t.Fatal("update already consumed by a session activated standby again")
	}
	if !state.shouldActivate(3) {
		t.Fatal("new terminal update did not activate standby")
	}
}

func TestTaskWakeStateDoesNotRetryRestoredUpdate(t *testing.T) {
	var state taskWakeState
	if !state.shouldActivate(1) {
		t.Fatal("first terminal update did not activate standby")
	}
	// The session failed to deliver and restored the update without advancing
	// the sequence, so standby must not re-activate in a loop.
	state.observeSession(1)
	if state.shouldActivate(1) {
		t.Fatal("restored terminal update re-activated standby")
	}
}

func TestTaskWakeStateObserveSessionKeepsNewerSequence(t *testing.T) {
	state := taskWakeState{lastSeq: 4}
	state.observeSession(2)
	if !state.shouldActivate(5) {
		t.Fatal("stale session observation moved the watermark back")
	}
	if state.shouldActivate(5) {
		t.Fatal("terminal update activated standby twice")
	}
}

type staticTaskRunner struct{ result string }

func (r staticTaskRunner) Run(context.Context, string) (string, error) { return r.result, nil }

func TestAgentTaskWakeActivatesStandbyAndKeepsDrainSignal(t *testing.T) {
	manager := agenttask.NewManager(staticTaskRunner{result: "the report is ready"})
	defer manager.Close()
	if _, err := manager.Create("prepare the report"); err != nil {
		t.Fatal(err)
	}

	var state taskWakeState
	select {
	case <-agentTaskWakeNotifications(manager):
	case <-time.After(time.Second):
		t.Fatal("terminal task did not signal standby")
	}
	if pending, _ := agentTaskTerminalState(manager); len(pending) != 1 {
		t.Fatalf("pending terminal tasks = %+v", pending)
	}
	if !state.shouldActivate(agentTaskTerminalSequence(manager)) {
		t.Fatal("terminal task did not activate standby")
	}
	// The wake consumer must not have stolen the session's drain signal.
	select {
	case <-agentTaskNotifications(manager):
	case <-time.After(time.Second):
		t.Fatal("wake consumer stole the terminal drain signal")
	}
	drained := manager.DrainTerminalTasks()
	if len(drained) != 1 || drained[0].Result != "the report is ready" {
		t.Fatalf("drained = %+v", drained)
	}
	// A delivered batch is no longer pending, so a stale wake signal cannot
	// open an empty session.
	if pending, _ := agentTaskTerminalState(manager); len(pending) != 0 {
		t.Fatalf("delivered update still pending: %+v", pending)
	}
	if state.shouldActivate(agentTaskTerminalSequence(manager)) {
		t.Fatal("standby re-activated for an already delivered update")
	}
}

func TestAgentTaskWakeAnnouncesUpdateDroppedBySession(t *testing.T) {
	manager := agenttask.NewManager(staticTaskRunner{result: "done"})
	defer manager.Close()
	if _, err := manager.Create("do the thing"); err != nil {
		t.Fatal(err)
	}
	var state taskWakeState
	select {
	case <-agentTaskWakeNotifications(manager):
	case <-time.After(time.Second):
		t.Fatal("terminal task did not signal standby")
	}
	if !state.shouldActivate(agentTaskTerminalSequence(manager)) {
		t.Fatal("terminal task did not activate standby")
	}
	state.observeSession(agentTaskTerminalSequence(manager))

	// The session drained the update but ended before injecting it, so
	// teardown returned it to the queue. It must still be pending for delivery.
	<-agentTaskNotifications(manager)
	manager.RestoreTerminalTasks(manager.DrainTerminalTasks())
	if pending, _ := agentTaskTerminalState(manager); len(pending) != 1 {
		t.Fatalf("restored update not pending: %+v", pending)
	}
	// The sequence did not advance, so a restored batch must not re-activate:
	// the update waits for the next user activation instead of looping.
	if state.shouldActivate(agentTaskTerminalSequence(manager)) {
		t.Fatal("restored update re-activated standby")
	}
}
