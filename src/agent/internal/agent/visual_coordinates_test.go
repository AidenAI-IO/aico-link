package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/agent/messages"
	"aiden-agent/internal/agent/model"
	"aiden-agent/internal/agent/screen"
	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

func visualTestMessage(t *testing.T, w, h int) messages.Message {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "screen.png")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return messages.Message{Role: messages.MessageRoleUser, Attachments: []messages.Attachment{{FilePath: path, MIMEType: "image/png", Source: messages.AttachmentSourceScreenshotObservation}}}
}

func visualTestScreen(t *testing.T, msg messages.Message) *screen.ScreenState {
	t.Helper()
	data, err := os.ReadFile(msg.Attachments[0].FilePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	state := &screen.ScreenState{}
	state.UpdateScreenshot(data, cfg.Width, cfg.Height)
	return state
}

func TestVisualCoordinatesOutboundImageAndReplay(t *testing.T) {
	for _, size := range [][2]int{{1179, 2556}, {2556, 1179}, {2000, 2000}, {447, 972}, {1, 100}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			original := visualTestMessage(t, size[0], size[1])
			before := original.Clone()
			v := newVisualCoordinates(nil)
			for range 2 {
				out := v.Transform([]messages.Message{original})
				if len(out) != 2 || !reflect.DeepEqual(out[0], before) || !reflect.DeepEqual(original, before) {
					t.Fatal("transform changed the source message or attachment")
				}
				if out[1].Role != messages.MessageRoleSystem || out[1].Content != visualCoordinateInstruction {
					t.Fatal("missing transient pixel protocol instruction")
				}
				standard := messages.ConvertMessageList(out)
				if len(standard[0].Parts) != 1 {
					t.Fatalf("unexpected caption or other content: %v", standard[0].Parts)
				}
				binary, ok := standard[0].Parts[0].(llms.BinaryContent)
				if !ok {
					t.Fatal("image missing")
				}
				source, err := os.ReadFile(original.Attachments[0].FilePath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(binary.Data, source) {
					t.Fatal("image bytes changed")
				}
				cfg, _, err := image.DecodeConfig(bytes.NewReader(binary.Data))
				if err != nil || cfg.Width != size[0] || cfg.Height != size[1] {
					t.Fatalf("image dimensions changed: %v, %v", cfg, err)
				}
			}
		})
	}
}

// TestVisualCoordinatesProtocolTravelsInToolDescriptions pins the second channel
// that carries the pixel protocol, next to the transient system message.
func TestVisualCoordinatesProtocolTravelsInToolDescriptions(t *testing.T) {
	v := newVisualCoordinates(nil)
	for _, name := range []string{"touch_gesture", "mouse_move", "enter_text"} {
		tools := v.wrap([]langtools.Tool{&visualRecordingTool{name: name}})
		if !strings.Contains(tools[0].Description(), visualCoordinateInstruction) {
			t.Fatalf("%s description lost the pixel protocol: %q", name, tools[0].Description())
		}
	}
	untouched := &visualRecordingTool{name: "screenshot"}
	if got := v.wrap([]langtools.Tool{untouched})[0]; got != langtools.Tool(untouched) {
		t.Fatal("non-geometry tool was wrapped")
	}
}

type visualRecordingTool struct {
	schema map[string]any
	name   string
	inputs []map[string]any
}

func (t *visualRecordingTool) Name() string        { return t.name }
func (t *visualRecordingTool) Description() string { return "test tool" }
func (t *visualRecordingTool) ArgsSchema() map[string]any {
	if t.schema == nil {
		t.schema = (&TouchGestureTool{}).ArgsSchema()
	}
	return t.schema
}
func (t *visualRecordingTool) Call(_ context.Context, input string) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", err
	}
	t.inputs = append(t.inputs, args)
	return "ok", nil
}

func TestVisualCoordinatesToolBoundary(t *testing.T) {
	state := visualTestScreen(t, visualTestMessage(t, 101, 201))
	// The source HDMI frame and active area can both differ from the image size.
	state.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 710, Width: 500, Height: 1080, Valid: true})
	v := newVisualCoordinates(state)
	base := &visualRecordingTool{name: "touch_gesture"}
	tool := v.wrap([]langtools.Tool{base})[0]
	for _, input := range []string{
		`{"point":{"x":101,"y":100}}`,
		`{"point":{"x":-1,"y":100}}`,
		`{"point":{"x":"50","y":100}}`,
	} {
		_, _ = tool.Call(context.Background(), `{"type":"tap",`+input[1:])
		if len(base.inputs) != 0 {
			t.Fatalf("invalid input reached device: %s", input)
		}
	}
	result, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":50,"y":100},"end":{"x":100,"y":200},"speed":2500}`)
	if err != nil || result != "ok" || len(base.inputs) != 1 {
		t.Fatalf("call = %s, %v", result, err)
	}
	got := base.inputs[0]
	if got["speed"] != float64(2500) || got["start"].(map[string]any)["x"] != float64(500) || got["end"].(map[string]any)["y"] != float64(1000) {
		t.Fatal(got)
	}
	// Reuse works without a transform, and subsequent captures are read at call time.
	_, _ = tool.Call(context.Background(), `{"type":"tap","point":{"x":50,"y":100}}`)
	state.UpdateScreenshot([]byte("new capture"), 201, 101)
	_, _ = tool.Call(context.Background(), `{"type":"tap","point":{"x":100,"y":50}}`)
	if len(base.inputs) != 3 || base.inputs[2]["point"].(map[string]any)["y"] != float64(500) {
		t.Fatal("adapter did not use current screenshot dimensions", base.inputs)
	}
}

func TestVisualCoordinatesAllGeometry(t *testing.T) {
	args := map[string]any{}
	if err := json.Unmarshal([]byte(`{"focus":{"x":50,"y":100},"actions":[{"point":{"x":100,"y":0},"speed":2500}]}`), &args); err != nil {
		t.Fatal(err)
	}
	if err := convertVisualArguments(args, visualFrame{width: 101, height: 201}); err != nil {
		t.Fatal(err)
	}
	if args["focus"].(map[string]any)["y"] != float64(500) {
		t.Fatal(args)
	}
	point := args["actions"].([]any)[0].(map[string]any)["point"].(map[string]any)
	if point["x"] != float64(1000) || point["y"] != float64(0) {
		t.Fatal(point)
	}
}

func TestVisualCoordinatesMissingOrInvalidatedCaptureFailsClosed(t *testing.T) {
	mappingOnly := &screen.ScreenState{}
	mappingOnly.Update(1920, 1080)
	invalidated := visualTestScreen(t, visualTestMessage(t, 101, 201))
	invalidated.InvalidateCapture()
	for _, state := range []*screen.ScreenState{nil, {}, mappingOnly, invalidated} {
		v := newVisualCoordinates(state)
		// Stored history is deliberately not used to reconstruct retired capture state.
		v.Transform([]messages.Message{visualTestMessage(t, 101, 201)})
		base := &visualRecordingTool{name: "touch_gesture"}
		tool := v.wrap([]langtools.Tool{base})[0]
		result, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":50,"y":100}}`)
		if err != nil || len(base.inputs) != 0 || !strings.Contains(result, "no screenshot available") {
			t.Fatalf("missing capture was actionable: %s, %v", result, err)
		}
	}
}

func TestVisualCoordinatesSchemaDoesNotMutateOriginal(t *testing.T) {
	base := &visualRecordingTool{name: "touch_gesture"}
	target := &visualCoordinateTool{Tool: base}
	original := base.ArgsSchema()
	before, _ := json.Marshal(original)
	schema := target.ArgsSchema()
	props := schema["properties"].(map[string]any)
	point := props["point"].(map[string]any)["properties"].(map[string]any)["x"].(map[string]any)
	if point["maximum"] != nil {
		t.Fatal("pixel schema unexpectedly carried normalized constraint", schema)
	}
	after, _ := json.Marshal(original)
	if !bytes.Equal(before, after) {
		t.Fatal("underlying schema mutated")
	}
}

type visualCapabilityTool struct {
	visualRecordingTool
	deviceType func() string
}

func (t *visualCapabilityTool) ReturnsVisualObservation() bool     { return true }
func (t *visualCapabilityTool) SetDeviceTypeFunc(fn func() string) { t.deviceType = fn }

func TestVisualCoordinatesForwardsCapabilities(t *testing.T) {
	base := &visualCapabilityTool{visualRecordingTool: visualRecordingTool{name: "touch_gesture"}}
	wrapped := newVisualCoordinates(nil).wrap([]langtools.Tool{base})[0]
	if !wrapped.(visualObservationTool).ReturnsVisualObservation() {
		t.Fatal("lost visual observation capability")
	}
	wrapped.(runtimeDeviceTypeConfigurable).SetDeviceTypeFunc(func() string { return "ios" })
	if base.deviceType == nil || base.deviceType() != "ios" {
		t.Fatal("lost device type callback")
	}
	plain := &visualCoordinateTool{Tool: &visualRecordingTool{name: "touch_gesture"}}
	if plain.ReturnsVisualObservation() {
		t.Fatal("plain tool became visual")
	}
	plain.SetDeviceTypeFunc(func() string { return "ios" })
}

type visualSchemaTool struct {
	langtools.Tool
	schema map[string]any
}

func (t *visualSchemaTool) ArgsSchema() map[string]any { return t.schema }

func TestVisualCoordinatesSchemaFallback(t *testing.T) {
	base := &visualRecordingTool{name: "touch_gesture"}
	noSchema := struct{ langtools.Tool }{base}
	if got := (&visualCoordinateTool{Tool: noSchema}).ArgsSchema(); got != nil {
		t.Fatal(got)
	}
	for _, schema := range []map[string]any{nil, {}, {"properties": "invalid"}, {"properties": map[string]any{}, "invalid": func() {}}} {
		tool := &visualCoordinateTool{Tool: &visualSchemaTool{Tool: base, schema: schema}}
		if got := tool.ArgsSchema(); len(got) != len(schema) {
			t.Fatalf("fallback changed schema: %v", got)
		}
	}
}

type visualLoopModel struct {
	calls    int
	provider string
}

func (m *visualLoopModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: m.provider, Name: "visual-test", ContextWindow: 100000}
}
func (m *visualLoopModel) CallOptions() []chains.ChainCallOption { return nil }
func (m *visualLoopModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", fmt.Errorf("unexpected Call")
}
func (m *visualLoopModel) GenerateContent(_ context.Context, input []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	m.calls++
	if m.calls > 1 {
		return contentResponse("done"), nil
	}
	hasImage, hasProtocol := false, false
	for _, msg := range input {
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case llms.BinaryContent:
				hasImage = true
			case llms.TextContent:
				if strings.Contains(p.Text, "Image dimensions:") || strings.Contains(p.Text, "Pixel centers span") {
					return nil, fmt.Errorf("unexpected image dimension caption")
				}
				hasProtocol = hasProtocol || strings.Contains(p.Text, visualCoordinateInstruction)
			}
		}
	}
	if !hasImage || hasProtocol != (m.provider == "anthropic") {
		return nil, fmt.Errorf("incorrect image/protocol wiring: image=%v protocol=%v", hasImage, hasProtocol)
	}
	args := `{"type":"tap","point":{"x":500,"y":500}}`
	if m.provider == "anthropic" {
		args = `{"type":"tap","point":{"x":50,"y":100}}`
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{ToolCalls: []llms.ToolCall{{ID: "visual_call", Type: "function", FunctionCall: &llms.FunctionCall{Name: "touch_gesture", Arguments: args}}}}}}, nil
}

func TestVisualCoordinatesAgentLoopWiring(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			msg := visualTestMessage(t, 101, 201)
			manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{msg})
			if err != nil {
				t.Fatal(err)
			}
			backend := &visualRecordingTool{name: "touch_gesture"}
			m := &visualLoopModel{provider: provider}
			loop := NewAgentLoop(m, RoleProfile{Tools: []langtools.Tool{backend}}, 3, nil, nil, manager)
			loop.ScreenState = visualTestScreen(t, msg)
			answer, err := loop.Run(context.Background(), "tap center")
			if err != nil || answer != "done" {
				t.Fatalf("Run = %q, %v", answer, err)
			}
			if len(backend.inputs) != 1 {
				t.Fatalf("device calls = %d", len(backend.inputs))
			}
			point := backend.inputs[0]["point"].(map[string]any)
			if point["x"] != float64(500) || point["y"] != float64(500) {
				t.Fatal(point)
			}
		})
	}
}

func TestVisualCoordinatesUploadsDoNotChangeScreenSpace(t *testing.T) {
	screenshot := visualTestMessage(t, 101, 201)
	upload := visualTestMessage(t, 800, 600)
	upload.Attachments[0].Source = ""
	state := visualTestScreen(t, screenshot)
	v := newVisualCoordinates(state)
	out := v.Transform([]messages.Message{screenshot, upload})
	if !reflect.DeepEqual(out[1], upload) {
		t.Fatal("ordinary upload was changed")
	}
	// Rebuilding a loop keeps using shared capture state, even with pruned history.
	resumed := newVisualCoordinates(state)
	resumed.Transform(nil)
	for _, adapter := range []*visualCoordinates{v, resumed} {
		args := map[string]any{"x": float64(50), "y": float64(100)}
		if _, err := adapter.convert(args); err != nil || args["x"] != float64(500) || args["y"] != float64(500) {
			t.Fatalf("screenshot mapping changed: %v, %v", args, err)
		}
	}
}

func TestVisualCoordinatesUsesObservedImageDimensions(t *testing.T) {
	msg := visualTestMessage(t, 101, 201)
	data, err := os.ReadFile(msg.Attachments[0].FilePath)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := json.Marshal(screenshotResult{
		Width: 101, Height: 201, SourceWidth: 1920, SourceHeight: 1080,
		ActiveArea: &screen.ScreenActiveArea{X: 710, Width: 500, Height: 1080, Valid: true},
		Data:       base64.StdEncoding.EncodeToString(data),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(observation), `{"screenshot":` + string(observation) + `}`} {
		state := &screen.ScreenState{}
		tool := &visualCapabilityTool{visualRecordingTool: visualRecordingTool{name: "touch_gesture"}}
		newScreenToolResultObserver(state)(context.Background(), ToolCall{Spec: NewToolSpec(tool)}, ToolResult{Output: output})
		args := map[string]any{"x": float64(50), "y": float64(100)}
		frame, err := newVisualCoordinates(state).convert(args)
		if err != nil || frame.width != 101 || frame.height != 201 || args["x"] != float64(500) || args["y"] != float64(500) {
			t.Fatalf("observer used source dimensions instead of image dimensions: %v, %v, %v", frame, args, err)
		}
	}
}
