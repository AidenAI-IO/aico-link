package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"aiden-agent/internal/logging"
)

// Dialog kinds the popup check reports.
const (
	popupKindNone             = "none"
	popupKindPastePermission  = "paste_permission"
	popupKindAd               = "ad"
	popupKindSystemPermission = "system_permission"
	popupKindLogin            = "login"
	popupKindConsent          = "consent"
	popupKindOther            = "other"
)

// popupSafeKinds are dismissed without asking the Agent: allowing a paste the
// automation itself just made, and closing an advertisement. Permission,
// login, privacy consent and anything else grant or change something on the
// user's behalf, so they are only reported.
var popupSafeKinds = map[string]bool{popupKindPastePermission: true, popupKindAd: true}

// Moments the popup check runs at; they only shape the prompt.
const (
	popupMomentPaste   = "after pasting into a text field left it empty"
	popupMomentAppOpen = "right after an app opened"
)

// popupCheck is a dialog found over the current screen.
type popupCheck struct {
	Kind    string          `json:"kind"`
	Summary string          `json:"summary"`
	SafeTap *focusPointArgs `json:"safe_tap"`
	// Handled is set when a safe dialog was tapped away.
	Handled bool `json:"-"`
}

// blocking reports a dialog that is open and was not handled.
func (p popupCheck) blocking() bool {
	return p.Kind != "" && p.Kind != popupKindNone && !p.Handled
}

func normalizePopup(p popupCheck) popupCheck {
	p.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
	switch p.Kind {
	case popupKindPastePermission, popupKindAd, popupKindSystemPermission, popupKindLogin, popupKindConsent, popupKindOther:
	default:
		p.Kind = popupKindNone
	}
	p.Summary = strings.TrimSpace(p.Summary)
	if !popupSafeKinds[p.Kind] {
		p.SafeTap = nil
	}
	// A tap point off the 0-1000 grid would miss; report the dialog instead.
	if p.SafeTap != nil && (p.SafeTap.X < 0 || p.SafeTap.X > 1000 || p.SafeTap.Y < 0 || p.SafeTap.Y > 1000) {
		p.SafeTap = nil
	}
	return p
}

// handlePopup checks shot for a dialog and taps a safe one away. Check
// failures are logged and read as no dialog: the check only helps, and a task
// must not fail because of it.
func handlePopup(ctx context.Context, hw *textInputHardwareDeps, vision textInputVision, hook func(context.Context, screenshotResult, string) (popupCheck, error), shot screenshotResult, platform, moment string) (popupCheck, int, error) {
	var (
		popup popupCheck
		calls int
		err   error
	)
	if hook != nil {
		popup, err = hook(ctx, shot, platform)
	} else if modelVision, ok := vision.(*llmTextInputVision); ok && modelVision != nil {
		popup, err = checkPopupLLM(ctx, modelVision, shot, platform, moment)
		calls = 1
	} else {
		return popupCheck{}, 0, nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return popupCheck{}, calls, ctxErr
		}
		logging.Warnf("agent", "popup", "popup check failed (%s): %v", moment, err)
		return popupCheck{}, calls, nil
	}
	popup = normalizePopup(popup)
	if popup.Kind == popupKindNone {
		return popup, calls, nil
	}
	if popup.SafeTap == nil || hw == nil || hw.touchGesture == nil {
		logging.Infof("agent", "popup", "dialog open (%s): kind=%s summary=%q", moment, popup.Kind, popup.Summary)
		return popup, calls, nil
	}
	logging.Infof("agent", "popup", "dismissing %s dialog (%s) at %.0f,%.0f: %q", popup.Kind, moment, popup.SafeTap.X, popup.SafeTap.Y, popup.Summary)
	if _, err := callTextInputTool(ctx, hw.touchGesture, jsonString(map[string]any{
		"type":  "tap",
		"point": map[string]any{"x": popup.SafeTap.X, "y": popup.SafeTap.Y},
	})); err != nil {
		return popup, calls, fmt.Errorf("tap %s dialog: %w", popup.Kind, err)
	}
	popup.Handled = true
	return popup, calls, nil
}

func buildPopupPrompt(platform, moment string) string {
	return strings.TrimSpace(fmt.Sprintf(`Analyze this %s phone screenshot taken %s.
Decide whether a dialog, sheet, or overlay is covering the app, and what kind it is.
Return JSON only:
{
  "kind": "none",
  "summary": "short description of the dialog and its buttons",
  "safe_tap": null
}

kind is one of:
- "none": no dialog; the app screen is usable. Toasts, banners, and keyboard bars that do not block the screen are none.
- "paste_permission": asks whether to allow pasting or clipboard access, such as iOS "Allow Paste"/"允许粘贴" and "Don't Allow Paste"/"不允许粘贴".
- "ad": an advertisement, promotion, or splash ad with a close, skip, or X button (关闭, 跳过).
- "system_permission": asks for a permission such as notifications, location, camera, contacts, or tracking.
- "login": asks the user to sign in or register.
- "consent": a privacy policy, user agreement, or terms dialog (隐私政策, 用户协议, 同意).
- "other": any other blocking dialog, such as an update prompt, rating request, or error.

Rules:
- safe_tap is set only for "paste_permission" (the button that allows the paste, never "Don't Allow Paste"/"不允许粘贴") and for "ad" (its close, skip, or X button). It is the button center in normalized 0-1000 coordinates. For every other kind, safe_tap is null.
- summary names the dialog and its buttons in a few words.`, platform, moment))
}

func checkPopupLLM(ctx context.Context, vision *llmTextInputVision, shot screenshotResult, platform, moment string) (popupCheck, error) {
	raw, err := vision.visionDecisionJSON(ctx, "popup_check", buildPopupPrompt(platform, moment), []string{"kind"}, shot)
	if err != nil {
		return popupCheck{}, err
	}
	var popup popupCheck
	if err := json.Unmarshal([]byte(raw), &popup); err != nil {
		return popupCheck{}, fmt.Errorf("parse popup check: %w", err)
	}
	return normalizePopup(popup), nil
}
