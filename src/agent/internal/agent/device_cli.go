package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/devicecli"
	"aico-agent/internal/logging"
	"github.com/google/uuid"
	"github.com/tmc/langchaingo/llms"
)

type deviceCLIRuntimeKey struct{}
type deviceCLICaptureKey struct{}
type deviceCLIActivityKey struct{}

type deviceCLIToolEvidence struct {
	Tool          string         `json:"tool"`
	Input         map[string]any `json:"input"`
	IsError       bool           `json:"is_error"`
	DurationMs    *int64         `json:"duration_ms,omitempty"`
	NotDispatched bool           `json:"not_dispatched,omitempty"`
}

const maxDeviceCLIActions = 64

type deviceCLICapture struct {
	calls             []deviceCLIToolEvidence
	deferObservation  bool // foreground shell, not standalone HTTP
	needsScreenshot   bool
	keyError          *ToolError // first failed key request, even if shell masks its exit status
	guardDimensions   [2]int     // screenshot orientation used for coordinates in this shell
	frame             visualScreenshotObservation
	drag              compositeDrag
	activityReported  bool
	activitySucceeded bool
}

// Prefer the installed sibling on native launches; otherwise use the image PATH.
func deviceCLIPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	name := "device-cli"
	if strings.HasSuffix(exe, ".exe") {
		name += ".exe"
	}
	if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
		return dir
	}
	return ""
}

// A foreground shell owns a short-lived HTTP endpoint. The normal tool handler
// performs validation and execution, but the endpoint accepts only CLI commands
// associated with this shell, not arbitrary tool names or caller-supplied frames.
func openDeviceCLIScope(ctx context.Context, r *Runtime, capture *deviceCLICapture) (url, token string, closeScope func(), err error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, err
	}
	if width, height, ok := r.screenState.ScreenshotDimensions(); ok {
		capture.guardDimensions = [2]int{width, height}
	}
	token = uuid.NewString()
	toolServer := &Server{runtime: r, logger: r.logger}
	scopeCtx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	closed := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/command" || req.Header.Get("Authorization") != "Bearer "+token {
			http.NotFound(w, req)
			return
		}
		var args []string
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 128<<10)).Decode(&args); err != nil {
			http.Error(w, "invalid CLI arguments", 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if closed || scopeCtx.Err() != nil {
			http.Error(w, "shell ended", 410)
			return
		}
		callCtx, stop := context.WithCancel(scopeCtx)
		defer stop()
		stopOnDisconnect := context.AfterFunc(req.Context(), stop)
		defer stopOnDisconnect()
		response, err := executeDeviceCLI(callCtx, toolServer, args, capture)
		if err != nil {
			response = ToolInvokeResponse{IsError: true, Error: err.Error()}
		}
		if len(args) > 0 && args[0] == "key" && response.IsError && capture.keyError == nil {
			capture.keyError = response.ToolError
			if capture.keyError == nil {
				capture.keyError = NewToolError(CodeToolExecutionFailed, response.Error)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	return "http://" + listener.Addr().String(), token, func() { cancel(); _ = srv.Close(); mu.Lock(); closed = true; mu.Unlock() }, nil
}

// handleDeviceCLIDrag keeps both phases and their cleanup on the controller,
// even when the standalone CLI disconnects between starting and releasing.
func (s *Server) handleDeviceCLIDrag(w http.ResponseWriter, req *http.Request) {
	s.handleStandaloneDeviceCLI(w, req, func(args []string) bool {
		return len(args) == 5 && args[0] == "drag" || len(args) == 3 && args[0] == "drag-start"
	}, "Expected drag X1 Y1 X2 Y2 or drag-start X Y")
}

func (s *Server) handleDeviceCLIApp(w http.ResponseWriter, req *http.Request) {
	s.handleStandaloneDeviceCLI(w, req, func(args []string) bool {
		return len(args) >= 2 && args[0] == "app"
	}, "Expected an app command")
}

func (s *Server) handleStandaloneDeviceCLI(w http.ResponseWriter, req *http.Request, accepts func([]string) bool, usage string) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var originGuard http.CrossOriginProtection
	if err := originGuard.Check(req); err != nil {
		http.Error(w, "Cross-origin writes are not allowed", http.StatusForbidden)
		return
	}
	var args []string
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 128<<10)).Decode(&args); err != nil || !accepts(args) {
		http.Error(w, usage, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), httpToolExecutionTimeout)
	defer cancel()
	result, err := executeDeviceCLI(ctx, s, args, &deviceCLICapture{})
	if err != nil {
		result = ToolInvokeResponse{IsError: true, Error: err.Error()}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

type compositeDragKey struct{}
type compositeDrag struct{ release func(context.Context) error }

func (d *compositeDrag) cleanup(ctx context.Context) {
	if d.release == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := d.release(cleanupCtx); err != nil {
		logging.Warnf("agent", "device_cli", "drag cleanup failed: %v", err)
	}
}

func executeDeviceCLI(ctx context.Context, s *Server, args []string, capture *deviceCLICapture) (ToolInvokeResponse, error) {
	if len(args) > 0 && args[0] == "wait" {
		delay, err := devicecli.ParseWaitDuration(args[1:])
		if err != nil {
			return ToolInvokeResponse{IsError: true, Error: err.Error(), ToolError: NewToolError(CodeInvalidArguments, err.Error())}, nil
		}
		if len(capture.calls) >= maxDeviceCLIActions {
			return ToolInvokeResponse{}, fmt.Errorf("device-cli action limit per shell reached; continue in a new shell call")
		}
		if err := sleepWithContext(ctx, delay); err != nil {
			return ToolInvokeResponse{}, err
		}
		if err := ctx.Err(); err != nil {
			return ToolInvokeResponse{}, err
		}
		// Preserve wait 0 as a way to request a model-facing observation,
		// but leave all capture to the existing shell finalizer.
		capture.needsScreenshot = true
		capture.calls = append(capture.calls, deviceCLIToolEvidence{
			Tool: "device_cli_wait", Input: map[string]any{"milliseconds": delay.Milliseconds()},
		})
		return ToolInvokeResponse{}, nil
	}
	calls, err := devicecli.Calls(args)
	if err != nil {
		return ToolInvokeResponse{IsError: true, Error: err.Error(), ToolError: NewToolError(CodeInvalidArguments, err.Error())}, nil
	}
	scoped := context.WithValue(ctx, deviceCLICaptureKey{}, capture)
	if len(capture.calls)+len(calls) > maxDeviceCLIActions {
		return ToolInvokeResponse{}, fmt.Errorf("device-cli action limit per shell reached; continue in a new shell call")
	}
	if args[0] == "look" {
		// Return the raw screenshot only to the CLI process, which saves the file.
		return capture.invoke(scoped, s, calls[0])
	}
	if calls[0].AppData {
		// Data-only operations do not trigger a final capture.
		return capture.invoke(scoped, s, calls[0])
	}
	if capture.deferObservation {
		return capture.executeInShell(scoped, s, calls)
	}
	// Standalone HTTP drag retains its existing per-phase verification.
	drag := &compositeDrag{}
	scoped = context.WithValue(scoped, compositeDragKey{}, drag)
	defer drag.cleanup(scoped)
	var result ToolInvokeResponse
	for i, call := range calls {
		if err := scoped.Err(); err != nil {
			return result, err
		}
		result, err = capture.invoke(scoped, s, call)
		if err != nil || result.IsError {
			return result, err
		}
		if visual, ok := parseScreenshotObservation(result.Output); ok {
			result.Output = visual.Result.ActionOutput
			if i+1 < len(calls) && visual.Result.ScreenStable != nil && !*visual.Result.ScreenStable {
				const message = "screen did not stabilize after drag start; destination release not sent"
				result.IsError, result.Error = true, message
				result.ToolError = NewToolError(CodeStepExpectationUnmet, message)
				return result, nil
			}
		}
	}
	if err := scoped.Err(); err != nil {
		return result, err
	}
	drag.release = nil
	return result, nil
}

func (c *deviceCLICapture) executeInShell(ctx context.Context, s *Server, calls []devicecli.Call) (ToolInvokeResponse, error) {
	ctx = context.WithValue(ctx, compositeDragKey{}, &c.drag)
	var result ToolInvokeResponse
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		c.needsScreenshot = true
		var err error
		result, err = c.invoke(ctx, s, call)
		if err != nil || result.IsError {
			return result, err
		}
		if call.Tool == "touch_gesture" && call.Input["type"] == "tap" {
			if err := sleepWithContext(ctx, devicecli.TapSettleDelay); err != nil {
				return result, err
			}
		}
		if call.Tool == "screenshot" {
			if visual, ok := parseScreenshotObservation(result.Output); ok {
				result.Output = screenshotObservationStatusSummary(call.Tool, visual.Result)
			}
		}
	}
	return result, ctx.Err()
}

// Finalize only after the shell and all in-flight CLI requests have stopped.
// Use the parent tool context so a shell deadline can still show partial effects.
func (c *deviceCLICapture) finishShell(ctx context.Context, r *Runtime, succeeded bool) error {
	defer c.drag.cleanup(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.needsScreenshot {
		return nil
	}
	if !succeeded && c.drag.release != nil {
		c.drag.cleanup(ctx)
		c.drag.release = nil
	}
	// A wait only describes the screen at that point in the shell. Even a
	// shell with no more CLI calls may have done other work since the wait.
	shotCtx, cancel := context.WithTimeout(context.WithValue(ctx, deviceCLICaptureKey{}, c), 5*time.Second)
	s := &Server{runtime: r, logger: r.logger}
	if c.activityReported {
		shotCtx = withToolProgressReporter(shotCtx, func(ToolProgress) {})
		if c.activitySucceeded {
			report := ctx.Value(deviceCLIActivityKey{}).(func(RunEvent))
			report(RunEvent{Type: runEventToolProgress, ToolStatus: "verifying", Content: "Reading the screen after the action"})
		}
	}
	result, err := invokeDeviceHTTP(shotCtx, s, devicecli.Call{Tool: "screenshot", Input: map[string]any{}})
	cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err == nil && !result.IsError {
		c.frame, _ = parseScreenshotObservation(result.Output)
	}
	if len(c.frame.ImageBytes) == 0 {
		return fmt.Errorf("final screenshot unavailable; inspect the screen before retrying any action")
	}
	// Ground only the final model-facing frame.
	// Keep the original capture if the detector is absent or fails.
	tools := r.toolSnapshot()
	if tools.UIGroundingEnabled() && c.frame.Result.Grounding == nil {
		tool, _ := tools.Get("screenshot")
		if grounder, ok := tool.(screenshotResultGrounder); ok {
			result := &c.frame.Result.screenshotResult
			grounder.groundResult(ctx, result)
			if result.Grounding != nil && result.Grounding.Error == "" {
				if marked, err := base64.StdEncoding.DecodeString(result.Data); err == nil && len(marked) > 0 {
					c.frame.ImageBytes = marked
				}
			}
		}
	}
	c.frame = compressCLIFrame(c.frame)
	c.frame.Result.Data = ""
	if err := ctx.Err(); err != nil {
		return err
	}
	c.drag.release = nil // a successful split start hands off to a later shell
	return nil
}

// Record the controller outcome, not what the shell text requested. Internal
// fallback screenshots and cleanup calls do not count as user-facing actions.
func (c *deviceCLICapture) invoke(ctx context.Context, s *Server, call devicecli.Call) (ToolInvokeResponse, error) {
	report, _ := ctx.Value(deviceCLIActivityKey{}).(func(RunEvent))
	if report != nil {
		c.activityReported = true
		report(RunEvent{Type: runEventToolCall, ToolName: call.Tool, ToolInput: deviceCLIActivityInput(call), Timestamp: time.Now()})
		ctx = withToolProgressReporter(ctx, func(progress ToolProgress) {
			switch progress.Status {
			case "running", "preparing", "verifying", "waiting_app":
				report(RunEvent{Type: runEventToolProgress, ToolName: call.Tool, ToolStatus: progress.Status})
			}
		})
	}
	result, err := invokeDeviceHTTP(ctx, s, call)
	completed := result.ToolError != nil && result.ToolError.Details[postActionCompletedDetail] == true
	skipped := actionNotDispatched(result.ToolError)
	evidence := deviceCLIToolEvidence{
		Tool: call.Tool, Input: call.Input, NotDispatched: skipped,
		IsError: err != nil || skipped || (result.IsError && !completed),
	}
	if err == nil {
		evidence.DurationMs = &result.DurationMs
	}
	c.calls = append(c.calls, evidence)
	if report != nil {
		c.activitySucceeded = !evidence.IsError
		event := RunEvent{Type: "tool_result", ToolName: call.Tool, IsError: evidence.IsError}
		if event.IsError && result.ToolError != nil &&
			(liveActivityToolRequiresApp(call.Tool) || (call.Tool == "enter_text" && call.Input["route"] == "app")) {
			switch result.ToolError.Code {
			case CodeBridgeNotConnected, CodeAppBackgrounded:
				event.ToolStatus = "waiting_app"
			}
		}
		report(event)
	}
	return result, err
}

// Keep only operation names used by Live Activity's localized labels.
func deviceCLIActivityInput(call devicecli.Call) string {
	if input := deviceCLIOperationInput(call.Tool, call.Input); len(input) != 0 {
		return jsonString(input)
	}
	return ""
}

func deviceCLIOperationInput(tool string, input map[string]any) map[string]any {
	switch tool {
	case toolBridgeClipboard, toolBridgeCalendar, toolBridgeContacts, toolBridgeNotification:
		action, _ := input["action"].(string)
		switch action {
		case "read", "write", "query", "create", "update", "delete", "send":
			return map[string]any{"action": action}
		}
	}
	return map[string]any{}
}

// App data also inherits the per-command BLE Wake and return-entry routes
// previously available through direct Bridge tools. App typing keeps its own gate.
func phoneBridgeCanAttemptAppData(ctx context.Context, bridge *PhoneBridge, call devicecli.Call) bool {
	if bridge == nil {
		return false
	}
	if phoneBridgeCanAttemptAppCLI(bridge) {
		return true
	}
	verb, _ := call.Input["action"].(string)
	command := strings.TrimPrefix(call.Tool, "bridge_") + "_" + verb
	status := bridge.getStatus()
	return phoneBridgeCanRestoreFromReturnEntry(status) ||
		(phoneBridgeCanUseBLEBackground(status, command) && bridge.bleWakeAvailable(ctx))
}

func invokeDeviceHTTP(ctx context.Context, s *Server, call devicecli.Call) (ToolInvokeResponse, error) {
	if call.AppData {
		tools := s.runtime.toolSnapshot()
		if tools == nil || !phoneBridgeCanAttemptAppData(ctx, tools.phoneBridge, call) {
			te := NewToolErrorWithDetails(CodeBridgeNotConnected, "No companion app route is available for this operation.", map[string]any{actionDispatchedDetail: false})
			return ToolInvokeResponse{IsError: true, Error: te.Message, ToolError: te}, nil
		}
	}
	body, err := json.Marshal(map[string]any{"input": call.Input})
	if err != nil {
		return ToolInvokeResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/api/tools/"+call.Tool, bytes.NewReader(body))
	if err != nil {
		return ToolInvokeResponse{}, err
	}
	out := httptest.NewRecorder()
	s.handleToolInvoke(out, req)
	if out.Code != http.StatusOK {
		return ToolInvokeResponse{}, fmt.Errorf("%s: %s", call.Tool, strings.TrimSpace(out.Body.String()))
	}
	var result ToolInvokeResponse
	err = json.NewDecoder(io.LimitReader(out.Body, 24<<20)).Decode(&result)
	return result, err
}

func compressCLIFrame(v visualScreenshotObservation) visualScreenshotObservation {
	// Strip the duplicate base64 copy before retaining the frame.
	v.Result.Data = ""
	if v.Result.Height <= 1080 {
		return v
	}
	src, _, err := image.Decode(bytes.NewReader(v.ImageBytes))
	if err != nil {
		return v
	}
	b := src.Bounds()
	if b.Empty() {
		return v
	}
	w := max(1, b.Dx()*1080/b.Dy())
	dst := image.NewRGBA(image.Rect(0, 0, w, 1080))
	for y := 0; y < 1080; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/w, b.Min.Y+y*b.Dy()/1080))
		}
	}
	var out bytes.Buffer
	if jpeg.Encode(&out, dst, &jpeg.Options{Quality: 78}) != nil {
		return v
	}
	v.ImageBytes = out.Bytes()
	v.MIMEType = "image/jpeg"
	v.Result.Width = w
	v.Result.Height = 1080
	v.Result.Size = out.Len()
	return v
}

func deviceCLIStateMessages(frame visualScreenshotObservation) []llms.MessageContent {
	if len(frame.ImageBytes) == 0 {
		return nil
	}
	caption := "Device CLI screenshot. Normalize coordinates within this image."
	if grounding := screenshotGroundingCaption(frame.Result.Grounding, "normalized device-cli coordinates (0–1000)"); grounding != "" {
		caption += "\n" + grounding
	}
	return []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{
		llms.TextPart(caption),
		buildImagePart(frame.MIMEType, frame.ImageBytes),
	}}}
}
