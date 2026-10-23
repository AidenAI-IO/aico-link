package agent

import (
	"aico-agent/internal/agent/screen"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
)

// handleCoordinateDebug serves the normalized-coordinate debug page.
func (s *Server) handleCoordinateDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(coordinateDebugHTML))
}

type coordinateDebugTapRequest struct {
	X             int    `json:"x"`
	Y             int    `json:"y"`
	Type          string `json:"type"`
	CropBlackBars *bool  `json:"crop_black_bars,omitempty"`
}

type coordinateDebugTapResponse struct {
	OK         bool                                       `json:"ok"`
	Error      string                                     `json:"error,omitempty"`
	ActionType string                                     `json:"action_type,omitempty"`
	Screenshot *coordinateDebugPostActionScreenshotResult `json:"screenshot,omitempty"`
}

type coordinateDebugPostActionScreenshotResult struct {
	coordinateDebugScreenshotResult
	ActionOutput  string   `json:"action_output,omitempty"`
	ScreenStable  *bool    `json:"screen_stable,omitempty"`
	StableWaitMs  *int64   `json:"stable_wait_ms,omitempty"`
	ScreenChanged *bool    `json:"screen_changed,omitempty"`
	LastDiff      *float64 `json:"last_diff,omitempty"`
}

func writeCoordinateDebugTapError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(coordinateDebugTapResponse{
		OK:    false,
		Error: message,
	})
}

func (s *Server) handleCoordinateDebugTap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeCoordinateDebugTapError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if s.runtime == nil || s.runtime.toolSnapshot() == nil {
		writeCoordinateDebugTapError(w, http.StatusServiceUnavailable, "runtime not configured")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeCoordinateDebugTapError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	var req coordinateDebugTapRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeCoordinateDebugTapError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeCoordinateDebugTapError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if req.X < 0 || req.X > 1000 || req.Y < 0 || req.Y > 1000 {
		writeCoordinateDebugTapError(w, http.StatusBadRequest, "x and y must be in range 0-1000")
		return
	}

	options := coordinateDebugScreenshotOptions{CropBlackBars: true}
	if req.CropBlackBars != nil {
		options.CropBlackBars = *req.CropBlackBars
	}

	gestureType := req.Type
	switch gestureType {
	case "", "tap":
		gestureType = "tap"
	case "double_tap", "long_press":
	default:
		http.Error(w, `{"ok":false,"error":"unsupported tap type"}`, http.StatusBadRequest)
		return
	}

	tool, ok := s.runtime.toolSnapshot().Get("touch_gesture")
	if !ok {
		http.Error(w, `{"ok":false,"error":"touch_gesture tool unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	currentScreen := s.coordinateDebugScreen()
	mappingState := screen.ScreenMappingState{}
	if currentScreen != nil {
		mappingState = currentScreen.MappingState()
		defer currentScreen.RestoreMappingState(mappingState)
	}

	toolInput, err := json.Marshal(map[string]any{
		"type": gestureType,
		"point": map[string]int{
			"x": req.X,
			"y": req.Y,
		},
	})
	if err != nil {
		http.Error(w, `{"ok":false,"error":"failed to encode tap payload"}`, http.StatusInternalServerError)
		return
	}

	toolCtx, _ := WithToolError(r.Context())
	output, err := tool.Call(toolCtx, string(toolInput))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if te := ToolErrorFromContext(toolCtx); te != nil {
		http.Error(w, fmt.Sprintf(`{"ok":false,"error":%q}`, te.Message), httpStatusForToolError(te))
		return
	}
	if currentScreen != nil {
		currentScreen.RestoreMappingState(mappingState)
	}

	var actionResult postActionScreenshotResult
	if err := json.Unmarshal([]byte(output), &actionResult); err != nil {
		http.Error(w, fmt.Sprintf(`{"ok":false,"error":"invalid tool output: %s"}`, err), http.StatusInternalServerError)
		return
	}
	var screenshotPayload coordinateDebugPostActionScreenshotResult
	if options.CropBlackBars && actionResult.Data != "" && actionResult.Format != "" &&
		coordinateDebugScreenshotMatchesMapping(actionResult.screenshotResult, mappingState) {
		screenshotPayload = coordinateDebugPostActionScreenshotResult{
			coordinateDebugScreenshotResult: *s.coordinateDebugScreenshotResultFromScreenState(actionResult.screenshotResult),
			ActionOutput:                    actionResult.ActionOutput,
			ScreenStable:                    actionResult.ScreenStable,
			StableWaitMs:                    actionResult.StableWaitMs,
			LastDiff:                        actionResult.LastDiff,
		}
	} else {
		screenshotResult, err := s.captureCoordinateDebugScreenshotResult(options)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"ok":false,"error":"post-action screenshot failed: %s"}`, err), http.StatusInternalServerError)
			return
		}
		screenshotPayload = coordinateDebugPostActionScreenshotResult{
			coordinateDebugScreenshotResult: *screenshotResult,
			ActionOutput:                    actionResult.ActionOutput,
			ScreenStable:                    actionResult.ScreenStable,
			StableWaitMs:                    actionResult.StableWaitMs,
			LastDiff:                        actionResult.LastDiff,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(coordinateDebugTapResponse{
		OK:         true,
		ActionType: gestureType,
		Screenshot: &screenshotPayload,
	})
}

func httpStatusForToolError(te *ToolError) int {
	if te == nil {
		return http.StatusInternalServerError
	}
	switch te.Category {
	case CategoryInvalidInput, CategoryUnsupported:
		return http.StatusBadRequest
	case CategoryPreconditionFailed, CategoryUserActionRequired, CategoryTransient:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
