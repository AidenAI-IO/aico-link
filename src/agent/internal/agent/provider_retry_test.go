package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"aico-agent/internal/agent/messages"
	"github.com/tmc/langchaingo/llms"
)

func TestProviderRetryOutcomes(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		retry bool
	}{
		{"timeout", fmt.Errorf("send request: %w", context.DeadlineExceeded), true},
		{"network timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, true},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"permanent network failure", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("permanent failure")}, false},
		{"EOF", io.EOF, true},
		{"truncated body", io.ErrUnexpectedEOF, true},
		{"request timeout", newProviderHTTPError(408, nil), true},
		{"conflict", newProviderHTTPError(409, nil), true},
		{"rate limit", newProviderHTTPError(429, []byte(`{"error":{"code":"rate_limit_exceeded"}}`)), true},
		{"overload", newProviderHTTPError(529, nil), true},
		{"server error", newProviderHTTPError(503, nil), true},
		{"bad request", newProviderHTTPError(400, nil), false},
		{"authentication", newProviderHTTPError(401, nil), false},
		{"permission", newProviderHTTPError(403, nil), false},
		{"context limit", newProviderHTTPError(429, []byte(`{"error":{"code":"context_length_exceeded"}}`)), false},
		{"canceled", context.Canceled, false},
		{"invalid response", errors.New("invalid response JSON"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			result, err := withProviderRetry(context.Background(), providerRetryPolicy{maxRetries: 2}, nil, func(context.Context, []llms.CallOption) (string, error) {
				attempts++
				if attempts == 1 {
					return "", tt.err
				}
				return "recovered", nil
			})
			if tt.retry {
				if err != nil || attempts != 2 || result != "recovered" {
					t.Fatalf("result=%q err=%v attempts=%d", result, err, attempts)
				}
			} else if !errors.Is(err, tt.err) || attempts != 1 {
				t.Fatalf("err=%v attempts=%d, want original error after one attempt", err, attempts)
			}
		})
	}
}

func TestProviderRetryRefreshesAttemptTimeout(t *testing.T) {
	parent := context.Background()
	attempts := 0
	var firstContext context.Context
	result, err := withProviderRetry(parent, providerRetryPolicy{maxRetries: 1, requestTimeout: 10 * time.Millisecond}, nil, func(ctx context.Context, _ []llms.CallOption) (string, error) {
		attempts++
		if attempts == 1 {
			firstContext = ctx
			<-ctx.Done()
			return "", fmt.Errorf("read response body: %w", ctx.Err())
		}
		if ctx == firstContext || ctx.Err() != nil || parent.Err() != nil {
			t.Fatal("retry did not receive a fresh attempt context")
		}
		return "recovered", nil
	})
	if err != nil || result != "recovered" || attempts != 2 {
		t.Fatalf("result=%q err=%v attempts=%d", result, err, attempts)
	}
}

func TestProviderRetryHonorsCallerLifetime(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := withProviderRetry(ctx, defaultProviderRetryPolicy(), nil, func(context.Context, []llms.CallOption) (string, error) {
			t.Fatal("called provider after cancellation")
			return "", nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("caller deadline during request", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		attempts := 0
		_, err := withProviderRetry(ctx, defaultProviderRetryPolicy(), nil, func(ctx context.Context, _ []llms.CallOption) (string, error) {
			attempts++
			<-ctx.Done()
			return "", ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) || attempts != 1 {
			t.Fatalf("err=%v attempts=%d", err, attempts)
		}
	})
	t.Run("Retry-After wait", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		attempts := 0
		client := &http.Client{Transport: &retryTransport{maxRetries: 5, wrapped: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"3600"}}, Body: io.NopCloser(strings.NewReader("rate limited")), Request: req}, nil
		})}}
		_, err := withProviderRetry(ctx, providerRetryPolicy{maxRetries: 2}, nil, func(ctx context.Context, _ []llms.CallOption) (string, error) {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
			resp, err := client.Do(req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			return "", newProviderHTTPError(resp.StatusCode, nil)
		})
		if !errors.Is(err, context.DeadlineExceeded) || attempts != 1 {
			t.Fatalf("err=%v attempts=%d, want deadline to interrupt Retry-After", err, attempts)
		}
	})
}

func TestProviderRetryGuardsStreamConsumers(t *testing.T) {
	for _, mode := range []string{"before output", "text", "reasoning", "reasoning content", "callback error"} {
		t.Run(mode, func(t *testing.T) {
			attempts := 0
			options := []llms.CallOption{
				llms.WithMaxTokens(99),
				llms.WithStreamingFunc(func(context.Context, []byte) error {
					if mode == "callback error" {
						return io.EOF
					}
					return nil
				}),
				llms.WithStreamingReasoningFunc(func(context.Context, []byte, []byte) error { return nil }),
			}
			_, err := withProviderRetry(context.Background(), providerRetryPolicy{maxRetries: 1, requestTimeout: time.Second}, options, func(ctx context.Context, options []llms.CallOption) (string, error) {
				attempts++
				if _, ok := ctx.Deadline(); ok {
					t.Fatal("streaming attempt received a default timeout")
				}
				opts := llms.CallOptions{}
				for _, option := range options {
					option(&opts)
				}
				if opts.MaxTokens != 99 {
					t.Fatal("call options were lost")
				}
				switch mode {
				case "text":
					_ = opts.StreamingFunc(ctx, []byte("hello"))
				case "reasoning":
					_ = opts.StreamingReasoningFunc(ctx, []byte("thinking"), nil)
				case "reasoning content":
					_ = opts.StreamingReasoningFunc(ctx, nil, []byte("hello"))
				case "callback error":
					return "", opts.StreamingFunc(ctx, nil)
				}
				return "", io.ErrUnexpectedEOF
			})
			want := 1
			if mode == "before output" {
				want = 2
			}
			if err == nil || attempts != want {
				t.Fatalf("err=%v attempts=%d, want %d", err, attempts, want)
			}
		})
	}
}

func TestProviderRetryHTTPIntegration(t *testing.T) {
	for _, protocol := range []struct {
		provider string
		apiMode  string
		response string
	}{
		{"openai", "", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`},
		{"openai", "responses", `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`},
		{"anthropic", "", `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`},
		{"gemini", "interactions", `{"id":"ix_1","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}]}`},
		{"ollama", "", `{"model":"test","message":{"role":"assistant","content":"ok"},"done":true}`},
	} {
		t.Run(protocol.provider+"/"+protocol.apiMode, func(t *testing.T) {
			for _, recover := range []bool{true, false} {
				t.Run(fmt.Sprint("recover=", recover), func(t *testing.T) {
					attempts := 0
					var firstBody string
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						attempts++
						body, _ := io.ReadAll(r.Body)
						if attempts == 1 {
							firstBody = string(body)
							if firstBody == "" {
								t.Error("empty provider request")
							}
						} else if string(body) != firstBody {
							t.Errorf("request changed on retry: %s", body)
						}
						w.Header().Set("Content-Type", "application/json")
						if attempts == 1 || !recover {
							w.WriteHeader(http.StatusTooManyRequests)
							if protocol.provider == "ollama" {
								_, _ = io.WriteString(w, `{"error":"rate limited"}`)
							} else {
								_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
							}
							return
						}
						_, _ = io.WriteString(w, protocol.response)
					}))
					defer server.Close()
					manager := NewModelManager(ModelConfig{Provider: protocol.provider, APIMode: protocol.apiMode, BaseURL: server.URL, Model: "test", APIKey: "test-key"}, ProxyConfig{})
					model, err := manager.build()
					if err != nil {
						t.Fatal(err)
					}
					result, err := withProviderRetry(context.Background(), providerRetryPolicy{maxRetries: 2, requestTimeout: time.Second}, nil, func(ctx context.Context, options []llms.CallOption) (*llms.ContentResponse, error) {
						return model.GenerateContent(ctx, []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hello")}, options...)
					})
					if recover {
						if err != nil || attempts != 2 || result.Choices[0].Content != "ok" {
							t.Fatalf("result=%#v err=%v attempts=%d", result, err, attempts)
						}
					} else if err == nil || !strings.Contains(err.Error(), "rate limited") || attempts != 3 {
						t.Fatalf("err=%v attempts=%d, want final provider error after shared budget", err, attempts)
					}
				})
			}
		})
	}
}

func TestProviderRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		header string
		want   time.Duration
	}{
		{"12", 12 * time.Second},
		{now.Add(time.Minute).Format(http.TimeFormat), time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"-1", 0},
		{"9223372036854775807", 0},
		{"invalid", 0},
	} {
		if got := providerRetryAfter(tt.header, now); got != tt.want {
			t.Errorf("Retry-After %q = %v, want %v", tt.header, got, tt.want)
		}
	}
}

type providerTimeoutBody struct{ ctx context.Context }

func (b providerTimeoutBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b providerTimeoutBody) Close() error { return nil }

func TestProviderRetryResponseBodyTimeout(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: &retryTransport{maxRetries: 5, wrapped: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"recovered"},"finish_reason":"stop"}]}`))
		if attempts == 1 {
			body = providerTimeoutBody{ctx: req.Context()}
		}
		return &http.Response{StatusCode: 200, Body: body, Request: req}, nil
	})}}
	model := newOpenAICompatibleModel("https://example.test", "test", "", client)
	result, err := withProviderRetry(context.Background(), providerRetryPolicy{maxRetries: 1, requestTimeout: 10 * time.Millisecond}, nil, func(ctx context.Context, options []llms.CallOption) (*llms.ContentResponse, error) {
		return model.GenerateContent(ctx, nil, options...)
	})
	if err != nil || attempts != 2 || result.Choices[0].Content != "recovered" {
		t.Fatalf("result=%#v err=%v attempts=%d", result, err, attempts)
	}
}

func TestProviderRetryAnthropicStreamBudget(t *testing.T) {
	for _, partialOutput := range []bool{false, true} {
		t.Run(fmt.Sprint("partialOutput=", partialOutput), func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				w.Header().Set("Content-Type", "text/event-stream")
				if partialOutput {
					_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n"+
						"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
						"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
				}
				_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n")
			}))
			defer server.Close()
			model := newAnthropicModel(server.URL, "test", "key", server.Client(), withAnthropicStreamRetry(5, 0))
			var output strings.Builder
			options := []llms.CallOption{llms.WithStreamingFunc(func(_ context.Context, chunk []byte) error {
				_, _ = output.Write(chunk)
				return nil
			})}
			_, err := withProviderRetry(context.Background(), providerRetryPolicy{maxRetries: 2}, options, func(ctx context.Context, options []llms.CallOption) (*llms.ContentResponse, error) {
				return model.GenerateContent(ctx, nil, options...)
			})
			wantAttempts := 3
			if partialOutput {
				wantAttempts = 1
				if output.String() != "hello" {
					t.Fatalf("output=%q", output.String())
				}
			}
			if err == nil || attempts != wantAttempts {
				t.Fatalf("err=%v attempts=%d, want %d", err, attempts, wantAttempts)
			}
		})
	}
}

type retryEntryPointModel struct {
	requestContextRecordingModel
	nativeCalled bool
	attempts     int
}

func (m *retryEntryPointModel) Call(ctx context.Context, _ string, _ ...llms.CallOption) (string, error) {
	return "ok", m.recordAttempt(ctx)
}

func (m *retryEntryPointModel) GenerateContent(ctx context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	return &llms.ContentResponse{}, m.recordAttempt(ctx)
}

func (m *retryEntryPointModel) GenerateContentFromMessageList(ctx context.Context, _ []messages.Message, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.nativeCalled = true
	return &llms.ContentResponse{}, m.recordAttempt(ctx)
}

func (m *retryEntryPointModel) recordAttempt(ctx context.Context) error {
	m.ctx = ctx
	m.attempts++
	if m.attempts == 1 {
		return io.EOF
	}
	return nil
}

func TestModelManagerRetryEntryPoints(t *testing.T) {
	for _, entry := range []string{"Call", "GenerateContent", "GenerateContentFromMessageList"} {
		t.Run(entry, func(t *testing.T) {
			model := &retryEntryPointModel{}
			manager := NewModelManager(ModelConfig{}, ProxyConfig{})
			manager.model = model
			var err error
			switch entry {
			case "Call":
				_, err = manager.Call(context.Background(), "hello")
			case "GenerateContent":
				_, err = manager.GenerateContent(context.Background(), nil)
			case "GenerateContentFromMessageList":
				_, err = manager.GenerateContentFromMessageList(context.Background(), nil)
				if !model.nativeCalled {
					t.Fatal("native transcript path was lost")
				}
			}
			if err != nil || model.attempts != 2 {
				t.Fatalf("entry point did not recover: err=%v attempts=%d", err, model.attempts)
			}
		})
	}
}
