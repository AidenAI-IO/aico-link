package agent

import (
	"fmt"
	"math"
	"time"
)

const streamingSTTPreReadyBufferDuration = 2 * time.Second

// sttPCMPreReadyBuffer retains the beginning of a recording while a provider
// streaming session negotiates. It is deliberately bounded so a slow or
// unavailable provider cannot turn the recording path into an unbounded queue.
type sttPCMPreReadyBuffer struct {
	maxBytes int
	bytes    int
	chunks   [][]byte
	overflow bool
}

func newSTTPCMPreReadyBuffer(sampleRate, channels, bitWidth int) *sttPCMPreReadyBuffer {
	bytesPerSample := bitWidth / 8
	if bytesPerSample <= 0 {
		bytesPerSample = 2
	}
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	if channels <= 0 {
		channels = 1
	}
	bytesPerSecond := int64(sampleRate) * int64(channels) * int64(bytesPerSample)
	maxBytes := bytesPerSecond * int64(streamingSTTPreReadyBufferDuration/time.Second)
	if maxBytes <= 0 || maxBytes > int64(math.MaxInt) {
		maxBytes = int64(math.MaxInt)
	}
	return &sttPCMPreReadyBuffer{maxBytes: int(maxBytes)}
}

// append copies pcm so callers may safely reuse their capture buffer. Once a
// chunk would exceed the bound, the buffer enters overflow state and drops all
// queued data; the caller must fall back to bounded batch transcription.
func (b *sttPCMPreReadyBuffer) append(pcm []byte) bool {
	if b == nil || len(pcm) == 0 {
		return true
	}
	if b.overflow || len(pcm) > b.maxBytes-b.bytes {
		if b != nil {
			b.overflow = true
			b.chunks = nil
			b.bytes = 0
		}
		return false
	}
	b.chunks = append(b.chunks, append([]byte(nil), pcm...))
	b.bytes += len(pcm)
	return true
}

func (b *sttPCMPreReadyBuffer) drain() ([][]byte, bool) {
	if b == nil {
		return nil, false
	}
	chunks, overflow := b.chunks, b.overflow
	b.chunks = nil
	b.bytes = 0
	b.overflow = false
	return chunks, overflow
}

func (b *sttPCMPreReadyBuffer) String() string {
	if b == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d/%d bytes (%d chunks)", b.bytes, b.maxBytes, len(b.chunks))
}
