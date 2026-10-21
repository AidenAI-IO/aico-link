package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

// decisionServer is a fake System One endpoint that answers every request with
// the next scripted body and records the questions it was asked.
type decisionServer struct {
	*httptest.Server
	mu        sync.Mutex
	bodies    []string
	requests  []map[string]any
	requested chan struct{}
}

func newDecisionServer(t *testing.T, bodies ...string) *decisionServer {
	t.Helper()
	s := &decisionServer{bodies: bodies, requested: make(chan struct{}, 16)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(data, &req)
		s.mu.Lock()
		s.requests = append(s.requests, req)
		body := `{}`
		if len(s.bodies) > 0 {
			body = s.bodies[0]
			if len(s.bodies) > 1 {
				s.bodies = s.bodies[1:]
			}
		}
		s.mu.Unlock()
		_, _ = io.WriteString(w, body)
		s.requested <- struct{}{}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *decisionServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *decisionServer) waitForRequest(t *testing.T) {
	t.Helper()
	select {
	case <-s.requested:
	case <-time.After(5 * time.Second):
		t.Fatal("decision model was not asked")
	}
}

func testDecisionAdvisor(server *decisionServer) *decisionAdvisor {
	advisor := &decisionAdvisor{}
	advisor.setConfig(func() DecisionModelConfig {
		return DecisionModelConfig{APIKey: "ts-key", BaseURL: server.URL}
	})
	return advisor
}

func decisionTestScreenshot(t *testing.T) screenshotResult {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 800))
	for y := 0; y < 800; y += 4 {
		for x := 0; x < 400; x += 4 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return screenshotResult{Width: 400, Height: 800, Format: "jpeg", Data: base64.StdEncoding.EncodeToString(buf.Bytes())}
}

func TestDecisionDisabledKeepsMainModel(t *testing.T) {
	server := newDecisionServer(t)
	advisor := &decisionAdvisor{}
	advisor.setConfig(func() DecisionModelConfig {
		return DecisionModelConfig{APIKey: "k", BaseURL: server.URL}
	})
	model := &textInputVisionRecordingModel{content: `{"mode":"ascii"}`}
	vision := &llmTextInputVision{models: model, decider: advisor}

	got, err := vision.ProbeInputMode(context.Background(), decisionTestScreenshot(t), "iOS", focusPointArgs{X: 500, Y: 300})
	if err != nil || got.Mode != textInputModeASCII || model.calls != 1 {
		t.Fatalf("mode = %v, err = %v, llm calls = %d", got.Mode, err, model.calls)
	}
	if server.requestCount() != 0 {
		t.Fatal("disabled decision model was called")
	}
}

func TestDecisionActiveUsesConfidentAnswer(t *testing.T) {
	server := newDecisionServer(t, `{"answers":{"mode":{"type":"choice","choice":"composition","confidence":0.95}}}`)
	model := &textInputVisionRecordingModel{content: `{"mode":"ascii"}`, delay: 3 * time.Second}
	vision := &llmTextInputVision{models: model, decider: testDecisionAdvisor(server)}

	started := time.Now()
	got, err := vision.ProbeInputMode(context.Background(), decisionTestScreenshot(t), "iOS", focusPointArgs{X: 500, Y: 300})
	if err != nil || got.Mode != textInputModeComposition {
		t.Fatalf("mode = %v, err = %v", got.Mode, err)
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("waited %s for the main model after a confident answer", elapsed)
	}
	req := server.requests[0]
	if req["model"] != "jev-preview" || req["assets"] == nil {
		t.Fatalf("request = %v", req)
	}
}

func TestDecisionActiveFallsBackOnLowConfidenceAndErrors(t *testing.T) {
	server := newDecisionServer(t, `{"answers":{"mode":{"type":"choice","choice":"composition","confidence":0.4}}}`)
	model := &textInputVisionRecordingModel{content: `{"mode":"ascii"}`}
	vision := &llmTextInputVision{models: model, decider: testDecisionAdvisor(server)}
	got, err := vision.ProbeInputMode(context.Background(), decisionTestScreenshot(t), "iOS", focusPointArgs{})
	if err != nil || got.Mode != textInputModeASCII || model.calls != 1 {
		t.Fatalf("low confidence: mode = %v, err = %v, llm calls = %d", got.Mode, err, model.calls)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnavailableForLegalReasons)
	}))
	defer failing.Close()
	advisor := &decisionAdvisor{}
	advisor.setConfig(func() DecisionModelConfig {
		return DecisionModelConfig{APIKey: "k", BaseURL: failing.URL}
	})
	model = &textInputVisionRecordingModel{content: `{"opened":true}`}
	vision = &llmTextInputVision{models: model, decider: advisor}
	result, usedLLM, err := decide(context.Background(), advisor, appOpenDecision(func(ctx context.Context) (bridgeAppOpenResult, error) {
		return confirmSearchOpenAppLLM(ctx, vision, decisionTestScreenshot(t), "微信")
	}, decisionTestScreenshot(t), "微信"))
	if err != nil || !result.Opened || !usedLLM {
		t.Fatalf("error fallback: %+v, usedLLM = %v, err = %v", result, usedLLM, err)
	}
}

func TestDecisionScreenAnalysisMergesConfidentFlagsKeepingFieldText(t *testing.T) {
	// Main model: the field reads 你好 but composition is pending (it read the
	// IME next-word predictions as candidates). Jev is confident it is
	// committed and matched.
	server := newDecisionServer(t, `{"answers":{"matched":{"type":"noul","noul":0.99},"pending":{"type":"noul","noul":0.01}}}`)
	model := &textInputVisionRecordingModel{content: `{"observed_mode":"ascii","field_text":"你好","target_matched":false,"composition_pending":true}`}
	vision := &llmTextInputVision{models: model, decider: testDecisionAdvisor(server)}
	got, err := vision.AnalyzeScreen(context.Background(), decisionTestScreenshot(t), textInputScreenAnalysisRequest{Platform: "iOS", TargetText: "你好"})
	if err != nil || !got.TargetMatched || got.CompositionPending || got.FieldText != "你好" || model.calls != 1 {
		t.Fatalf("analysis = %+v, err = %v, llm calls = %d", got, err, model.calls)
	}

	// An unsure decision model leaves the main model's answer untouched.
	unsure := newDecisionServer(t, `{"answers":{"matched":{"type":"noul","noul":0.6},"pending":{"type":"noul","noul":0.4}}}`)
	vision.decider = testDecisionAdvisor(unsure)
	got, err = vision.AnalyzeScreen(context.Background(), decisionTestScreenshot(t), textInputScreenAnalysisRequest{Platform: "iOS", TargetText: "你好"})
	if err != nil || got.TargetMatched || !got.CompositionPending {
		t.Fatalf("unsure analysis = %+v, err = %v", got, err)
	}

	// A stalled decision model delays the main model's answer by at most the
	// grace period.
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the canceled request.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer stalled.Close()
	vision.decider = &decisionAdvisor{}
	vision.decider.setConfig(func() DecisionModelConfig {
		return DecisionModelConfig{APIKey: "k", BaseURL: stalled.URL}
	})
	started := time.Now()
	got, err = vision.AnalyzeScreen(context.Background(), decisionTestScreenshot(t), textInputScreenAnalysisRequest{Platform: "iOS", TargetText: "你好"})
	if err != nil || !got.CompositionPending || time.Since(started) > decisionMergeGrace+time.Second {
		t.Fatalf("stalled analysis = %+v, err = %v after %v", got, err, time.Since(started))
	}
}

func TestDecisionAppSearchTapsBoxCenter(t *testing.T) {
	server := newDecisionServer(t, `{"answers":{"found":{"type":"noul","noul":0.97},`+
		`"where":{"type":"box","box":{"top":0.2,"bottom":0.3,"left":0.1,"right":0.3},"confidence":0.9}}}`)
	model := &textInputVisionRecordingModel{content: `{"found":false}`, delay: 3 * time.Second}
	vision := &llmTextInputVision{models: model, decider: testDecisionAdvisor(server)}

	result, usedLLM, err := decide(context.Background(), vision.decider, appSearchDecision(vision, decisionTestScreenshot(t), "微信"))
	if err != nil || usedLLM || !result.Found || result.TapPoint == nil {
		t.Fatalf("result = %+v, usedLLM = %v, err = %v", result, usedLLM, err)
	}
	if result.TapPoint.X != 200 || result.TapPoint.Y != 250 {
		t.Fatalf("tap = %+v, want the box center 200,250", *result.TapPoint)
	}

	// A found answer whose box is not confident is left to the main model.
	server = newDecisionServer(t, `{"answers":{"found":{"type":"noul","noul":0.97},`+
		`"where":{"type":"box","box":{"top":0.2,"bottom":0.3,"left":0.1,"right":0.3},"confidence":0.3}}}`)
	vision.decider = testDecisionAdvisor(server)
	_, usedLLM, _ = decide(context.Background(), vision.decider, appSearchDecision(vision, decisionTestScreenshot(t), "微信"))
	if !usedLLM {
		t.Fatal("low-confidence box was used without the main model")
	}

	// A confident "not found" is never trusted: the main model decides.
	server = newDecisionServer(t, `{"answers":{"found":{"type":"noul","noul":0.01},`+
		`"where":{"type":"box","box":{"top":0.2,"bottom":0.3,"left":0.1,"right":0.3},"confidence":0.9}}}`)
	vision.decider = testDecisionAdvisor(server)
	vision.models = &textInputVisionRecordingModel{content: `{"found":true,"tap_point":{"x":150,"y":250}}`}
	result, usedLLM, err = decide(context.Background(), vision.decider, appSearchDecision(vision, decisionTestScreenshot(t), "微信"))
	if err != nil || !usedLLM || !result.Found {
		t.Fatalf("result = %+v, usedLLM = %v, err = %v", result, usedLLM, err)
	}
}

func TestConfirmSearchLeftComparesBothScreenshots(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{"left_search":true,"reason":"settings search page"}`}
	vision := &llmTextInputVision{models: model}
	shot := decisionTestScreenshot(t)

	result, err := confirmSearchLeftLLM(context.Background(), vision, shot, shot, "设置")
	if err != nil || !result.Opened || result.Reason != "settings search page" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if len(model.messages) != 1 {
		t.Fatalf("calls = %d", len(model.messages))
	}
	images := 0
	for _, msg := range model.messages[0] {
		for _, part := range msg.Parts {
			if _, ok := part.(llms.ImageURLContent); ok {
				images++
			}
		}
	}
	if images != 2 {
		t.Fatalf("images sent = %d, want the search page and the screen after the tap", images)
	}
}

func TestDecisionOpenAIProviderLeavesTapPointsToMainModel(t *testing.T) {
	server := newDecisionServer(t, `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"found","probability":0.97}]}`)
	advisor := &decisionAdvisor{}
	advisor.setConfig(func() DecisionModelConfig {
		return DecisionModelConfig{Provider: DecisionProviderOpenAI, APIKey: "sk", BaseURL: server.URL + "/v1"}
	})
	model := &textInputVisionRecordingModel{content: `{"found":true,"tap_point":{"x":200,"y":250}}`}
	vision := &llmTextInputVision{models: model, decider: advisor}

	result, usedLLM, err := decide(context.Background(), advisor, appSearchDecision(vision, decisionTestScreenshot(t), "微信"))
	if err != nil || !usedLLM || result.TapPoint == nil || result.TapPoint.X != 200 {
		t.Fatalf("result = %+v, usedLLM = %v, err = %v", result, usedLLM, err)
	}
	server.waitForRequest(t)
	server.mu.Lock()
	questions, _ := server.requests[0]["questions"].([]any)
	server.mu.Unlock()
	if server.requestCount() != 1 || len(questions) != 1 || server.requests[0]["model"] != "gpt-6-luna" {
		t.Fatalf("openai request = %v", server.requests)
	}
}

func TestDecisionAppOpenNeverTrustsNotOpened(t *testing.T) {
	// A confident "not opened" is never trusted: the main model decides.
	server := newDecisionServer(t, `{"answers":{"opened":{"type":"noul","noul":0.01}}}`)
	result, usedLLM, err := decide(context.Background(), testDecisionAdvisor(server), appOpenDecision(
		func(context.Context) (bridgeAppOpenResult, error) { return bridgeAppOpenResult{Opened: true}, nil },
		decisionTestScreenshot(t), "地图"))
	if err != nil || !usedLLM || !result.Opened {
		t.Fatalf("result = %+v, usedLLM = %v, err = %v", result, usedLLM, err)
	}
	server.waitForRequest(t)
}

func TestDecisionAppOpenTrustsOnlyOpened(t *testing.T) {
	server := newDecisionServer(t, `{"answers":{"opened":{"type":"noul","noul":0.97}}}`)
	result, usedLLM, err := decide(context.Background(), testDecisionAdvisor(server), appOpenDecision(
		func(ctx context.Context) (bridgeAppOpenResult, error) {
			// A slow main model: the confident answer must cancel it.
			<-ctx.Done()
			return bridgeAppOpenResult{}, ctx.Err()
		}, decisionTestScreenshot(t), "地图"))
	if err != nil || usedLLM || !result.Opened {
		t.Fatalf("result = %+v, usedLLM = %v, err = %v", result, usedLLM, err)
	}
}

func TestDecisionRaceUnconfidentAnswerAddsNoLatency(t *testing.T) {
	// The decision model is slow and unsure; the main model answers first and
	// its answer is returned without waiting for the decision model.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(w, `{"answers":{"opened":{"type":"noul","noul":0.5}}}`)
	}))
	defer slow.Close()
	advisor := &decisionAdvisor{}
	advisor.setConfig(func() DecisionModelConfig {
		return DecisionModelConfig{APIKey: "k", BaseURL: slow.URL}
	})
	started := time.Now()
	result, usedLLM, err := decide(context.Background(), advisor, appOpenDecision(func(context.Context) (bridgeAppOpenResult, error) {
		time.Sleep(50 * time.Millisecond)
		return bridgeAppOpenResult{Opened: true}, nil
	}, decisionTestScreenshot(t), "地图"))
	if err != nil || !usedLLM || !result.Opened {
		t.Fatalf("result = %+v, usedLLM = %v, err = %v", result, usedLLM, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("waited %s; an unconfident decision model must not delay the main model", elapsed)
	}
}
