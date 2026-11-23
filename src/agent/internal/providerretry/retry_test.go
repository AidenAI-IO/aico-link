package providerretry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDoFreshAttemptTimeoutAndNestedBudget(t *testing.T) {
	calls := 0
	started := time.Now()
	_, err := Do(context.Background(), Policy{MaxRetries: 2, Timeout: 20 * time.Millisecond}, func(ctx context.Context) (string, error) {
		return Do(ctx, VoicePolicy(), func(ctx context.Context) (string, error) {
			calls++
			if calls < 3 {
				<-ctx.Done()
				return "", ctx.Err()
			}
			return "ok", nil
		})
	})
	if err != nil || calls != 3 || time.Since(started) < 40*time.Millisecond {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestDoCancellationInterruptsRetryAfter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	calls := 0
	_, err := Do(ctx, VoicePolicy(), func(context.Context) (string, error) { calls++; return "", &StatusError{Status: 429, After: time.Hour} })
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestStructuredErrorsDoNotRetryPermanentFailures(t *testing.T) {
	for _, tt := range []struct {
		body  string
		retry bool
	}{
		{`{"error":{"code":429}}`, true},
		{`{"error":{"status":"UNAVAILABLE"}}`, true},
		{`{"error":{"type":"rate_limit_error"}}`, true},
		{`{"error":{"code":"invalid_api_key","message":"retry 429 timeout"}}`, false},
		{`{"error":{"type":"invalid_request_error"}}`, false},
	} {
		err := &StatusError{Status: JSONStatus([]byte(tt.body)), Body: tt.body}
		if got := Retryable(err); got != tt.retry {
			t.Errorf("%s retry=%v", tt.body, got)
		}
	}
}

func TestStreamTimeoutBoundsHeadersAndReads(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "body" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			req, _ := http.NewRequest("GET", server.URL, nil)
			resp, err := RoundTrip(http.DefaultTransport, req, 30*time.Millisecond)
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestStreamActivityOutlivesTimeoutAndConsumerPause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 5; i++ {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL, nil)
	resp, err := RoundTrip(http.DefaultTransport, req, 120*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	one := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, one); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // A slow consumer must not kill a healthy stream.
	rest, err := io.ReadAll(resp.Body)
	if err != nil || len(rest) != 4 {
		t.Fatalf("body=%q err=%v", rest, err)
	}
}

func TestHTTPReplaysBodyAndKeepsSuccessfulResponseReadable(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "mint" {
			t.Errorf("body=%q", body)
		}
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte("credentials"))
	}))
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL, strings.NewReader("mint"))
	resp, err := HTTP(context.Background(), server.Client(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "credentials" || calls != 2 {
		t.Fatalf("body=%q calls=%d err=%v", body, calls, err)
	}
}

func TestNestedOperationKeepsTimeoutWithoutExtraAttempts(t *testing.T) {
	calls := 0
	_, err := Do(context.Background(), Policy{MaxRetries: 1}, func(ctx context.Context) (string, error) {
		return Do(ctx, Policy{MaxRetries: 5, Timeout: 20 * time.Millisecond}, func(ctx context.Context) (string, error) {
			calls++
			if calls == 1 {
				<-ctx.Done()
				return "", ctx.Err()
			}
			return "ok", nil
		})
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
