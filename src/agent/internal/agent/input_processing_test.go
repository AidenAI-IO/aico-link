package agent

import (
	"context"
	"strings"
	"testing"
)

func TestNewSTTClientFromConfigTrimsProviderWhitespace(t *testing.T) {
	clearAgentProxyEnv(t)

	client, err := NewSTTClientFromConfig(Config{
		STT: STTConfig{
			Provider: " openai ",
		},
	})
	if err != nil {
		t.Fatalf("NewSTTClientFromConfig() error = %v", err)
	}
	if client == nil {
		t.Fatal("expected STT client")
	}
}

func TestNewSTTClientFromConfigRejectsInvalidProxyEnvironment(t *testing.T) {
	clearAgentProxyEnv(t)
	t.Setenv("HTTP_PROXY", " http://proxy.example:7890")

	_, err := NewSTTClientFromConfig(Config{
		STT: STTConfig{
			Provider: "openai",
		},
	})
	if err == nil {
		t.Fatal("NewSTTClientFromConfig() error = nil, want proxy validation error")
	}
	if !strings.Contains(err.Error(), "proxy environment") {
		t.Fatalf("NewSTTClientFromConfig() error = %v, want proxy environment", err)
	}
}

func TestPrepareAudioInputAudioModeBuildsCanonicalTurn(t *testing.T) {
	wav := pcm16MonoToWAV([]int16{100, -100, 200, -200}, 16000)

	input, err := PrepareAudioInput("audio", nil, wav, "", "", nil)
	if err != nil {
		t.Fatalf("PrepareAudioInput() error = %v", err)
	}

	if input.Modality != "audio" {
		t.Fatalf("Modality = %q, want audio", input.Modality)
	}
	if input.InputText != voiceAudioInputPlaceholder {
		t.Fatalf("InputText = %q, want %q", input.InputText, voiceAudioInputPlaceholder)
	}
	if input.OriginalText != "" {
		t.Fatalf("OriginalText = %q, want empty", input.OriginalText)
	}
	if input.Transcript != "" {
		t.Fatalf("Transcript = %q, want empty", input.Transcript)
	}
	if len(input.Attachments) != 1 || input.Attachments[0].Kind != AttachmentKindAudio || len(input.Attachments[0].Data) == 0 {
		t.Fatalf("audio attachment missing from runtime input: %#v", input.Attachments)
	}
	if len(input.Artifacts) != 1 {
		t.Fatalf("Artifacts len = %d, want 1: %#v", len(input.Artifacts), input.Artifacts)
	}
	artifact := input.Artifacts[0]
	if artifact.Kind != AttachmentKindAudio || artifact.MIMEType != "audio/wav" || artifact.Name != "recording.wav" {
		t.Fatalf("artifact metadata = %#v", artifact)
	}
	if artifact.Size != int64(len(wav)) {
		t.Fatalf("artifact size = %d, want %d", artifact.Size, len(wav))
	}
	if artifact.DurationMS <= 0 {
		t.Fatalf("artifact duration should be positive: %#v", artifact)
	}
	if len(artifact.Data) != 0 {
		t.Fatalf("artifact must not carry binary data: %#v", artifact)
	}
}

func TestPrepareAudioInputSTTUsesTranscriptHintBeforeTranscribing(t *testing.T) {
	wav := pcm16MonoToWAV([]int16{100, -100, 200, -200}, 16000)
	client := &stubSTTClient{transcript: "should not be used"}

	input, err := PrepareAudioInput("stt", client, wav, "streaming transcript", "", nil)
	if err != nil {
		t.Fatalf("PrepareAudioInput() error = %v", err)
	}

	if input.Transcript != "streaming transcript" {
		t.Fatalf("Transcript = %q, want streaming transcript", input.Transcript)
	}
	if len(client.inputs) != 0 {
		t.Fatalf("expected transcript hint to skip one-shot STT, got %d TranscribeWAV calls", len(client.inputs))
	}
}

func TestPrepareAudioInputSTTUsesTranscriptHintWithoutClient(t *testing.T) {
	wav := pcm16MonoToWAV([]int16{100, -100, 200, -200}, 16000)

	input, err := PrepareAudioInput("stt", nil, wav, "streaming transcript", "", nil)
	if err != nil {
		t.Fatalf("PrepareAudioInput() error = %v", err)
	}

	if input.Transcript != "streaming transcript" {
		t.Fatalf("Transcript = %q, want streaming transcript", input.Transcript)
	}
}

func TestPrepareAudioInputWithContextCancelsSTTBeforeBuildingTurn(t *testing.T) {
	wav := pcm16MonoToWAV([]int16{100, -100}, 16000)
	client := &contextAwareSTTTestClient{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareAudioInputWithContext(ctx, "stt", client, wav, "", "", nil); err == nil {
		t.Fatal("PrepareAudioInputWithContext() error = nil, want cancellation")
	}
	if client.contextCalls != 0 || client.legacyCalls != 0 {
		t.Fatalf("context calls=%d legacy calls=%d", client.contextCalls, client.legacyCalls)
	}
}
