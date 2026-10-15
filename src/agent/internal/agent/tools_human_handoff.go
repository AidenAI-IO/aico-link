package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type UserActionHandler func(context.Context, HumanHandoffRequest) error

type userActionHandlerContextKey struct{}

func WithUserActionHandler(ctx context.Context, handler UserActionHandler) context.Context {
	return context.WithValue(ctx, userActionHandlerContextKey{}, handler)
}

func userActionHandlerFromContext(ctx context.Context) UserActionHandler {
	if ctx == nil {
		return nil
	}
	handler, _ := ctx.Value(userActionHandlerContextKey{}).(UserActionHandler)
	return handler
}

// HumanHandoffTool allows the agent to request human intervention when it encounters
// situations that require human judgment, input, or actions beyond its capabilities.
//
// Inspired by Open-AutoGLM's take_over mechanism, this tool enables the agent to
// gracefully hand off control to a human operator in scenarios such as:
// - Authentication/login requiring credentials
// - CAPTCHA or verification code input
// - Sensitive operations (payment, banking)
// - Ambiguous situations requiring human judgment
// - Black screen or inaccessible UI elements
//
// Unlike blocking callback approaches, this tool returns immediately with a structured
// handoff marker. The agent will naturally wait for the user's next message in the
// conversation flow.
type HumanHandoffTool struct{}

// HumanHandoffRequest contains details about why human intervention is needed.
type HumanHandoffRequest struct {
	// Reason is a brief category of why handoff is needed
	Reason string `json:"reason"`

	// Details provides specific context about the situation
	Details string `json:"details"`

	// SuggestedAction optionally suggests what the human should do
	SuggestedAction string `json:"suggested_action,omitempty"`
}

// HandoffReason defines common categories for human handoff requests
type HandoffReason string

const (
	HandoffReasonAuthentication         HandoffReason = "authentication"
	HandoffReasonLoginMethodSelection   HandoffReason = "login_method_selection"
	HandoffReasonCAPTCHA                HandoffReason = "captcha"
	HandoffReasonVerification           HandoffReason = "verification_code"
	HandoffReasonSensitive              HandoffReason = "sensitive_operation"
	HandoffReasonRedirectConfirmation   HandoffReason = "redirect_confirmation"
	HandoffReasonPermissionConfirmation HandoffReason = "permission_confirmation"
	HandoffReasonBlackScreen            HandoffReason = "black_screen"
	HandoffReasonAmbiguous              HandoffReason = "ambiguous_situation"
	HandoffReasonUnsupported            HandoffReason = "unsupported_action"
	HandoffReasonStuck                  HandoffReason = "stuck"
	HandoffReasonOther                  HandoffReason = "other"
)

const toolUserActionStep = "request_user_action"

var validHandoffReasonValues = []string{
	string(HandoffReasonAuthentication),
	string(HandoffReasonLoginMethodSelection),
	string(HandoffReasonCAPTCHA),
	string(HandoffReasonVerification),
	string(HandoffReasonSensitive),
	string(HandoffReasonRedirectConfirmation),
	string(HandoffReasonPermissionConfirmation),
	string(HandoffReasonBlackScreen),
	string(HandoffReasonAmbiguous),
	string(HandoffReasonUnsupported),
	string(HandoffReasonStuck),
	string(HandoffReasonOther),
}

func isValidHandoffReason(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, valid := range validHandoffReasonValues {
		if value == valid {
			return true
		}
	}
	return false
}

// NewHumanHandoffTool creates a new HumanHandoffTool
func NewHumanHandoffTool() *HumanHandoffTool {
	return &HumanHandoffTool{}
}

func (t *HumanHandoffTool) Name() string {
	return toolUserActionStep
}

func (t *HumanHandoffTool) Description() string {
	return `Request user action for credentials, CAPTCHA, verification codes, sensitive operations, or when stuck. ` +
		`suggested_action should tell the human what to do without asking them to share private credentials in chat. ` +
		`This tool returns immediately with a handoff marker; wait for the user to continue, then verify the result with a screenshot before proceeding.`
}

func (t *HumanHandoffTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"reason": stringEnumArgSchema(
			"Why human handoff is needed.",
			string(HandoffReasonAuthentication),
			string(HandoffReasonLoginMethodSelection),
			string(HandoffReasonCAPTCHA),
			string(HandoffReasonVerification),
			string(HandoffReasonSensitive),
			string(HandoffReasonRedirectConfirmation),
			string(HandoffReasonPermissionConfirmation),
			string(HandoffReasonBlackScreen),
			string(HandoffReasonAmbiguous),
			string(HandoffReasonUnsupported),
			string(HandoffReasonStuck),
			string(HandoffReasonOther),
		),
		"details":          stringArgSchema("Specific context explaining the situation."),
		"suggested_action": stringArgSchema("Optional instruction for what the human should do."),
	}, "reason", "details")
}

func (t *HumanHandoffTool) Call(ctx context.Context, input string) (string, error) {
	var args struct {
		Reason          string `json:"reason"`
		Details         string `json:"details"`
		SuggestedAction string `json:"suggested_action"`
	}

	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}

	// Validate required fields. Trim first so whitespace-only values are
	// rejected as missing rather than slipping through to the reason allowlist.
	if strings.TrimSpace(args.Reason) == "" {
		return "", fmt.Errorf("reason is required")
	}
	if strings.TrimSpace(args.Details) == "" {
		return "", fmt.Errorf("details is required")
	}

	// Normalize reason
	reason := strings.ToLower(strings.TrimSpace(args.Reason))
	if !isValidHandoffReason(reason) {
		return "", fmt.Errorf("invalid reason: %q, must be one of: %s", args.Reason, strings.Join(validHandoffReasonValues, ", "))
	}

	details := strings.TrimSpace(args.Details)
	suggestedAction := strings.TrimSpace(args.SuggestedAction)
	request := HumanHandoffRequest{Reason: reason, Details: details, SuggestedAction: suggestedAction}
	if handler := userActionHandlerFromContext(ctx); handler != nil {
		if err := handler(ctx, request); err != nil {
			return "", err
		}
	}

	status := "HUMAN_HANDOFF_REQUESTED"
	if userActionHandlerFromContext(ctx) != nil {
		status = "USER_ACTION_REQUESTED"
	}
	response := struct {
		Status          string `json:"status"`
		Reason          string `json:"reason"`
		Details         string `json:"details"`
		SuggestedAction string `json:"suggested_action,omitempty"`
	}{
		Status:          status,
		Reason:          reason,
		Details:         details,
		SuggestedAction: suggestedAction,
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
