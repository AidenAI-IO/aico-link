package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

type plannedTextInputVision struct {
	*stubTextInputVision
	plans      map[string][]string
	partitions map[string][]string
}

type concurrentPartPlanningVision struct {
	*stubTextInputVision
	started chan string
	release chan struct{}
}

type concurrentPartPartitionVision struct {
	*stubTextInputVision
	started chan string
	release chan struct{}
}

func (v *concurrentPartPartitionVision) PartitionComposition(ctx context.Context, text string) ([]string, error) {
	v.started <- text
	select {
	case <-v.release:
		runes := []rune(text)
		return []string{string(runes[:3]), string(runes[3:])}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (v *concurrentPartPlanningVision) PlanComposition(ctx context.Context, text string) ([]string, error) {
	v.started <- text
	select {
	case <-v.release:
		if text == "甲" {
			return []string{"jia"}, nil
		}
		return []string{"yi"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type probePlanningConcurrentVision struct {
	planStarted  chan struct{}
	probeStarted chan struct{}
	analyses     []textInputScreenAnalysis
}

func (v *probePlanningConcurrentVision) PlanComposition(ctx context.Context, _ string) ([]string, error) {
	close(v.planStarted)
	select {
	case <-v.probeStarted:
		return []string{"ni"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (v *probePlanningConcurrentVision) ProbeInputMode(ctx context.Context, _ screenshotResult, _ string, _ focusPointArgs) (textInputProbeAnalysis, error) {
	close(v.probeStarted)
	select {
	case <-v.planStarted:
		return textInputProbeAnalysis{Mode: textInputModeComposition}, nil
	case <-ctx.Done():
		return textInputProbeAnalysis{}, ctx.Err()
	}
}

func (v *probePlanningConcurrentVision) AnalyzeScreen(_ context.Context, _ screenshotResult, _ textInputScreenAnalysisRequest) (textInputScreenAnalysis, error) {
	out := v.analyses[0]
	v.analyses = v.analyses[1:]
	return out, nil
}

func (v *probePlanningConcurrentVision) DecideCandidateAction(_ context.Context, _ screenshotResult, _ textInputScreenAnalysisRequest) (textInputCandidateAction, error) {
	return textInputCandidateAction{Action: textInputCandidateActionSelect, Text: "你"}, nil
}

func TestIMEPartsArePlannedConcurrently(t *testing.T) {
	vision := &concurrentPartPlanningVision{
		stubTextInputVision: &stubTextInputVision{},
		started:             make(chan string, 2),
		release:             make(chan struct{}),
	}
	engine := newTextInputEngine(textInputHardwareDeps{}, vision)
	resultCh := make(chan error, 1)
	go func() {
		_, err := engine.planCompositionSegmentsForChunks(context.Background(), []textInputChunk{{text: "甲"}, {text: "乙"}})
		resultCh <- err
	}()
	for index := 0; index < 2; index++ {
		select {
		case <-vision.started:
		case <-time.After(time.Second):
			t.Fatal("IME part planning did not start concurrently")
		}
	}
	close(vision.release)
	if err := <-resultCh; err != nil {
		t.Fatalf("planCompositionSegmentsForChunks() error = %v", err)
	}
}

func TestLongIMEPartsArePartitionedConcurrently(t *testing.T) {
	vision := &concurrentPartPartitionVision{
		stubTextInputVision: &stubTextInputVision{},
		started:             make(chan string, 2),
		release:             make(chan struct{}),
	}
	engine := newTextInputEngine(textInputHardwareDeps{}, vision)
	resultCh := make(chan error, 1)
	go func() {
		_, err := engine.partitionIMEChunks(context.Background(), []textInputChunk{
			{text: "甲乙丙丁戊己"},
			{text: "，", input: ",", ascii: true},
			{text: "庚辛壬癸子丑"},
		})
		resultCh <- err
	}()
	for index := 0; index < 2; index++ {
		select {
		case <-vision.started:
		case <-time.After(time.Second):
			t.Fatal("long IME part partitioning did not start concurrently")
		}
	}
	close(vision.release)
	if err := <-resultCh; err != nil {
		t.Fatalf("partitionIMEChunks() error = %v", err)
	}
}

func TestProbeAndIMEPlanningRunConcurrently(t *testing.T) {
	vision := &probePlanningConcurrentVision{
		planStarted:  make(chan struct{}),
		probeStarted: make(chan struct{}),
		analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeComposition, CompositionPending: true},
			{ObservedMode: textInputModeComposition, FieldText: "你", TargetMatched: true},
		},
	}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, testNoWaitSleep)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := engine.RunSegmented(ctx, textInputArgs{Text: "你"})
	if err != nil || !result.Committed {
		t.Fatalf("RunSegmented() = %+v, %v; want concurrent probe/planning success", result, err)
	}
}

func (v *plannedTextInputVision) PlanComposition(_ context.Context, text string) ([]string, error) {
	return v.plans[text], nil
}

func (v *plannedTextInputVision) PartitionComposition(_ context.Context, text string) ([]string, error) {
	if parts, ok := v.partitions[text]; ok {
		return parts, nil
	}
	return []string{text}, nil
}

func TestLongIMEChunksArePartitionedBeforeCompositionPlanning(t *testing.T) {
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{},
		partitions: map[string][]string{
			"经理不是技术出身的": {"经理", "不是", "技术", "出身", "的"},
		},
	}
	chunks, err := splitTextInputChunks("A经理不是技术出身的，B")
	if err != nil {
		t.Fatal(err)
	}
	partitioned, err := newTextInputEngine(textInputHardwareDeps{}, vision).partitionIMEChunks(context.Background(), chunks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "经理", "不是", "技术", "出身", "的", "，", "B"}
	got := make([]string, len(partitioned))
	for index := range partitioned {
		got[index] = partitioned[index].text
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("partitioned parts=%v, want %v", got, want)
	}
}

func TestSplitTextInputChunksPreservesASCIIAndIMERuns(t *testing.T) {
	chunks, err := splitTextInputChunks("A你好-42世界")
	if err != nil {
		t.Fatalf("splitTextInputChunks() error = %v", err)
	}
	want := []textInputChunk{
		{text: "A", input: "A", ascii: true},
		{text: "你好", ascii: false},
		{text: "-", input: "-", ascii: true},
		{text: "42", input: "42", ascii: true},
		{text: "世界", ascii: false},
	}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("chunks = %#v, want %#v", chunks, want)
	}
}

func TestSplitTextInputChunksMakesEachSpaceAStandalonePart(t *testing.T) {
	chunks, err := splitTextInputChunks("HDMI 4k60 test")
	if err != nil {
		t.Fatal(err)
	}
	want := []textInputChunk{
		{text: "HDMI", input: "HDMI", ascii: true},
		{text: " ", input: " ", ascii: true, space: true},
		{text: "4k60", input: "4k60", ascii: true},
		{text: " ", input: " ", ascii: true, space: true},
		{text: "test", input: "test", ascii: true},
	}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("chunks=%#v, want %#v", chunks, want)
	}
}

func TestRunSegmentedTypesSpaceWithoutVisionVerification(t *testing.T) {
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{
		{ObservedMode: textInputModeASCII, FieldText: "a"},
	}}
	kbText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	kbTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		keyboardTap:  kbTap,
		keyboardText: kbText,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, testNoWaitSleep)

	result, err := engine.RunSegmented(context.Background(), textInputArgs{
		Text: "hello test", Focus: focusPointArgs{X: 10, Y: 10},
	})
	if err != nil || !result.Committed {
		t.Fatalf("RunSegmented()=%+v err=%v", result, err)
	}
	if len(kbText.calls) != 3 || len(kbTap.calls) != 2 || !strings.Contains(kbTap.calls[0], `"meta"`) || !strings.Contains(kbTap.calls[0], `"z"`) || !strings.Contains(kbTap.calls[1], "space") {
		t.Fatalf("keyboard_text=%v keyboard_tap=%v", kbText.calls, kbTap.calls)
	}
}

func TestTextInputProbeWaitsForConfiguredSettleDelayBeforeCapture(t *testing.T) {
	var delays []time.Duration
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}, func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	})

	mode, _, err := engine.probeTextInputMode(context.Background(), "ios", focusPointArgs{})
	if err != nil || mode != textInputModeASCII {
		t.Fatalf("probeTextInputMode() mode=%s err=%v", mode, err)
	}
	if len(delays) == 0 || delays[0] != textInputProbeSettleDelay {
		t.Fatalf("probe delays=%v, want first delay %s", delays, textInputProbeSettleDelay)
	}
}

func TestTextInputProbeSupportsWindowsAndLinuxPlatforms(t *testing.T) {
	for _, platform := range []string{"windows", "linux"} {
		t.Run(platform, func(t *testing.T) {
			engine := newTextInputEngineWithSleep(textInputHardwareDeps{
				keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
				keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
				screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
			}, &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}, func(_ context.Context, _ time.Duration) error {
				return nil
			})

			mode, _, err := engine.probeTextInputMode(context.Background(), platform, focusPointArgs{})
			if err != nil || mode != textInputModeASCII {
				t.Fatalf("probeTextInputMode(%q) mode=%s err=%v", platform, mode, err)
			}
		})
	}
}

type probeVerifyingVision struct {
	analyses          []textInputScreenAnalysis
	probeStillVisible bool
}

func (v *probeVerifyingVision) ProbeInputMode(_ context.Context, _ screenshotResult, _ string, _ focusPointArgs) (textInputProbeAnalysis, error) {
	if len(v.analyses) == 0 {
		return textInputProbeAnalysis{Mode: textInputModeASCII, Evidence: "stub default"}, nil
	}
	out := v.analyses[0]
	v.analyses = v.analyses[1:]
	return textInputProbeAnalysis{Mode: out.ObservedMode, Evidence: "stub analysis"}, nil
}

func (v *probeVerifyingVision) VerifyProbeCleanup(_ context.Context, _, _ screenshotResult, _ string, _ focusPointArgs) (bool, error) {
	return v.probeStillVisible, nil
}

func (v *probeVerifyingVision) PartitionComposition(_ context.Context, text string) ([]string, error) {
	return []string{text}, nil
}

func (v *probeVerifyingVision) AnalyzeScreen(_ context.Context, _ screenshotResult, _ textInputScreenAnalysisRequest) (textInputScreenAnalysis, error) {
	return textInputScreenAnalysis{ObservedMode: textInputModeASCII}, nil
}

func (v *probeVerifyingVision) DecideCandidateAction(_ context.Context, _ screenshotResult, _ textInputScreenAnalysisRequest) (textInputCandidateAction, error) {
	return textInputCandidateAction{Action: textInputCandidateActionNone}, nil
}

func TestTextInputProbeVerificationSendsBackspaceWhenCharacterStillVisible(t *testing.T) {
	// Create a vision that reports probe character is still visible
	vision := &probeVerifyingVision{
		analyses:          []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}},
		probeStillVisible: true,
	}

	keyboardTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		keyboardTap:  keyboardTap,
		keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, func(_ context.Context, _ time.Duration) error {
		return nil
	})

	mode, _, err := engine.probeTextInputMode(context.Background(), "ios", focusPointArgs{})
	if err != nil || mode != textInputModeASCII {
		t.Fatalf("probeTextInputMode() mode=%s err=%v", mode, err)
	}

	// Verify that backspace was sent after undo
	undoIndex, firstBackspaceIndex := -1, -1
	for index, call := range keyboardTap.calls {
		if strings.Contains(call, "meta") && strings.Contains(call, "z") {
			undoIndex = index
		}
		if strings.Contains(call, "backspace") && firstBackspaceIndex < 0 {
			firstBackspaceIndex = index
		}
	}
	if undoIndex < 0 {
		t.Fatal("expected undo keys to be sent")
	}
	if firstBackspaceIndex < 0 {
		t.Fatal("expected backspace to be sent when probe character is still visible")
	}
	if undoIndex > firstBackspaceIndex {
		t.Fatal("expected undo keys before backspace")
	}
}

func TestTextInputProbeVerificationSkipsBackspaceWhenCharacterRemoved(t *testing.T) {
	// Create a vision that reports probe character was successfully removed
	vision := &probeVerifyingVision{
		analyses:          []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}},
		probeStillVisible: false,
	}

	keyboardTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		keyboardTap:  keyboardTap,
		keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, func(_ context.Context, _ time.Duration) error {
		return nil
	})

	mode, _, err := engine.probeTextInputMode(context.Background(), "ios", focusPointArgs{})
	if err != nil || mode != textInputModeASCII {
		t.Fatalf("probeTextInputMode() mode=%s err=%v", mode, err)
	}

	// Verify that backspace was NOT sent
	var foundBackspace bool
	for _, call := range keyboardTap.calls {
		if strings.Contains(call, "backspace") {
			foundBackspace = true
		}
	}
	if foundBackspace {
		t.Fatal("backspace should not be sent when probe character is successfully removed")
	}
}

func TestTextInputKeyboardKeysForPlatforms(t *testing.T) {
	tests := []struct {
		platform  string
		imeSwitch []string
		selectAll []string
		undo      []string
	}{
		{platform: "ios", imeSwitch: []string{"capslock"}, selectAll: []string{"meta", "a"}, undo: []string{"meta", "z"}},
		{platform: "mac", imeSwitch: []string{"ctrl", "space"}, selectAll: []string{"meta", "a"}, undo: []string{"meta", "z"}},
		{platform: "android", imeSwitch: []string{"KEYCODE_LANGUAGE_SWITCH"}, selectAll: []string{"ctrl", "a"}, undo: []string{"ctrl", "z"}},
		{platform: "windows", imeSwitch: []string{"alt", "shift"}, selectAll: []string{"ctrl", "a"}, undo: []string{"ctrl", "z"}},
		{platform: "linux", imeSwitch: []string{"ctrl", "space"}, selectAll: []string{"ctrl", "a"}, undo: []string{"ctrl", "z"}},
	}
	for _, test := range tests {
		t.Run(test.platform, func(t *testing.T) {
			gotSwitch, err := textInputKeyboardKeysForIMESwitch(test.platform)
			if err != nil || strings.Join(gotSwitch, ",") != strings.Join(test.imeSwitch, ",") {
				t.Errorf("IME switch keys = %v, %v; want %v, nil", gotSwitch, err, test.imeSwitch)
			}
			gotSelectAll, err := textInputKeyboardKeysForSelectAll(test.platform)
			if err != nil || strings.Join(gotSelectAll, ",") != strings.Join(test.selectAll, ",") {
				t.Errorf("select all keys = %v, %v; want %v, nil", gotSelectAll, err, test.selectAll)
			}
			gotUndo, err := textInputKeyboardKeysForUndo(test.platform)
			if err != nil || strings.Join(gotUndo, ",") != strings.Join(test.undo, ",") {
				t.Errorf("undo keys = %v, %v; want %v, nil", gotUndo, err, test.undo)
			}
		})
	}
}

func TestSplitTextInputChunksMakesEachPunctuationMarkAStandalonePart(t *testing.T) {
	const target = "你好我是Aiden，是一个硬件智能Agent助手，现在我在混合输入各种内容，例如英文Hello和数字12345"
	chunks, err := splitTextInputChunks(target)
	if err != nil {
		t.Fatalf("splitTextInputChunks() error = %v", err)
	}
	want := []textInputChunk{
		{text: "你好我是", ascii: false},
		{text: "Aiden", input: "Aiden", ascii: true},
		{text: "，", input: ",", ascii: true},
		{text: "是一个硬件智能", ascii: false},
		{text: "Agent", input: "Agent", ascii: true},
		{text: "助手", ascii: false},
		{text: "，", input: ",", ascii: true},
		{text: "现在我在混合输入各种内容", ascii: false},
		{text: "，", input: ",", ascii: true},
		{text: "例如英文", ascii: false},
		{text: "Hello", input: "Hello", ascii: true},
		{text: "和数字", ascii: false},
		{text: "12345", input: "12345", ascii: true},
	}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("chunks = %#v, want %#v", chunks, want)
	}
}

func TestSplitTextInputChunksSeparatesASCIIPunctuation(t *testing.T) {
	if !isTextInputPunctuation('(') {
		t.Fatal("opening parenthesis must be treated as punctuation")
	}
	chunks, err := splitTextInputChunks("hello, world! (test)")
	if err != nil {
		t.Fatal(err)
	}
	want := []textInputChunk{
		{text: "hello", input: "hello", ascii: true},
		{text: ",", input: ",", ascii: true},
		{text: " ", input: " ", ascii: true, space: true},
		{text: "world", input: "world", ascii: true},
		{text: "!", input: "!", ascii: true},
		{text: " ", input: " ", ascii: true, space: true},
		{text: "(", input: "(", ascii: true},
		{text: "test", input: "test", ascii: true},
		{text: ")", input: ")", ascii: true},
	}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("chunks=%#v, want %#v", chunks, want)
	}
}

func TestTextInputEngineRunSegmentedUsesKeyboardAndIMEInOrder(t *testing.T) {
	keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeASCII, FieldText: "a"},
			{ObservedMode: textInputModeComposition, CompositionPending: true},
			{ObservedMode: textInputModeComposition, FieldText: "A你好", TargetMatched: true},
		}, actions: []textInputCandidateAction{{Action: textInputCandidateActionSelect, Offset: 0, Text: "你好"}}},
		plans: map[string][]string{"你好": {"ni", "hao"}},
	}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: keyboard,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, testNoWaitSleep)

	result, err := engine.RunSegmented(context.Background(), textInputArgs{
		Text: "A你好B", Focus: focusPointArgs{X: 10, Y: 10},
	})
	if err != nil {
		t.Fatalf("RunSegmented() error = %v", err)
	}
	if !result.Committed || result.FieldText != "A你好" {
		t.Fatalf("result = %+v, want ordered input", result)
	}
	wantCalls := []string{jsonString(map[string]string{"text": "a"}), jsonString(map[string]string{"text": "A"}), jsonString(map[string]string{"text": "ni"}), jsonString(map[string]string{"text": "hao"}), jsonString(map[string]string{"text": "B"})}
	if !reflect.DeepEqual(keyboard.calls, wantCalls) {
		t.Fatalf("keyboard calls = %#v, want %#v", keyboard.calls, wantCalls)
	}
}

func TestTextInputEngineRunSegmentedPlansIMESegmentsInternally(t *testing.T) {
	keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeASCII, FieldText: "a"},
			{ObservedMode: textInputModeComposition, FieldText: "A你好", TargetMatched: true},
		}},
		// The planner may return a pinyin phrase in one array entry; the engine
		// must split it before passing it to keyboard_text.
		plans: map[string][]string{"你好": {"ni hao"}},
	}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: keyboard,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, testNoWaitSleep)

	result, err := engine.RunSegmented(context.Background(), textInputArgs{
		Text: "A你好B", Focus: focusPointArgs{X: 10, Y: 10},
	})
	if err != nil || !result.Committed {
		t.Fatalf("RunSegmented() = %+v, %v; want internally planned committed result", result, err)
	}
	if want := []string{jsonString(map[string]string{"text": "a"}), jsonString(map[string]string{"text": "A"}), jsonString(map[string]string{"text": "ni"}), jsonString(map[string]string{"text": "hao"}), jsonString(map[string]string{"text": "B"})}; !reflect.DeepEqual(keyboard.calls, want) {
		t.Fatalf("keyboard calls = %#v, want %#v", keyboard.calls, want)
	}
}

func TestRunSegmentedVerifiesCurrentIMEPartAtCommittedFieldSuffix(t *testing.T) {
	keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeComposition, CompositionPending: true},
			{
				ObservedMode:       textInputModeComposition,
				FieldText:          "Aiden 是一个接在手机上的硬件 Agent。它通过 USB 把自己模拟成",
				CompositionPending: true,
			},
			{
				ObservedMode: textInputModeASCII,
				FieldText:    "Aiden 是一个接在手机上的硬件 Agent。它通过 USB 把自己模拟成键盘",
			},
		}, actions: []textInputCandidateAction{{Action: textInputCandidateActionSelect, Text: "键盘"}}},
		plans: map[string][]string{"模拟成键盘": {"mo", "ni", "cheng", "jian", "pan"}},
	}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		pointerMode:  "absolute",
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: keyboard,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, testNoWaitSleep)

	result, err := engine.RunSegmented(context.Background(), textInputArgs{
		Text: "模拟成键盘",
	})
	if err != nil || !result.Committed {
		t.Fatalf("RunSegmented()=%+v err=%v, want suffix verification success", result, err)
	}
}

func TestTextInputEngineDoesNotVerifyDirectPartsOrUsePointer(t *testing.T) {
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeComposition, CompositionPending: true},
			{ObservedMode: textInputModeComposition, FieldText: "你", TargetMatched: true},
		}},
		plans: map[string][]string{"你": {"ni"}},
	}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		pointerMode:  "absolute",
		touchGesture: touch,
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, testNoWaitSleep)

	result, err := engine.RunSegmented(context.Background(), textInputArgs{
		Text: "你A", Focus: focusPointArgs{X: 10, Y: 10},
	})
	if err != nil || !result.Committed {
		t.Fatalf("RunSegmented() = %+v, %v; want committed result", result, err)
	}
	if len(touch.calls) != 0 {
		t.Fatalf("touch=%v, want no pointer input", touch.calls)
	}
}

func TestEnterTextToolSchemaKeepsIMESegmentsInternal(t *testing.T) {
	props, ok := (&EnterTextTool{}).ArgsSchema()["properties"].(map[string]any)
	if !ok {
		t.Fatal("enter_text schema properties missing")
	}
	if _, found := props["segments"]; found {
		t.Fatal("enter_text must not expose IME segments")
	}
	if _, found := props["platform"]; found {
		t.Fatal("enter_text must infer the platform from runtime device state")
	}
	if _, found := props["max_attempts"]; found {
		t.Fatal("enter_text must not expose max_attempts")
	}
	if _, found := props["send_after_commit"]; found {
		t.Fatal("enter_text must not expose send_after_commit")
	}
}

func TestEnterTextToolRejectsMissingNullAndIncompleteFocus(t *testing.T) {
	tool := &EnterTextTool{engine: &textInputEngine{}}
	for _, input := range []string{
		`{"text":"hello"}`,
		`{"text":"hello","focus":null}`,
		`{"text":"hello","focus":{"x":500}}`,
		`{"text":"hello","focus":{"y":500}}`,
	} {
		out, err := tool.Call(context.Background(), input)
		if err != nil {
			t.Fatalf("Call(%s) error = %v", input, err)
		}
		if !strings.Contains(out, `"ok": false`) || !strings.Contains(out, "valid JSON containing text and focus") {
			t.Fatalf("Call(%s) output = %q, want invalid focus failure", input, out)
		}
	}
}

func TestTextInputPlatformDefaultsToDeviceType(t *testing.T) {
	if got := (textInputHardwareDeps{pointerMode: "absolute"}).platform(); got != "ios" {
		t.Fatalf("absolute pointer mode platform = %q, want default device_type ios", got)
	}
	if got := (textInputHardwareDeps{pointerMode: "touchscreen"}).platform(); got != "ios" {
		t.Fatalf("touchscreen pointer mode platform = %q, want default device_type ios", got)
	}
}

func TestTextInputPlatformUsesDeviceTypeProvider(t *testing.T) {
	if got := (textInputHardwareDeps{
		pointerMode:  "absolute",
		deviceTypeFn: func() string { return "Android" },
	}).platform(); got != "android" {
		t.Fatalf("device_type platform = %q, want android", got)
	}
	if got := (textInputHardwareDeps{
		pointerMode:  "touchscreen",
		deviceTypeFn: func() string { return "macOS" },
	}).platform(); got != "mac" {
		t.Fatalf("device_type platform = %q, want mac", got)
	}
}

func TestTextInputPlatformIgnoresInvalidDeviceTypeProvider(t *testing.T) {
	if got := (textInputHardwareDeps{
		pointerMode:  "absolute",
		deviceTypeFn: func() string { return "invalid" },
	}).platform(); got != "ios" {
		t.Fatalf("invalid device_type platform = %q, want default ios", got)
	}
}

func TestEnterTextResultContainsOnlySuccessStatus(t *testing.T) {
	encoded := enterTextToolResultString(textInputResult{
		OK: true, Committed: true, FieldText: "secret",
	})
	var fields map[string]any
	if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields["ok"] != true {
		t.Fatalf("public enter_text success result = %s, want only ok=true", encoded)
	}
}

func TestEnterTextOutputOKReadsCompactResult(t *testing.T) {
	if !enterTextOutputOK(`{"ok":true}`, nil) {
		t.Fatal("ok result must be recognized")
	}
	if enterTextOutputOK(`{"ok":false,"suggestion":"retry"}`, nil) {
		t.Fatal("failed result must not be recognized as successful")
	}
	if enterTextOutputOK(`{"ok":true}`, context.Canceled) {
		t.Fatal("call error must override output success")
	}
}

func TestEnterTextFailureResultContainsOnlyStatusAndSuggestion(t *testing.T) {
	encoded := enterTextToolResultString(textInputResult{
		Reason: "internal diagnostic", FieldText: "wrong text",
	})
	var fields map[string]any
	if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["ok"] != false || strings.TrimSpace(fields["suggestion"].(string)) == "" {
		t.Fatalf("public enter_text failure result = %s, want only ok=false and suggestion", encoded)
	}
	for _, hidden := range []string{"internal diagnostic", "secret", "wrong text", "vlm_calls", "reason", "field_text"} {
		if strings.Contains(encoded, hidden) {
			t.Fatalf("public enter_text failure result leaked %q: %s", hidden, encoded)
		}
	}
}

func TestTextInputEngineRunSegmentedUsesProbeStateInsteadOfASCIIVerification(t *testing.T) {
	keyboardTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeComposition, FieldText: "a", CompositionPending: true},
			{ObservedMode: textInputModeComposition, FieldText: "A你", TargetMatched: true},
		}},
		plans: map[string][]string{"你": {"ni"}},
	}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		keyboardTap:  keyboardTap,
		keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, testNoWaitSleep)

	result, err := engine.RunSegmented(context.Background(), textInputArgs{
		Text: "A你", Focus: focusPointArgs{X: 10, Y: 10},
	})
	if err != nil || !result.Committed {
		t.Fatalf("RunSegmented() = %+v, %v; want committed result", result, err)
	}
	if want := []string{
		jsonString(map[string][]string{"keys": {"meta", "z"}}),
		jsonString(map[string]any{"keys": []string{"capslock"}}),
		jsonString(map[string]any{"keys": []string{"capslock"}}),
	}; !reflect.DeepEqual(keyboardTap.calls, want) {
		t.Fatalf("keyboard_tap calls = %#v, want %#v", keyboardTap.calls, want)
	}
}

func TestTextInputEngineRunSegmentedDoesNotVerifyFinalASCIIPart(t *testing.T) {
	keyboardTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeComposition, FieldText: "a", CompositionPending: true},
			{ObservedMode: textInputModeComposition, FieldText: "你", TargetMatched: true},
		}},
		plans: map[string][]string{"你": {"ni"}},
	}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		keyboardTap:  keyboardTap,
		keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, vision, testNoWaitSleep)

	result, err := engine.RunSegmented(context.Background(), textInputArgs{
		Text: "你A", Focus: focusPointArgs{X: 10, Y: 10},
	})
	if err != nil || !result.Committed {
		t.Fatalf("RunSegmented() = %+v, %v; want committed result", result, err)
	}
	if want := []string{
		jsonString(map[string][]string{"keys": {"meta", "z"}}),
		jsonString(map[string]any{"keys": []string{"capslock"}}),
	}; !reflect.DeepEqual(keyboardTap.calls, want) {
		t.Fatalf("keyboard_tap calls = %#v, want %#v", keyboardTap.calls, want)
	}
}

func TestTextInputEngineTypesFullWidthPunctuationInIMEMode(t *testing.T) {
	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	keyboardTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		pointerMode:  "absolute",
		keyboardTap:  keyboardTap,
		keyboardText: keyboardText,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}, testNoWaitSleep)

	result, err := engine.RunSegmented(context.Background(), textInputArgs{Text: "A，B"})
	if err != nil || !result.Committed {
		t.Fatalf("RunSegmented() = %+v, %v", result, err)
	}
	if want := []string{
		jsonString(map[string]string{"text": "a"}),
		jsonString(map[string]string{"text": "A"}),
		jsonString(map[string]string{"text": ","}),
		jsonString(map[string]string{"text": "B"}),
	}; !reflect.DeepEqual(keyboardText.calls, want) {
		t.Fatalf("keyboard_text calls = %#v, want %#v", keyboardText.calls, want)
	}
	if want := []string{
		jsonString(map[string][]string{"keys": {"meta", "z"}}),
		jsonString(map[string]any{"keys": []string{"capslock"}}),
		jsonString(map[string]any{"keys": []string{"capslock"}}),
	}; !reflect.DeepEqual(keyboardTap.calls, want) {
		t.Fatalf("keyboard_tap calls = %#v, want %#v", keyboardTap.calls, want)
	}
}

func TestEnterTextToolPrefersAvailableBridge(t *testing.T) {
	pb := newTestPhoneBridge(t)
	pb.platform = "android"
	pb.connected = true
	pb.appState = "background"
	tool := &EnterTextTool{bridgeTool: &textInputBridge{hw: &textInputHardwareDeps{pointerMode: "absolute"}, bridgeFn: func() *PhoneBridge { return pb }}}
	tool.SetDeviceTypeFunc(func() string { return "Android" })
	if !tool.bridgeAvailable(textInputArgs{Text: "ASCII is bridged too"}) {
		t.Fatal("available Android bridge should be preferred before local entry")
	}
	tool.SetDeviceTypeFunc(func() string { return "iOS" })
	if tool.bridgeAvailable(textInputArgs{Text: "hello"}) {
		t.Fatal("iOS device_type state should not select an Android bridge clipboard route")
	}
}

func TestEnterTextToolFallsBackToLocalAfterBridgeFailure(t *testing.T) {
	pb := newTestPhoneBridge(t)
	pb.platform = "android"
	pb.connected = true
	pb.appState = "background"
	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	hw := &textInputHardwareDeps{
		pointerMode:  "touchscreen",
		deviceTypeFn: func() string { return "Android" },
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: keyboardText,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}
	bridgeWrites := 0
	tool := &EnterTextTool{
		engine: newFastTextInputEngine(*hw, vision),
		bridgeTool: &textInputBridge{
			hw:       hw,
			vision:   vision,
			bridgeFn: func() *PhoneBridge { return pb },
			clipboardWriteFn: func(context.Context, *PhoneBridge, string) error {
				bridgeWrites++
				return context.Canceled
			},
		},
	}
	tool.SetDeviceTypeFunc(hw.deviceTypeFn)

	out, err := tool.Call(context.Background(), `{"text":"Aiden","focus":{"x":500,"y":120}}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if bridgeWrites != 1 {
		t.Fatalf("bridge writes = %d, want public enter_text to attempt Bridge once", bridgeWrites)
	}
	if len(keyboardText.calls) == 0 {
		t.Fatal("keyboard_text was not called for local fallback after attempted Bridge path")
	}
	if !enterTextOutputOK(out, nil) {
		t.Fatalf("Call() output = %s, want successful local fallback", out)
	}
}

func TestEnterTextToolFallsBackToLocalAfterDynamicIslandRestoreFailure(t *testing.T) {
	pb := newTestPhoneBridge(t)
	pb.platform = "ios"
	pb.connected = true
	pb.appState = "background"
	pb.returnEntry = "dynamic_island"
	pb.returnEntrySeen = true
	pb.returnEntryOK = true

	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	hw := &textInputHardwareDeps{
		pointerMode:  "touchscreen",
		deviceTypeFn: func() string { return "iOS" },
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: keyboardText,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}
	restoreAttempts := 0
	restorer := NewPhoneBridgeRestorer(pb, nil)
	restorer.tapReturnEntry = func(context.Context, PhoneBridgeStatus) error {
		restoreAttempts++
		return context.DeadlineExceeded
	}
	tool := &EnterTextTool{
		engine: newFastTextInputEngine(*hw, vision),
		bridgeTool: &textInputBridge{
			hw:       hw,
			vision:   vision,
			bridgeFn: func() *PhoneBridge { return pb },
			restorer: restorer,
		},
	}
	tool.SetDeviceTypeFunc(hw.deviceTypeFn)
	tool.iosKeyboardIsolation = newTestIOSKeyboardIsolationController(&[]string{})

	out, err := tool.Call(context.Background(), `{"text":"Aiden","focus":{"x":500,"y":120}}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !enterTextOutputOK(out, nil) {
		t.Fatalf("Call() output = %s, want successful HID fallback after Dynamic Island failure", out)
	}
	if len(keyboardText.calls) == 0 {
		t.Fatal("keyboard_text was not called after Dynamic Island restore failure")
	}
	if restoreAttempts == 0 {
		t.Fatal("Dynamic Island restore route was not attempted")
	}
}

func TestEnterTextToolLocalPathRestoresIsolationOnSuccessAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keyboardOut string
		wantOK      bool
	}{
		{name: "success", keyboardOut: "ok", wantOK: true},
		{name: "probe failure", keyboardOut: `{"ok":false,"message":"keyboard unavailable"}`, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			controller := newTestIOSKeyboardIsolationController(&events)
			keyboard := &recordingTextInputTool{name: "keyboard_text", out: tc.keyboardOut}
			touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
			engine := newTextInputEngineWithSleep(textInputHardwareDeps{
				pointerMode:  "absolute",
				touchGesture: touch,
				keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
				keyboardText: keyboard,
				screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
			}, &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}, testNoWaitSleep)
			tool := &EnterTextTool{engine: engine, iosKeyboardIsolation: controller}

			out, err := tool.Call(context.Background(), `{"text":"x","focus":{"x":10,"y":10}}`)
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			if gotOK := strings.Contains(out, `"ok": true`); gotOK != tc.wantOK {
				t.Fatalf("output = %s, want ok=%v", out, tc.wantOK)
			}
			if want := []string{"isolate", "restore"}; !reflect.DeepEqual(events, want) {
				t.Fatalf("isolation events = %v, want %v", events, want)
			}
			if len(touch.calls) != 0 {
				t.Fatalf("touch_gesture calls = %v, want none", touch.calls)
			}
		})
	}
}

func TestEnterTextToolIOSLocalPathRequiresIsolation(t *testing.T) {
	keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		pointerMode:  "absolute",
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: keyboard,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, &stubTextInputVision{}, testNoWaitSleep)
	tool := &EnterTextTool{engine: engine}

	out, err := tool.Call(context.Background(), `{"text":"x","focus":{"x":10,"y":10}}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if out != jsonString(enterTextToolResult{OK: false, Suggestion: "Enable iOS keyboard isolation, then retry enter_text."}) {
		t.Fatalf("output = %s", out)
	}
	if len(keyboard.calls) != 0 {
		t.Fatalf("keyboard_text calls = %v, want no probe before isolation", keyboard.calls)
	}
}

func TestEnterTextFailureSuggestionUsesTypedKeyboardBackendFailure(t *testing.T) {
	backendSuggestion := "The input backend could not execute keyboard input. Refocusing will not fix this; report the input connection or unsupported operation."
	if got := enterTextFailureSuggestion(textInputResult{Reason: "remote keyboard input failed"}); got == backendSuggestion {
		t.Fatal("an untyped reason string must not be classified as a keyboard backend failure")
	}
	if got := enterTextFailureSuggestion(textInputResult{BackendFailure: true}); got != backendSuggestion {
		t.Fatalf("typed backend failure suggestion = %q", got)
	}
	if got := enterTextFailureSuggestion(textInputResult{BackendFailure: true, Reason: "context deadline exceeded"}); !strings.Contains(got, "times out") {
		t.Fatalf("timeout suggestion = %q; want retry guidance", got)
	}
}

func TestTextInputEngineMarksOnlyKeyboardExecutionFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		want bool
	}{
		{name: "backend", code: CodeToolExecutionFailed, want: true},
		{name: "unavailable", code: CodeModuleUnavailable, want: true},
		{name: "invalid input", code: CodeInvalidArguments, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newTextInputEngineWithSleep(textInputHardwareDeps{
				deviceTypeFn: func() string { return "Android" },
				keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
				keyboardText: &contextToolErrorStub{name: "keyboard_text", toolErr: NewToolError(tc.code, "unfamiliar error")},
				screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
			}, &stubTextInputVision{}, testNoWaitSleep)
			result, err := engine.RunSegmented(context.Background(), textInputArgs{
				Text: "x", Focus: focusPointArgs{X: 10, Y: 10},
			})
			if err != nil || result.BackendFailure != tc.want {
				t.Fatalf("RunSegmented() = %+v, %v; want backend failure %t", result, err, tc.want)
			}
		})
	}
}

func TestEnterTextToolBridgeIOSBypassesLocalIsolationRequirement(t *testing.T) {
	keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	engine := newTextInputEngineWithSleep(textInputHardwareDeps{
		pointerMode:  "absolute",
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: keyboard,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}, &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}, testNoWaitSleep)
	tool := &EnterTextTool{engine: engine, allowIOSKeyboardIsolationBypass: true}

	out, err := tool.Call(context.Background(), `{"text":"x","focus":{"x":10,"y":10}}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !enterTextOutputOK(out, nil) {
		t.Fatalf("Call() output = %s, want bridge fallback to proceed without local isolation", out)
	}
}
