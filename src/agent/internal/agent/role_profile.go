package agent

import (
	"strings"

	langtools "github.com/tmc/langchaingo/tools"
)

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
		"When moving a home-screen app icon, locate both its center and a vacant grid cell before drag_start. Empty cells can be below the last occupied row; infer a candidate from the visible column centers and row spacing, but keep it inside the launcher workspace above search, dock, and navigation. After drag_start, confirm that the destination is still empty in its returned screenshot before drag_release. Never release over another icon or its label: that creates a folder. Verify the final screenshot shows the icon in the vacant cell.",
		"For a numeric wheel picker, identify the visible number rows and selected center before swiping. Convert the row bounds using screenshot height; the swipe start must be inside the intended column AND within those visible rows, not in settings or blank space below the picker. For finger-up start near the lower visible row; for finger-down start near the upper one. Read the centered value after each swipe and adjust distance and direction from the observed change. If a swipe has no effect, choose a new start inside the wheel rather than repeating the same path.",
		"For copying between two visible input fields, identify both fields first. After entering text in the first, use quick_action select_all and copy while it is focused, then focus the distinct second field and use quick_action paste. Verify the second field contains the text before claiming success; if selection or focus is uncertain, inspect a fresh screenshot instead of repeating taps.",
		"When scrolling to the bottom of a page, finger-up swipes reveal lower content. Inspect the last visible item after each swipe and stop only when another swipe makes no further progress; blank padding alone does not prove there is more content.",
		"Keep your tool choices tied to the original user request, not just a self-invented subtask.",
		"If the current screenshot clearly identifies the app/page, visible text, dialogs, or other UI state, use that observed UI context when choosing tools; keep OS selection tied to global device_type state.",
		"Call request_user_action when the task requires credentials, login-method selection, verification, system/app redirect confirmation, permission dialog confirmation, or human judgment your tools cannot fulfill, or when the user refers to a target you cannot unambiguously identify from the screen. Ask the user to complete it on the device; do not ask them to send credentials or private verification details in chat.",
		"Only latest <state> content is valid, previous old states may have been invalidated or expired.",
		"When open_app or open_url returns ok=true, inspect its returned screenshot before answering. ok=true only confirms that the OS accepted the launch; screen_changed=false or an omitted screen_changed field without visual confirmation is not proof that the requested target opened.",
	}
	return rules
}

func buildProfile(
	cfg AgentConfig,
	skills *SkillManager,
	tools []langtools.Tool,
	roleRules []string,
) RoleProfile {
	promptParts := []string{
		"You are the Aiden agent.",
		currentDateContext(cfg.Locale, cfg.Timezone),
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
