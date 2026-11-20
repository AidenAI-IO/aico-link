package agent

import (
	"bytes"
	"context"
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

type deviceCLIFrame struct {
	action int // zero-based index into this shell call’s action log
	visual visualScreenshotObservation
}

type deviceCLICapture struct {
	frames  []deviceCLIFrame
	actions []deviceCLIAction
}

func (r *Runtime) deviceCLIEnabled() bool {
	return r.deviceCLI && (r.ConfigSnapshot().InputModeOrDefault() != "realtime" || r.ConfigSnapshot().UseBackendAgentEffective())
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
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&args); err != nil || len(args) != 5 || args[0] != "drag" {
		http.Error(w, "Expected drag X1 Y1 X2 Y2", http.StatusBadRequest)
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

func executeDeviceCLI(ctx context.Context, s *Server, args []string, capture *deviceCLICapture) (ToolInvokeResponse, error) {
	calls, err := devicecli.Calls(args)
	if err != nil {
		return ToolInvokeResponse{}, err
	}
	scoped := context.WithValue(ctx, deviceCLICaptureKey{}, capture)
	if args[0] == "look" {
		// Return the raw screenshot only to the CLI process, which saves the file.
		return invokeDeviceHTTP(scoped, s, calls[0])
	}
	if len(capture.actions)+len(calls) > maxDeviceCLIActions {
		return ToolInvokeResponse{}, fmt.Errorf("device-cli action limit per shell reached; continue in a new shell call")
	}
	var result ToolInvokeResponse
	var last visualScreenshotObservation
	defer func() {
		if len(last.ImageBytes) == 0 && ctx.Err() == nil {
			fallback, _ := invokeDeviceHTTP(scoped, s, devicecli.Call{Tool: "screenshot", Input: map[string]any{}})
			last, _ = parseScreenshotObservation(fallback.Output)
		}
		last = compressCLIFrame(last)
		capture.add(len(capture.actions)-1, last)
	}()
	if len(calls) > 1 {
		drag := &compositeDrag{}
		scoped = context.WithValue(scoped, compositeDragKey{}, drag)
		defer func() {
			if drag.release == nil {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(scoped), 3*time.Second)
			defer cancel()
			if err := drag.release(cleanupCtx); err != nil {
				logging.Warnf("agent", "device_cli", "drag cleanup failed: %v", err)
			}
		}()
	}
	for i, call := range calls {
		if err := scoped.Err(); err != nil {
			return result, err
		}
		action := newDeviceCLIAction(args[0], call)
		result, err = invokeDeviceHTTP(scoped, s, call)
		action.failed = err != nil || result.IsError
		capture.actions = append(capture.actions, action)
		if err != nil {
			return result, err
		}
		visual, ok := parseScreenshotObservation(result.Output)
		last = visual
		if ok {
			result.Output = visual.Result.ActionOutput
			if visual.Result.ScreenChanged != nil && !*visual.Result.ScreenChanged {
				result.Output += "\nNo meaningful screen change; verify visually."
			}
		}
		if result.IsError {
			return result, nil
		}
		if i+1 < len(calls) {
			if !ok || visual.Result.ScreenStable == nil || !*visual.Result.ScreenStable {
				capture.actions[len(capture.actions)-1].failed = true
				err = fmt.Errorf("drag did not reach a stable screen; inspect the screenshot before retrying")
				return result, err
			}
			// Preserve the drag-start frame separately from drag-release.
			capture.add(len(capture.actions)-1, compressCLIFrame(visual))
		}
	}
	return result, nil
}

func invokeDeviceHTTP(ctx context.Context, s *Server, call devicecli.Call) (ToolInvokeResponse, error) {
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

func (c *deviceCLICapture) add(action int, v visualScreenshotObservation) {
	if len(v.ImageBytes) == 0 {
		return
	}
	// Keep the last five captured frames, even if a step did not change the
	// screen: its panel is the only place to see that step's marker.
	if len(c.frames) == 5 {
		c.frames = c.frames[1:]
	}
	c.frames = append(c.frames, deviceCLIFrame{action: action, visual: v})
}

func deviceCLIStateMessages(frames []deviceCLIFrame, actions []deviceCLIAction) []llms.MessageContent {
	if len(frames) == 0 {
		return nil
	}
	last := frames[len(frames)-1].visual
	imageBytes, latestPanel := renderDeviceCLISheet(frames, actions)
	mimeType := "image/jpeg"
	caption := "Device CLI screenshot sheet. Panels are ordered left to right, then top to bottom. Markers show past actions, not UI targets."
	if len(imageBytes) == 0 {
		imageBytes, mimeType = last.ImageBytes, last.MIMEType
		caption = "Device CLI screenshot. Use this image for the latest available device state."
	} else {
		caption += fmt.Sprintf(" Normalize new coordinates to 0–1000 within the last panel only (top-left pixel %d,%d; size %d×%d px), not across the sheet.", latestPanel.Min.X, latestPanel.Min.Y, latestPanel.Dx(), latestPanel.Dy())
	}
	status := last.Result
	status.Width, status.Height = 0, 0
	if notes := screenshotObservationStatusSummary("shell", status); notes != "" {
		caption += " " + notes
	}
	return []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{
		llms.TextPart(caption), buildImagePart(mimeType, imageBytes),
	}}}
}
