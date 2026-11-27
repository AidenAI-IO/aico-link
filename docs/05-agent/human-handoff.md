---
sidebar_position: 14
---

# Human Handoff

Human handoff lets the Agent pause a task when the next step requires private input, user approval, human judgment, or an action the available tools cannot safely perform.

The handoff is conversational rather than blocking. The tool returns immediately, the Agent explains what the user should do, and the current turn ends. After the user reports completion, the Agent observes the screen again before continuing.

## Interaction Flow

1. The Agent observes a step that requires human action.
2. It calls `request_user_action` with a reason, details, and an optional suggested action.
3. The Agent tells the user what to do without requesting private credentials in chat.
4. The user completes the action directly on the phone and replies when finished.
5. The Agent takes a fresh screenshot or uses another appropriate observation to verify the new state.
6. The task continues in a new conversation turn.

## Handoff Reasons

| Reason | Use |
| --- | --- |
| `authentication` | A password or another private credential must be entered. |
| `login_method_selection` | The user must choose an account or login method. |
| `captcha` | A CAPTCHA requires human completion. |
| `verification_code` | An SMS, email, authenticator, or other verification code is required. |
| `sensitive_operation` | Payment, deletion, permission, or another consequential action needs approval. |
| `redirect_confirmation` | The user must approve leaving an app or opening an external destination. |
| `permission_confirmation` | An operating-system permission prompt requires user review. |
| `black_screen` | The screen cannot be observed, commonly because an app blocks capture. |
| `ambiguous_situation` | Multiple valid choices require user preference or judgment. |
| `unsupported_action` | The requested step is outside the available tool or platform capabilities. |
| `stuck` | Observation and safe fallback attempts cannot make progress. |
| `other` | Another situation genuinely requires human intervention. |

## Tool Contract

Tool name:

```text
request_user_action
```

Input:

```json
{
  "reason": "authentication",
  "details": "The App Store login screen requires the account password.",
  "suggested_action": "Enter the password on the phone, submit the form, and tell me when it is complete."
}
```

Fields:

| Field | Required | Description |
| --- | --- | --- |
| `reason` | Yes | One of the reason values listed above. |
| `details` | Yes | Specific context explaining why the Agent cannot safely continue. |
| `suggested_action` | No | A concise instruction for the action the user should perform. |

The tool returns a structured marker immediately:

```json
{
  "status": "HUMAN_HANDOFF_REQUESTED",
  "reason": "authentication",
  "details": "The App Store login screen requires the account password.",
  "suggested_action": "Enter the password on the phone, submit the form, and tell me when it is complete."
}
```

For a background task, the same tool is mode-aware and returns
`USER_ACTION_REQUESTED` to the orchestration layer. The task stays `running`
while its agent loop ends. The realtime foreground agent presents the action to
the user; after the user confirms completion it calls `response_user_action`.
Ordinary legacy runs continue to return `HUMAN_HANDOFF_REQUESTED`.

## Agent Behavior

- Explain why help is needed and identify the exact action to perform.
- Ask the user to enter passwords, verification codes, or payment details directly on the controlled device, never in chat.
- End the turn after requesting handoff instead of repeatedly polling or retrying the blocked action.
- When the user returns, observe before acting. A statement such as “done” is not sufficient evidence by itself.
- If screenshot capture remains unavailable, use other visible or system evidence where possible and state any remaining uncertainty.
- Do not use human handoff as the first response to a recoverable tool error. Try safe observation and supported fallback paths first.

## Examples

### Authentication

```json
{
  "reason": "authentication",
  "details": "The login screen requires a password that the Agent does not have.",
  "suggested_action": "Enter the password on the phone and submit the login form."
}
```

### Sensitive confirmation

```json
{
  "reason": "sensitive_operation",
  "details": "The checkout screen is ready to confirm a payment of ¥299.",
  "suggested_action": "Verify the recipient and amount, then approve or cancel the payment."
}
```

### Screen capture blocked

```json
{
  "reason": "black_screen",
  "details": "The banking app returns a black screenshot, so the current fields and controls cannot be verified.",
  "suggested_action": "Complete the required step manually in the banking app and return to a screen that can be observed."
}
```

## Verification

Run the focused unit tests from `src/agent`:

```bash
go test ./internal/agent -run TestHumanHandoff
```

The implementation and reason allowlist are defined in `internal/agent/tools_human_handoff.go`.
