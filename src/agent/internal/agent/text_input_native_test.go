package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
)

type nativeInsertionStub struct {
	supported bool
	err       error
	calls     int
	text      string
	replace   bool
}

func (p *nativeInsertionStub) SupportsTextInsertion(context.Context) (bool, error) {
	return p.supported, nil
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
