package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

//go:embed quick_actions.json
var defaultQuickActionsJSON []byte

// QuickActionsFileName is the filename used to look up runtime overrides under
// configDir, and the bundled file shipped with the firmware.
const QuickActionsFileName = "quick_actions.json"

// BundledQuickActionsPath is the on-device install path, populated by
// the aico-business Debian package from
// src/agent/internal/agent/quick_actions.json.
const BundledQuickActionsPath = "/usr/share/aico/" + QuickActionsFileName

const (
	quickActionStatusActive   = "active"
	quickActionStatusReserved = "reserved"
)

var supportedQuickActionPlatforms = []string{"ios", "android", "mac", "windows", "linux"}

type quickActionBinding struct {
	Status       string               `json:"status"`
	Tool         string               `json:"tool"`
	Input        json.RawMessage      `json:"input"`
	Steps        []quickActionStep    `json:"steps,omitempty"`
	Note         string               `json:"note,omitempty"`
	Alternatives []quickActionBinding `json:"alternatives,omitempty"`
}

type quickActionStep struct {
	Tool         string          `json:"tool"`
	Input        json.RawMessage `json:"input"`
	DelayMsAfter int             `json:"delay_ms_after,omitempty"`
	Note         string          `json:"note,omitempty"`
}

type quickActionDefinition struct {
	Label     string                        `json:"label"`
	Aliases   []string                      `json:"aliases,omitempty"`
	Category  string                        `json:"category"`
	Platforms map[string]quickActionBinding `json:"platforms"`
}

type quickActionsDocument struct {
	Version int                              `json:"version"`
	Actions map[string]quickActionDefinition `json:"actions"`
}

type quickActionsTable struct {
	mu       sync.RWMutex
	document quickActionsDocument
	aliasMap map[string]string
	matchMap map[string]string
	source   string
}

var globalQuickActions = newQuickActionsTable()

func newQuickActionsTable() *quickActionsTable {
	t := &quickActionsTable{}
	if err := t.loadFromBytes(defaultQuickActionsJSON, "embedded"); err != nil {
		t.document = quickActionsDocument{Actions: map[string]quickActionDefinition{}}
		t.aliasMap = map[string]string{}
		t.matchMap = map[string]string{}
		t.source = "embedded(invalid)"
	}
	return t
}

func (t *quickActionsTable) loadFromBytes(data []byte, source string) error {
	var doc quickActionsDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc.Actions == nil {
		doc.Actions = map[string]quickActionDefinition{}
	}
	aliasMap := make(map[string]string)
	matchMap := make(map[string]string)
	for id, action := range doc.Actions {
		addQuickActionAlias(aliasMap, matchMap, id, id)
		addQuickActionAlias(aliasMap, matchMap, id, action.Label)
		for _, alias := range action.Aliases {
			addQuickActionAlias(aliasMap, matchMap, id, alias)
		}
	}
	t.mu.Lock()
	t.document = doc
	t.aliasMap = aliasMap
	t.matchMap = matchMap
	t.source = source
	t.mu.Unlock()
	return nil
}

func (t *quickActionsTable) loadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return t.loadFromBytes(data, path)
}

func normalizeQuickActionKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

func normalizeQuickActionMatchKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	var builder strings.Builder
	lastUnderscore := false
	for _, r := range key {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			lastUnderscore = false
		case unicode.IsSpace(r) || r == '_' || r == '-' || r == '/' || r == '.':
			if builder.Len() > 0 && !lastUnderscore {
				builder.WriteRune('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(builder.String(), "_")
}

func addQuickActionAlias(aliasMap, matchMap map[string]string, id, alias string) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return
	}
	aliasMap[normalizeQuickActionKey(alias)] = id
	matchMap[normalizeQuickActionMatchKey(alias)] = id
}

func normalizeQuickActionPlatform(platform string) (string, error) {
	platform = normalizeQuickActionKey(platform)
	switch platform {
	case "ios", "iphone", "ipad", "ipados":
		return "ios", nil
	case "android":
		return "android", nil
	case "mac", "macos", "osx":
		return "mac", nil
	case "windows", "win", "win32":
		return "windows", nil
	case "linux":
		return "linux", nil
	case "":
		return "", fmt.Errorf("platform is required (ios, android, mac, windows, linux)")
	default:
		return "", fmt.Errorf("unsupported platform %q (expected ios, android, mac, windows, linux)", platform)
	}
}

func (t *quickActionsTable) resolveActionID(action string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	id, ok := t.aliasMap[normalizeQuickActionKey(action)]
	if ok {
		return id, true
	}
	id, ok = t.matchMap[normalizeQuickActionMatchKey(action)]
	return id, ok
}

func (t *quickActionsTable) suggestActionIDs(action string, limit int) []string {
	query := normalizeQuickActionMatchKey(action)
	if query == "" || limit <= 0 {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	scores := map[string]int{}
	for alias, id := range t.matchMap {
		if alias == "" {
			continue
		}
		score := quickActionSuggestionScore(query, alias, id)
		if score <= 0 {
			continue
		}
		if score > scores[id] {
			scores[id] = score
		}
	}

	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] == scores[ids[j]] {
			return ids[i] < ids[j]
		}
		return scores[ids[i]] > scores[ids[j]]
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}

func quickActionSuggestionScore(query, alias, id string) int {
	switch {
	case query == alias || query == id:
		return 100
	case strings.Contains(alias, query):
		return 80 - len(alias) + len(query)
	case strings.Contains(query, alias):
		return 70 - len(query) + len(alias)
	}

	queryParts := strings.Split(query, "_")
	aliasParts := strings.Split(alias, "_")
	shared := 0
	for _, qp := range queryParts {
		if qp == "" {
			continue
		}
		for _, ap := range aliasParts {
			if qp == ap {
				shared++
				break
			}
		}
	}
	if shared == 0 {
		return 0
	}
	return 10 + shared*10
}

func (t *quickActionsTable) lookup(actionID, platform string) (quickActionDefinition, quickActionBinding, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	action, ok := t.document.Actions[actionID]
	if !ok {
		return quickActionDefinition{}, quickActionBinding{}, false
	}
	binding, ok := action.Platforms[platform]
	if !ok {
		return action, quickActionBinding{}, false
	}
	return action, binding, true
}

func (t *quickActionsTable) activeActionIDs(platform string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	ids := make([]string, 0, len(t.document.Actions))
	for id, action := range t.document.Actions {
		if platform == "" {
			for _, binding := range action.Platforms {
				if bindingStatus(binding) == quickActionStatusActive {
					ids = append(ids, id)
					break
				}
			}
			continue
		}
		binding, ok := action.Platforms[platform]
		if ok && bindingStatus(binding) == quickActionStatusActive {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// loadQuickActionsForConfig picks the highest-priority mapping file available
// and loads it into the global table. Order: configDir override → bundled
// firmware file → embedded defaults (already loaded at init).
func loadQuickActionsForConfig(configDir string, logger *Logger) {
	candidates := make([]string, 0, 2)
	if configDir != "" {
		candidates = append(candidates, filepath.Join(configDir, QuickActionsFileName))
	}
	candidates = append(candidates, BundledQuickActionsPath)

	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := globalQuickActions.loadFromFile(path); err != nil {
			if logger != nil {
				logger.Warn("quick_actions: failed to load %s: %v (falling back)", path, err)
			}
			continue
		}
		if logger != nil {
			logger.Info("quick_actions: loaded %s", path)
		}
		return
	}
	if logger != nil {
		logger.Info("quick_actions: using embedded defaults")
	}
}

// QuickActionTool executes predefined platform-specific shortcuts and gestures.
type QuickActionTool struct {
	keyboard             *KeyboardTapTool
	touch                *TouchGestureTool
	deviceTypeFn         func() string
	iosKeyboardIsolation *iosKeyboardIsolationController
}

func (t *QuickActionTool) Name() string { return "quick_action" }

func (t *QuickActionTool) SetDeviceTypeFunc(fn func() string) {
	if t != nil {
		t.deviceTypeFn = fn
	}
}

func (t *QuickActionTool) Description() string {
	return strings.TrimSpace(`Execute a predefined, platform-aware shortcut from quick_actions.json. Cataloged semantic actions MUST use quick_action instead of manually constructed modifier chords. Explicit physical-key requests and uncataloged app-specific shortcuts may use keyboard_tap; a cataloged chord fallback requires a quick_action result in the current run that explicitly reports reserved/unavailable before execution. ` +
		`For go-home/home-screen requests such as 回到桌面, call {"action":"home"} first; on Android this delegates to keyboard_tap {"keys":["KEYCODE_HOME"]}, while gesture fallbacks use touch_gesture type=swipe with explicit edge coordinates. ` +
		`The runtime selects device-specific bindings from global device_type state. Use {"action":"list"} to inspect actions for the configured device_type.`)
}

func (t *QuickActionTool) ArgsSchema() map[string]any {
	actionDescription := `Required active action id for the configured device_type, or "list" to inspect active actions.`
	actionSchema := stringArgSchema(actionDescription)
	if platform := t.schemaPlatform(); platform != "" {
		actions := append([]string{"list"}, globalQuickActions.activeActionIDs(platform)...)
		actionSchema = stringEnumArgSchema(actionDescription, actions...)
	}
	return objectArgsSchema(map[string]any{
		"action":            actionSchema,
		"list":              boolArgSchema("Legacy alternative to action=list. New calls should always provide action."),
		"alternative":       boolArgSchema("Set true to execute an alternative binding listed by a previous quick_action result."),
		"alternative_index": minIntegerArgSchema("1-based alternative binding index; defaults to 1 when alternative=true.", 1),
	}, "action")
}

func (t *QuickActionTool) schemaPlatform() string {
	if t != nil && t.deviceTypeFn != nil {
		rawPlatform := quickActionPlatformFromDeviceType(t.deviceTypeFn())
		platform, err := normalizeQuickActionPlatform(rawPlatform)
		if err == nil {
			return platform
		}
		return normalizeAgentToolPlatform(rawPlatform)
	}
	return ""
}

type quickActionArgs struct {
	Action       string `json:"action"`
	List         bool   `json:"list"`
	Alternative  bool   `json:"alternative"`
	AlternativeN int    `json:"alternative_index"`
}

func (t *QuickActionTool) Call(ctx context.Context, input string) (string, error) {
	var controller *iosKeyboardIsolationController
	if t != nil {
		controller = t.iosKeyboardIsolation
	}
	return withIOSKeyboardIsolationBatchCall(ctx, controller, func(batchCtx context.Context) (string, error) {
		return t.call(batchCtx, input)
	})
}

func (t *QuickActionTool) call(ctx context.Context, input string) (string, error) {
	var args quickActionArgs
	trimmed := strings.TrimSpace(input)
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("quick_action start input_len=%d mapping_before={%s}", len(input), formatTouchscreenRCAQuickActionMappingSummary(t))
	}
	if trimmed == "" {
		te := NewToolError(CodeInvalidArguments, "action or list is required")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if strings.HasPrefix(trimmed, "{") {
		if err := decodeStrictJSONObject(trimmed, &args); err != nil {
			te := NewToolError(CodeInvalidArguments, fmt.Sprintf("invalid input: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	} else {
		args.Action = trimmed
	}

	platformInput := ""
	if t != nil && t.deviceTypeFn != nil {
		platformInput = strings.TrimSpace(quickActionPlatformFromDeviceType(t.deviceTypeFn()))
	}
	platform, err := normalizeQuickActionPlatform(platformInput)
	if err != nil {
		te := NewToolError(CodeInvalidArguments, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	if strings.EqualFold(strings.TrimSpace(args.Action), "list") {
		args.List = true
	}
	if args.List {
		return t.listJSON(platform), nil
	}

	actionID, ok := globalQuickActions.resolveActionID(args.Action)
	if !ok {
		suggestions := globalQuickActions.suggestActionIDs(args.Action, 5)
		message := fmt.Sprintf("unknown action %q; use {\"action\":\"list\"}", args.Action)
		if len(suggestions) > 0 {
			message = fmt.Sprintf("%s; suggested actions: %s", message, strings.Join(suggestions, ", "))
		}
		te := NewToolErrorWithDetails(CodeQuickActionUnknown, message,
			map[string]any{"action": args.Action, "platform": platform, "suggestions": suggestions})
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	action, binding, ok := globalQuickActions.lookup(actionID, platform)
	if !ok {
		te := NewToolErrorWithDetails(CodeQuickActionUnsupportedPlatform,
			fmt.Sprintf("action %q is not defined for platform %q", actionID, platform),
			map[string]any{"action": actionID, "platform": platform})
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	selected := binding
	selectedSource := "primary"
	if args.Alternative {
		idx := args.AlternativeN
		if idx <= 0 {
			idx = 1
		}
		if len(binding.Alternatives) < idx {
			te := NewToolError(CodeInvalidArguments,
				fmt.Sprintf("action %q has no alternative #%d on platform %q", actionID, idx, platform))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
		selected = binding.Alternatives[idx-1]
		selectedSource = fmt.Sprintf("alternative_%d", idx)
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf(
			"quick_action selected action=%q platform=%q binding=%q status=%q tool=%q steps=%d mapping_before_delegate={%s}",
			actionID,
			platform,
			selectedSource,
			selected.Status,
			selected.Tool,
			len(selected.Steps),
			formatTouchscreenRCAQuickActionMappingSummary(t),
		)
	}

	status := strings.TrimSpace(selected.Status)
	if status == "" {
		status = quickActionStatusReserved
	}
	if status == quickActionStatusReserved {
		note := strings.TrimSpace(selected.Note)
		if note == "" {
			note = "reserved on this platform"
		}
		te := NewToolErrorWithDetails(CodeQuickActionReserved, note,
			map[string]any{
				"action":   actionID,
				"label":    action.Label,
				"platform": platform,
				"status":   quickActionStatusReserved,
			})
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if status != quickActionStatusActive {
		te := NewToolError(CodeQuickActionInvalidBinding,
			fmt.Sprintf("action %q has unsupported status %q on platform %q", actionID, status, platform))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	toolName := strings.TrimSpace(selected.Tool)
	var payload []byte
	var output string
	var stepResults []map[string]interface{}
	if len(selected.Steps) > 0 {
		output = "ok"
		stepResults = make([]map[string]interface{}, 0, len(selected.Steps))
		for i, step := range selected.Steps {
			stepTool := strings.TrimSpace(step.Tool)
			if stepTool == "" {
				te := NewToolError(CodeQuickActionInvalidBinding,
					fmt.Sprintf("action %q step %d has no tool binding configured", actionID, i+1))
				SetToolError(ctx, te)
				return toolErrorString(te), nil
			}
			stepPayload, err := json.Marshal(step.Input)
			if err != nil {
				te := NewToolError(CodeQuickActionInvalidBinding,
					fmt.Sprintf("invalid action step %d input: %v", i+1, err))
				SetToolError(ctx, te)
				return toolErrorString(te), nil
			}
			stepOutput, subErr, err := t.delegate(ctx, stepTool, string(stepPayload))
			if err != nil {
				return "", err
			}
			stepResult := map[string]interface{}{
				"index":  i + 1,
				"tool":   stepTool,
				"input":  json.RawMessage(stepPayload),
				"output": stepOutput,
			}
			if step.DelayMsAfter > 0 {
				stepResult["delay_ms_after"] = step.DelayMsAfter
			}
			stepResults = append(stepResults, stepResult)
			if subErr != nil {
				te := NewToolErrorWithDetails(CodeSubtoolFailed,
					fmt.Sprintf("step %d (%s) failed: %s", i+1, stepTool, subErr.Message),
					map[string]any{
						"source":        "tool:quick_action",
						"action":        actionID,
						"label":         action.Label,
						"platform":      platform,
						"binding":       selectedSource,
						"failed_step":   i + 1,
						"subtool":       stepTool,
						"subtool_error": subErr,
						"steps":         stepResults,
					})
				te.Category = subErr.Category
				SetToolError(ctx, te)
				return toolErrorString(te), nil
			}
			if err := sleepQuickActionDelay(ctx, step.DelayMsAfter); err != nil {
				return "", err
			}
		}
	} else {
		if toolName == "" {
			note := strings.TrimSpace(selected.Note)
			if note == "" {
				note = "no tool binding configured"
			}
			te := NewToolError(CodeQuickActionInvalidBinding, note)
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}

		var err error
		payload, err = json.Marshal(selected.Input)
		if err != nil {
			te := NewToolError(CodeQuickActionInvalidBinding, fmt.Sprintf("invalid action input: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}

		var subErr *ToolError
		output, subErr, err = t.delegate(ctx, toolName, string(payload))
		if err != nil {
			return "", err
		}
		if subErr != nil {
			te := NewToolErrorWithDetails(CodeSubtoolFailed,
				fmt.Sprintf("step 1 (%s) failed: %s", toolName, subErr.Message),
				map[string]any{
					"source":        "tool:quick_action",
					"action":        actionID,
					"label":         action.Label,
					"platform":      platform,
					"binding":       selectedSource,
					"failed_step":   1,
					"subtool":       toolName,
					"subtool_error": subErr,
				})
			te.Category = subErr.Category
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}

	result := map[string]interface{}{
		"ok":       true,
		"action":   actionID,
		"label":    action.Label,
		"platform": platform,
		"binding":  selectedSource,
		"output":   output,
	}
	if len(selected.Steps) > 0 {
		result["tool"] = "sequence"
		result["steps"] = stepResults
	} else {
		result["tool"] = toolName
		result["input"] = json.RawMessage(payload)
	}
	if note := strings.TrimSpace(selected.Note); note != "" {
		result["note"] = note
	}
	if len(binding.Alternatives) > 0 {
		alts := make([]map[string]interface{}, 0, len(binding.Alternatives))
		for i, alt := range binding.Alternatives {
			alts = append(alts, map[string]interface{}{
				"index":  i + 1,
				"status": alt.Status,
				"tool":   alt.Tool,
				"note":   alt.Note,
			})
		}
		result["alternatives"] = alts
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("quick_action completed action=%q platform=%q tool=%q mapping_after_delegate={%s}", actionID, platform, toolName, formatTouchscreenRCAQuickActionMappingSummary(t))
	}
	return jsonString(result), nil
}

func quickActionPlatformFromDeviceType(deviceType string) string {
	normalized, ok := normalizeDeviceType(deviceType)
	if !ok {
		return ""
	}
	return normalizeAgentToolPlatform(deviceTypePlatform(normalized))
}

func (t *QuickActionTool) delegate(ctx context.Context, toolName, payload string) (string, *ToolError, error) {
	subCtx, _ := WithToolError(ctx)
	var output string
	var err error
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("quick_action delegate start tool=%q payload_len=%d mapping_before={%s}", toolName, len(payload), formatTouchscreenRCAQuickActionMappingSummary(t))
	}
	switch toolName {
	case "keyboard_tap":
		if t.keyboard == nil {
			return "", nil, fmt.Errorf("keyboard_tap is not available")
		}
		output, err = t.keyboard.Call(subCtx, payload)
	case "touch_gesture":
		if t.touch == nil {
			return "", nil, fmt.Errorf("touch_gesture is not available")
		}
		output, err = t.touch.Call(subCtx, payload)
	default:
		return "", nil, fmt.Errorf("unsupported delegated tool %q", toolName)
	}
	if err != nil {
		if touchscreenRCADebugEnabledCached() {
			touchscreenRCALogf("quick_action delegate error tool=%q err_type=%T mapping_after={%s}", toolName, err, formatTouchscreenRCAQuickActionMappingSummary(t))
		}
		return output, nil, err
	}
	if te := ToolErrorFromContext(subCtx); te != nil {
		if touchscreenRCADebugEnabledCached() {
			touchscreenRCALogf("quick_action delegate tool_error tool=%q code=%q category=%q mapping_after={%s}", toolName, te.Code, te.Category, formatTouchscreenRCAQuickActionMappingSummary(t))
		}
		return output, te, nil
	}
	if legacyToolOutputLooksLikeError(output) {
		trimmed := strings.TrimSpace(output)
		message := strings.TrimSpace(trimmed[len("error:"):])
		if touchscreenRCADebugEnabledCached() {
			touchscreenRCALogf("quick_action delegate legacy_error tool=%q output_len=%d mapping_after={%s}", toolName, len(output), formatTouchscreenRCAQuickActionMappingSummary(t))
		}
		return output, NewToolError(CodeToolExecutionFailed, message), nil
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("quick_action delegate completed tool=%q output_len=%d mapping_after={%s}", toolName, len(output), formatTouchscreenRCAQuickActionMappingSummary(t))
	}
	return output, nil, nil
}

var sleepQuickActionDelay = realSleepQuickActionDelay

func realSleepQuickActionDelay(ctx context.Context, delayMs int) error {
	if delayMs <= 0 {
		return nil
	}
	timer := time.NewTimer(time.Duration(delayMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *QuickActionTool) listJSON(platform string) string {
	type listItem struct {
		ID       string `json:"id"`
		Label    string `json:"label"`
		Category string `json:"category"`
		Platform string `json:"platform,omitempty"`
		Status   string `json:"status"`
		Tool     string `json:"tool,omitempty"`
		Note     string `json:"note,omitempty"`
	}

	globalQuickActions.mu.RLock()
	defer globalQuickActions.mu.RUnlock()

	ids := make([]string, 0, len(globalQuickActions.document.Actions))
	for id := range globalQuickActions.document.Actions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	items := make([]listItem, 0, len(ids))
	for _, id := range ids {
		action := globalQuickActions.document.Actions[id]
		if platform == "" {
			for _, p := range supportedQuickActionPlatforms {
				binding, ok := action.Platforms[p]
				if !ok {
					continue
				}
				if bindingStatus(binding) != quickActionStatusActive {
					continue
				}
				items = append(items, listItem{
					ID:       id,
					Label:    action.Label,
					Category: action.Category,
					Platform: p,
					Status:   bindingStatus(binding),
					Tool:     strings.TrimSpace(binding.Tool),
					Note:     firstNonEmpty(binding.Note, platformNote(id, p, binding)),
				})
			}
			continue
		}
		binding, ok := action.Platforms[platform]
		if !ok {
			continue
		}
		if bindingStatus(binding) != quickActionStatusActive {
			continue
		}
		items = append(items, listItem{
			ID:       id,
			Label:    action.Label,
			Category: action.Category,
			Platform: platform,
			Status:   bindingStatus(binding),
			Tool:     strings.TrimSpace(binding.Tool),
			Note:     firstNonEmpty(binding.Note, platformNote(id, platform, binding)),
		})
	}

	return jsonString(map[string]interface{}{
		"ok":       true,
		"platform": platform,
		"actions":  items,
	})
}

func bindingStatus(binding quickActionBinding) string {
	status := strings.TrimSpace(binding.Status)
	if status == "" {
		return quickActionStatusReserved
	}
	return status
}

func platformNote(actionID, platform string, binding quickActionBinding) string {
	if strings.TrimSpace(binding.Tool) != "" {
		return ""
	}
	return fmt.Sprintf("%s on %s has no primary tool binding", actionID, platform)
}
