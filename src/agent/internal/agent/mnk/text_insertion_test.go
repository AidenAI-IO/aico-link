package mnk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type insertionSink struct {
	Provider
	text    string
	replace bool
}

func (p *insertionSink) SupportsTextInsertion(context.Context) (bool, error) { return true, nil }
func (p *insertionSink) InsertText(_ context.Context, text string, replace bool) error {
	p.text, p.replace = text, replace
	return nil
}

func TestNativeTextHTTPForwarding(t *testing.T) {
	sink := &insertionSink{Provider: NewMockProvider()}
	handler := NewHTTPHandler(sink)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(BenchmarkTaskIDHeader) != "mirror" {
			t.Error("capability and insertion calls must retain task routing")
		}
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	forward := NewHTTPProvider(HTTPProviderConfig{BaseURL: remote.URL, TaskID: "mirror"})
	proxy := httptest.NewServer(NewHTTPHandler(forward))
	defer proxy.Close()
	client := NewHTTPProvider(HTTPProviderConfig{BaseURL: proxy.URL})
	ctx := context.Background()
	if supported, err := client.SupportsTextInsertion(ctx); err != nil || !supported {
		t.Fatalf("capability = %t, %v", supported, err)
	}
	const text = "中文 ABC🔋\n第二行"
	if err := client.InsertText(ctx, text, true); err != nil {
		t.Fatal(err)
	}
	if sink.text != text || !sink.replace {
		t.Fatalf("received %q replace=%t", sink.text, sink.replace)
	}
	// The old primitive remains ASCII-only, even for a Unicode-capable bridge.
	if err := client.KeyboardText(ctx, "中文"); err == nil {
		t.Fatal("keyboard_text accepted Unicode")
	}
}

func TestNativeTextCapabilityCompatibility(t *testing.T) {
	for _, status := range []int{200, 404, 405, 501, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{"capabilities": map[string]any{}})
			}))
			defer server.Close()
			p := NewHTTPProvider(HTTPProviderConfig{BaseURL: server.URL})
			supported, err := p.SupportsTextInsertion(context.Background())
			if supported || (err != nil) != (status == 503) {
				t.Fatalf("status %d: supported=%t err=%v", status, supported, err)
			}
		})
	}
}

func TestNativeTextValidationBeforeDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid text reached input backend")
	}))
	defer server.Close()
	p := NewHTTPProvider(HTTPProviderConfig{BaseURL: server.URL})
	for _, value := range []string{"", "\x00", "\r", "\x7f", "\u0085", string([]byte{0xff}), strings.Repeat("🔋", 2049)} {
		if err := p.InsertText(context.Background(), value, true); err == nil {
			t.Fatal("invalid text was accepted")
		}
	}
}
