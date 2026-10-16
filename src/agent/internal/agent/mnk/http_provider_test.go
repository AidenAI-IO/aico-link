package mnk

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type recordingTextProvider struct {
	Provider
	text string
}

func (p *recordingTextProvider) KeyboardText(ctx context.Context, text string) error {
	p.text = text
	return nil
}

func TestHTTPHandlerKeyboardTextProviderFallback(t *testing.T) {
	for _, mode := range []string{"default", "nil", "override"} {
		t.Run(mode, func(t *testing.T) {
			provider := &recordingTextProvider{Provider: NewMockProvider()}
			override := &recordingTextProvider{}
			handler := NewHTTPHandler(provider)
			if mode == "nil" {
				handler = NewHTTPHandler(provider, nil)
			} else if mode == "override" {
				handler = NewHTTPHandler(provider, override)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/providers/mnk",
				bytes.NewBufferString(`{"operation":"keyboard_text","keyboard_text":{"text":"hello"}}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("expected success, got %d: %s", response.Code, response.Body.String())
			}
			if mode == "override" {
				if override.text != "hello" || provider.text != "" {
					t.Fatalf("unexpected routing: default=%q override=%q", provider.text, override.text)
				}
			} else if provider.text != "hello" {
				t.Fatalf("default provider received %q", provider.text)
			}
		})
	}
}

// TestHTTPProvider tests the HTTP provider implementation
func TestHTTPProvider(t *testing.T) {
	// Create a mock provider to handle requests
	mockProvider := NewMockProvider()

	// Create HTTP handler
	handler := NewHTTPHandler(mockProvider)

	// Create test server
	server := httptest.NewServer(handler)
	defer server.Close()

	// Create HTTP provider pointing to test server
	httpProvider := NewHTTPProvider(HTTPProviderConfig{
		BaseURL: server.URL,
	})

	t.Run("click", func(t *testing.T) {
		mockProvider.Reset()

		err := httpProvider.Click(context.Background(), 500, 500, "left", 0)
		if err != nil {
			t.Fatalf("Click failed: %v", err)
		}

		if len(mockProvider.clicks) != 1 {
			t.Fatalf("expected 1 click, got %d", len(mockProvider.clicks))
		}

		click := mockProvider.clicks[0]
		if click.X != 500 || click.Y != 500 {
			t.Errorf("expected (500, 500), got (%.0f, %.0f)", click.X, click.Y)
		}
		if click.Button != "left" {
			t.Errorf("expected button 'left', got %q", click.Button)
		}
	})

	t.Run("long_press", func(t *testing.T) {
		mockProvider.Reset()

		err := httpProvider.Click(context.Background(), 500, 500, "left", 500)
		if err != nil {
			t.Fatalf("Long press failed: %v", err)
		}

		if len(mockProvider.clicks) != 1 {
			t.Fatalf("expected 1 click, got %d", len(mockProvider.clicks))
		}

		if mockProvider.clicks[0].HoldMs != 500 {
			t.Errorf("expected holdMs=500, got %d", mockProvider.clicks[0].HoldMs)
		}
	})

	t.Run("double_click", func(t *testing.T) {
		mockProvider.Reset()

		err := httpProvider.DoubleClick(context.Background(), 300, 400, "left")
		if err != nil {
			t.Fatalf("DoubleClick failed: %v", err)
		}

		if len(mockProvider.doubleClicks) != 1 {
			t.Fatalf("expected 1 double click, got %d", len(mockProvider.doubleClicks))
		}

		dc := mockProvider.doubleClicks[0]
		if dc.X != 300 || dc.Y != 400 {
			t.Errorf("expected (300, 400), got (%.0f, %.0f)", dc.X, dc.Y)
		}
	})

	t.Run("swipe", func(t *testing.T) {
		mockProvider.Reset()

		path := [][2]float64{{700, 500}, {300, 500}}
		if err := httpProvider.Swipe(context.Background(), path, "left"); err != nil {
			t.Fatalf("Swipe failed: %v", err)
		}
		if len(mockProvider.swipes) != 1 {
			t.Fatalf("expected 1 swipe, got %d", len(mockProvider.swipes))
		}
		if mockProvider.swipes[0].Path[0] != path[0] || mockProvider.swipes[0].Path[1] != path[1] {
			t.Fatalf("swipe path = %#v, want %#v", mockProvider.swipes[0].Path, path)
		}
		if mockProvider.swipes[0].DurationMs != defaultSwipeGestureDurationMs {
			t.Fatalf("swipe duration = %d, want %d", mockProvider.swipes[0].DurationMs, defaultSwipeGestureDurationMs)
		}
	})

	t.Run("timed_swipe", func(t *testing.T) {
		mockProvider.Reset()

		path := [][2]float64{{500, 800}, {500, 200}}
		if err := httpProvider.SwipeWithDuration(context.Background(), path, "left", 240); err != nil {
			t.Fatalf("SwipeWithDuration failed: %v", err)
		}
		if len(mockProvider.swipes) != 1 || mockProvider.swipes[0].DurationMs != 240 {
			t.Fatalf("swipes = %#v, want one 240ms swipe", mockProvider.swipes)
		}
	})

	t.Run("swipe_options", func(t *testing.T) {
		mockProvider.Reset()

		path := [][2]float64{{500, 800}, {500, 200}}
		if err := httpProvider.SwipeWithOptions(context.Background(), path, "left", SwipeOptions{
			DurationMs:   240,
			HoldBeforeMs: 120,
			HoldAfterMs:  80,
			Steps:        12,
		}); err != nil {
			t.Fatalf("SwipeWithOptions failed: %v", err)
		}
		if len(mockProvider.swipes) != 1 {
			t.Fatalf("swipes = %#v, want one swipe", mockProvider.swipes)
		}
		swipe := mockProvider.swipes[0]
		if swipe.DurationMs != 240 || swipe.HoldBeforeMs != 120 || swipe.HoldAfterMs != 80 || swipe.Steps != 12 {
			t.Fatalf("swipe options = %+v, want duration=240 before=120 after=80 steps=12", swipe)
		}
	})

	t.Run("drag_start_release", func(t *testing.T) {
		mockProvider.Reset()

		if err := httpProvider.DragStart(context.Background(), 100, 500, "left"); err != nil {
			t.Fatalf("DragStart failed: %v", err)
		}
		if len(mockProvider.dragStarts) != 1 || mockProvider.dragStarts[0].X != 100 || mockProvider.dragStarts[0].Y != 500 {
			t.Fatalf("drag starts = %#v, want point 100,500", mockProvider.dragStarts)
		}
		if err := httpProvider.DragRelease(context.Background(), 900, 500); err != nil {
			t.Fatalf("DragRelease failed: %v", err)
		}
		if len(mockProvider.dragReleases) != 1 || mockProvider.dragReleases[0].X != 900 || mockProvider.dragReleases[0].Y != 500 {
			t.Fatalf("drag releases = %#v, want point 900,500", mockProvider.dragReleases)
		}
	})

	t.Run("keypress", func(t *testing.T) {
		mockProvider.Reset()

		err := httpProvider.Keypress(context.Background(), []string{"ctrl", "a"})
		if err != nil {
			t.Fatalf("Keypress failed: %v", err)
		}

		if len(mockProvider.keypresses) != 1 {
			t.Fatalf("expected 1 keypress, got %d", len(mockProvider.keypresses))
		}

		kp := mockProvider.keypresses[0]
		if len(kp.Keys) != 2 || kp.Keys[0] != "ctrl" || kp.Keys[1] != "a" {
			t.Errorf("expected [ctrl, a], got %v", kp.Keys)
		}
	})

	t.Run("move", func(t *testing.T) {
		mockProvider.Reset()

		err := httpProvider.Move(context.Background(), 250, 750)
		if err != nil {
			t.Fatalf("Move failed: %v", err)
		}

		if len(mockProvider.moves) != 1 {
			t.Fatalf("expected 1 move, got %d", len(mockProvider.moves))
		}

		move := mockProvider.moves[0]
		if move.X != 250 || move.Y != 750 {
			t.Errorf("expected (250, 750), got (%.0f, %.0f)", move.X, move.Y)
		}
	})

	t.Run("scroll", func(t *testing.T) {
		mockProvider.Reset()

		err := httpProvider.Scroll(context.Background(), 0, -3)
		if err != nil {
			t.Fatalf("Scroll failed: %v", err)
		}

		if len(mockProvider.scrolls) != 1 {
			t.Fatalf("expected 1 scroll, got %d", len(mockProvider.scrolls))
		}

		scroll := mockProvider.scrolls[0]
		if scroll.ScrollX != 0 || scroll.ScrollY != -3 {
			t.Errorf("expected (0, -3), got (%d, %d)", scroll.ScrollX, scroll.ScrollY)
		}
	})

	t.Run("touch_actions", func(t *testing.T) {
		mockProvider.Reset()
		actions := []TouchAction{
			{Type: "touch_down", Point: &Point{X: 500, Y: 700}},
			{Type: "wait", DurationMs: 25},
			{Type: "move_to", Point: &Point{X: 500, Y: 300}},
			{Type: "touch_up"},
		}
		if err := httpProvider.TouchActions(context.Background(), actions); err != nil {
			t.Fatalf("TouchActions failed: %v", err)
		}
		got := mockProvider.TouchActionCalls()
		if len(got) != len(actions) || got[0].Type != "touch_down" || got[1].DurationMs != 25 || got[3].Type != "touch_up" {
			t.Fatalf("touch actions = %#v, want %#v", got, actions)
		}
	})
}

// TestHTTPHandlerErrors tests error handling in HTTP handler
func TestHTTPHandlerErrors(t *testing.T) {
	mockProvider := NewMockProvider()
	handler := NewHTTPHandler(mockProvider)
	server := httptest.NewServer(handler)
	defer server.Close()

	httpProvider := NewHTTPProvider(HTTPProviderConfig{
		BaseURL: server.URL,
	})

	t.Run("drag_release_without_start", func(t *testing.T) {
		mockProvider.Reset()
		err := httpProvider.DragRelease(context.Background(), 500, 500)
		if err == nil {
			t.Error("expected error without drag_start, got nil")
		}
	})

	t.Run("duplicate_drag_start", func(t *testing.T) {
		mockProvider.Reset()
		if err := httpProvider.DragStart(context.Background(), 500, 500, "left"); err != nil {
			t.Fatalf("first DragStart failed: %v", err)
		}
		err := httpProvider.DragStart(context.Background(), 600, 600, "left")
		if err == nil {
			t.Error("expected error for duplicate drag_start, got nil")
		}
	})

	t.Run("empty_keys", func(t *testing.T) {
		mockProvider.Reset()

		// Empty keys should fail
		err := httpProvider.Keypress(context.Background(), []string{})
		if err == nil {
			t.Error("expected error for empty keys, got nil")
		}
	})
}

// TestHTTPHandlerMethodNotAllowed tests method validation
func TestHTTPHandlerMethodNotAllowed(t *testing.T) {
	mockProvider := NewMockProvider()
	handler := NewHTTPHandler(mockProvider)

	req := httptest.NewRequest(http.MethodGet, "/api/providers/mnk", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

// TestHTTPHandlerInvalidJSON tests invalid JSON handling
func TestHTTPHandlerInvalidJSON(t *testing.T) {
	mockProvider := NewMockProvider()
	handler := NewHTTPHandler(mockProvider)

	req := httptest.NewRequest(http.MethodPost, "/api/providers/mnk",
		http.NoBody)
	req.Body = http.NoBody
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

// TestHTTPHandlerUnknownOperation tests unknown operation handling
func TestHTTPHandlerUnknownOperation(t *testing.T) {
	mockProvider := NewMockProvider()
	handler := NewHTTPHandler(mockProvider)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Create a request with unknown operation
	reqBody := MNKRequest{
		Operation: "unknown_operation",
	}

	client := &http.Client{}
	reqJSON, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/providers/mnk",
		bytes.NewReader(reqJSON))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, resp.StatusCode)
	}
}

// TestHTTPProviderTimeout tests timeout handling
func TestHTTPProviderTimeout(t *testing.T) {
	// Create a slow server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	// Create HTTP provider with short timeout
	httpProvider := NewHTTPProvider(HTTPProviderConfig{
		BaseURL: server.URL,
		Timeout: 100 * time.Millisecond,
	})

	err := httpProvider.Click(context.Background(), 500, 500, "left", 0)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

// TestRegisterHandler tests handler registration
func TestRegisterHandler(t *testing.T) {
	mockProvider := NewMockProvider()
	mux := http.NewServeMux()

	RegisterHandler(mux, mockProvider)

	server := httptest.NewServer(mux)
	defer server.Close()

	httpProvider := NewHTTPProvider(HTTPProviderConfig{
		BaseURL: server.URL,
	})

	err := httpProvider.Click(context.Background(), 500, 500, "left", 0)
	if err != nil {
		t.Fatalf("Click through registered handler failed: %v", err)
	}

	if len(mockProvider.clicks) != 1 {
		t.Errorf("expected 1 click, got %d", len(mockProvider.clicks))
	}
}

// TestTaskIDHeader tests task ID header forwarding
func TestTaskIDHeader(t *testing.T) {
	mockProvider := NewMockProvider()

	var receivedTaskID string
	var receivedPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTaskID = r.Header.Get(BenchmarkTaskIDHeader)
		receivedPath = r.URL.Path

		// Delegate to actual handler
		h := NewHTTPHandler(mockProvider)
		h.ServeHTTP(w, r)
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	httpProvider := NewHTTPProvider(HTTPProviderConfig{
		BaseURL: "  " + server.URL + "/  ",
		TaskID:  "  test-task-123  ",
	})

	err := httpProvider.Click(context.Background(), 500, 500, "left", 0)
	if err != nil {
		t.Fatalf("Click failed: %v", err)
	}

	if receivedTaskID != "test-task-123" {
		t.Errorf("expected task ID 'test-task-123', got %q", receivedTaskID)
	}
	if receivedPath != "/api/providers/mnk" {
		t.Errorf("expected MNK provider path, got %q", receivedPath)
	}
}
