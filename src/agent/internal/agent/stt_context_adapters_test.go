package agent

import (
	"context"
	"errors"
	"testing"
)

func TestContextAwareSTTAdaptersHonorCanceledContext(t *testing.T) {
	wav := pcm16MonoToWAV([]int16{100, -100}, 16000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	google, err := NewGoogleCloudSTT("test-key", "https://example.invalid/speech", "en", "", nil)
	if err != nil {
		t.Fatalf("NewGoogleCloudSTT() error = %v", err)
	}
	providers := []struct {
		name   string
		client STTContextTranscriber
	}{
		{name: "google-cloud", client: google},
		{name: "tencent", client: NewTencentASRSTT("", "", "", "", "", "")},
		{name: "dashscope", client: NewDashScopeRealtimeSTT("", "", "", "")},
	}
	for _, provider := range providers {
		t.Run(provider.name, func(t *testing.T) {
			_, err := provider.client.TranscribeWAVWithContext(ctx, wav)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("TranscribeWAVWithContext() error = %v, want context.Canceled", err)
			}
		})
	}
}
