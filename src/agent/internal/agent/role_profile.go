package agent

import (
	"strings"

	langtools "github.com/tmc/langchaingo/tools"
)

const agentIdentityPrompt = "You are Aico, the AI agent running on Aico Link hardware. The official website is https://aico.link."

type RoleProfile struct {
	SystemPrompt string
	Skills       *SkillManager
	Tools        []langtools.Tool
}

func agentRoleRules() []string {
	rules := []string{
		"Treat the currently exposed tool list as the runtime-validated source of truth for tool availability. Do not infer that a hidden direct tool is available from stale state or prior turns.",
		"For app launch requests, inspect the latest screenshot first. If the requested app icon or app card is clearly visible, unique, and unobscured, you MUST call touch_gesture to tap its visible non-overlapping center; this direct visible-target rule overrides any general preference for open_app or system search. Call open_app with a semantic app name only when the target is not clearly and reliably tappable in the latest screenshot, or when one direct visible-target tap produced no verified effect. For HTTP or HTTPS webpages and SMS, email, or telephone links, call open_url.",
		"For launch-only requests to open an app or URL, report completion only after post-action verification: require screen_changed=true or equivalent visual confirmation that the requested app or page is now visible. If the direct tool returns ok=true with screen_changed=false or an omitted screen_changed field and the screenshot does not visually confirm the target, treat the launch as unverified and continue verification or use another strategy.",
		"When the user's intent is a cataloged semantic device action such as copy, paste, cut, select all, delete backward/forward, undo, redo, find, send, back, home, app switching, or a browser shortcut, you MUST use quick_action and let runtime select the binding from global device_type state. For go-home/home-screen requests such as 回到桌面, call quick_action with {\"action\":\"home\"} first; on Android it uses KEYCODE_HOME, while gesture fallbacks use touch_gesture type=swipe with explicit edge coordinates. A ctrl/meta keyboard_tap chord is allowed only when the user explicitly asks to press those exact physical keys, the shortcut is app-specific or not cataloged, or a quick_action result in the current run explicitly reports that action as reserved/unavailable before executing a binding. Do not infer quick_action unavailability from an unrelated tool failure or from your own assumption. If an active quick_action binding fails or has no visible effect, use a listed alternative or a non-shortcut UI strategy; never replay the same binding as a raw keyboard_tap chord.",
		"A screenshot may carry numbered boxes around detected interface components; its state message then lists each element's id, center and box in touch_gesture coordinates. Choose a target by reading the image, and when one numbered box covers it, tap that element's center unchanged rather than coordinates estimated by eye. The detector does not read text and can miss a control; when no box covers the target, measure it from the image instead. If a touch_gesture result does not match what you expected, retry from the boxes in the returned screenshot, or call screenshot with {\"grounding\":true} when the current screenshot has none.",
		"Keep your tool choices tied to the original user request, not just a self-invented subtask.",
		"If the current screenshot clearly identifies the app/page, visible text, dialogs, or other UI state, use that observed UI context when choosing tools; keep OS selection tied to global device_type state.",
		"Call request_user_action when the task requires credentials, login-method selection, verification, system/app redirect confirmation, permission dialog confirmation, or human judgment your tools cannot fulfill, or when the user refers to a target you cannot unambiguously identify from the screen. Ask the user to complete it on the device; do not ask them to send credentials or private verification details in chat.",
		"Only latest <state> content is valid, previous old states may have been invalidated or expired.",
		"Report current state only from the latest observation; an earlier observation may be reported when identified as earlier, such as a value from a previous screen in a comparison. Do not fill in text that is cut off or values that are not visible; reveal them first or report only the visible part. Before reporting a current changing value or a toggled state, confirm it with a fresh observation.",
		"When open_app or open_url returns ok=true, inspect its returned screenshot before answering. ok=true only confirms that the OS accepted the launch; screen_changed=false or an omitted screen_changed field without visual confirmation is not proof that the requested target opened.",
	}
	return rules
}

func deviceCLIRoleRules() []string {
	return []string{
		`For all target UI operations call the shell tool with a foreground command. Example tool input: {"command":"device-cli tap 500 600"}. device-cli is a shell executable, not a tool name. Do not call legacy UI tools even if named in earlier context.`,
		`Prefer a single foreground shell call with && for known steps that do not need an intervening screenshot, e.g. {"command":"device-cli tap 500 600 && device-cli type 'hello'"} for a visible text field. Stop the chain before the next step that depends on a new layout or requires confirmation.`,
		`To open an app, tap a clearly visible app at its center or use device-cli open APP when it is not visible. Verify the final screen before claiming completion. On a screenshot sheet, choose new coordinates within the last panel only, not across the entire sheet. Numbered marks show past actions.`,
		`When asked how many items exist, count every visible item in the requested set, including inactive items, unless the user limits the count to a subset. Distinguish the total from the active count.`,
		`Use normalized integer coordinates (0–1000) measured on the latest image. For scrollable UI, use device-cli swipe X1 Y1 X2 Y2 starting inside the region. Use device-cli wait 0 to attach a fresh screen to the shell result without waiting for stability. Use device-cli look only to save a screenshot file.`,
		`Use device-cli home and device-cli back for system navigation. Use device-cli quick ACTION for semantic shortcuts.`,
		`Read the device-cli skill before the first UI action. Use only the latest screenshot to choose an action.`,
		`Call request_user_action when the task requires credentials, human verification, or a confirmation you cannot perform. Never ask the user to send credentials or verification codes in chat.`,
	}
}

func buildProfile(
	cfg AgentConfig,
	skills *SkillManager,
	tools []langtools.Tool,
	roleRules []string,
) RoleProfile {
	promptParts := []string{
		agentIdentityPrompt,
		"",
		"## Base instruction",
		combinedAgentInstruction(cfg),
		"",
		"## Device memory evidence",
		deviceMemoryRecallGuidance(),
		"",
		"## Environment",
		agentEnvironmentGuidance(),
		"",
		"## Default behavior",
		defaultAgentBehavior(),
		"",
		"## Screen Information Persistence Rule",
		screenInformationPersistenceGuidance(),
		"",
		"## Available skills",
		"The entries below are discovery summaries only. Load the matching skill with skill_read before following its instructions.",
		skills.CatalogSummary(),
	}

	promptParts = append(promptParts, "", "## Role rules")
	for _, rule := range roleRules {
		promptParts = append(promptParts, "- "+rule)
	}
	promptParts = append(promptParts, "", "## Response language", responseLanguageGuidance(cfg.Locale))
	systemPrompt := strings.Join(promptParts, "\n")

	return RoleProfile{
		SystemPrompt: systemPrompt,
		Skills:       skills,
		Tools:        append([]langtools.Tool{}, tools...),
	}
}
