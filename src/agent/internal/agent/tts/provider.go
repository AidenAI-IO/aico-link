// Package tts provides a pluggable TTS (Text-to-Speech) module.
//
// All providers conform to the same streaming interface (TTSProvider +
// StreamSession). Non-streaming backends (such as Minimax) buffer text
// internally inside their adapter; the upper layer only sees a unified
// streaming API.
package tts

import (
	"context"

	"aico-agent/internal/netproxy"
)

// TTSProvider is the unified abstraction for all TTS backends.
type TTSProvider interface {
	// Name returns the provider identifier (e.g. "minimax", "fish-audio").
	Name() string

	// Capabilities declares what the provider supports.
	Capabilities() Capabilities

	// BeginStream opens a streaming synthesis session. The caller pushes
	// text fragments via session.WriteText(); audio is written to sink as
	// it becomes available.
	BeginStream(ctx context.Context, sink AudioSink) (StreamSession, error)

	// Close releases provider-level resources (connection pools, goroutines).
	// Active sessions created from this provider must be closed first.
	Close() error
}

// StreamSession is one synthesis session.
type StreamSession interface {
	// WriteText pushes a text fragment. Adapter decides when to actually
	// synthesize: streaming providers push immediately, non-streaming
	// providers buffer until a sentence boundary.
	WriteText(text string) error

	// Flush forces the adapter to emit any buffered text.
	Flush() error

	// Close signals end-of-input, waits for remaining audio to play, and
	// releases session resources.
	Close() error

	// Err returns any cumulative error observed during the session.
	Err() error
}

// Capabilities describes provider features.
type Capabilities struct {
	// SupportsContextContinuation is true if the provider can keep prosody
	// continuous across multiple sentences (e.g. Cartesia context_id).
	SupportsContextContinuation bool

	// SupportedSampleRates lists output sample rates the provider supports.
	SupportedSampleRates []int

	// MaxTextLength is the maximum text length per request (0 = no limit).
	MaxTextLength int

	// RegionRestricted is true if the endpoint requires a proxy from
	// mainland China (Fish Audio, Cartesia, ElevenLabs, etc.).
	RegionRestricted bool
}

// AudioSink is the destination for PCM audio produced by a TTS provider.
type AudioSink interface {
	// Format returns the expected audio format for this sink.
	Format() AudioFormat

	// WritePCM writes PCM audio data.
	WritePCM(data []byte) error

	// Drain blocks until all queued audio has been played.
	Drain(ctx context.Context) error

	// Stop immediately stops playback (used for interruption).
	Stop() error
}

// AudioFormat specifies PCM audio parameters.
type AudioFormat struct {
	SampleRate int
	Channels   int
	BitWidth   int
}

// ProviderConfig is the unified configuration passed to factories.
type ProviderConfig struct {
	Provider   string
	APIKey     string
	Endpoint   string // optional, override default endpoint
	Voice      string // provider-specific voice ID
	Language   string // BCP-47, e.g. "zh-CN"
	SampleRate int
	SpeedRatio float64 // 1.0 = normal
	Proxy      ProxyConfig

	// Extra holds provider-specific parameters not covered by the common
	// fields above (e.g. emotion, reference_id, context_id_prefix).
	Extra map[string]any
}

// ProxyConfig mirrors the agent-level proxy settings. Adapters build HTTP
// clients with Transport and WebSocket dialers with ConfigureWebSocketDialer so
// they follow the same precedence and NO_PROXY rules as the Agent.
type ProxyConfig = netproxy.Config
