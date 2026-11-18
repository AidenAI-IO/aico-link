package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/google/uuid"
	"github.com/tmc/langchaingo/llms"
)

type deviceCLIRuntimeKey struct{}
type deviceCLICaptureKey struct{}

type deviceCLIFrame struct {
	step   int
	visual visualScreenshotObservation
}

type deviceCLICapture struct{ frames []deviceCLIFrame }

func (r *Runtime) deviceCLIEnabled() bool { return r.deviceCLI }

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
		response, err := r.executeDeviceCLI(callCtx, toolServer, args, capture)
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

func (r *Runtime) executeDeviceCLI(ctx context.Context, s *Server, args []string, capture *deviceCLICapture) (ToolInvokeResponse, error) {
	calls, err := devicecli.Calls(args)
	if err != nil {
		return ToolInvokeResponse{}, err
	}
	var result ToolInvokeResponse
	for i, call := range calls {
		result, err = invokeDeviceHTTP(context.WithValue(ctx, deviceCLICaptureKey{}, capture), s, call)
		if err != nil {
			return result, err
		}
		visual, ok := parseScreenshotObservation(result.Output)
		if ok {
			capture.add(ctx, args, visual)
			// The screenshot takes the normal state-message path, not stdout.
			result.Output = stripScreenshotData(result.Output)
		}
		if result.IsError {
			return result, nil
		}
		if len(calls) > 1 && i == 0 && (!ok || visual.Result.ScreenStable != nil && !*visual.Result.ScreenStable) {
			return result, fmt.Errorf("drag did not reach a stable screen; inspect the screenshot before retrying")
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

func (c *deviceCLICapture) add(ctx context.Context, args []string, v visualScreenshotObservation) {
	step := len(c.frames) + 1
	c.frames = append(c.frames, deviceCLIFrame{step: step, visual: v})
}

func deviceCLIStateMessages(frames []deviceCLIFrame) []llms.MessageContent {
	var messages []llms.MessageContent
	for i, frame := range frames {
		v := frame.visual
		if i < len(frames)-1 && (i < len(frames)-5 || v.Result.ScreenChanged != nil && !*v.Result.ScreenChanged) {
			continue
		}
		caption := fmt.Sprintf("Device CLI screenshot, Step #%d. Coordinates are normalized 0–1000, not pixels. %s", frame.step, screenshotObservationStatusSummary("shell", v.Result))
		messages = append(messages, llms.MessageContent{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart(caption), buildImagePart(v.MIMEType, v.ImageBytes)}})
	}
	return messages
}
