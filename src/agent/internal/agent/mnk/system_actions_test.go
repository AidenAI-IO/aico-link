package mnk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type systemActionSink struct {
	Provider
	calls int
	err   error
}

func (p *systemActionSink) SupportsSystemAction(_ context.Context, action string) (bool, error) {
	return action == "home", nil
}

func (p *systemActionSink) SystemAction(_ context.Context, action string) error {
	p.calls++
	return p.err
}

func TestNativeSystemActionHTTPForwarding(t *testing.T) {
	sink := &systemActionSink{Provider: NewMockProvider()}
	handler := NewHTTPHandler(sink)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(BenchmarkTaskIDHeader) != "ios-device" {
			t.Error("native navigation lost task routing")
		}
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	forward := NewHTTPProvider(HTTPProviderConfig{BaseURL: remote.URL, TaskID: "ios-device"})
	proxy := httptest.NewServer(NewHTTPHandler(forward))
	defer proxy.Close()
	client := NewHTTPProvider(HTTPProviderConfig{BaseURL: proxy.URL})
	ctx := context.Background()
	if supported, err := client.SupportsSystemAction(ctx, "home"); err != nil || !supported {
		t.Fatalf("capability = %t, %v", supported, err)
	}
	if err := client.SystemAction(ctx, "home"); err != nil || sink.calls != 1 {
		t.Fatalf("Home = %v, calls = %d", err, sink.calls)
	}
	sink.err = errors.New("timeout: delivery is ambiguous")
	if err := client.SystemAction(ctx, "home"); err == nil || sink.calls != 2 {
		t.Fatalf("failed Home must not be replayed: %v, calls = %d", err, sink.calls)
	}
	if err := client.SystemAction(ctx, "power"); err == nil || sink.calls != 2 {
		t.Fatal("invalid action reached native input")
	}
}

func TestNativeSystemActionCapabilityCompatibility(t *testing.T) {
	for _, status := range []int{200, 404, 405, 501, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{"capabilities": map[string]any{"insert_text": true}})
			}))
			defer server.Close()
			client := NewHTTPProvider(HTTPProviderConfig{BaseURL: server.URL})
			supported, err := client.SupportsSystemAction(context.Background(), "home")
			if supported || (err != nil) != (status == 503) {
				t.Fatalf("status %d: supported=%t err=%v", status, supported, err)
			}
		})
	}
}
