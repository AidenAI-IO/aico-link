package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

func testPromptProfile(cfg AgentConfig) RoleProfile {
	return buildProfile(cfg, NewSkillManager(NewSkillIndex()), nil, agentRoleRules())
}

func newPromptTestContextManager(t *testing.T) *contextmanager.ContextManager {
	t.Helper()
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	return manager
}

func TestRolePromptsIncludeCurrentDate(t *testing.T) {
	originalNow := promptNow
	promptNow = func() time.Time {
		return time.Date(2026, time.June, 1, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	}
	t.Cleanup(func() { promptNow = originalNow })

	want := "Current date: 2026-06-01 (Monday, timezone: UTC)"
	profile := testPromptProfile(AgentConfig{})
	if !strings.Contains(profile.SystemPrompt, want) {
		t.Fatalf("system prompt missing current date %q:\n%s", want, profile.SystemPrompt)
	}
}

func TestRolePromptConvertsCurrentDateToConfiguredTimezone(t *testing.T) {
	originalNow := promptNow
	promptNow = func() time.Time {
		return time.Date(2026, time.September, 14, 18, 30, 0, 0, time.UTC)
	}
	t.Cleanup(func() { promptNow = originalNow })

	profile := testPromptProfile(AgentConfig{Locale: "en-US", Timezone: "Asia/Shanghai"})
	want := "Current date: 2026-09-15 (Tuesday, timezone: Asia/Shanghai)"
	if !strings.Contains(profile.SystemPrompt, want) {
		t.Fatalf("system prompt missing timezone-aware date %q:\n%s", want, profile.SystemPrompt)
	}
}

func TestRolePromptExplainsNotificationMemoryAndRawHistoryLookup(t *testing.T) {
	profile := testPromptProfile(AgentConfig{})
	for _, want := range []string{"Phone notifications", "recall_memory", "/userdata/agent/memory/notifications/events/", "YYYY-MM-DD.jsonl", "exact original notification"} {
		if !strings.Contains(profile.SystemPrompt, want) {
			t.Fatalf("system prompt missing notification guidance %q:\n%s", want, profile.SystemPrompt)
		}
	}
}

func TestRolePromptRequiresRecallForNaturalLanguagePriorDeviceExperience(t *testing.T) {
	profile := testPromptProfile(AgentConfig{})
	for _, want := range []string{"## Device memory evidence", "must call recall_device_memory", "previously learned behavior", "prior device experience", "earlier workaround", "not a substitute for the saved evidence", "answer appears obvious", "not an unnecessary tool call"} {
		if !strings.Contains(profile.SystemPrompt, want) {
			t.Fatalf("system prompt missing natural-language device recall guidance %q:\n%s", want, profile.SystemPrompt)
		}
	}
}

func TestRolePromptDirectsRemoteSkillURLsToInstallAction(t *testing.T) {
	profile := testPromptProfile(AgentConfig{})
	for _, want := range []string{"skill_manage", "action=install", "source_url", "Do not fetch the skill with web_scraper or shell/curl"} {
		if !strings.Contains(profile.SystemPrompt, want) {
			t.Fatalf("system prompt missing remote skill install guidance %q:\n%s", want, profile.SystemPrompt)
		}
	}
}

func TestRolePromptConstrainsArtifactRecoveryReads(t *testing.T) {
	profile := testPromptProfile(AgentConfig{})
	for _, want := range []string{
		"read the artifact only when needed information is missing",
		"never use cat",
		"explicit output bound",
		"Bound both record counts and string lengths",
		"line limits alone do not bound bytes",
		"dd if=FILE bs=1 skip=0 count=4096",
		"fq -r '.results[0].summary[0:2000]' FILE for recall_session_chunks",
		"Read the next bounded range",
		"correct the field or command before retrying",
		"change strategy or report the blocker",
	} {
		if !strings.Contains(profile.SystemPrompt, want) {
			t.Fatalf("system prompt missing artifact recovery guidance %q:\n%s", want, profile.SystemPrompt)
		}
	}
}

// jqCommandFormPattern matches jq presented as a runnable command rather than
// discussed in prose. It keys off what follows jq — an option (-r,
// --raw-output), a quoted or bare filter ('.a', ".a", .a, $x), an
// uppercase FILE-style operand, a pipe, or end of line — plus membership in a
// slash-separated tool list (dd/jq/fq). It deliberately does not match English
// prose such as "evaluates jq expressions", "; jq is absent", or "a jq command
// fails", so guidance can still explain why jq must not be invoked.
//
// Note it must catch jq inside a comma-separated example list ("count=4096,
// jq -c FILE") and inside a slash-separated tool list, since those are the two
// forms the original guidance actually used.
var jqCommandFormPattern = regexp.MustCompile(`/jq\b|\bjq(?:[ \t]+(?:-{1,2}[a-zA-Z]|['".$]|[A-Z_]{2,})|[ \t]*(?:$|\||/))`)

// findJqCommandForms reports every jq invocation in text. The board rootfs
// ships fq/yq/rg but no jq, so guidance must never hand the model a jq
// command: it fails with "not found" and the model then falls back to python3
// with an ever-growing slice bound, looping on one artifact.
func findJqCommandForms(text string) []string {
	var found []string
	for _, match := range jqCommandFormPattern.FindAllString(text, -1) {
		found = append(found, strings.TrimSpace(match))
	}
	return found
}

func TestRolePromptDoesNotSuggestUninstalledJqBinary(t *testing.T) {
	profile := testPromptProfile(AgentConfig{})
	if forms := findJqCommandForms(profile.SystemPrompt); len(forms) > 0 {
		t.Fatalf("system prompt invokes uninstalled jq binary %v; use fq, which evaluates jq expressions:\n%s", forms, profile.SystemPrompt)
	}
	for _, want := range []string{
		"inspect the actual structure",
		"do not repeatedly expand the same prefix",
		"fq",
		"yq",
	} {
		if !strings.Contains(profile.SystemPrompt, want) {
			t.Fatalf("system prompt missing fq/anti-loop artifact guidance %q:\n%s", want, profile.SystemPrompt)
		}
	}
}

func TestJqCommandFormPatternMatchesInvocationsNotProse(t *testing.T) {
	for _, invocation := range []string{
		"jq '.[0:20]' FILE",
		"jq -r '.field' FILE",
		"jq -c FILE",
		"jq --raw-output FILE",
		"jq FILE",
		"cat FILE | jq",
		"cat FILE | jq -r '.a'",
		"grep x FILE && jq -e .",
		"sed -n '1p' FILE; jq .",
		"out=$(jq -r '.a' FILE)",
		// The two shapes the original guidance actually used: a comma-separated
		// example list, and a slash-separated tool list.
		"dd if=FILE bs=1 skip=0 count=4096, jq '.[0:20]' FILE, or fq '.[0:20]' FILE",
		"dd if=FILE bs=1 skip=0 count=4096, jq -c FILE",
		"Use bounded grep/sed/dd/jq/fq reads",
	} {
		if len(findJqCommandForms(invocation)) == 0 {
			t.Errorf("findJqCommandForms(%q) found nothing, want a match", invocation)
		}
	}
	for _, prose := range []string{
		"fq, which evaluates jq expressions",
		"fq runs jq expressions; jq is absent",
		"a jq command fails with 'not found'",
		"fq -r '.results[0].content[0:2000]' FILE",
		"use yq or fq instead",
	} {
		if forms := findJqCommandForms(prose); len(forms) > 0 {
			t.Errorf("findJqCommandForms(%q) = %v, want no match for prose", prose, forms)
		}
	}
}

func TestRolePromptIncludesConfiguredResponseLocaleInSystemPrompt(t *testing.T) {
	manager := NewSkillManager(NewSkillIndex())
	zh := buildProfile(AgentConfig{Locale: "zh-CN"}, manager, nil, agentRoleRules())
	en := buildProfile(AgentConfig{Locale: "en-US"}, manager, nil, agentRoleRules())

	if zh.SystemPrompt == en.SystemPrompt {
		t.Fatalf("system prompt did not change with locale:\nzh=%q\nen=%q", zh.SystemPrompt, en.SystemPrompt)
	}
	if !strings.Contains(zh.SystemPrompt, "zh-CN") {
		t.Fatalf("Chinese system prompt missing configured locale: %q", zh.SystemPrompt)
	}
	if !strings.Contains(en.SystemPrompt, "en-US") {
		t.Fatalf("English system prompt missing configured locale: %q", en.SystemPrompt)
	}
}

func TestStateHookDoesNotInjectResponseLocale(t *testing.T) {
	runtime := NewRuntimeWithDeps(Config{Locale: "en-US"}, nil, nil, nil, NewSkillIndex())
	manager := newPromptTestContextManager(t)
	runtime.contextManager = manager
	manager.AddAppendMessageHook(runtime.getStateHook())
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	messageList := manager.MessageListDump().Messages
	if len(messageList) != 2 ||
		messageList[0].Role != messages.MessageRoleState ||
		messageList[1].Role != messages.MessageRoleUser {
		t.Fatalf("messages = %#v, want device state followed by the user message", messageList)
	}
	if strings.Contains(messageList[0].Content, "response locale") || strings.Contains(messageList[0].Content, "en-US") {
		t.Fatalf("state message injected response locale: %q", messageList[0].Content)
	}
	if !strings.Contains(messageList[0].Content, "device_type") {
		t.Fatalf("state message missing device state: %q", messageList[0].Content)
	}
	if strings.Contains(messageList[0].Content, "<state>") {
		t.Fatalf("persisted state message must remain unwrapped: %q", messageList[0].Content)
	}
}

func TestStateHookAttachesFreshScreenshotToUserTurn(t *testing.T) {
	imageData := []byte("current screenshot")
	screenshot := &stubTool{
		name:        "screenshot",
		description: "Capture screenshot.",
		output: `{"width":390,"height":844,"format":"jpeg","size":18,"data":"` +
			base64.StdEncoding.EncodeToString(imageData) + `"}`,
	}
	runtime := NewRuntimeWithDeps(Config{}, nil, nil, &ToolSet{tools: map[string]langtools.Tool{
		"screenshot": screenshot,
	}}, NewSkillIndex())
	manager := newPromptTestContextManager(t)
	runtime.contextManager = manager
	manager.AddAppendMessageHook(runtime.getStateHook())

	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	messageList := manager.MessageListDump().Messages
	if len(messageList) != 2 || messageList[0].Role != messages.MessageRoleState || messageList[1].Role != messages.MessageRoleUser {
		t.Fatalf("messages = %#v, want state screenshot followed by user message", messageList)
	}
	if len(messageList[0].Attachments) != 1 {
		t.Fatalf("state attachments = %#v, want one screenshot", messageList[0].Attachments)
	}
	attachment := messageList[0].Attachments[0]
	if attachment.MIMEType != "image/jpeg" || attachment.Source != messages.AttachmentSourceScreenshotObservation {
		t.Fatalf("state screenshot attachment = %#v", attachment)
	}

	standard := messages.ConvertMessageList(messageList)
	foundImage := false
	for _, part := range standard[0].Parts {
		if binary, ok := part.(llms.BinaryContent); ok && binary.MIMEType == "image/jpeg" && bytes.Equal(binary.Data, imageData) {
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatalf("converted state message has no screenshot binary: %#v", standard[0].Parts)
	}
	if len(screenshot.inputs) != 1 || screenshot.inputs[0] != "{}" {
		t.Fatalf("screenshot inputs = %#v, want one empty-object call", screenshot.inputs)
	}
}

func TestStateHookSkipsFreshScreenshotWhenUserProvidesImage(t *testing.T) {
	screenshot := &stubTool{
		name:        "screenshot",
		description: "Capture screenshot.",
		output:      `{"width":390,"height":844,"format":"jpeg","size":18,"data":"unused"}`,
	}
	runtime := NewRuntimeWithDeps(Config{}, nil, nil, &ToolSet{tools: map[string]langtools.Tool{
		"screenshot": screenshot,
	}}, NewSkillIndex())
	runtime.stateManager.SetState("device", "connected")
	manager := newPromptTestContextManager(t)
	runtime.contextManager = manager
	manager.AddAppendMessageHook(runtime.getStateHook())

	message := userMessageFromInput(manager, "analyze this", []InputAttachment{{
		Kind:     AttachmentKindImage,
		MIMEType: "image/png",
		Data:     []byte("user image"),
	}})
	if err := manager.AppendMessage(message); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	messageList := manager.MessageListDump().Messages
	if len(messageList) != 2 || messageList[0].Role != messages.MessageRoleState || messageList[1].Role != messages.MessageRoleUser {
		t.Fatalf("messages = %#v, want text state followed by the user message", messageList)
	}
	if len(messageList[0].Attachments) != 0 {
		t.Fatalf("state attachments = %#v, want no automatic screenshot", messageList[0].Attachments)
	}
	if len(messageList[1].Attachments) != 1 || messageList[1].Attachments[0].MIMEType != "image/png" {
		t.Fatalf("user attachments = %#v, want uploaded image", messageList[1].Attachments)
	}
	if len(screenshot.inputs) != 0 {
		t.Fatalf("screenshot inputs = %#v, want no screenshot call", screenshot.inputs)
	}
}

func TestStateHookStillAttachesFreshScreenshotForNonImageAttachment(t *testing.T) {
	imageData := []byte("current screenshot")
	screenshot := &stubTool{
		name:        "screenshot",
		description: "Capture screenshot.",
		output: `{"width":390,"height":844,"format":"jpeg","size":18,"data":"` +
			base64.StdEncoding.EncodeToString(imageData) + `"}`,
	}
	runtime := NewRuntimeWithDeps(Config{}, nil, nil, &ToolSet{tools: map[string]langtools.Tool{
		"screenshot": screenshot,
	}}, NewSkillIndex())
	manager := newPromptTestContextManager(t)
	runtime.contextManager = manager
	manager.AddAppendMessageHook(runtime.getStateHook())

	message := userMessageFromInput(manager, "listen to this", []InputAttachment{{
		Kind:     AttachmentKindAudio,
		MIMEType: "audio/wav",
		Data:     []byte("audio"),
	}})
	if err := manager.AppendMessage(message); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	messageList := manager.MessageListDump().Messages
	if len(messageList) != 2 || len(messageList[0].Attachments) != 1 {
		t.Fatalf("messages = %#v, want state screenshot followed by the user message", messageList)
	}
	if messageList[0].Attachments[0].Source != messages.AttachmentSourceScreenshotObservation {
		t.Fatalf("state attachment = %#v, want screenshot observation", messageList[0].Attachments[0])
	}
	if len(screenshot.inputs) != 1 || screenshot.inputs[0] != "{}" {
		t.Fatalf("screenshot inputs = %#v, want one empty-object call", screenshot.inputs)
	}
}

func TestStateHookKeepsTextStateWhenScreenshotFails(t *testing.T) {
	screenshot := &stubTool{
		name:        "screenshot",
		description: "Capture screenshot.",
		err:         errors.New("capture unavailable"),
	}
	runtime := NewRuntimeWithDeps(Config{}, nil, nil, &ToolSet{tools: map[string]langtools.Tool{
		"screenshot": screenshot,
	}}, NewSkillIndex())
	runtime.stateManager.SetState("device", "connected")
	manager := newPromptTestContextManager(t)
	runtime.contextManager = manager
	manager.AddAppendMessageHook(runtime.getStateHook())

	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	messageList := manager.MessageListDump().Messages
	if len(messageList) != 2 || messageList[0].Role != messages.MessageRoleState {
		t.Fatalf("messages = %#v, want text state followed by user message", messageList)
	}
	if !strings.Contains(messageList[0].Content, "device: connected") {
		t.Fatalf("state content = %q, want device state", messageList[0].Content)
	}
	if len(messageList[0].Attachments) != 0 {
		t.Fatalf("state attachments = %#v, want none after screenshot failure", messageList[0].Attachments)
	}
}

func TestStateHookOmitsEmptyStateValues(t *testing.T) {
	runtime := NewRuntimeWithDeps(Config{}, nil, nil, nil, NewSkillIndex())
	runtime.stateManager.SetState("app_connected", "false")
	runtime.stateManager.SetState("app_platform", "")
	manager := newPromptTestContextManager(t)
	runtime.contextManager = manager
	manager.AddAppendMessageHook(runtime.getStateHook())

	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	messageList := manager.MessageListDump().Messages
	if len(messageList) != 2 || messageList[0].Role != messages.MessageRoleState {
		t.Fatalf("messages = %#v, want filtered state followed by user message", messageList)
	}
	if !strings.Contains(messageList[0].Content, "app_connected: false") {
		t.Fatalf("state content = %q, want app_connected", messageList[0].Content)
	}
	if strings.Contains(messageList[0].Content, "app_platform") {
		t.Fatalf("state content = %q, want empty app_platform omitted", messageList[0].Content)
	}
}

func TestRolePromptsIncludeRealHostRuntimeInfo(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("Hostname() error = %v", err)
	}
	operatingSystem := mustUname(t, "-s")
	architecture := mustUname(t, "-m")
	profile := testPromptProfile(AgentConfig{})
	for _, value := range []string{operatingSystem, hostname, architecture} {
		if !strings.Contains(profile.SystemPrompt, value) {
			t.Fatalf("system prompt missing runtime value %q: %q", value, profile.SystemPrompt)
		}
	}
	if strings.Contains(profile.SystemPrompt, "kernel=") {
		t.Fatalf("system prompt should not include kernel info:\n%s", profile.SystemPrompt)
	}
}

func mustUname(t *testing.T, flag string) string {
	t.Helper()
	out, err := exec.Command("uname", flag).Output()
	if err != nil {
		t.Fatalf("uname %s error = %v", flag, err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		t.Fatalf("uname %s returned empty output", flag)
	}
	return value
}

func TestCombinedAgentInstructionFallsBackWhenEmpty(t *testing.T) {
	if got := combinedAgentInstruction(AgentConfig{}); got != "" {
		t.Fatalf("combinedAgentInstruction() = %q, want empty string", got)
	}
}

func TestRolePromptsGuideSkillCatalogAndPreloadedSkills(t *testing.T) {
	index := NewSkillIndex()
	index.skills["planner"] = &SkillDefinition{Name: "planner", Description: "Plan before acting", Instructions: "Make a plan."}
	manager := NewSkillManager(index)
	profile := buildProfile(AgentConfig{}, manager, nil, agentRoleRules())

	if !strings.Contains(profile.SystemPrompt, "- planner: Plan before acting") {
		t.Fatalf("system prompt missing configured skill summary:\n%s", profile.SystemPrompt)
	}
}

func TestRolePromptPrioritizesClearlyVisibleAppTargetOverOpenApp(t *testing.T) {
	profile := testPromptProfile(AgentConfig{})
	for _, expected := range []string{
		"inspect the latest screenshot first",
		"you MUST call touch_gesture",
		"overrides any general preference for open_app or system search",
		"Call open_app with a semantic app name only when the target is not clearly and reliably tappable",
	} {
		if !strings.Contains(profile.SystemPrompt, expected) {
			t.Fatalf("system prompt missing visible app-target rule %q:\n%s", expected, profile.SystemPrompt)
		}
	}
	if strings.Contains(profile.SystemPrompt, "For app launch requests, call open_app with a semantic app name") {
		t.Fatalf("system prompt retained unconditional open_app rule:\n%s", profile.SystemPrompt)
	}
}

func TestRolePromptRequiresVerifiedAppLaunchCompletion(t *testing.T) {
	profile := testPromptProfile(AgentConfig{})
	for _, expected := range []string{
		"report completion only after post-action verification",
		"require screen_changed=true or equivalent visual confirmation",
		"ok=true only confirms that the OS accepted the launch",
		"screen_changed=false or an omitted screen_changed field without visual confirmation is not proof",
	} {
		if !strings.Contains(profile.SystemPrompt, expected) {
			t.Fatalf("system prompt missing launch verification rule %q:\n%s", expected, profile.SystemPrompt)
		}
	}
	for _, outdated := range []string{
		"success from the matching direct tool is enough",
		"treat the launch as complete unless the user requested additional actions",
	} {
		if strings.Contains(profile.SystemPrompt, outdated) {
			t.Fatalf("system prompt retained unverified launch completion rule %q:\n%s", outdated, profile.SystemPrompt)
		}
	}
}

func TestRolePromptOmitsRuntimeAndMemoryContext(t *testing.T) {
	profile := testPromptProfile(AgentConfig{})

	if !strings.Contains(profile.SystemPrompt, "## Role rules") {
		t.Fatalf("prompt missing role rules section:\n%s", profile.SystemPrompt)
	}
	for _, unwanted := range []string{"## Runtime context", "Phone bridge status", "session memory tail"} {
		if strings.Contains(profile.SystemPrompt, unwanted) {
			t.Fatalf("system prompt should not include dynamic context %q:\n%s", unwanted, profile.SystemPrompt)
		}
	}
}

// Saved memory rides in each turn's state rather than the session's system
// prompt, which is written once per session: a memory the user forgets must
// stop appearing on the next turn.
func TestStateHookCarriesCurrentSavedUserMemory(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	if _, err := store.AddMemory(ctx, MemoryItem{ID: "mem_brevity", Type: "preference", Priority: 80, Confidence: 0.9, Content: "用户希望回复尽量简短。", EvidenceExcerpts: []string{"说短一点"}}); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntimeWithDeps(Config{}, nil, NewMemoryManager(t.TempDir(), WithLongTermMemoryStore(store)), nil, NewSkillIndex())
	manager := newPromptTestContextManager(t)
	runtime.contextManager = manager
	manager.AddAppendMessageHook(runtime.getStateHook())

	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	first := manager.MessageListDump().Messages[0]
	if first.Role != messages.MessageRoleState || !strings.Contains(first.Content, savedUserMemoryStateHeader+"\n- (preference) 用户希望回复尽量简短。") {
		t.Fatalf("first state = %#v, want the saved preference", first)
	}

	if err := store.Forget(ctx, "mem_brevity", "user asked"); err != nil {
		t.Fatal(err)
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "again"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	for _, message := range manager.MessageListDump().Messages[2:] {
		if strings.Contains(message.Content, "saved_user_memory") {
			t.Fatalf("forgotten memory still in the next turn's state: %q", message.Content)
		}
	}
}
