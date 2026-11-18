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
	"github.com/google/uuid"
	"github.com/tmc/langchaingo/llms"
)

type deviceCLIRuntimeKey struct{}
type deviceCLICaptureKey struct{}

type deviceCLIFrame struct {
	step   int
	visual visualScreenshotObservation
}

type deviceCLICapture struct {
	frames  []deviceCLIFrame
	last    visualScreenshotObservation
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
	turn, _ := ctx.Value(utgTurnKey{}).(*utgTurn)
	if len(args) > 0 && (args[0] == "utg-status" || args[0] == "utg-goto" || args[0] == "utg-step" || args[0] == "utg-tag") {
		if len(capture.actions) == maxDeviceCLIActions {
			return ToolInvokeResponse{}, fmt.Errorf("device-cli action limit per shell reached; continue in a new shell call")
		}
		index := len(capture.actions)
		capture.actions = append(capture.actions, deviceCLIAction{name: args[0]})
		result, err := r.runUTGCommand(ctx, s, args, capture)
		capture.actions[index].failed = err != nil || result.IsError
		if len(capture.frames) == 0 && ctx.Err() == nil {
			result, _ := invokeDeviceHTTP(context.WithValue(ctx, deviceCLICaptureKey{}, capture), s, devicecli.Call{Tool: "screenshot", Input: map[string]any{}})
			frame, _ := parseScreenshotObservation(result.Output)
			capture.add(-1, compressCLIFrame(frame))
		}
		return result, err
	}
	calls, err := devicecli.Calls(args)
	if err != nil {
		return ToolInvokeResponse{}, err
	}
	if len(capture.actions)+len(calls) > maxDeviceCLIActions {
		return ToolInvokeResponse{}, fmt.Errorf("device-cli action limit per shell reached; continue in a new shell call")
	}
	scoped := context.WithValue(ctx, deviceCLICaptureKey{}, capture)
	if turn != nil && len(turn.steps) == 0 && args[0] != "look" {
		result, _ := invokeDeviceHTTP(scoped, s, devicecli.Call{Tool: "screenshot", Input: map[string]any{}})
		if v, ok := parseScreenshotObservation(result.Output); ok {
			turn.steps = append(turn.steps, utgObservation{frame: compressCLIFrame(v)})
		}
	}
	var result ToolInvokeResponse
	var last visualScreenshotObservation
	defer func() {
		if len(last.ImageBytes) == 0 && ctx.Err() == nil {
			fallback, _ := invokeDeviceHTTP(scoped, s, devicecli.Call{Tool: "screenshot", Input: map[string]any{}})
			last, _ = parseScreenshotObservation(fallback.Output)
		}
		last = compressCLIFrame(last)
		if turn != nil && len(turn.steps) < maxUTGSteps {
			capture.add(len(turn.steps), last)
			turn.steps = append(turn.steps, utgObservation{args: args, frame: last, successful: err == nil && !result.IsError})
		} else {
			capture.add(-1, last)
		}
	}()
	for i, call := range calls {
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
		if len(calls) > 1 && i == 0 && (!ok || visual.Result.ScreenStable != nil && !*visual.Result.ScreenStable) {
			capture.actions[len(capture.actions)-1].failed = true
			err = fmt.Errorf("drag did not reach a stable screen; inspect the screenshot before retrying")
			return result, err
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

func (c *deviceCLICapture) add(step int, v visualScreenshotObservation) {
	c.last = v
	if len(v.ImageBytes) == 0 {
		return
	}
	// Retain the final observation and up to four visually useful transitions.
	if n := len(c.frames); n > 0 {
		changed := c.frames[n-1].visual.Result.ScreenChanged
		if changed != nil && !*changed {
			c.frames = c.frames[:n-1]
		}
	}
	if len(c.frames) == 5 {
		c.frames = c.frames[1:]
	}
	c.frames = append(c.frames, deviceCLIFrame{step: step, visual: v})
}

func deviceCLIStateMessages(ctx context.Context, frames []deviceCLIFrame, actions []deviceCLIAction) []llms.MessageContent {
	var messages []llms.MessageContent
	turn, _ := ctx.Value(utgTurnKey{}).(*utgTurn)
	for i, frame := range frames {
		v := frame.visual
		if i < len(frames)-1 && v.Result.ScreenChanged != nil && !*v.Result.ScreenChanged {
			continue
		}
		label := "Device CLI screenshot"
		var annotation string
		if i == len(frames)-1 && len(actions) > 0 {
			v, annotation = annotateDeviceCLIFrame(v, actions)
		}
		if frame.step >= 0 && turn != nil && frame.step < len(turn.steps) {
			label += fmt.Sprintf(", Step #%d", frame.step)
			// The annotated frame is for the agent; classification still uses the clean image.
			turn.steps[frame.step].shown = annotation == ""
		}
		caption := label + ". Coordinates are normalized 0–1000, not pixels."
		if annotation == "" {
			caption += " " + screenshotObservationStatusSummary("shell", v.Result)
		}
		caption += annotation
		messages = append(messages, llms.MessageContent{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart(caption), buildImagePart(v.MIMEType, v.ImageBytes)}})
	}
	return messages
}
