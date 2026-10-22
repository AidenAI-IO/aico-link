package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

const (
	toolInputModeJSON = "json"
	toolInputModeText = "text"
)

// dynamicExampleInputProvider allows tools to provide dynamic example inputs
// that reflect runtime configuration instead of static hardcoded values.
type dynamicExampleInputProvider interface {
	DynamicExampleInput() string
}

type ToolSpec struct {
	Tool         langtools.Tool
	Name         string
	Description  string
	Category     string
	InputMode    string
	ExampleInput string
	AgentExposed bool
	HTTPExposed  bool
	// AgentPlatforms lists device_type-derived platforms where the tool should
	// be shown to the conversational model. Empty means all platforms.
	AgentPlatforms []string
}

type ToolSpecs struct {
	byName map[string]ToolSpec
	names  []string
}

type ToolHTTPBinding struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type ToolDescriptor struct {
	Name         string          `json:"name"`
	Category     string          `json:"category"`
	Description  string          `json:"description"`
	InputMode    string          `json:"input_mode"`
	ExampleInput string          `json:"example_input"`
	ArgsSchema   map[string]any  `json:"args_schema"`
	HTTP         ToolHTTPBinding `json:"http"`
}

type ToolSkillDefinition struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ToolNames   []string `json:"tool_names"`
	Markdown    string   `json:"markdown"`
}

type toolSpecMetadata struct {
	Category       string
	InputMode      string
	ExampleInput   string
	AgentExposed   *bool
	HTTPExposed    *bool
	AgentPlatforms []string
}

var builtInToolSpecMetadata = map[string]toolSpecMetadata{
	"audio_volume": {
		Category:     "audio",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{}`,
	},
	toolWaitForWakeup: {
		Category:     "system",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"reason":"user asked me to wait for wakeup"}`,
	},
	"inspect_episode": {
		Category:     "memory",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"id":"ep_..."}`,
	},
	"keyboard_tap": {
		Category:     "input",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"keys":["enter"]}`,
	},
	"enter_text": {
		Category:       "input",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"text":"hello你好","focus":{"x":450,"y":105}}`,
		AgentPlatforms: []string{"ios", "android", "macos", "windows", "linux"},
	},
	"mouse_move": {
		Category:     "input",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"x":500,"y":500}`,
	},
	"mouse_scroll": {
		Category:     "input",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"delta":-3}`,
	},
	"quick_action": {
		Category:       "input",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"action":"list"}`,
		AgentPlatforms: []string{"ios", "android", "macos"},
	},
	"recall_device_memory": {
		Category:     "memory",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"terms":["微信"],"tags":["登录"],"entities":["微信App"],"types":["procedure","failure"],"device_id":"default","limit":5}`,
	},
	"recall_session_chunks": {
		Category:     "memory",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"tags":["login"],"limit":5}`,
	},
	"recall_memory": {
		Category:     "memory",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"tags":["login"],"limit":5}`,
	},
	"save_memory": {
		Category:     "memory",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"type":"procedure","title":"Login flow","content":"...","tags":["login"]}`,
	},
	"forget_memory": {
		Category:     "memory",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"id":"mem_...","reason":"obsolete"}`,
	},
	"screenshot": {
		Category:     "observation",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{}`,
	},
	"shell": {
		Category:     "system",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"command":"pwd"}`,
	},
	"skill_list": {
		Category:     "skills",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"query":"device","include_archived":false}`,
	},
	"skill_mark_used": {
		Category:     "skills",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"name":"device-operator"}`,
		HTTPExposed:  toolSpecBoolPtr(false),
	},
	"skill_read": {
		Category:     "skills",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"name":"device-operator"}`,
	},
	"touch_gesture": {
		Category:     "input",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"type":"tap","point":{"x":500,"y":500}}`,
	},
	"weather": {
		Category:     "system",
		InputMode:    toolInputModeText,
		ExampleInput: `{"location":"Shanghai"}`,
	},
	"web_search": {
		Category:     "web",
		InputMode:    toolInputModeText,
		ExampleInput: `{"query":"Aiden hardware agent"}`,
	},
	"wikipedia": {
		Category:     "web",
		InputMode:    toolInputModeText,
		ExampleInput: `{"query":"Raspberry Pi"}`,
	},
	"web_scraper": {
		Category:     "web",
		InputMode:    toolInputModeText,
		ExampleInput: `{"url":"https://example.com"}`,
	},
	"wait_for_stable_screen": {
		Category:     "observation",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"timeout_ms":2200,"stable_ms":250,"diff_threshold":6}`,
	},
	"request_user_action": {
		Category:     "handoff",
		InputMode:    toolInputModeJSON,
		ExampleInput: `{"reason":"authentication","details":"Login screen requires password","suggested_action":"Please enter your credentials on the device"}`,
	},
	"skill_manage": {
		Category:     "skills",
		InputMode:    toolInputModeJSON,
		ExampleInput: skillManageInputExample,
		HTTPExposed:  toolSpecBoolPtr(false),
	},
	toolOpenApp: {
		Category:       "bridge",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"app":"微信"}`,
		AgentPlatforms: []string{"ios", "android", "macos"},
	},
	toolOpenURL: {
		Category:       "bridge",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"url":"https://example.com"}`,
		AgentPlatforms: []string{"ios", "android"},
	},
	toolSearchLaunchApp: {
		Category:       "phone",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"app":"WeChat"}`,
		AgentExposed:   toolSpecBoolPtr(false),
		HTTPExposed:    toolSpecBoolPtr(false),
		AgentPlatforms: []string{"ios", "macos"},
	},
	toolBridgeOpenApp: {
		Category:       "bridge",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"app":"微信"}`,
		AgentExposed:   toolSpecBoolPtr(false),
		HTTPExposed:    toolSpecBoolPtr(false),
		AgentPlatforms: []string{"ios", "android"},
	},
	toolBridgeClipboard: {
		Category:       "bridge",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"action":"read"}`,
		AgentPlatforms: []string{"ios", "android"},
	},
	toolBridgeCalendar: {
		Category:       "bridge",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"action":"query","from":"2026-07-10T00:00:00+08:00","to":"2026-07-11T00:00:00+08:00"}`,
		AgentPlatforms: []string{"ios", "android"},
	},
	toolBridgeContacts: {
		Category:       "bridge",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"action":"query","query":"Alice","limit":20}`,
		AgentPlatforms: []string{"ios", "android"},
	},
	toolBridgeNotification: {
		Category:       "bridge",
		InputMode:      toolInputModeJSON,
		ExampleInput:   `{"action":"query","limit":20}`,
		AgentPlatforms: []string{"ios", "android"},
	},
}

func toolSpecBoolPtr(value bool) *bool {
	return &value
}

func normalizeAgentToolPlatforms(platforms []string) []string {
	seen := make(map[string]struct{}, len(platforms))
	result := make([]string, 0, len(platforms))
	for _, platform := range platforms {
		platform = normalizeAgentToolPlatform(platform)
		if platform == "" {
			continue
		}
		if _, ok := seen[platform]; ok {
			continue
		}
		seen[platform] = struct{}{}
		result = append(result, platform)
	}
	sort.Strings(result)
	return result
}

func normalizeAgentToolPlatform(platform string) string {
	platform = strings.TrimSpace(platform)
	if platform == "" {
		return ""
	}
	if deviceType, ok := normalizeDeviceType(platform); ok {
		return deviceTypePlatform(deviceType)
	}
	return strings.ToLower(platform)
}

func NewToolSpecs(tools []langtools.Tool) *ToolSpecs {
	specs := &ToolSpecs{
		byName: make(map[string]ToolSpec, len(tools)),
		names:  make([]string, 0, len(tools)),
	}
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		spec := NewToolSpec(tool)
		key := toolSpecKey(spec.Name)
		if _, exists := specs.byName[key]; !exists {
			specs.names = append(specs.names, spec.Name)
		}
		specs.byName[key] = spec
	}
	sort.Strings(specs.names)
	return specs
}

func NewToolSpec(tool langtools.Tool) ToolSpec {
	if tool == nil {
		return ToolSpec{}
	}
	name := tool.Name()
	meta := builtInToolSpecMetadata[name]
	// Agent tools can be injected by runtime deps without exposure metadata.
	agentExposed := true
	if meta.AgentExposed != nil {
		agentExposed = *meta.AgentExposed
	}
	httpExposed := true
	if meta.HTTPExposed != nil {
		httpExposed = *meta.HTTPExposed
	}

	// Use dynamic example input if the tool provides it
	exampleInput := meta.ExampleInput
	if provider, ok := tool.(dynamicExampleInputProvider); ok {
		if dynamicExample := provider.DynamicExampleInput(); dynamicExample != "" {
			exampleInput = dynamicExample
		}
	}

	return ToolSpec{
		Tool:           tool,
		Name:           name,
		Description:    strings.TrimSpace(tool.Description()),
		Category:       defaultString(meta.Category, "general"),
		InputMode:      defaultString(meta.InputMode, toolInputModeText),
		ExampleInput:   exampleInput,
		AgentExposed:   agentExposed,
		HTTPExposed:    httpExposed,
		AgentPlatforms: normalizeAgentToolPlatforms(meta.AgentPlatforms),
	}
}

func (s *ToolSpecs) Lookup(name string) (ToolSpec, bool) {
	if s == nil {
		return ToolSpec{}, false
	}
	spec, ok := s.byName[toolSpecKey(name)]
	return spec, ok
}

func (s *ToolSpecs) All() []ToolSpec {
	if s == nil {
		return nil
	}
	result := make([]ToolSpec, 0, len(s.names))
	for _, name := range s.names {
		if spec, ok := s.Lookup(name); ok {
			result = append(result, spec)
		}
	}
	return result
}

// AgentTools returns conversational tools without device platform filtering.
// Runtime model callers should use AgentToolsForPlatform.
func (s *ToolSpecs) AgentTools() []langtools.Tool {
	return s.AgentToolsForPlatform("")
}

// AgentToolsForPlatform returns conversational tools for the current
// device_type-derived platform. Platform-specific tools must declare their
// allowed platforms in builtInToolSpecMetadata; empty metadata means portable.
func (s *ToolSpecs) AgentToolsForPlatform(platform string) []langtools.Tool {
	if s == nil {
		return nil
	}
	tools := make([]langtools.Tool, 0, len(s.names))
	for _, spec := range s.All() {
		if !spec.AgentExposed {
			continue
		}
		if !spec.AgentAvailableForPlatform(platform) {
			continue
		}
		tools = append(tools, spec.Tool)
	}
	return tools
}

func (spec ToolSpec) AgentAvailableForPlatform(platform string) bool {
	if len(spec.AgentPlatforms) == 0 {
		return true
	}
	platform = normalizeAgentToolPlatform(platform)
	if platform == "" {
		return true
	}
	for _, allowed := range spec.AgentPlatforms {
		if allowed == platform {
			return true
		}
	}
	return false
}

func (s *ToolSpecs) HTTPDescriptors() []ToolDescriptor {
	if s == nil {
		return nil
	}
	descriptors := make([]ToolDescriptor, 0, len(s.names))
	for _, spec := range s.All() {
		if spec.HTTPExposed {
			descriptors = append(descriptors, spec.Descriptor())
		}
	}
	return descriptors
}

func (s *ToolSpecs) LookupHTTP(name string) (ToolSpec, bool) {
	spec, ok := s.Lookup(name)
	if !ok || !spec.HTTPExposed {
		return ToolSpec{}, false
	}
	return spec, true
}

func (spec ToolSpec) Descriptor() ToolDescriptor {
	return ToolDescriptor{
		Name:         spec.Name,
		Category:     defaultString(spec.Category, "general"),
		Description:  strings.TrimSpace(spec.Description),
		InputMode:    defaultString(spec.InputMode, toolInputModeText),
		ExampleInput: spec.ExampleInput,
		ArgsSchema:   spec.LLMSchema(),
		HTTP: ToolHTTPBinding{
			Method: "POST",
			Path:   "/api/tools/" + spec.Name,
		},
	}
}

func (spec ToolSpec) LLMTool() llms.Tool {
	return llms.Tool{
		Type: "function",
		Function: &llms.FunctionDefinition{
			Name:        spec.Name,
			Description: strings.TrimSpace(spec.Description),
			Parameters:  spec.LLMSchema(),
		},
	}
}

func (spec ToolSpec) LLMSchema() map[string]any {
	if structured, ok := spec.Tool.(structuredInputTool); ok {
		if schema := structured.ArgsSchema(); len(schema) > 0 {
			return toolParametersSchema(schema)
		}
	}
	return genericToolParameters()
}

func (spec ToolSpec) NormalizeInput(input string) string {
	input = normalizeToolInput(input)
	input = unwrapCompatibleToolInput(input)
	input = normalizeToolInput(input)
	if defaultString(spec.InputMode, toolInputModeJSON) == toolInputModeJSON {
		input = repairJSONStringValueQuotes(input)
		input = repairJSONControlCharsInStrings(input)
	}
	return input
}

func (spec ToolSpec) ValidateInput(input string) error {
	if defaultString(spec.InputMode, toolInputModeJSON) != toolInputModeJSON {
		return nil
	}
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil
	}
	if !json.Valid([]byte(trimmed)) {
		if spec.Name == "touch_gesture" {
			return fmt.Errorf(`touch_gesture arguments are invalid JSON, so no touch action was executed.
Retry touch_gesture now with strict JSON.
For atomic touch programs, use {"actions":[{"action":"touch_down","point":{"x":500,"y":500}},{"action":"touch_up"}]}.
point/start/end must be JSON objects containing both named keys "x" and "y".
Invalid: {"type":"tap","point":{"x":500,500}}
Correct: {"type":"tap","point":{"x":500,"y":500}}
Do not continue with another action until the corrected call succeeds.`)
		}
		return fmt.Errorf("%s input must be valid JSON after compatibility conversion", spec.Name)
	}
	if spec.Name == "touch_gesture" {
		return validateTouchGestureFields(trimmed)
	}
	return nil
}

// Reject incomplete standard gestures before a tool wrapper captures its baseline.
func validateTouchGestureFields(input string) error {
	var args struct {
		Type      string          `json:"type"`
		Actions   json.RawMessage `json:"actions"`
		Point     json.RawMessage `json:"point"`
		Start     json.RawMessage `json:"start"`
		End       json.RawMessage `json:"end"`
		Direction string          `json:"direction"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return fmt.Errorf("touch_gesture requires a JSON object: %w", err)
	}
	present := func(value json.RawMessage) bool { return len(value) > 0 && string(value) != "null" }
	if present(args.Actions) {
		return nil // The adapter validates the advanced atomic program.
	}
	var missing string
	switch strings.ToLower(strings.TrimSpace(args.Type)) {
	case "":
		missing = "type is required"
	case "tap", "double_tap", "long_press", "drag_start", "drag_release":
		if !present(args.Point) {
			missing = "point is required for " + args.Type
		}
	case "swipe":
		if !present(args.Start) {
			missing = "start is required for swipe"
		} else if !present(args.End) && strings.TrimSpace(args.Direction) == "" {
			missing = "end or direction is required for swipe"
		}
	}
	if missing == "" {
		for _, field := range []struct {
			name  string
			value json.RawMessage
		}{
			{"point", args.Point},
			{"start", args.Start},
			{"end", args.End},
		} {
			if len(field.value) != 0 && !validTouchGestureCoordinate(field.value) {
				missing = field.name + " must be a JSON object containing both numeric x and y coordinates"
				break
			}
		}
	}
	if missing != "" {
		return fmt.Errorf("%s; no touch action was executed. Retry with all required fields together, preserving the intended gesture. Swipe example: {\"type\":\"swipe\",\"start\":{\"x\":500,\"y\":900},\"direction\":\"up\"}. Tap example: {\"type\":\"tap\",\"point\":{\"x\":500,\"y\":500}}", missing)
	}
	return nil
}

func validTouchGestureCoordinate(raw json.RawMessage) bool {
	var point map[string]json.RawMessage
	if err := json.Unmarshal(raw, &point); err != nil {
		return false
	}
	for _, axis := range []string{"x", "y"} {
		var value *float64
		if err := json.Unmarshal(point[axis], &value); err != nil || value == nil {
			return false
		}
	}
	return true
}

func unwrapCompatibleToolInput(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return input
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
		return input
	}
	rawArg, ok := fields["__arg1"]
	if !ok {
		return input
	}
	var arg string
	if err := json.Unmarshal(rawArg, &arg); err != nil {
		return input
	}
	return arg
}

func repairJSONControlCharsInStrings(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || json.Valid([]byte(trimmed)) {
		return input
	}

	var b strings.Builder
	b.Grow(len(input))
	inString := false
	escaped := false
	for _, r := range input {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if inString {
			switch r {
			case '\\':
				b.WriteRune(r)
				escaped = true
			case '"':
				b.WriteRune(r)
				inString = false
			default:
				if !writeEscapedJSONControlRune(&b, r) {
					b.WriteRune(r)
				}
			}
			continue
		}

		b.WriteRune(r)
		if r == '"' {
			inString = true
		}
	}

	repaired := b.String()
	if json.Valid([]byte(strings.TrimSpace(repaired))) {
		return repaired
	}
	return input
}

func repairJSONStringValueQuotes(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || json.Valid([]byte(trimmed)) {
		return input
	}

	runes := []rune(input)
	var b strings.Builder
	b.Grow(len(input))
	inString := false
	escaped := false
	valueString := false
	var prevSignificant rune
	var containers []rune
	for i, r := range runes {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if inString {
			switch r {
			case '\\':
				b.WriteRune(r)
				escaped = true
			case '"':
				next := nextNonJSONSpaceRune(runes, i+1)
				if stringQuoteCloses(valueString, next) {
					b.WriteRune(r)
					inString = false
				} else {
					b.WriteString(`\"`)
				}
			default:
				if !writeEscapedJSONControlRune(&b, r) {
					b.WriteRune(r)
				}
			}
			continue
		}

		b.WriteRune(r)
		if r == '"' {
			inString = true
			escaped = false
			valueString = startsJSONStringValue(prevSignificant, containers)
			continue
		}
		if !isJSONSpace(r) {
			containers = updateJSONContainerStack(containers, r)
			prevSignificant = r
		}
	}

	repaired := b.String()
	if json.Valid([]byte(strings.TrimSpace(repaired))) {
		return repaired
	}
	return input
}

func writeEscapedJSONControlRune(b *strings.Builder, r rune) bool {
	switch r {
	case '\n':
		b.WriteString(`\n`)
	case '\r':
		b.WriteString(`\r`)
	case '\t':
		b.WriteString(`\t`)
	case '\b':
		b.WriteString(`\b`)
	case '\f':
		b.WriteString(`\f`)
	default:
		if r < 0x20 {
			fmt.Fprintf(b, `\u%04x`, r)
			return true
		}
		return false
	}
	return true
}

func startsJSONStringValue(prevSignificant rune, containers []rune) bool {
	return prevSignificant == ':' || prevSignificant == '[' || (prevSignificant == ',' && currentJSONContainer(containers) == '[')
}

func updateJSONContainerStack(containers []rune, r rune) []rune {
	switch r {
	case '{', '[':
		return append(containers, r)
	case '}', ']':
		if len(containers) > 0 {
			return containers[:len(containers)-1]
		}
	}
	return containers
}

func currentJSONContainer(containers []rune) rune {
	if len(containers) == 0 {
		return 0
	}
	return containers[len(containers)-1]
}

func stringQuoteCloses(valueString bool, next rune) bool {
	if valueString {
		return next == 0 || next == ',' || next == '}' || next == ']'
	}
	return next == ':'
}

func nextNonJSONSpaceRune(runes []rune, start int) rune {
	for i := start; i < len(runes); i++ {
		if !isJSONSpace(runes[i]) {
			return runes[i]
		}
	}
	return 0
}

func isJSONSpace(r rune) bool {
	return r == ' ' || r == '\n' || r == '\r' || r == '\t'
}

func toolSpecKey(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}
