package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	modelpkg "aiden-agent/internal/agent/model"

	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
)

type retryingTextInputVision struct {
	calls int
}

func (v *retryingTextInputVision) AnalyzeScreen(_ context.Context, _ screenshotResult, req textInputScreenAnalysisRequest) (textInputScreenAnalysis, error) {
	v.calls++
	if v.calls == 1 {
		var truncated any
		err := json.Unmarshal([]byte(`{"target_matched":`), &truncated)
		return textInputScreenAnalysis{}, fmt.Errorf("parse screen analysis: %w", err)
	}
	return textInputScreenAnalysis{FieldText: req.TargetText, TargetMatched: true}, nil
}

type textInputVisionRecordingModel struct {
	options  []llms.CallOption
	content  string
	contents []string
	calls    int
	messages [][]llms.MessageContent
}

func (m *textInputVisionRecordingModel) GenerateContent(_ context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	m.options = options
	m.messages = append(m.messages, messages)
	m.calls++
	content := m.content
	if len(m.contents) > 0 {
		index := m.calls - 1
		if index >= len(m.contents) {
			index = len(m.contents) - 1
		}
		content = m.contents[index]
	}
	if content == "" {
		content = `{}`
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: content}}}, nil
}

func (m *textInputVisionRecordingModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", nil
}

func (m *textInputVisionRecordingModel) CallOptions() []chains.ChainCallOption { return nil }

func (m *textInputVisionRecordingModel) Spec() modelpkg.ModelSpec { return modelpkg.ModelSpec{} }

func TestVisionJSONBoundsOutputTokens(t *testing.T) {
	model := &textInputVisionRecordingModel{}
	vision := &llmTextInputVision{models: model}
	if _, err := vision.visionJSON(context.Background(), "test", "return json", screenshotResult{Data: "ZmFrZQ=="}); err != nil {
		t.Fatalf("visionJSON() error = %v", err)
	}
	options := llms.CallOptions{}
	for _, option := range model.options {
		option(&options)
	}
	if !options.JSONMode {
		t.Fatal("visionJSON() must request JSON mode")
	}
	if options.MaxTokens != textInputVisionMaxTokens {
		t.Fatalf("MaxTokens = %d, want %d", options.MaxTokens, textInputVisionMaxTokens)
	}
	if options.MaxTokens < 4096 {
		t.Fatalf("MaxTokens = %d, want at least 4096", options.MaxTokens)
	}
}

func TestTextInputMetricsCountVLLMCalls(t *testing.T) {
	ctx, metrics := withTextInputMetrics(context.Background())
	model := &textInputVisionRecordingModel{content: `{}`}
	vision := &llmTextInputVision{models: model}
	if _, err := vision.visionJSON(ctx, "test_operation", "return json", screenshotResult{Data: "ZmFrZQ=="}); err != nil {
		t.Fatal(err)
	}
	if calls := metrics.vllmCalls.Load(); calls != 1 {
		t.Fatalf("VLLM calls=%d, want 1", calls)
	}
}

func TestTextInputDurationPerCharacter(t *testing.T) {
	if got := textInputDurationPerCharacter(3*time.Second, 6); got != 500*time.Millisecond {
		t.Fatalf("duration per character=%s, want 500ms", got)
	}
	if got := textInputDurationPerCharacter(time.Second, 0); got != 0 {
		t.Fatalf("zero-character duration=%s, want 0", got)
	}
}

func TestCandidateActionParsesSelectResponse(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{"action":"select","offset":0,"text":"你好"}`}
	vision := &llmTextInputVision{models: model}
	action, err := vision.DecideCandidateAction(context.Background(), screenshotResult{Data: "ZmFrZQ=="}, textInputScreenAnalysisRequest{TargetText: "你好"})
	if err != nil {
		t.Fatalf("DecideCandidateAction() error = %v", err)
	}
	if action.Action != textInputCandidateActionSelect || action.Offset != 0 || action.Text != "你好" {
		t.Fatalf("candidate action = %+v", action)
	}
}

func TestCandidateActionParsesUpWithoutSelectionFields(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{"action":"up","offset":3,"text":"ignored","completes_part":true}`}
	vision := &llmTextInputVision{models: model}
	action, err := vision.DecideCandidateAction(context.Background(), screenshotResult{Data: "ZmFrZQ=="}, textInputScreenAnalysisRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if action.Action != textInputCandidateActionUp || action.Offset != 0 || action.Text != "" || action.CompletesPart {
		t.Fatalf("action=%+v, want clean up action", action)
	}
}

func TestTextInputProbeParsesCompositionResponse(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{"mode":"composition","evidence":"candidate 啊 is visible"}`}
	vision := &llmTextInputVision{models: model}
	analysis, err := vision.ProbeInputMode(context.Background(), screenshotResult{Data: "ZmFrZQ=="}, "ios", focusPointArgs{X: 500, Y: 300})
	if err != nil {
		t.Fatalf("ProbeInputMode() error = %v", err)
	}
	if analysis.Mode != textInputModeComposition || analysis.Evidence != "candidate 啊 is visible" {
		t.Fatalf("analysis = %+v", analysis)
	}
}

func TestTextInputProbeCJKCandidatePopupOverridesASCIIClassification(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{
		"typed_a_visible":true,
		"inline_preedit_visible":false,
		"candidate_popup_visible":true,
		"cjk_candidate_visible":true,
		"onscreen_keyboard_visible":false,
		"mode":"ascii",
		"evidence":"a has a cursor but numbered Chinese candidates are visible below it"
	}`}
	vision := &llmTextInputVision{models: model}
	analysis, err := vision.ProbeInputMode(context.Background(), screenshotResult{Data: "ZmFrZQ=="}, "ios", focusPointArgs{})
	if err != nil {
		t.Fatalf("ProbeInputMode() error = %v", err)
	}
	if analysis.Mode != textInputModeComposition {
		t.Fatalf("mode = %s, want composition for visible CJK candidate popup", analysis.Mode)
	}
}

func TestTextInputProbeCleanupRejectsUnsafeVisibleCharacter(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{"probe_character_visible":true,"cleanup_safe":false,"evidence":"before state is ambiguous"}`}
	vision := &llmTextInputVision{models: model}
	_, err := vision.VerifyProbeCleanup(
		context.Background(),
		screenshotResult{Data: "YmVmb3Jl"},
		screenshotResult{Data: "YWZ0ZXI"},
		"ios",
		focusPointArgs{},
	)
	if err == nil || !strings.Contains(err.Error(), "cleanup is unsafe") {
		t.Fatalf("VerifyProbeCleanup() error=%v, want unsafe cleanup error", err)
	}
}

func TestTextInputProbeCleanupRejectsMissingVisibilityDecision(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{"cleanup_safe":true}`}
	vision := &llmTextInputVision{models: model}
	_, err := vision.VerifyProbeCleanup(
		context.Background(),
		screenshotResult{Data: "YmVmb3Jl"},
		screenshotResult{Data: "YWZ0ZXI"},
		"ios",
		focusPointArgs{},
	)
	if err == nil || !strings.Contains(err.Error(), "missing probe_character_visible") {
		t.Fatalf("VerifyProbeCleanup() error=%v, want missing decision error", err)
	}
}

func TestTextInputPartitionParsesExactResponse(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{"parts":["经理","不是","技术","出身","的"]}`}
	vision := &llmTextInputVision{models: model}
	parts, err := vision.PartitionComposition(context.Background(), "经理不是技术出身的")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"经理", "不是", "技术", "出身", "的"}
	if strings.Join(parts, "") != strings.Join(want, "") || len(parts) != len(want) {
		t.Fatalf("parts=%v, want %v", parts, want)
	}
	options := llms.CallOptions{}
	for _, option := range model.options {
		option(&options)
	}
	if !options.JSONMode || options.MaxTokens < 4096 {
		t.Fatalf("partition options JSONMode=%t MaxTokens=%d", options.JSONMode, options.MaxTokens)
	}
}

func TestTextInputPartitionRejectsLossyOrOversizedParts(t *testing.T) {
	if _, err := parseTextInputPartition("完整目标", `{"parts":["完整"]}`); err == nil {
		t.Fatal("lossy partition must be rejected")
	}
	if _, err := parseTextInputPartition("一二三四五六七", `{"parts":["一二三四五六七"]}`); err == nil {
		t.Fatal("oversized partition must be rejected")
	}
}

func TestAnalyzeScreenRetriesTruncatedVisionJSON(t *testing.T) {
	vision := &retryingTextInputVision{}
	engine := newTextInputEngine(textInputHardwareDeps{
		screenshot: textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision)

	analysis, calls, err := engine.analyzeScreen(context.Background(), "ios", textInputArgs{
		Text: "你好",
	}, nil)
	if err != nil {
		t.Fatalf("analyzeScreen() error = %v", err)
	}
	if !analysis.TargetMatched {
		t.Fatal("analyzeScreen() did not return the successful retry")
	}
	if calls != 2 || vision.calls != 2 {
		t.Fatalf("calls = %d, vision calls = %d; want 2", calls, vision.calls)
	}
}

func TestAnalyzeScreenRetriesFormatEchoWithFeedback(t *testing.T) {
	model := &textInputVisionRecordingModel{contents: []string{
		`{"type": "json_object"}`,
		`{"observed_mode":"composition","field_text":"wei xin","target_matched":false,"composition_pending":true}`,
	}}
	vision := &llmTextInputVision{models: model}
	analysis, err := vision.AnalyzeScreen(context.Background(), screenshotResult{Data: "ZmFrZQ=="}, textInputScreenAnalysisRequest{TargetText: "微信"})
	if err != nil || !analysis.CompositionPending || model.calls != 2 {
		t.Fatalf("analysis=%+v calls=%d err=%v", analysis, model.calls, err)
	}
	retry := fmt.Sprint(model.messages[1][1].Parts[0])
	if !strings.Contains(retry, "missing required fields: observed_mode, target_matched, composition_pending") {
		t.Fatalf("retry prompt lacks validation feedback: %s", retry)
	}
}

func TestAnalyzeScreenRejectsRepeatedFormatEcho(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{"type": "json_object"}`}
	vision := &llmTextInputVision{models: model}
	if _, err := vision.AnalyzeScreen(context.Background(), screenshotResult{Data: "ZmFrZQ=="}, textInputScreenAnalysisRequest{}); err == nil {
		t.Fatal("a response without decision fields must not become an unknown observation")
	}
	if model.calls != textInputVisionParseAttempts {
		t.Fatalf("calls=%d, want %d", model.calls, textInputVisionParseAttempts)
	}
}

func TestPlanCompositionReordersPerCharacterMappingsByTargetIndex(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{
		"mappings":[
			{"index":4,"text":"率","input":"lv"},
			{"index":0,"text":"我","input":"wo"},
			{"index":3,"text":"概","input":"gai"},
			{"index":2,"text":"大","input":"da"},
			{"index":1,"text":"们","input":"men"}
		]
	}`}
	vision := &llmTextInputVision{models: model}
	segments, err := vision.PlanComposition(context.Background(), "我们大概率")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"wo", "men", "da", "gai", "lv"}
	if len(segments) != len(want) {
		t.Fatalf("segments=%v, want %v", segments, want)
	}
	for index := range want {
		if segments[index] != want[index] {
			t.Fatalf("segments=%v, want %v", segments, want)
		}
	}
}

func TestPlanCompositionRejectsCharacterIndexMismatch(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{
		"mappings":[
			{"index":0,"text":"们","input":"men"},
			{"index":1,"text":"我","input":"wo"}
		]
	}`}
	vision := &llmTextInputVision{models: model}
	if _, err := vision.PlanComposition(context.Background(), "我们"); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("PlanComposition() error=%v, want character mismatch", err)
	}
}

func TestPlanCompositionRetriesTruncatedJSONWithLargerTokenBudget(t *testing.T) {
	model := &textInputVisionRecordingModel{contents: []string{
		`{"mappings":[{"index":0,"text":"我","input":"wo"}`,
		`{"mappings":[{"index":0,"text":"我","input":"wo"},{"index":1,"text":"们","input":"men"}]}`,
	}}
	vision := &llmTextInputVision{models: model}
	segments, err := vision.PlanComposition(context.Background(), "我们")
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 2 {
		t.Fatalf("model calls=%d, want 2", model.calls)
	}
	if len(segments) != 2 || segments[0] != "wo" || segments[1] != "men" {
		t.Fatalf("segments=%v, want [wo men]", segments)
	}
	options := llms.CallOptions{}
	for _, option := range model.options {
		option(&options)
	}
	if options.MaxTokens != textInputPlanMaxTokens {
		t.Fatalf("planner MaxTokens=%d, want %d", options.MaxTokens, textInputPlanMaxTokens)
	}
	if options.MaxTokens < 4096 {
		t.Fatalf("planner MaxTokens=%d, want at least 4096", options.MaxTokens)
	}
}
