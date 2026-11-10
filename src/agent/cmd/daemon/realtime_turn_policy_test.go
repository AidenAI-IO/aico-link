package main

import (
	"context"
	"strings"
	"testing"

	"aico-agent/internal/agent"
)

func TestRealtimeUserTranscriptIsEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want bool
	}{
		{"", true},
		{"   ", true},
		{"\n\t", true},
		{"退下", false},
		{"hello", false},
	}
	for _, tc := range cases {
		if got := realtimeUserTranscriptIsEmpty(tc.text); got != tc.want {
			t.Fatalf("realtimeUserTranscriptIsEmpty(%q) = %t, want %t", tc.text, got, tc.want)
		}
	}
}

func TestRealtimeConversationExitIntent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want bool
	}{
		{"退下", true},
		{"退下。", true},
		{"退一下吧", true},
		{" 退下吧！ ", true},
		{"再见", true},
		{"goodbye", true},
		{"Good Bye!", true},
		{"stop listening", true},
		{"go idle", true},
		{"您好", false},
		{"退下之后帮我开门", false},
		{"我想睡觉了但先别退下", false},
		{"不用了", false},
		{"没有了", false},
		{"没事了", false},
		{"先这样", false},
		{"sleep", false},
		{"", false},
		{"谁抢？", false},
		{"はいはい", false},
	}
	for _, tc := range cases {
		if got := realtimeConversationExitIntent(tc.text); got != tc.want {
			t.Fatalf("realtimeConversationExitIntent(%q) = %t, want %t", tc.text, got, tc.want)
		}
	}
}

func TestRealtimeEndConversationToolMentionsChineseExitPhrases(t *testing.T) {
	t.Parallel()
	var description string
	for _, tool := range realtimeVoiceToolDefinitions(agent.Config{}, nil) {
		if tool.Name == realtimeEndConversationTool {
			description = tool.Description
			break
		}
	}
	if description == "" {
		t.Fatal("end_conversation tool definition missing")
	}
	for _, phrase := range []string{"退下", "退一下", "再见", "goodbye"} {
		if !strings.Contains(description, phrase) {
			t.Fatalf("end_conversation description missing %q: %s", phrase, description)
		}
	}
}

func TestRealtimeEmptyTurnGuardScopesToTurn(t *testing.T) {
	t.Parallel()
	var guard realtimeEmptyTurnGuard
	if guard.shouldSuppressTurn(1) {
		t.Fatal("fresh guard should not suppress")
	}
	guard.suppressTurn(2)
	if guard.shouldSuppressTurn(1) {
		t.Fatal("guard suppressed the wrong turn")
	}
	if !guard.shouldSuppressTurn(2) {
		t.Fatal("suppressTurn() did not arm the expected turn")
	}
	guard.clear()
	if guard.shouldSuppressTurn(2) {
		t.Fatal("clear() left the guard armed")
	}
}

func TestCancelRealtimeOutputForPolicyNoopsWithoutActiveResponse(t *testing.T) {
	t.Parallel()
	cancelPending := false
	state := realtimeTurnState{}
	playback := realtimePlaybackState{}
	awaiting, err := cancelRealtimeOutputForPolicy(
		context.Background(),
		&state,
		&cancelPending,
		func() {},
		func(string) {},
		true,
		&fakeRealtimeResponseInterrupter{},
		&playback,
		&fakeRealtimePlaybackAudio{},
		agent.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16},
		nil,
		nil,
		"empty user transcript",
	)
	if err != nil {
		t.Fatalf("cancelRealtimeOutputForPolicy() error = %v", err)
	}
	if awaiting || cancelPending {
		t.Fatalf("awaiting=%t cancelPending=%t, want no provider wait", awaiting, cancelPending)
	}
}

func TestCancelRealtimeOutputForPolicyInterruptsActiveResponse(t *testing.T) {
	t.Parallel()
	cancelPending := false
	state := realtimeTurnState{responseActive: true, responseID: "resp-1"}
	playback := realtimePlaybackState{}
	interrupter := &fakeRealtimeResponseInterrupter{}
	audio := &fakeRealtimePlaybackAudio{}
	format := agent.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16}
	if err := playback.beginResponse(audio, format); err != nil {
		t.Fatalf("beginResponse: %v", err)
	}
	awaiting, err := cancelRealtimeOutputForPolicy(
		context.Background(),
		&state,
		&cancelPending,
		func() {},
		func(string) {},
		true,
		interrupter,
		&playback,
		audio,
		format,
		nil,
		&realtimeResponseWatchdog{},
		"local conversation-exit intent",
	)
	if err != nil {
		t.Fatalf("cancelRealtimeOutputForPolicy() error = %v", err)
	}
	if !awaiting || !cancelPending {
		t.Fatalf("awaiting=%t cancelPending=%t, want provider wait", awaiting, cancelPending)
	}
	if interrupter.calls != 1 {
		t.Fatalf("Interrupt calls = %d, want 1", interrupter.calls)
	}
	if state.responseActive {
		t.Fatal("responseInterrupted() did not clear responseActive")
	}
	if !state.responseTerminalPending {
		t.Fatal("responseTerminalPending was not set")
	}
	finished := state
	if !finished.responseFinished("resp-1") {
		t.Fatal("canceled response lost terminal ownership")
	}
}

func TestCancelRealtimeOutputForPolicyWithoutInterruptDoesNotWait(t *testing.T) {
	t.Parallel()
	cancelPending := false
	state := realtimeTurnState{responseActive: true, responseID: "resp-1", responseRequestPending: true}
	playback := realtimePlaybackState{}
	audio := &fakeRealtimePlaybackAudio{}
	format := agent.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16}
	if err := playback.beginResponse(audio, format); err != nil {
		t.Fatalf("beginResponse: %v", err)
	}
	awaiting, err := cancelRealtimeOutputForPolicy(
		context.Background(),
		&state,
		&cancelPending,
		func() { t.Fatal("cancel ack timer must not start without provider interrupt") },
		func(string) {},
		false,
		nil,
		&playback,
		audio,
		format,
		nil,
		&realtimeResponseWatchdog{},
		"local conversation-exit intent",
	)
	if err != nil {
		t.Fatalf("cancelRealtimeOutputForPolicy() error = %v", err)
	}
	if awaiting || cancelPending {
		t.Fatalf("awaiting=%t cancelPending=%t, want local-only drop", awaiting, cancelPending)
	}
	if state.responseActive {
		t.Fatal("responseInterrupted() did not clear responseActive")
	}
}

func TestRealtimeTurnStateResponseOwnerTurn(t *testing.T) {
	t.Parallel()
	state := realtimeTurnState{inputTurnSequence: 4, responseRequestPending: true, responseRequestTurn: 2}
	if got := state.responseOwnerTurn(); got != 2 {
		t.Fatalf("responseOwnerTurn() = %d, want request turn 2", got)
	}
	state.responseRequestPending = false
	if got := state.responseOwnerTurn(); got != 4 {
		t.Fatalf("responseOwnerTurn() = %d, want input turn 4", got)
	}
}
