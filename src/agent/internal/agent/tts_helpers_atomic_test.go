package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	ttsmodule "aico-agent/internal/agent/tts"
)

func TestBeginManagedTTSStreamPinsCapabilitiesToSessionProvider(t *testing.T) {
	oldProvider := &generationPinningTTSProvider{
		name:                "old",
		caps:                ttsmodule.Capabilities{SupportedSampleRates: []int{24000}},
		capabilitiesEntered: make(chan struct{}),
		releaseCapabilities: make(chan struct{}),
		beginFormats:        make(chan ttsmodule.AudioFormat, 1),
	}
	nextProvider := &generationPinningTTSProvider{
		name:         "next",
		caps:         ttsmodule.Capabilities{SupportedSampleRates: []int{16000}},
		beginFormats: make(chan ttsmodule.AudioFormat, 1),
	}
	manager := ttsmodule.NewProviderManager(oldProvider, nil)

	type beginResult struct {
		stream *streamSessionWriter
		err    error
	}
	beginDone := make(chan beginResult, 1)
	go func() {
		stream, err := beginManagedTTSStream(context.Background(), manager, noopTTSPlaybackBackend{}, Config{})
		beginDone <- beginResult{stream: stream, err: err}
	}()

	select {
	case <-oldProvider.capabilitiesEntered:
	case <-time.After(time.Second):
		t.Fatal("old provider capabilities were not read")
	}

	swapDone := make(chan ttsmodule.TTSProvider, 1)
	go func() {
		swapDone <- manager.Holder().Swap(nextProvider)
	}()
	waitForTTSManagerCurrent(t, manager, "next")
	close(oldProvider.releaseCapabilities)

	var result beginResult
	select {
	case result = <-beginDone:
	case <-time.After(time.Second):
		t.Fatal("beginManagedTTSStream did not return")
	}
	if result.err != nil {
		t.Fatalf("beginManagedTTSStream() error = %v", result.err)
	}

	select {
	case format := <-oldProvider.beginFormats:
		if format.SampleRate != 24000 {
			t.Errorf("old provider sink sample rate = %d, want 24000", format.SampleRate)
		}
	case format := <-nextProvider.beginFormats:
		t.Errorf("session started on replacement provider with sink format %#v", format)
	case <-time.After(time.Second):
		t.Error("neither provider started a session")
	}

	if result.stream != nil {
		if err := result.stream.closeAndWait(); err != nil {
			t.Errorf("closeAndWait() error = %v", err)
		}
	}
	select {
	case old := <-swapDone:
		if old != oldProvider {
			t.Errorf("Swap returned %#v, want old provider", old)
		}
	case <-time.After(time.Second):
		t.Error("Swap did not return after the stream closed")
	}
	if err := manager.Close(); err != nil {
		t.Errorf("manager.Close() error = %v", err)
	}
}

func waitForTTSManagerCurrent(t *testing.T, manager *ttsmodule.ProviderManager, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if manager.Current() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("manager.Current() = %q, want %q", manager.Current(), want)
}

type generationPinningTTSProvider struct {
	name                string
	caps                ttsmodule.Capabilities
	capabilitiesOnce    sync.Once
	capabilitiesEntered chan struct{}
	releaseCapabilities chan struct{}
	beginFormats        chan ttsmodule.AudioFormat
}

func (p *generationPinningTTSProvider) Name() string { return p.name }

func (p *generationPinningTTSProvider) Capabilities() ttsmodule.Capabilities {
	if p.capabilitiesEntered != nil {
		p.capabilitiesOnce.Do(func() { close(p.capabilitiesEntered) })
		<-p.releaseCapabilities
	}
	return p.caps
}

func (p *generationPinningTTSProvider) BeginStream(_ context.Context, sink ttsmodule.AudioSink) (ttsmodule.StreamSession, error) {
	p.beginFormats <- sink.Format()
	return generationPinningTTSSession{}, nil
}

func (p *generationPinningTTSProvider) Close() error { return nil }

type generationPinningTTSSession struct{}

func (generationPinningTTSSession) WriteText(string) error { return nil }
func (generationPinningTTSSession) Flush() error           { return nil }
func (generationPinningTTSSession) Close() error           { return nil }
func (generationPinningTTSSession) Err() error             { return nil }

type noopTTSPlaybackBackend struct{}

func (noopTTSPlaybackBackend) StartPlayback(ttsmodule.AudioFormat) (uint64, error) { return 1, nil }
func (noopTTSPlaybackBackend) WritePlayChunk(uint64, []byte, bool) error           { return nil }
func (noopTTSPlaybackBackend) StopPlayback(uint64) error                           { return nil }
