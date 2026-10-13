package mnk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"aiden-agent/internal/logging"
)

// HTTPHandler handles HTTP requests for MNK operations
type HTTPHandler struct {
	provider     Provider
	textProvider TextProvider
}

// NewHTTPHandler creates a new HTTP handler for MNK operations
func NewHTTPHandler(provider Provider, textProvider ...TextProvider) *HTTPHandler {
	h := &HTTPHandler{provider: provider}
	h.textProvider, _ = provider.(TextProvider)
	if len(textProvider) > 0 {
		h.textProvider = textProvider[0]
	}
	return h
}

// ServeHTTP handles HTTP requests for MNK operations
// POST /api/providers/mnk
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only accept POST requests
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, fmt.Sprintf("read request body: %v", err))
		return
	}
	defer r.Body.Close()

	// Parse request
	var req MNKRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, fmt.Sprintf("parse request: %v", err))
		return
	}

	// Log request (optional, can be controlled by log level)
	taskID := r.Header.Get(BenchmarkTaskIDHeader)
	if taskID != "" {
		logging.Infof("agent", "mnk", "Task %s: %s", taskID, req.Operation)
	}

	// Execute operation
	var execErr error
	reqCtx := r.Context()
	switch req.Operation {
	case "click":
		execErr = h.handleClick(reqCtx, req.Click)
	case "double_click":
		execErr = h.handleDoubleClick(reqCtx, req.DoubleClick)
	case "swipe":
		execErr = h.handleSwipe(reqCtx, req.Swipe)
	case "drag_start":
		execErr = h.handleDragStart(reqCtx, req.DragStart)
	case "drag_release":
		execErr = h.handleDragRelease(reqCtx, req.DragRelease)
	case "keypress":
		execErr = h.handleKeypress(reqCtx, req.Keypress)
	case "keyboard_text":
		execErr = h.handleKeyboardText(reqCtx, req.KeyboardText)
	case "move":
		execErr = h.handleMove(reqCtx, req.Move)
	case "scroll":
		execErr = h.handleScroll(reqCtx, req.Scroll)
	case "touch_actions":
		execErr = h.handleTouchActions(reqCtx, req.TouchActions)
	default:
		h.writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown operation: %q", req.Operation))
		return
	}

	// Handle execution error with proper status code mapping
	if execErr != nil {
		statusCode := http.StatusInternalServerError
		// Map typed errors to appropriate status codes
		if e := AsError(execErr); e != nil && (e.Kind == ErrInvalidArguments || e.Kind == ErrModuleUnavailable) {
			statusCode = http.StatusBadRequest
		}
		h.writeError(w, statusCode, fmt.Sprintf("execute operation: %v", execErr))
		return
	}

	// Write success response
	h.writeSuccess(w)
}

func (h *HTTPHandler) handleKeyboardText(ctx context.Context, params *KeyboardTextParams) error {
	if params == nil || params.Text == "" {
		return InvalidArguments("keyboard_text requires non-empty ASCII text")
	}
	for _, c := range params.Text {
		if c > 127 {
			return InvalidArguments("keyboard_text requires ASCII text")
		}
	}
	if h.textProvider == nil {
		return ModuleUnavailable("keyboard_text is not supported by this provider")
	}
	return h.textProvider.KeyboardText(ctx, params.Text)
}

func (h *HTTPHandler) handleTouchActions(ctx context.Context, actions []TouchAction) error {
	if len(actions) == 0 {
		return InvalidArguments("touch_actions must contain at least one action")
	}
	atomic, ok := h.provider.(TouchActionProvider)
	if !ok {
		return ModuleUnavailable("atomic touch actions are not supported by this provider")
	}
	return atomic.TouchActions(ctx, actions)
}

func (h *HTTPHandler) handleClick(ctx context.Context, params *ClickParams) error {
	if params == nil {
		return InvalidArguments("click params required")
	}
	return h.provider.Click(ctx, params.X, params.Y, params.Button, params.HoldMs)
}

func (h *HTTPHandler) handleDoubleClick(ctx context.Context, params *DoubleClickParams) error {
	if params == nil {
		return InvalidArguments("double_click params required")
	}
	return h.provider.DoubleClick(ctx, params.X, params.Y, params.Button)
}

func (h *HTTPHandler) handleSwipe(ctx context.Context, params *SwipeParams) error {
	if params == nil {
		return InvalidArguments("swipe params required")
	}
	if len(params.Path) < 2 {
		return InvalidArguments("swipe path must contain at least 2 points")
	}
	options := SwipeOptions{
		DurationMs:   params.DurationMs,
		HoldBeforeMs: params.HoldBeforeMs,
		HoldAfterMs:  params.HoldAfterMs,
		Steps:        params.Steps,
	}
	if err := validateSwipeOptions(options); err != nil {
		return err
	}
	return swipeWithOptions(ctx, h.provider, params.Path, params.Button, options)
}

func validateSwipeOptions(options SwipeOptions) error {
	if options.DurationMs < 0 || options.DurationMs > MaxSwipeDurationMs {
		return InvalidArgumentsf("duration_ms must be in range [0, %d]", MaxSwipeDurationMs)
	}
	if options.HoldBeforeMs < 0 || options.HoldBeforeMs > MaxSwipeHoldMs {
		return InvalidArgumentsf("hold_before_ms must be in range [0, %d]", MaxSwipeHoldMs)
	}
	if options.HoldAfterMs < 0 || options.HoldAfterMs > MaxSwipeHoldMs {
		return InvalidArgumentsf("hold_after_ms must be in range [0, %d]", MaxSwipeHoldMs)
	}
	if options.Steps < 0 || options.Steps > MaxSwipeSteps {
		return InvalidArgumentsf("steps must be in range [0, %d]", MaxSwipeSteps)
	}
	return nil
}

func (h *HTTPHandler) handleDragStart(ctx context.Context, params *DragPointParams) error {
	if params == nil {
		return InvalidArguments("drag_start params required")
	}
	return h.provider.DragStart(ctx, params.X, params.Y, params.Button)
}

func (h *HTTPHandler) handleDragRelease(ctx context.Context, params *DragPointParams) error {
	if params == nil {
		return InvalidArguments("drag_release params required")
	}
	return h.provider.DragRelease(ctx, params.X, params.Y)
}

func (h *HTTPHandler) handleKeypress(ctx context.Context, params *KeypressParams) error {
	if params == nil {
		return InvalidArguments("keypress params required")
	}
	if len(params.Keys) == 0 {
		return InvalidArguments("keys array must not be empty")
	}
	return h.provider.Keypress(ctx, params.Keys)
}

func (h *HTTPHandler) handleMove(ctx context.Context, params *MoveParams) error {
	if params == nil {
		return InvalidArguments("move params required")
	}
	return h.provider.Move(ctx, params.X, params.Y)
}

func (h *HTTPHandler) handleScroll(ctx context.Context, params *ScrollParams) error {
	if params == nil {
		return InvalidArguments("scroll params required")
	}
	return h.provider.Scroll(ctx, params.ScrollX, params.ScrollY)
}

func (h *HTTPHandler) writeSuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(MNKResponse{
		Success: true,
	})
}

func (h *HTTPHandler) writeError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(MNKErrorResponse{
		Error: message,
	})
}

// RegisterHandler registers the MNK HTTP handler with a ServeMux
func RegisterHandler(mux *http.ServeMux, provider Provider) {
	handler := NewHTTPHandler(provider)
	mux.Handle("/api/providers/mnk", handler)
}

// RegisterHandlerWithPath registers the MNK HTTP handler with a custom path
func RegisterHandlerWithPath(mux *http.ServeMux, path string, provider Provider) {
	handler := NewHTTPHandler(provider)
	mux.Handle(path, handler)
}
