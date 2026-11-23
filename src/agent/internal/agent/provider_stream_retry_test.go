package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

func TestProviderRetryStreamErrorsAndOutputBoundary(t *testing.T) {
	for _, provider := range []string{"openai", "gemini"} {
		for _, code := range []string{`"rate_limit_exceeded"`, `503`, `"invalid_api_key"`} {
			for _, afterOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/output=%v", provider, code, afterOutput), func(t *testing.T) {
					var calls atomic.Int32
					prefix := `data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n" + `data: {"choices":[{"delta":{"content":" tail"}}]}` + "\n\n"
					success := `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
					if provider == "gemini" {
						prefix = "data: {\"event_type\":\"step.start\",\"index\":0,\"step\":{\"type\":\"model_output\"}}\n\ndata: {\"event_type\":\"step.delta\",\"index\":0,\"delta\":{\"type\":\"text\",\"text\":\"hello\"}}\n\n"
						success = "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"ix_ok\",\"status\":\"completed\",\"steps\":[{\"type\":\"model_output\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}}\n\n"
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						if calls.Add(1) == 1 {
							if afterOutput {
								_, _ = io.WriteString(w, prefix)
							}
							_, _ = fmt.Fprintf(w, "data: {\"event_type\":\"error\",\"error\":{\"code\":%s,\"message\":\"failure\"}}\n\n", code)
							return
						}
						_, _ = io.WriteString(w, success)
					}))
					defer server.Close()
					manager := NewModelManager(ModelConfig{Provider: provider, BaseURL: server.URL, APIKey: "k", Model: "test"}, ProxyConfig{})
					model, err := manager.build()
					if err != nil {
						t.Fatal(err)
					}
					var output strings.Builder
					_, err = withProviderRetry(context.Background(), providerRetryPolicy{maxRetries: 1, requestTimeout: time.Second}, []llms.CallOption{llms.WithStreamingFunc(func(_ context.Context, b []byte) error { output.Write(b); return nil })}, func(ctx context.Context, opts []llms.CallOption) (*llms.ContentResponse, error) {
						return model.GenerateContent(ctx, []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "go")}, opts...)
					})
					shouldRetry := !afterOutput && code != `"invalid_api_key"`
					wantCalls := int32(1)
					if shouldRetry {
						wantCalls = 2
					}
					if calls.Load() != wantCalls || (err == nil) != shouldRetry {
						t.Fatalf("calls=%d err=%v output=%q", calls.Load(), err, output.String())
					}
					if afterOutput && output.String() != "hello" {
						t.Fatalf("partial output=%q", output.String())
					}
				})
			}
		}
	}
}

func TestProviderRetryStreamIdleTimeout(t *testing.T) {
	for _, afterOutput := range []bool{false, true} {
		t.Run(fmt.Sprint(afterOutput), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					if afterOutput {
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
					}
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			manager := NewModelManager(ModelConfig{Provider: "openai", BaseURL: server.URL, APIKey: "k", Model: "test"}, ProxyConfig{})
			model, err := manager.build()
			if err != nil {
				t.Fatal(err)
			}
			_, err = withProviderRetry(context.Background(), providerRetryPolicy{maxRetries: 1, requestTimeout: 60 * time.Millisecond}, []llms.CallOption{llms.WithStreamingFunc(func(context.Context, []byte) error { return nil })}, func(ctx context.Context, opts []llms.CallOption) (*llms.ContentResponse, error) {
				return model.GenerateContent(ctx, []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "go")}, opts...)
			})
			if afterOutput {
				if err == nil || calls.Load() != 1 {
					t.Fatalf("calls=%d err=%v", calls.Load(), err)
				}
			} else if err != nil || calls.Load() != 2 {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
