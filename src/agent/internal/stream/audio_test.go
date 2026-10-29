package stream

import (
	"testing"
	"time"
)

func TestAudioFrameValidationDurationAndClone(t *testing.T) {
	format := AudioFormat{Codec: CodecPCM16LE, SampleRate: 16000, Channels: 1, BitsPerSample: 16}
	frame := AudioFrame{
		ConversationID: "conversation-1", SessionEpoch: 2, OutputEpoch: 9,
		StreamID: "audio-1", FrameSeq: 7, PresentationSample: 320,
		Format: format, SampleCount: 160, Data: make([]byte, format.BytesForSamples(160)),
	}
	if err := frame.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, want := frame.Duration(), 10*time.Millisecond; got != want {
		t.Fatalf("duration=%s want %s", got, want)
	}
	if frame.StaleFor(9) || !frame.StaleFor(10) {
		t.Fatal("audio output epoch filtering is incorrect")
	}
	clone := frame.Clone()
	clone.Data[0] = 99
	if frame.Data[0] == clone.Data[0] {
		t.Fatal("clone shares mutable audio buffer")
	}
	frame.Data = frame.Data[:len(frame.Data)-1]
	if err := frame.Validate(); err == nil {
		t.Fatal("expected byte-count validation error")
	}
	frame.Data = make([]byte, format.BytesForSamples(160))
	frame.FrameSeq = 0 // zero is a valid first frame sequence number.
	if err := frame.Validate(); err != nil {
		t.Fatalf("first frame sequence should be accepted: %v", err)
	}
}

func TestAudioFormatSupportsNegotiatedChannels(t *testing.T) {
	format := AudioFormat{Codec: "pcm_f32le", SampleRate: 48000, Channels: 2, BitsPerSample: 32}
	if got, want := format.BytesPerSample(), 8; got != want {
		t.Fatalf("bytes per sample=%d want %d", got, want)
	}
	if got, want := format.BytesForSamples(48), 384; got != want {
		t.Fatalf("bytes for samples=%d want %d", got, want)
	}
}
