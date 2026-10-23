package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/screen"
	langtools "github.com/tmc/langchaingo/tools"
)

type visualFrame struct {
	width, height int
}

// visualPoint uses pixel centers in the captured screenshot. Device code receives
// only normalizedPoint; no provider-specific scaling belongs in that layer.
type visualPoint struct{ X, Y float64 }
type normalizedPoint struct{ X, Y float64 }

func (f visualFrame) normalize(point visualPoint) (normalizedPoint, error) {
	x, err := normalizeVisualAxis(point.X, f.width)
	if err != nil {
		return normalizedPoint{}, fmt.Errorf("x: %w", err)
	}
	y, err := normalizeVisualAxis(point.Y, f.height)
	if err != nil {
		return normalizedPoint{}, fmt.Errorf("y: %w", err)
	}
	return normalizedPoint{X: x, Y: y}, nil
}

func normalizeVisualAxis(value float64, size int) (float64, error) {
	if size < 1 || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(size-1) {
		return 0, fmt.Errorf("pixel value must be between 0 and %d", size-1)
	}
	return value / float64(max(1, size-1)) * 1000, nil
}

// The coordinate space comes from the latest captured screenshot in ScreenState.
// No image data or per-run frame registry is needed by the adapter.
type visualCoordinates struct {
	screen *screen.ScreenState
}

func newVisualCoordinates(screenStates ...*screen.ScreenState) *visualCoordinates {
	var screenState *screen.ScreenState
	if len(screenStates) > 0 {
		screenState = screenStates[0]
	}
	return &visualCoordinates{screen: screenState}
}

// visualCoordinateInstruction states the pixel protocol to the model. It reaches
// each request through two channels: a transient system message appended on every
// call, and the tail of every wrapped tool's description. The system-message form
// is never written to the conversation store, so it cannot accumulate.
const visualCoordinateInstruction = "Visual coordinate protocol: for touch_gesture, mouse_move and enter_text.focus geometry, use pixel coordinates in the latest screenshot. Do not rescale coordinates or call a normalization tool. Speed parameters retain normalized units per second."

func (v *visualCoordinates) Transform(input []messages.Message) []messages.Message {
	out := make([]messages.Message, len(input), len(input)+1)
	for i, msg := range input {
		out[i] = msg.Clone()
	}
	// Only the protocol instruction is transient; attachments are unchanged.
	return append(out, messages.Message{Role: messages.MessageRoleSystem, Content: visualCoordinateInstruction})
}

// Read captured image dimensions, not the source frame or phone display size.
// Existing screenshot and device safety checks remain owned by the device tools.
func (v *visualCoordinates) convert(args map[string]any) (visualFrame, error) {
	width, height, ok := v.screen.ScreenshotDimensions()
	if !ok {
		return visualFrame{}, fmt.Errorf("no screenshot available; request a fresh screenshot")
	}
	frame := visualFrame{width: width, height: height}
	if err := convertVisualArguments(args, frame); err != nil {
		return visualFrame{}, err
	}
	return frame, nil
}

func (v *visualCoordinates) wrap(tools []langtools.Tool) []langtools.Tool {
	out := append([]langtools.Tool(nil), tools...)
	for i, tool := range out {
		switch tool.Name() {
		case "touch_gesture", "mouse_move", "enter_text":
			out[i] = &visualCoordinateTool{Tool: tool, frames: v}
		}
	}
	return out
}

type visualCoordinateTool struct {
	langtools.Tool
	frames *visualCoordinates
}

func (t *visualCoordinateTool) ReturnsVisualObservation() bool {
	visual, ok := t.Tool.(visualObservationTool)
	return ok && visual.ReturnsVisualObservation()
}

func (t *visualCoordinateTool) SetDeviceTypeFunc(fn func() string) {
	if tool, ok := t.Tool.(runtimeDeviceTypeConfigurable); ok {
		tool.SetDeviceTypeFunc(fn)
	}
}

func (t *visualCoordinateTool) Description() string {
	description := t.Tool.Description()
	// Keep gesture semantics, but remove the old instruction to do arithmetic.
	if start := strings.Index(description, "Base coordinates on the latest screenshot"); start >= 0 {
		if end := strings.Index(description[start:], "Swipe direction names"); end >= 0 {
			description = description[:start] + description[start+end:]
		}
	}
	if t.Name() == "mouse_move" {
		description = "Move the mouse without clicking."
	}
	return description + " " + visualCoordinateInstruction
}

func (t *visualCoordinateTool) ArgsSchema() map[string]any {
	provider, ok := t.Tool.(interface{ ArgsSchema() map[string]any })
	if !ok {
		return nil
	}
	// Deep-copy before changing nested schemas owned by the underlying tool.
	original := provider.ArgsSchema()
	data, err := json.Marshal(original)
	if err != nil {
		return original
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil || schema == nil {
		return original
	}
	var rewrite func(map[string]any)
	rewrite = func(node map[string]any) {
		delete(node, "examples")
		if props, ok := node["properties"].(map[string]any); ok {
			for key, value := range props {
				child, ok := value.(map[string]any)
				if !ok {
					continue
				}
				switch key {
				case "x", "y":
					props[key] = numberArgSchema("Pixel coordinate in the image; must be inside the image bounds.")
				default:
					rewrite(child)
				}
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			rewrite(items)
		}
	}
	rewrite(schema)
	return schema
}

func (t *visualCoordinateTool) DynamicExampleInput() string {
	// Reuse non-coordinate example fields; pixel coordinates are self-explanatory.
	return builtInToolSpecMetadata[t.Name()].ExampleInput
}

func (t *visualCoordinateTool) Call(ctx context.Context, input string) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(input), &args); err != nil || args == nil {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Expected a JSON object with image pixel coordinates"), nil
	}
	frame, err := t.frames.convert(args)
	if err != nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "%v", err), nil
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	if recorder := EpisodeRecorderFromContext(ctx); recorder != nil {
		recorder.RecordEvent(TaskEpisodeEvent{Type: "visual_coordinate_mapping", Ts: time.Now().Format(time.RFC3339Nano), Metadata: map[string]interface{}{
			"tool": t.Name(), "image_width": frame.width, "image_height": frame.height,
			"pixel_input": json.RawMessage(input), "normalized_input": json.RawMessage(data),
		}})
	}
	return t.Tool.Call(ctx, string(data))
}

func convertVisualArguments(args map[string]any, frame visualFrame) error {
	_, hasX := args["x"]
	_, hasY := args["y"]
	if hasX != hasY {
		return fmt.Errorf("pixel points require both x and y")
	}
	if hasX {
		x, xok := args["x"].(float64)
		y, yok := args["y"].(float64)
		if !xok || !yok {
			return fmt.Errorf("pixel x and y must be numbers")
		}
		point, err := frame.normalize(visualPoint{X: x, Y: y})
		if err != nil {
			return err
		}
		args["x"], args["y"] = point.X, point.Y
	}
	for key, value := range args {
		if key == "x" || key == "y" {
			continue
		}
		switch key {
		case "point", "start", "end", "focus":
			point, ok := value.(map[string]any)
			if !ok || point["x"] == nil || point["y"] == nil {
				return fmt.Errorf("%s requires a pixel point object containing x and y", key)
			}
		}
		switch key {
		case "coordinate", "start_x", "start_y", "end_x", "end_y":
			return fmt.Errorf("%s is unsupported by the visual coordinate protocol; use named point/start/end objects", key)
		}

		switch child := value.(type) {
		case map[string]any:
			if err := convertVisualArguments(child, frame); err != nil {
				return err
			}
		case []any:
			for _, item := range child {
				if obj, ok := item.(map[string]any); ok {
					if err := convertVisualArguments(obj, frame); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
