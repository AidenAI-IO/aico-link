package mnk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const BenchmarkTaskIDHeader = "benchmark-task-id"

// HTTPProvider implements Provider interface by forwarding operations to a remote HTTP server.
// This allows remote control and cross-process communication.
type HTTPProvider struct {
	baseURL    string
	httpClient *http.Client
	taskID     string // Optional task ID for request tracking
}

// HTTPProviderConfig configuration for HTTP Provider
type HTTPProviderConfig struct {
	BaseURL string        // Base URL of the MNK HTTP server
	Timeout time.Duration // HTTP request timeout
	TaskID  string        // Optional task ID for request tracking
}

// NewHTTPProvider creates a new HTTP-based MNK provider
func NewHTTPProvider(config HTTPProviderConfig) *HTTPProvider {
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}

	return &HTTPProvider{
		baseURL: strings.TrimRight(strings.TrimSpace(config.BaseURL), "/"),
		httpClient: &http.Client{
			Timeout: config.Timeout,
		},
		taskID: strings.TrimSpace(config.TaskID),
	}
}

// Click performs a click operation via HTTP
func (p *HTTPProvider) Click(ctx context.Context, x, y float64, button string, holdMs int) error {
	req := MNKRequest{
		Operation: "click",
		Click: &ClickParams{
			X:      x,
			Y:      y,
			Button: button,
			HoldMs: holdMs,
		},
	}
	return p.sendRequest(ctx, req)
}

// DoubleClick performs a double-click operation via HTTP
func (p *HTTPProvider) DoubleClick(ctx context.Context, x, y float64, button string) error {
	req := MNKRequest{
		Operation: "double_click",
		DoubleClick: &DoubleClickParams{
			X:      x,
			Y:      y,
			Button: button,
		},
	}
	return p.sendRequest(ctx, req)
}

// Swipe performs a swipe operation via HTTP.
func (p *HTTPProvider) Swipe(ctx context.Context, path [][2]float64, button string) error {
	return p.SwipeWithOptions(ctx, path, button, SwipeOptions{})
}

// SwipeWithDuration performs a swipe operation via HTTP with explicit timing.
func (p *HTTPProvider) SwipeWithDuration(ctx context.Context, path [][2]float64, button string, durationMs int) error {
	return p.SwipeWithOptions(ctx, path, button, SwipeOptions{DurationMs: durationMs})
}

// SwipeWithOptions performs a swipe operation via HTTP with explicit timing
// and interpolation settings.
func (p *HTTPProvider) SwipeWithOptions(ctx context.Context, path [][2]float64, button string, options SwipeOptions) error {
	req := MNKRequest{
		Operation: "swipe",
		Swipe: &SwipeParams{
			Path:         path,
			Button:       button,
			DurationMs:   options.DurationMs,
			HoldBeforeMs: options.HoldBeforeMs,
			HoldAfterMs:  options.HoldAfterMs,
			Steps:        options.Steps,
		},
	}
	return p.sendRequest(ctx, req)
}

// DragStart starts a persistent drag contact via HTTP.
func (p *HTTPProvider) DragStart(ctx context.Context, x, y float64, button string) error {
	req := MNKRequest{
		Operation: "drag_start",
		DragStart: &DragPointParams{
			X:      x,
			Y:      y,
			Button: button,
		},
	}
	return p.sendRequest(ctx, req)
}

// DragRelease moves and releases the active drag contact via HTTP.
func (p *HTTPProvider) DragRelease(ctx context.Context, x, y float64) error {
	return p.sendRequest(ctx, MNKRequest{
		Operation:   "drag_release",
		DragRelease: &DragPointParams{X: x, Y: y},
	})
}

// Keypress performs a keypress operation via HTTP
func (p *HTTPProvider) Keypress(ctx context.Context, keys []string) error {
	req := MNKRequest{
		Operation: "keypress",
		Keypress: &KeypressParams{
			Keys: keys,
		},
	}
	return p.sendRequest(ctx, req)
}

// KeyboardText sends an internal typing primitive, not an enter_text shortcut.
func (p *HTTPProvider) KeyboardText(ctx context.Context, text string) error {
	return p.sendRequest(ctx, MNKRequest{
		Operation:    "keyboard_text",
		KeyboardText: &KeyboardTextParams{Text: text},
	})
}

// Move performs a move operation via HTTP
func (p *HTTPProvider) Move(ctx context.Context, x, y float64) error {
	req := MNKRequest{
		Operation: "move",
		Move: &MoveParams{
			X: x,
			Y: y,
		},
	}
	return p.sendRequest(ctx, req)
}

// Scroll performs a scroll operation via HTTP
func (p *HTTPProvider) Scroll(ctx context.Context, scrollX, scrollY int) error {
	req := MNKRequest{
		Operation: "scroll",
		Scroll: &ScrollParams{
			ScrollX: scrollX,
			ScrollY: scrollY,
		},
	}
	return p.sendRequest(ctx, req)
}

// TouchActions forwards one atomic touch program so the remote provider can
// preserve contact state and execute all waits in a single request.
func (p *HTTPProvider) TouchActions(ctx context.Context, actions []TouchAction) error {
	if len(actions) == 0 {
		return InvalidArguments("touch actions must not be empty")
	}
	return p.sendRequest(ctx, MNKRequest{
		Operation:    "touch_actions",
		TouchActions: actions,
	})
}

// sendRequest sends an MNK request to the HTTP server
func (p *HTTPProvider) sendRequest(ctx context.Context, req MNKRequest) error {
	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	// Create HTTP request with caller's context
	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		p.baseURL+"/api/providers/mnk",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return fmt.Errorf("create http request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	if p.taskID != "" {
		httpReq.Header.Set(BenchmarkTaskIDHeader, p.taskID)
	}

	// Send request
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send http request: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	// Check status code
	if resp.StatusCode != http.StatusOK {
		var errResp MNKErrorResponse
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			return fmt.Errorf("mnk operation failed: %s", errResp.Error)
		}
		return fmt.Errorf("mnk operation failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse success response
	var successResp MNKResponse
	if err := json.Unmarshal(respBody, &successResp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if !successResp.Success {
		return fmt.Errorf("mnk operation failed: %s", successResp.Error)
	}

	return nil
}

// MNKRequest represents an MNK operation request
type MNKRequest struct {
	KeyboardText *KeyboardTextParams `json:"keyboard_text,omitempty"`
	Operation    string              `json:"operation"`
	Click        *ClickParams        `json:"click,omitempty"`
	DoubleClick  *DoubleClickParams  `json:"double_click,omitempty"`
	Swipe        *SwipeParams        `json:"swipe,omitempty"`
	DragStart    *DragPointParams    `json:"drag_start,omitempty"`
	DragRelease  *DragPointParams    `json:"drag_release,omitempty"`
	Keypress     *KeypressParams     `json:"keypress,omitempty"`
	Move         *MoveParams         `json:"move,omitempty"`
	Scroll       *ScrollParams       `json:"scroll,omitempty"`
	TouchActions []TouchAction       `json:"touch_actions,omitempty"`
}

// ClickParams parameters for click operation
type ClickParams struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Button string  `json:"button"`
	HoldMs int     `json:"hold_ms"`
}

// DoubleClickParams parameters for double-click operation
type DoubleClickParams struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Button string  `json:"button"`
}

// SwipeParams contains the path and timing options for a swipe operation.
type SwipeParams struct {
	Path         [][2]float64 `json:"path"`
	Button       string       `json:"button"`
	DurationMs   int          `json:"duration_ms,omitempty"`
	HoldBeforeMs int          `json:"hold_before_ms,omitempty"`
	HoldAfterMs  int          `json:"hold_after_ms,omitempty"`
	Steps        int          `json:"steps,omitempty"`
}

// DragPointParams contains one endpoint for a persistent drag session.
type DragPointParams struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Button string  `json:"button,omitempty"`
}

type KeyboardTextParams struct {
	Text string `json:"text"`
}

// KeypressParams parameters for keypress operation
type KeypressParams struct {
	Keys []string `json:"keys"`
}

// MoveParams parameters for move operation
type MoveParams struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// ScrollParams parameters for scroll operation
type ScrollParams struct {
	ScrollX int `json:"scroll_x"`
	ScrollY int `json:"scroll_y"`
}

// MNKResponse represents an MNK operation response
type MNKResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// MNKErrorResponse represents an error response
type MNKErrorResponse struct {
	Error string `json:"error"`
}
