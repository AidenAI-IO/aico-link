package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func lookupStringField(jsonText, field string) string {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonText), &probe); err != nil {
		return ""
	}
	raw, ok := probe[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// extractToolCallCoords 解析一个 touch/swipe 类工具的输入，给出形如
// "x=500,y=850"（tap）或 "x=500,y=800→x=500,y=400"（swipe）的紧凑坐标摘要。
// 找不到坐标返回空串。
func extractToolCallCoords(toolInput string) string {
	input := strings.TrimSpace(toolInput)
	if input == "" || input[0] != '{' {
		return ""
	}
	target := input
	// 拆开 __arg1 包裹，如果存在的话。
	if arg := lookupStringField(input, "__arg1"); arg != "" && strings.HasPrefix(strings.TrimSpace(arg), "{") {
		target = arg
	}

	var probe struct {
		Type  string         `json:"type"`
		Point map[string]any `json:"point"`
		Start map[string]any `json:"start"`
		End   map[string]any `json:"end"`
	}
	if err := json.Unmarshal([]byte(target), &probe); err != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(probe.Type)) {
	case "swipe":
		start := pointSummary(probe.Start)
		end := pointSummary(probe.End)
		if start != "" && end != "" {
			return start + "→" + end
		}
		return ""
	default:
		return pointSummary(probe.Point)
	}
}

func pointSummary(point map[string]any) string {
	if len(point) == 0 {
		return ""
	}
	x, okX := numericField(point, "x")
	y, okY := numericField(point, "y")
	if !okX || !okY {
		return ""
	}
	return fmt.Sprintf("x=%.0f,y=%.0f", x, y)
}

func numericField(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch typed := v.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case json.Number:
		f, err := typed.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return f, err == nil
	}
	return 0, false
}

// extractToolCallText 提取常见的"输入文本"参数（text / query / keyword 等）。
func extractToolCallText(toolInput string) string {
	input := strings.TrimSpace(toolInput)
	if input == "" || input[0] != '{' {
		return ""
	}
	target := input
	if arg := lookupStringField(input, "__arg1"); arg != "" && strings.HasPrefix(strings.TrimSpace(arg), "{") {
		target = arg
	}
	for _, key := range []string{"text", "query", "keyword", "input", "value"} {
		if v := lookupStringField(target, key); v != "" {
			return v
		}
	}
	return ""
}

// observedPagesByApp 收集 episode 中每个 app 出现过的所有 page_name，结果按
// 出现顺序去重，便于写入 app_profile.PagesSeen。
func observedPagesByApp(events []TaskEpisodeEvent) map[string][]string {
	out := map[string][]string{}
	seen := map[string]map[string]bool{}
	add := func(app, page string) {
		app = strings.TrimSpace(app)
		page = strings.TrimSpace(page)
		if app == "" || page == "" {
			return
		}
		if seen[app] == nil {
			seen[app] = map[string]bool{}
		}
		if seen[app][page] {
			return
		}
		seen[app][page] = true
		out[app] = append(out[app], page)
	}
	for _, evt := range events {
		if evt.ObservedState == nil {
			continue
		}
		add(evt.ObservedState.AppName, evt.ObservedState.PageName)
	}
	return out
}

// observedToolsByApp 统计每个 app 在成功 tool_call 中用到的工具名。
// 工具调用归属到当前 app（从之前最近的 verifier 观察或之后最近的观察推断）。
func observedToolsByApp(events []TaskEpisodeEvent) map[string][]string {
	out := map[string][]string{}
	seen := map[string]map[string]bool{}

	// 预扫描：记录每个 verifier_decision 的位置和 app
	appAtIndex := make([]string, len(events))
	lastApp := ""
	for i := range events {
		if events[i].Type == "verifier_decision" && events[i].ObservedState != nil {
			lastApp = strings.TrimSpace(events[i].ObservedState.AppName)
		}
		appAtIndex[i] = lastApp
	}

	// 扫描 tool_call，归属到当前 app（如果为空，往后找最近的）
	for i, evt := range events {
		if evt.Type != runEventToolCall || strings.TrimSpace(evt.ToolName) == "" {
			continue
		}
		app := appAtIndex[i]
		if app == "" {
			// 当前没有 app，往后找最近的 verifier
			for j := i + 1; j < len(events); j++ {
				if events[j].Type == "verifier_decision" && events[j].ObservedState != nil {
					app = strings.TrimSpace(events[j].ObservedState.AppName)
					break
				}
			}
		}
		if app == "" {
			continue
		}
		if seen[app] == nil {
			seen[app] = map[string]bool{}
		}
		if seen[app][evt.ToolName] {
			continue
		}
		seen[app][evt.ToolName] = true
		out[app] = append(out[app], evt.ToolName)
	}
	for app := range out {
		sort.Strings(out[app])
	}
	return out
}
