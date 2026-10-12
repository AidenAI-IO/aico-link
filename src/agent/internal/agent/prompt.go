package agent

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

var promptNow = time.Now
var promptHostRuntimeInfo = detectHostRuntimeInfo

type hostRuntimeInfo struct {
	OperatingSystem string
	Hostname        string
	Architecture    string
}

func currentDateContext(locale, timezone string) string {
	return formatCurrentDate(configuredTime(promptNow(), timezone), locale, timezone)
}

func hostRuntimeInfoContext() string {
	infoFn := promptHostRuntimeInfo
	if infoFn == nil {
		infoFn = detectHostRuntimeInfo
	}
	return formatHostRuntimeInfo(infoFn())
}

func formatHostRuntimeInfo(info hostRuntimeInfo) string {
	return fmt.Sprintf(
		"Host: os=%s, hostname=%s, arch=%s",
		hostInfoValue(info.OperatingSystem),
		hostInfoValue(info.Hostname),
		hostInfoValue(info.Architecture),
	)
}

func detectHostRuntimeInfo() hostRuntimeInfo {
	hostname, _ := os.Hostname()
	operatingSystem := unameValue("-s")
	if strings.TrimSpace(operatingSystem) == "" {
		operatingSystem = runtime.GOOS
	}
	architecture := unameValue("-m")
	if strings.TrimSpace(architecture) == "" {
		architecture = runtime.GOARCH
	}

	return hostRuntimeInfo{
		OperatingSystem: operatingSystem,
		Hostname:        hostname,
		Architecture:    architecture,
	}
}

func unameValue(flag string) string {
	out, err := exec.Command("uname", flag).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func hostInfoValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func formatCurrentDate(t time.Time, locale, timezone string) string {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		timezone = defaultTimezone
	}
	if normalizeResponseLocale(locale) == localeEnglishUS {
		weekdays := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
		return "Current date: " + t.Format("2006-01-02") + " (" + weekdays[t.Weekday()] + ", timezone: " + timezone + ")"
	}
	weekdays := []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}
	return "Current date: " + t.Format("2006-01-02") + " (" + weekdays[t.Weekday()] + ", timezone: " + timezone + ")"
}

func normalizeResponseLocale(locale string) string {
	switch strings.ToLower(strings.TrimSpace(locale)) {
	case "en-us":
		return localeEnglishUS
	case "zh-cn":
		return localeSimplifiedChinese
	default:
		return defaultLocale
	}
}

func responseLanguageGuidance(locale string) string {
	locale = normalizeResponseLocale(locale)
	language := "Simplified Chinese"
	if locale == localeEnglishUS {
		language = "English"
	}
	return strings.Join([]string{
		"The configured response locale is " + locale + ".",
		"Write all conversational assistant content in " + language + " from the first generated token. This includes progress messages, final answers, and all text inside <tts>...</tts>.",
		"Only use another language for content the user explicitly asks to translate, quote, draft, or generate. Keep any surrounding explanation in " + language + ".",
		"Do not infer the response language from STT language, phone locale, screenshot language, proper nouns, or an isolated foreign-language phrase.",
		"IMPORTANT: Always respond in " + language + ", regardless of the language used by the user, except when the user explicitly asks to translate, quote, draft, or generate content in another language.",
	}, "\n")
}

// ResponseLanguageGuidance returns the shared response-language rule for
// agent entry points that do not build a full RoleProfile, such as realtime.
func ResponseLanguageGuidance(locale string) string {
	return responseLanguageGuidance(locale)
}

func combinedAgentInstruction(cfg AgentConfig) string {
	parts := make([]string, 0, 2)
	if text := strings.TrimSpace(cfg.Instruction); text != "" {
		parts = append(parts, text)
	}
	if text := strings.TrimSpace(cfg.Prompt); text != "" {
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

func agentEnvironmentGuidance() string {
	return strings.Join([]string{
		"- You run on the Aiden hardware controller (" + hostRuntimeInfoContext() + "); you are not the device shown in screenshots.",
		"- shell, local files, processes, and system commands only affect the Aiden hardware controller; they do not operate the target UI in screenshots. Use shell on the Aiden controller for diagnostics, precise controller clock or timezone queries, deterministic calculations, or when the user explicitly asks to run controller commands.",
		"- The controller image provides fq for jq-style JSON queries, yq for YAML/JSON queries, and rg for text search. Prefer these installed tools; do not assume jq is available.",
		"- Prefer the Python standard library and already installed packages for controller-side Python work. If a dependency is missing, first avoid installation when /run/agent/storage_level reports a non-normal level or /userdata is visibly low on free space. Otherwise use /usr/bin/python3 -m pip install for one exact top-level name==version with --only-binary=:all:, then run /usr/bin/python3 -m pip check with a shell timeout of at least 120 seconds. If pip check times out, retry it once with a longer bounded timeout; do not claim installation succeeded unless pip check completes successfully. Run Python code with /usr/bin/python3, and append $PYTHONUSERBASE/bin to PATH only for an invocation of a package CLI. Never self-upgrade the firmware-provided pip, setuptools, or wheel; inspect recoverable errors, avoid unbounded retries, retry only with a compatible exact version, and report native or system-library failures instead of installing system packages.",
		"- Treat the global device_type state as the source of truth for the controlled target device and OS. Use screenshots, connection metadata, behavioral probing, and user input to identify the current app, page, UI state, and mismatches, not to override the device_type-derived OS.",
		"- Aiden can control phones, tablets, or desktop OS targets depending on device_type; do not assume a mobile OS unless device_type says so.",
		"- Do not infer target device or OS information from the host OS or architecture; when operating the target UI, do not use local system commands instead of target control tools.",
	}, "\n")
}

func deviceMemoryRecallGuidance() string {
	return strings.Join([]string{
		"Before answering or planning, decide whether the task materially depends on saved device or app experience rather than only on the current request and general knowledge.",
		"If it depends on a saved procedure, navigation path, failure-prevention lesson, device or app profile, calibration note, or fact, you must call recall_device_memory and use the returned evidence first.",
		"Natural-language references to previously learned behavior, prior device experience, or an earlier workaround require this lookup even when the user summarizes the experience or the answer appears obvious. The user's summary identifies what to recall; it is not a substitute for the saved evidence.",
		"This required evidence lookup is not an unnecessary tool call. Device memory recall is non-visual and does not operate or inspect the current screen. Do not call it merely because a device or app is mentioned, or for unrelated text-only questions.",
	}, "\n")
}

func defaultAgentBehavior() string {
	rules := []string{
		"- Keep final replies natural; avoid Markdown tables or long lists unless the user asks for them.",
		"- Every assistant response must begin with exactly one <tts>...</tts> block before any whitespace or visible text. The literal <tts> start tag must be the first generated bytes. After the closing </tts> tag, write the ordinary user-facing text. Never emit a second <tts> block in the same response. Content outside <tts> is not spoken. The leading <tts> text may be a concise spoken form and does not need to match the visible text exactly.",
		"- When invoking tools, begin the assistant content with the same single leading <tts>...</tts> block containing brief spoken progress, then add brief visible progress text before the tool call. Do not put speech or description arguments in tool inputs. Do not put the final answer in tool-call assistant content.",
		"- Do not use JSON, final_answer fields, or \"Final Answer:\" wrappers for final responses.",
		"- Most user input arrives as voice transcribed by STT and may contain homophone, near-sound, segmentation, or named-entity errors. Interpret commands by intent and context instead of matching transcript text literally. When searching the web, apps, contacts, settings, files, or page content, choose likely canonical keywords and try reasonable alternate terms rather than requiring exact transcript wording.",
		"- The system prompt already includes the current date and weekday. Answer ordinary date or weekday questions from that context. When a precise clock time, timezone conversion, offset, timestamp, elapsed-time result, or verified calculation is required, use shell utilities on the Aiden controller; do not treat controller-local results as target-device state unless that relationship is known.",
		"- When an answer depends on saved long-term preferences, rules, procedures, facts, or screen content the user deliberately captured earlier, call recall_memory first; do not answer from general knowledge alone. Requests about something the user saw, saved, or noted down on screen — such as a tracking number, address, amount, or message — are screen memories: query recall_memory with types [\"screen_snapshot\"], adding topic keywords when the user gave any.",
		"- Phone notifications may be available as temporary or long-term memories. When the user asks what a notification said, whether a delivery/calendar/payment update was received, or asks about a prior phone notification, call recall_memory first with notification-related tags/entities; it searches both temporary and long-term memory. If the user asks for the exact original notification, a raw record, a date range, or an audit/debug view, use shell to read the date-sharded JSONL files under `/userdata/agent/memory/notifications/events/` with normal read-only shell utilities. Each `YYYY-MM-DD.jsonl` file uses a UTC date and contains one original notification record per line. Do not modify these files or use bridge_notification query as a substitute for the durable local log when historical records are needed.",
		"- Do not use tools unnecessarily for ordinary questions. For text-only arithmetic, comparison, summarization, translation, or simple Q&A tasks unrelated to saved long-term or device/UI memory, answer directly or use only the non-visual tool needed for the task; do not observe, wait on, or operate the connected display.",
		"- When the user asks to install a skill from an HTTP(S) or GitHub URL, call skill_manage directly with action=install and source_url set to the original URL. Do not fetch the skill with web_scraper or shell/curl, and do not copy remote contents through create, edit, or patch; action=install downloads, validates, and commits the complete skill.",
		"- When the user asks to inspect or operate a device, app, settings, contacts, messages, websites, TV UI, or other external state, you must use tools; do not claim state has changed without tool results or screenshot confirmation. An explicit request to remember information or a rule for future conversations also requires a state change: call save_memory successfully before confirming it is remembered. A conversational acknowledgement alone does not persist memory.",
		"- When a tool result says its full or partial output was saved to a result file, first use the inline preview and read the artifact only when needed information is missing. Use shell with a focused query or range and an explicit output bound; never use cat or print the entire artifact. For JSON, inspect the actual structure and query the relevant field with fq (for example, fq -r '.results[0].summary[0:2000]' FILE for recall_session_chunks); use yq when the artifact is YAML. Bound both record counts and string lengths; line limits alone do not bound bytes in a single-line JSON file. For raw text, use a byte range such as dd if=FILE bs=1 skip=0 count=4096. Read the next bounded range when more content is needed, but do not repeatedly expand the same prefix without new information. If a query fails, correct the field or command before retrying. If repeated reads return the same error or no new information, change strategy or report the blocker. Recover continuation identifiers, completion status, errors, and relevant output tails; do not repeat a completed action or start a replacement session merely because a preview omitted them.",
		"- Entries under Available skills are discovery summaries only, not active instructions. When operating visible target UI, if the full device-operator instructions are not already present in the current conversation, call skill_read for device-operator before the first screenshot or UI action, then follow the loaded instructions. Keep detailed UI playbooks in skills; do not copy them into the default prompt.",
		"- Prefer direct or semantic tools that match the requested operation only when the latest state satisfies their preconditions; use low-level input tools when the direct path is unavailable or ineffective.",
		"- Before irreversible or sensitive actions—send message/email, place order, pay, delete data, change privacy/security settings, grant permissions, or start a call—request confirmation unless the user explicitly asks for that final action.",
	}
	return strings.Join(rules, "\n")
}
