package agent

// Opt-in real-provider perception evaluation. This uses the production model
// client, outbound transform, tool schema, and coordinate adapter. The receiving
// tool records the normalized point instead of sending input to a real device.
import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/executor"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/screen"
	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

type livePerceptionTask struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
	Image  string `json:"input_screenshot"`
	Rubric []struct {
		Check string `json:"check"`
	} `json:"rubric"`
}

type livePerceptionResult struct {
	Task            string         `json:"task"`
	Variant         string         `json:"variant"`
	Repeat          int            `json:"repeat"`
	Model           string         `json:"model"`
	LatencyMS       int64          `json:"latency_ms"`
	Bounds          [4]float64     `json:"bounds"`
	ModelInput      string         `json:"model_input,omitempty"`
	NormalizedInput map[string]any `json:"normalized_input,omitempty"`
	Hit             bool           `json:"hit"`
	Error           string         `json:"error,omitempty"`
	Response        string         `json:"response,omitempty"`
	Usage           map[string]any `json:"usage,omitempty"`
}

func TestVisualCoordinatesLive(t *testing.T) {
	if os.Getenv("AICO_VISUAL_LIVE") != "1" {
		t.Skip("set AICO_VISUAL_LIVE=1 to make paid real-model requests")
	}
	base, token := os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_AUTH_TOKEN")
	modelName := os.Getenv("AICO_VISUAL_MODEL")
	output := os.Getenv("AICO_VISUAL_RESULTS")
	if base == "" || token == "" || modelName == "" || output == "" {
		t.Fatal("requires ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN, AICO_VISUAL_MODEL, AICO_VISUAL_RESULTS")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	repeats := 1
	if raw := os.Getenv("AICO_VISUAL_REPEATS"); raw != "" {
		var err error
		repeats, err = strconv.Atoi(raw)
		if err != nil || repeats < 1 || repeats > 10 {
			t.Fatal("invalid repeat count")
		}
	}
	suitePath, err := filepath.Abs("../../../../benchmark/suites/perception/perception_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(suitePath)
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		PromptPrefix string               `json:"prompt_prefix"`
		Tasks        []livePerceptionTask `json:"tasks"`
	}
	if err = json.Unmarshal(data, &suite); err != nil {
		t.Fatal(err)
	}
	slots := make(chan struct{}, 3)
	variants := []string{"baseline", "prepared"}
	for repeat := 1; repeat <= repeats; repeat++ {
		for _, task := range suite.Tasks {
			if filter := os.Getenv("AICO_VISUAL_TASK"); filter != "" && task.ID != filter {
				continue
			}
			for _, variant := range variants {
				t.Run(fmt.Sprintf("%s/%s/%d", task.ID, variant, repeat), func(t *testing.T) {
					t.Parallel()
					slots <- struct{}{}
					defer func() { <-slots }()
					result := livePerceptionResult{Task: task.ID, Variant: variant, Repeat: repeat, Model: modelName}
					defer func() {
						data, err := json.MarshalIndent(result, "", "  ")
						if err != nil {
							t.Error(err)
							return
						}
						if err = os.WriteFile(filepath.Join(output, fmt.Sprintf("%s-%s-%02d.json", task.ID, variant, repeat)), data, 0600); err != nil {
							t.Error(err)
						}
						t.Logf("hit=%t latency_ms=%d error=%s", result.Hit, result.LatencyMS, result.Error)
					}()
					for _, rubric := range task.Rubric {
						matches := regexp.MustCompile(`\[(\d+),\s*(\d+)\]`).FindAllStringSubmatch(rubric.Check, -1)
						if len(matches) == 2 {
							for i, m := range matches {
								result.Bounds[i*2], _ = strconv.ParseFloat(m[1], 64)
								result.Bounds[i*2+1], _ = strconv.ParseFloat(m[2], 64)
							}
						}
					}
					if result.Bounds == [4]float64{} {
						t.Fatal("missing target bounds")
					}
					imagePath := filepath.Join(filepath.Dir(suitePath), task.Image)
					f, err := os.Open(imagePath)
					if err != nil {
						t.Fatal(err)
					}
					size, _, err := image.DecodeConfig(f)
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
					input := []messages.Message{
						{Role: messages.MessageRoleSystem, Content: defaultAgentBehavior() + "\nThis is a single-frame perception evaluation. Locate the requested target in the attached screenshot and make exactly one touch_gesture tap. No screenshot, skill lookup, probing, or retry is available in this evaluation."},
						{Role: messages.MessageRoleUser, Content: suite.PromptPrefix + "\n" + task.Prompt, Attachments: []messages.Attachment{{FilePath: imagePath, MIMEType: "image/jpeg", Source: messages.AttachmentSourceScreenshotObservation}}},
					}
					manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), input)
					if err != nil {
						t.Fatal(err)
					}
					backend := &visualRecordingTool{name: "touch_gesture"}
					var tool langtools.Tool = &livePerceptionTool{visualRecordingTool: backend}
					var transforms []executor.OutboundMessageTransform
					if variant == "prepared" {
						state := &screen.ScreenState{}
						imageData, err := os.ReadFile(imagePath)
						if err != nil {
							t.Fatal(err)
						}
						state.UpdateScreenshot(imageData, size.Width, size.Height)
						frames := newVisualCoordinates(state)
						transforms = append(transforms, frames)
						tool = frames.wrap([]langtools.Tool{tool})[0]
					}
					client := newAnthropicModel(base, modelName, token, &http.Client{Timeout: 120 * time.Second}).(*anthropicModel)
					client.streamMaxRetries = 0
					client.protocolRetries = 0
					exec := executor.NewLLMExecutor(client, manager, transforms...)
					ctx, cancel := context.WithTimeout(context.Background(), 125*time.Second)
					defer cancel()
					started := time.Now()
					response, err := exec.GenerateContent(ctx, llms.WithTools([]llms.Tool{NewToolSpec(tool).LLMTool()}), llms.WithMaxTokens(4096), llms.WithTemperature(0))
					result.LatencyMS = time.Since(started).Milliseconds()
					if err != nil {
						result.Error = err.Error()
						return
					}
					if len(response.Choices) == 0 {
						result.Error = "empty choices"
						return
					}
					choice := response.Choices[0]
					result.Response = choice.Content
					result.Usage = choice.GenerationInfo
					if len(choice.ToolCalls) != 1 || choice.ToolCalls[0].FunctionCall == nil {
						result.Error = "expected exactly one tool call"
						return
					}
					call := choice.ToolCalls[0].FunctionCall
					result.ModelInput = call.Arguments
					if call.Name != "touch_gesture" {
						result.Error = "unexpected tool: " + call.Name
						return
					}
					out, err := tool.Call(ctx, call.Arguments)
					if err != nil {
						result.Error = err.Error()
						return
					}
					if len(backend.inputs) != 1 {
						result.Error = strings.TrimSpace(out)
						return
					}
					result.NormalizedInput = backend.inputs[0]
					if result.NormalizedInput["type"] != "tap" {
						result.Error = "not a tap"
						return
					}
					point, ok := result.NormalizedInput["point"].(map[string]any)
					if !ok {
						result.Error = "missing point"
						return
					}
					x, xok := point["x"].(float64)
					y, yok := point["y"].(float64)
					result.Hit = xok && yok && x >= result.Bounds[0] && x <= result.Bounds[1] && y >= result.Bounds[2] && y <= result.Bounds[3]
				})
			}
		}
	}
}

type livePerceptionTool struct{ *visualRecordingTool }

func (t *livePerceptionTool) Description() string { return (&TouchGestureTool{}).Description() }

// Factorial controls: identical prompt, caption, JPEG encoder and minimal tap
// schema in all four cells. Only resizing and coordinate units differ.
