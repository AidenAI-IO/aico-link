package stream

import (
	"errors"
	"fmt"
	"time"
)

// AudioFormat describes the bytes carried by AudioFrame.  Codec is an
// explicit value so callers do not silently assume PCM16 when a provider uses
// another negotiated format.
type AudioFormat struct {
	Codec         string `json:"codec"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bits_per_sample"`
}

const CodecPCM16LE = "pcm_s16le"

// AudioFrame is a binary-path metadata wrapper.  Data must not be base64
// encoded by internal callers; transport adapters decide how to serialize it.
type AudioFrame struct {
	// Correlation fields mirror the semantic event envelope.  They are
	// optional for standalone audio (for example a private notification), but
	// when present they let playback receipts reject stale output epochs.
	ConversationID     string      `json:"conversation_id,omitempty"`
	SessionEpoch       uint64      `json:"session_epoch,omitempty"`
	TurnID             string      `json:"turn_id,omitempty"`
	RunID              string      `json:"run_id,omitempty"`
	ResponseID         string      `json:"response_id,omitempty"`
	ItemID             string      `json:"item_id,omitempty"`
	OutputEpoch        uint64      `json:"output_epoch,omitempty"`
	StreamID           string      `json:"stream_id"`
	FrameSeq           uint64      `json:"frame_seq"` // zero-based within StreamID
	PresentationSample uint64      `json:"presentation_sample,omitempty"`
	Format             AudioFormat `json:"format"`
	SampleCount        uint32      `json:"sample_count"`
	Data               []byte      `json:"-"`
}

func (f AudioFormat) Validate() error {
	if f.Codec == "" {
		return errors.New("audio format: codec is required")
	}
	if f.SampleRate <= 0 {
		return errors.New("audio format: sample rate must be positive")
	}
	if f.Channels <= 0 {
		return errors.New("audio format: channels must be positive")
	}
	if f.BitsPerSample <= 0 || f.BitsPerSample%8 != 0 {
		return errors.New("audio format: bits per sample must be a positive multiple of 8")
	}
	return nil
}

func (f AudioFormat) BytesPerSample() int {
	return f.Channels * f.BitsPerSample / 8
}

func (f AudioFormat) BytesForSamples(samples uint32) int {
	return int(samples) * f.BytesPerSample()
}

func (f AudioFrame) Validate() error {
	if f.StreamID == "" {
		return errors.New("audio frame: stream_id is required")
	}
	if f.SampleCount == 0 {
		return errors.New("audio frame: sample_count must be positive")
	}
	if err := f.Format.Validate(); err != nil {
		return err
	}
	want := f.Format.BytesForSamples(f.SampleCount)
	if len(f.Data) != want {
		return fmt.Errorf("audio frame: data has %d bytes, want %d for %d samples", len(f.Data), want, f.SampleCount)
	}
	return nil
}

func (f AudioFrame) Duration() time.Duration {
	if f.Format.SampleRate <= 0 || f.SampleCount == 0 {
		return 0
	}
	return time.Duration(uint64(f.SampleCount)*uint64(time.Second)) / time.Duration(f.Format.SampleRate)
}

// StaleFor reports whether this frame belongs to an invalidated output epoch.
// Frames without an output epoch are not filtered by this helper.
func (f AudioFrame) StaleFor(currentOutputEpoch uint64) bool {
	return f.OutputEpoch != 0 && f.OutputEpoch != currentOutputEpoch
}

// Clone copies the frame metadata and PCM bytes so a queue consumer can safely
// retain it after the producer reuses its capture buffer.
func (f AudioFrame) Clone() AudioFrame {
	clone := f
	clone.Data = append([]byte(nil), f.Data...)
	return clone
}
