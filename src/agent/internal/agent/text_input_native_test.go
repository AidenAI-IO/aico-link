package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
)

type nativeInsertionStub struct {
	supported bool
	probeErr  error
	err       error
	calls     int
	text      string
	replace   bool
}

func (p *nativeInsertionStub) SupportsTextInsertion(context.Context) (bool, error) {
	return p.supported, p.probeErr
}
func (p *nativeInsertionStub) InsertText(_ context.Context, text string, replace bool) error {
	p.calls++
	p.text, p.replace = text, replace
	return p.err
}

type nativeVerificationStub struct {
	analysis textInputScreenAnalysis
	err      error
	calls    int
	request  textInputScreenAnalysisRequest
}

func (v *nativeVerificationStub) AnalyzeScreen(_ context.Context, _ screenshotResult, req textInputScreenAnalysisRequest) (textInputScreenAnalysis, error) {
	v.calls++
	v.request = req
	return v.analysis, v.err
}

func TestNativeTextEnterTextNoIMEAndNoReplay(t *testing.T) {
	for _, mode := range []string{"append", "middle_insertion", "replace", "partial_failure", "unverified", "candidate_only", "vision_failure"} {
		t.Run(mode, func(t *testing.T) {
			sink := &nativeInsertionStub{supported: true}
			vision := &nativeVerificationStub{analysis: textInputScreenAnalysis{TargetMatched: true, FieldText: "中文🔋"}}
			if mode == "middle_insertion" {
				vision.analysis.FieldText = "abc中文🔋def"
			}
			if mode == "partial_failure" {
				sink.err = errors.New("lost Accessibility permission")
			}
			if mode == "unverified" {
				vision.analysis.TargetMatched = false
			}
			if mode == "candidate_only" {
				vision.analysis.CompositionPending = true
			}
			if mode == "vision_failure" {
				vision.err = errors.New("model unavailable")
			}
			keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
			focus := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
			engine := newTextInputEngineWithSleep(textInputHardwareDeps{
				textInsertion: sink, keyboardText: keyboard, keyboardTap: keyboard, touchGesture: focus,
				screenshot: textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
			}, vision, testNoWaitSleep)
			tool := &EnterTextTool{engine: engine}
			tool.SetDeviceTypeFunc(func() string { return "iOS" })
			out, err := tool.enterTextInner(context.Background(), `{"text":"中文🔋","focus":{"x":350,"y":940}}`, enterTextOptions{replaceField: mode == "replace"})
			wantOK := mode == "append" || mode == "middle_insertion" || mode == "replace"
			if err != nil || enterTextOutputOK(out, err) != wantOK {
				t.Fatalf("result = %s, %v", out, err)
			}
			if !wantOK && !strings.Contains(out, "Text may already be present") {
				t.Fatalf("failure must warn against duplicate insertion: %s", out)
			}
			if sink.calls != 1 || sink.text != "中文🔋" || sink.replace != (mode == "replace") {
				t.Fatalf("insertion = %+v", sink)
			}
			if len(keyboard.calls) != 0 || len(focus.calls) != 1 {
				t.Fatalf("unexpected IME fallback or refocus: keyboard=%v focus=%v", keyboard.calls, focus.calls)
			}
			if mode != "partial_failure" && (vision.calls != 1 || vision.request.MatchTextSuffix ||
				(vision.request.InsertionBefore != nil) != (mode != "replace")) {
				t.Fatalf("verification = %+v", vision)
			}
		})
	}
}

func TestNativeTextOpenAppSearchAvoidsPhysicalKeys(t *testing.T) {
	for _, mode := range []string{"ascii", "unicode", "spaces", "shorter_term", "partial_failure", "unverified", "probe_failure", "missing_field", "locate_failure"} {
		t.Run(mode, func(t *testing.T) {
			query, wantText, wantCalls := "WeChat", "WeChat", 1
			sink := &nativeInsertionStub{supported: true}
			vision := &nativeVerificationStub{analysis: textInputScreenAnalysis{TargetMatched: true}}
			wantOK := true
			switch mode {
			case "unicode":
				query, wantText = "微信", "微信"
			case "spaces":
				query, wantText = "App Store", "App Store"
			case "shorter_term":
				query, wantText, wantCalls = "Aico Bridge", "Aico", 2
			case "partial_failure":
				sink.err = errors.New("WDA timed out after delivery")
				wantOK = false
			case "unverified":
				vision.analysis.TargetMatched = false
				wantOK = false
			case "probe_failure":
				sink.probeErr = errors.New("WDA capabilities unavailable")
				wantOK, wantCalls = false, 0
			case "missing_field", "locate_failure":
				wantOK, wantCalls = false, 0
			}
			keyboard := &recordingTextInputTool{name: "keyboard_text", out: "error: WDA does not support physical text keys"}
			keys := &recordingTextInputTool{name: "keyboard_tap", out: "error: WDA does not support modifier chords"}
			quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
			touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
			hw := &textInputHardwareDeps{
				textInsertion: sink, deviceTypeFn: func() string { return "ios" },
				keyboardText: keyboard, keyboardTap: keys, touchGesture: touch, quickAction: quick,
				screenshot: textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
			}
			entry := &EnterTextTool{engine: newTextInputEngineWithSleep(*hw, vision, testNoWaitSleep)}
			lookups := 0
			focus := focusPointArgs{X: 500, Y: 572}
			cfg := appSearchOpenFlowConfig{
				hw: hw, vision: vision, entryTool: entry, platform: "ios", searchTerm: query, sleep: testNoWaitSleep,
				checkSearchFieldFn: func(context.Context, screenshotResult, string) (searchFieldCheck, error) {
					if mode == "missing_field" {
						return searchFieldCheck{}, nil
					}
					if mode == "locate_failure" {
						return searchFieldCheck{}, errors.New("search field detection failed")
					}
					return searchFieldCheck{SearchField: &focus}, nil
				},
				findAppTapFn: func(_ context.Context, _ screenshotResult, term string) (bridgeSearchResult, error) {
					lookups++
					return bridgeSearchResult{Found: term == wantText, TapPoint: &focusPointArgs{X: 200, Y: 300}}, nil
				},
				confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
					return bridgeAppOpenResult{Opened: true}, nil
				},
			}
			out, err := runAppSearchOpenFlow(context.Background(), cfg)
			if (err != nil) == wantOK || out.Opened != wantOK {
				t.Fatalf("app search = %+v, %v", out, err)
			}
			if sink.calls != wantCalls || (wantCalls > 0 && (sink.text != wantText || !sink.replace)) {
				t.Fatalf("native insertion = %+v", sink)
			}
			if len(keyboard.calls) != 0 || len(keys.calls) != 0 {
				t.Fatalf("physical keyboard input: text=%v keys=%v", keyboard.calls, keys.calls)
			}
			if wantCalls > 0 {
				var tap struct {
					Point focusPointArgs `json:"point"`
				}
				if len(touch.calls) == 0 {
					t.Fatal("native input did not focus the detected search field")
				}
				if err := json.Unmarshal([]byte(touch.calls[0]), &tap); err != nil || tap.Point != focus {
					t.Fatalf("search field focus = %s, %v", touch.calls[0], err)
				}
			} else if len(touch.calls) != 0 {
				t.Fatalf("touched the screen before native input was ready: %v", touch.calls)
			}
			if len(quick.calls) != 1 || !strings.Contains(quick.calls[0], "spotlight_search") {
				t.Fatalf("quick actions = %v, want only Spotlight", quick.calls)
			}
			if !wantOK && lookups != 0 {
				t.Fatalf("continued search after native input failure: %+v", out)
			}
			if mode == "partial_failure" || mode == "unverified" {
				if !strings.Contains(err.Error(), "Text may already be present") {
					t.Fatalf("missing inspection guidance: %v", err)
				}
			}
		})
	}
}

func TestNativeTextAppSearchKeepsQuickQueryWhenUnsupported(t *testing.T) {
	for _, mode := range []string{"no_provider", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
			keys := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
			hw := &textInputHardwareDeps{keyboardText: keyboard, keyboardTap: keys}
			if mode == "unsupported" {
				hw.textInsertion = &nativeInsertionStub{supported: false}
			}
			vision := &nativeVerificationStub{}
			engine := newTextInputEngineWithSleep(*hw, vision, testNoWaitSleep)
			cfg := appSearchOpenFlowConfig{hw: hw, platform: "ios", entryTool: &EnterTextTool{engine: engine}}
			entered, err := enterSearchQuery(context.Background(), cfg, engine, "WeChat", false, true)
			if err != nil || entered != searchQueryQuick || len(keyboard.calls) != 1 || len(keys.calls) != 2 {
				t.Fatalf("entry=%v err=%v keyboard=%v keys=%v", entered, err, keyboard.calls, keys.calls)
			}
		})
	}
}

func TestNativeTextInvalidAndUnsupportedDoNotTouchField(t *testing.T) {
	sink := &nativeInsertionStub{supported: true}
	focus := &recordingTextInputTool{name: "touch_gesture"}
	engine := newTextInputEngine(textInputHardwareDeps{textInsertion: sink, touchGesture: focus}, &nativeVerificationStub{})
	if result, handled := engine.runNativeTextInsertion(context.Background(), textInputArgs{Text: "\x00"}); !handled || result.OK {
		t.Fatal("invalid input was accepted")
	}
	sink.supported = false
	if _, handled := engine.runNativeTextInsertion(context.Background(), textInputArgs{Text: "中文"}); handled {
		t.Fatal("unsupported bridge did not keep legacy flow")
	}
	if len(focus.calls) != 0 || sink.calls != 0 {
		t.Fatal("field changed before validation")
	}
}

func TestNativeTextVerificationComparesInsertionFrames(t *testing.T) {
	model := &textInputVisionRecordingModel{content: `{"observed_mode":"ascii","target_matched":true,"composition_pending":false,"field_text":"abcXdef"}`}
	vision := &llmTextInputVision{models: model}
	before := screenshotResult{Data: "YmVmb3Jl"}
	after := screenshotResult{Data: "YWZ0ZXI="}
	result, err := vision.AnalyzeScreen(context.Background(), after, textInputScreenAnalysisRequest{
		TargetText: "X", InsertionBefore: &before,
	})
	if err != nil || !result.TargetMatched || result.FieldText != "abcXdef" {
		t.Fatalf("insertion verification = %+v, %v", result, err)
	}
	parts := model.messages[0][1].Parts
	if len(parts) != 3 {
		t.Fatalf("expected prompt and two images, got %d parts", len(parts))
	}
	for i, want := range []string{before.Data, after.Data} {
		image, ok := parts[i+1].(llms.ImageURLContent)
		if !ok || image.URL != "data:image/jpeg;base64,"+want {
			t.Fatalf("frame %d = %+v", i, parts[i+1])
		}
	}
}

func TestNativeTextCaptureFailureDoesNotInsert(t *testing.T) {
	sink := &nativeInsertionStub{supported: true}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		textInsertion: sink,
		touchGesture:  textInputStubTool{out: "ok"},
		screenshot:    textInputStubTool{out: `{"data":""}`},
	}, &nativeVerificationStub{}, testNoWaitSleep)
	result, handled := engine.runNativeTextInsertion(context.Background(), textInputArgs{Text: "X"})
	if !handled || result.OK || sink.calls != 0 || result.TextMayBeInserted {
		t.Fatalf("capture failure must precede insertion: %+v, calls=%d", result, sink.calls)
	}
}
