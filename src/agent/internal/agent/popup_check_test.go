package agent

import (
	"context"
	"strings"
	"testing"
)

func TestHandlePopupTapsOnlySafeDialogs(t *testing.T) {
	for _, tc := range []struct {
		popup    popupCheck
		tapped   bool
		blocking bool
	}{
		{popupCheck{Kind: popupKindAd, Summary: "开屏广告 跳过", SafeTap: &focusPointArgs{X: 900, Y: 80}}, true, false},
		{popupCheck{Kind: popupKindPastePermission, SafeTap: &focusPointArgs{X: 700, Y: 560}}, true, false},
		// A tap point off the 0-1000 grid is not tapped; the dialog is reported.
		{popupCheck{Kind: popupKindAd, SafeTap: &focusPointArgs{X: 1400, Y: 80}}, false, true},
		// A tap point on a consent dialog must never be used.
		{popupCheck{Kind: popupKindConsent, Summary: "隐私政策 同意/不同意", SafeTap: &focusPointArgs{X: 700, Y: 900}}, false, true},
		{popupCheck{Kind: popupKindSystemPermission, Summary: "允许通知"}, false, true},
		{popupCheck{Kind: "weird"}, false, false},
		{popupCheck{Kind: popupKindNone}, false, false},
	} {
		touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
		got, _, err := handlePopup(context.Background(), &textInputHardwareDeps{touchGesture: touch}, nil,
			func(context.Context, screenshotResult, string) (popupCheck, error) { return tc.popup, nil },
			screenshotResult{}, "android", popupMomentAppOpen)
		if err != nil {
			t.Fatal(err)
		}
		if (len(touch.calls) == 1) != tc.tapped || got.Handled != tc.tapped || got.blocking() != tc.blocking {
			t.Fatalf("%s: taps=%v popup=%+v", tc.popup.Kind, touch.calls, got)
		}
	}
}

func TestOpenAppReportsBlockingDialogAfterOpen(t *testing.T) {
	h := newAndroidSearchEntryHarness(&androidSearchEntryStore{}, "phone-1", androidSearchCheck{SearchOpen: true})
	cfg := h.cfg
	cfg.hw.quickAction = &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
	cfg.searchTerm, cfg.exactEntryOnly = "Files", true
	cfg.entryTool = &EnterTextTool{engine: h.engine}
	cfg.findAppTapFn = func(context.Context, screenshotResult, string) (bridgeSearchResult, error) {
		return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 200, Y: 300}}, nil
	}
	cfg.confirmAppOpenFn = func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
		return bridgeAppOpenResult{Opened: true}, nil
	}
	cfg.checkPopupFn = func(context.Context, screenshotResult, string) (popupCheck, error) {
		return popupCheck{Kind: popupKindConsent, Summary: "文件管理 隐私政策：同意 / 不同意"}, nil
	}
	result, err := runAppSearchOpenFlow(context.Background(), cfg)
	if err != nil || !result.Opened || result.Popup == nil || result.Popup.Kind != popupKindConsent {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if last := result.Steps[len(result.Steps)-1]; !strings.Contains(last, "consent dialog is open") {
		t.Fatalf("steps=%q", result.Steps)
	}
}
