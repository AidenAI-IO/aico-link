package agent

import (
	"fmt"
	"sort"
	"strings"

	langtools "github.com/tmc/langchaingo/tools"
)

func (r *Runtime) OwnedTools() []langtools.Tool {
	owned := make([]langtools.Tool, 0, len(r.toolSnapshot().tools))
	owned = append(owned, r.toolSnapshot().All()...)
	sort.Slice(owned, func(i, j int) bool {
		return owned[i].Name() < owned[j].Name()
	})
	return owned
}

func (r *Runtime) ToolSpecs() *ToolSpecs {
	return NewToolSpecs(r.OwnedTools())
}

func (r *Runtime) ToolDescriptors() []ToolDescriptor {
	return r.ToolSpecs().HTTPDescriptors()
}

func (r *Runtime) ToolDescriptorByName(name string) (ToolDescriptor, bool) {
	spec, ok := r.ToolSpecs().LookupHTTP(name)
	if !ok {
		return ToolDescriptor{}, false
	}
	return spec.Descriptor(), true
}

const defaultHTTPToolSkillBaseURL = "http://127.0.0.1:8080"

func (r *Runtime) HTTPToolSkills(baseURL string) []ToolSkillDefinition {
	descriptors := r.ToolDescriptors()
	if len(descriptors) == 0 {
		return nil
	}

	baseURL = normalizeHTTPToolSkillBaseURL(baseURL)

	names := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		names = append(names, descriptor.Name)
	}

	return []ToolSkillDefinition{{
		Name:        "aico-http-tool-suite",
		Description: "Use the Aico HTTP tool API to observe and operate the connected device with all HTTP-exposed tools.",
		ToolNames:   names,
		Markdown: buildHTTPToolSkillMarkdown(
			"aico-http-tool-suite",
			"Use the Aico HTTP tool API to observe and operate the connected device with all HTTP-exposed tools.",
			baseURL,
			descriptors,
		),
	}}
}

func buildHTTPToolSkillMarkdown(name, description string, baseURL string, descriptors []ToolDescriptor) string {
	var builder strings.Builder
	builder.WriteString("---\n")
	builder.WriteString(fmt.Sprintf("name: %s\n", name))
	builder.WriteString(fmt.Sprintf("description: %s\n", description))
	builder.WriteString("metadata:\n")
	builder.WriteString("  transport: http\n")
	builder.WriteString("  source: aico-agent\n")
	builder.WriteString("  base_url_env: AICO_AGENT_BASE_URL\n")
	builder.WriteString(fmt.Sprintf("  base_url_default: %s\n", baseURL))
	builder.WriteString("  tool_names:\n")
	for _, descriptor := range descriptors {
		builder.WriteString(fmt.Sprintf("    - %s\n", descriptor.Name))
	}
	builder.WriteString("---\n\n")
	builder.WriteString("Use the Aico HTTP tool API instead of assuming direct local access to the connected device.\n\n")
	builder.WriteString("Connection:\n")
	builder.WriteString("- Prefer the base URL from `AICO_AGENT_BASE_URL`.\n")
	builder.WriteString(fmt.Sprintf("- If `AICO_AGENT_BASE_URL` is unset, use `%s`.\n", baseURL))
	if baseURL != defaultHTTPToolSkillBaseURL {
		builder.WriteString(fmt.Sprintf("- Local fallback when the published host is unavailable: `%s`.\n", defaultHTTPToolSkillBaseURL))
	}
	builder.WriteString("- When the base URL points at a private IP or LAN hostname, bypass outbound HTTP proxies (`NO_PROXY`, `no_proxy`, or client-specific proxy bypass settings).\n\n")
	builder.WriteString("Discovery:\n")
	builder.WriteString("- `GET /api/tools` lists every exposed tool, its description, example input, and HTTP path.\n")
	builder.WriteString("- `GET /api/tool-skills` returns generated skill bundles like this one.\n\n")
	builder.WriteString("Invocation:\n")
	builder.WriteString("- `POST /api/tools/{tool_name}`\n")
	builder.WriteString("- Request body: `{\"input\": <JSON object or string>}` or `{\"raw_input\": \"<plain tool input>\"}`.\n")
	builder.WriteString("- Success and tool failures both return JSON; inspect `is_error` and `output`.\n\n")
	builder.WriteString("Transport failures:\n")
	builder.WriteString("- Treat connection errors, timeouts, empty replies, and non-2xx HTTP status codes as transport failures before inspecting any tool payload.\n")
	builder.WriteString("- For private device URLs, suspect proxy interference first if the TCP port is reachable but HTTP returns gateway/proxy errors.\n\n")
	builder.WriteString("Recommended workflow:\n")
	builder.WriteString("- Start with `GET /api/tools` if you need to confirm the tool list or example payloads.\n")
	builder.WriteString("- Use `screenshot` before input actions when you need current screen context; keyboard, mouse, and touch tools automatically wait for screen stability (or until the configured timeout) and return a post-action screenshot.\n")
	builder.WriteString("- After actions that may animate, navigate, or load content, call `wait_for_stable_screen` before judging the result. It returns a screenshot observation; `screen_changed=false` means no visible change was observed during the wait window, and `stable=false` means the screen kept changing (for example video playback), so treat the screenshot as a best-effort observation.\n")
	builder.WriteString("- After any screenshot, wait_for_stable_screen screenshot, or post-action screenshot, inspect the current screen before choosing the next action; do not repeat the same click, gesture, or key unless the image proves the previous action did not take effect.\n")
	builder.WriteString("- For app launch requests, inspect the latest screenshot first. If the requested app icon or card is clearly visible, unique, and unobscured, use `touch_gesture` on its visible non-overlapping center instead of `open_app`; use `open_app` only when the target is not clearly tappable or one direct tap failed. Use `open_url` for HTTP/HTTPS webpages, `sms:<phone_number>?body=<message>`, `mailto:<email_address>?subject=<subject>`, or `tel:<phone_number>`.\n")
	builder.WriteString("- Always inspect the post-action screenshot returned by `open_app` or `open_url` before answering or continuing. `ok:true` only confirms that the OS accepted the launch; it does not prove that the requested app or page is visible.\n")
	builder.WriteString("- `open_app` success confirms only that the app opened. Before any text-entry tool, the latest screenshot must clearly show the actual editable field/composer and the focus point must be inside it; app home screens, folder/list views, blank areas, and create/new buttons are not input fields.\n")
	builder.WriteString("- The HTTP catalog stays complete for direct diagnostics. The conversational Agent receives a runtime-filtered subset: `open_app` stays available because it can fall back to visible system search; App-only tools and actions are exposed only when foreground Phone Bridge, PiP/FGS polling, Dynamic Island restore, or iOS BLE Wake can execute them. iOS BLE Wake does not expose clipboard read/write or contacts update. `bridge_notification` may also remain available for direct BLE notification queries.\n")
	builder.WriteString("- For input field entry, use `enter_text`. It first uses the Phone Bridge clipboard route when available, otherwise it preserves text order by combining HID ASCII typing with IME entry for non-ASCII runs. It owns clipboard write, paste, and verification, so do not stage `bridge_clipboard` manually. If a Chinese IME is active but the target text is English, switch to the English/Latin keyboard first, commonly with the globe/input-method key.\n")
	builder.WriteString("- Pointer and touch coordinates always use the normalized 0-1000 scale: (0,0) is top-left, (1000,1000) is bottom-right, and (500,500) is center. Click the visible target center from the latest screenshot.\n")
	builder.WriteString("- For `shell` background sessions, use `action:start`, then `poll`/`write`/`submit`/`send_keys`, and always finish with `stop`.\n\n")
	builder.WriteString("Available tools in this skill:\n")
	for _, descriptor := range descriptors {
		builder.WriteString(fmt.Sprintf("- `%s`: %s\n", descriptor.Name, descriptor.Description))
		if descriptor.Name == "screenshot" {
			builder.WriteString("  Successful output includes the captured screen image.\n")
		} else if descriptor.Name == "wait_for_stable_screen" {
			builder.WriteString("  Successful output includes stability and screen-change metadata plus the captured screen image.\n")
		} else if descriptor.Name == "enter_text" {
			builder.WriteString("  `action_output` contains only `{\"ok\":true}` on success, or `{\"ok\":false,\"suggestion\":\"...\"}` on failure. The outer output also includes an optional `screen_changed` comparison and a short-delay post-action screenshot; `enter_text` does not perform the stable-screen wait, so `screen_stable` and `stable_wait_ms` are not included.\n")
		} else if descriptor.Category == "input" {
			builder.WriteString("  On successful execution, output includes `action_output`, `screen_stable`, `stable_wait_ms`, optional `screen_changed`, and a post-action screenshot. When present, `screen_changed` reports meaningful structural change between a pre-action baseline and the final settled screenshot while ignoring the top status area and minor image noise; `screen_stable=false` is not a failure.\n")
		}
		if strings.TrimSpace(descriptor.ExampleInput) != "" {
			builder.WriteString(fmt.Sprintf("  Example input: `%s`\n", descriptor.ExampleInput))
		}
	}
	builder.WriteString("\nExecution rules:\n")
	builder.WriteString("- Treat `is_error=true` or outputs that start with `error:` as failures.\n")
	builder.WriteString("- For successful keyboard, mouse, and touch calls, inspect the returned post-action screenshot before deciding the next step. When present, `screen_changed=false` means no meaningful structural change was detected between the pre-action baseline and final settled screenshot; an omitted field means comparison was unavailable, not that no change occurred. `screen_stable=false` means the wait timed out while the screen kept changing; continue if the screenshot is still useful. For separate observations after an action, call `wait_for_stable_screen` and inspect its returned screenshot.\n")
	builder.WriteString("- Do not use repeated scrolling as the first search strategy on phone UIs; try available search controls first.\n")
	builder.WriteString("- Keep tool input minimal and deterministic; prefer the example payloads as a starting point.\n")
	return builder.String()
}

func normalizeHTTPToolSkillBaseURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return defaultHTTPToolSkillBaseURL
	}
	return baseURL
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
