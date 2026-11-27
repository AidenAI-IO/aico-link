// Package devicecli maps the device CLI protocol to the existing HTTP Tool API.
package devicecli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TapSettleDelay gives the UI time to respond before the next CLI command.
const TapSettleDelay = 50 * time.Millisecond

// Text routes for the enter_text tool. Omitting route preserves legacy behavior.
const (
	TextRouteDevice = "device"
	TextRouteApp    = "app"
)

type Call struct {
	Tool    string
	Input   map[string]any
	AppData bool // companion app required; no visual observation
}

func Calls(args []string) ([]Call, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("missing command")
	}
	cmd, a := args[0], args[1:]
	one := func(tool string, fields map[string]any) ([]Call, error) {
		return []Call{{Tool: tool, Input: fields}}, nil
	}
	point := func(i int) (map[string]int, error) {
		if len(a) < i+2 {
			return nil, fmt.Errorf("%s expects coordinate pairs", cmd)
		}
		x, ex := strconv.Atoi(a[i])
		y, ey := strconv.Atoi(a[i+1])
		if ex != nil || ey != nil || x < 0 || x > 1000 || y < 0 || y > 1000 {
			return nil, fmt.Errorf("coordinates must be integers between 0 and 1000")
		}
		return map[string]int{"x": x, "y": y}, nil
	}
	switch cmd {
	case "tap", "double-tap", "long-press", "drag-start", "drag-release", "move":
		if len(a) != 2 {
			break
		}
		p, e := point(0)
		if e != nil {
			return nil, e
		}
		if cmd == "move" {
			return one("mouse_move", map[string]any{"x": p["x"], "y": p["y"]})
		}
		kinds := map[string]string{"double-tap": "double_tap", "long-press": "long_press", "drag-start": "drag_start", "drag-release": "drag_release"}
		kind := kinds[cmd]
		if kind == "" {
			kind = cmd
		}
		return one("touch_gesture", map[string]any{"type": kind, "point": p})
	case "swipe", "drag":
		if len(a) != 4 && !(cmd == "swipe" && len(a) == 5) {
			break
		}
		p, e := point(0)
		if e != nil {
			return nil, e
		}
		q, e := point(2)
		if e != nil {
			return nil, e
		}
		if cmd == "drag" {
			return []Call{{Tool: "touch_gesture", Input: map[string]any{"type": "drag_start", "point": p}}, {Tool: "touch_gesture", Input: map[string]any{"type": "drag_release", "point": q}}}, nil
		}
		v := map[string]any{"type": "swipe", "start": p, "end": q}
		if len(a) == 5 {
			ms, e := strconv.Atoi(a[4])
			if e != nil || ms < 1 || ms > 30000 {
				return nil, fmt.Errorf("invalid swipe duration")
			}
			v["duration_ms"] = ms
		}
		return one("touch_gesture", v)
	case "look":
		if _, err := ParseLookOptions(a); err != nil {
			return nil, err
		}
		return one("screenshot", map[string]any{})
	case "home", "back":
		if len(a) == 0 {
			return one("quick_action", map[string]any{"action": cmd})
		}
	case "enter":
		if len(a) == 0 {
			return one("keyboard_tap", map[string]any{"keys": []string{"enter"}})
		}
	case "key":
		if len(a) > 0 {
			keys := make([]string, 0, len(a))
			for _, arg := range a {
				parts := []string{arg}
				if arg != "+" {
					parts = strings.Split(arg, "+")
				}
				for _, key := range parts {
					key = strings.TrimSpace(key)
					if key == "" {
						return nil, fmt.Errorf("invalid key chord %q: empty key", arg)
					}
					keys = append(keys, key)
				}
			}
			return one("keyboard_tap", map[string]any{"keys": keys})
		}
	case "type":
		if len(a) == 1 {
			return one("enter_text", map[string]any{"text": a[0], "route": TextRouteDevice})
		}
	case "app":
		if len(a) == 2 && a[0] == "type" {
			return one("enter_text", map[string]any{"text": a[1], "route": TextRouteApp})
		}
		return appDataCalls(a)
	case "open":
		if len(a) == 1 {
			return one("open_app", map[string]any{"app": a[0]})
		}
	case "url":
		if len(a) == 1 {
			return one("open_url", map[string]any{"url": a[0]})
		}
	case "quick":
		if len(a) == 1 {
			return one("quick_action", map[string]any{"action": a[0]})
		}
	case "scroll":
		if len(a) == 1 {
			n, e := strconv.Atoi(a[0])
			if e != nil {
				return nil, fmt.Errorf("invalid scroll delta")
			}
			return one("mouse_scroll", map[string]any{"delta": n})
		}
	}
	return nil, fmt.Errorf("invalid arguments or unknown command: %s", cmd)
}

// ParseWaitDuration parses the arguments after wait. An omitted duration
// keeps the previous default two-second budget as a fixed delay.
func ParseWaitDuration(args []string) (time.Duration, error) {
	if len(args) == 0 {
		return 2 * time.Second, nil
	}
	if len(args) == 1 {
		ms, err := strconv.Atoi(args[0])
		if err == nil && ms >= 0 && ms <= 30000 {
			return time.Duration(ms) * time.Millisecond, nil
		}
	}
	return 0, fmt.Errorf("usage: device-cli wait [milliseconds] (0–30000)")
}

// App data commands reuse the existing Bridge schemas.
func appDataCalls(args []string) ([]Call, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("expected app type, clipboard, calendar, contacts, or notification command")
	}
	domain, action := args[0], args[1]
	fields := map[string]any{}
	switch domain {
	case "clipboard":
		switch {
		case action == "read" && len(args) == 2:
		case action == "write" && len(args) == 3:
			fields["text"] = args[2]
		default:
			return nil, fmt.Errorf("usage: device-cli app clipboard read|write [TEXT]")
		}
	case "calendar", "contacts", "notification":
		var err error
		fields, err = appFlagFields(domain+" "+action, args[2:])
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown app command: %s", domain)
	}
	fields["action"] = action
	return []Call{{Tool: "bridge_" + domain, Input: fields, AppData: true}}, nil
}

// appFlagFields maps named flags to the existing Phone Bridge payload.
func appFlagFields(command string, args []string) (map[string]any, error) {
	const contactFields = "phone-number email organization notes"
	var required, optional string
	switch command {
	case "calendar query":
		required = "from to"
	case "calendar create":
		required = "title start-at"
		optional = "end-at all-day location notes alarm-minutes-before"
	case "calendar delete":
		required = "event-id"
	case "contacts query":
		optional = "query limit"
	case "contacts create":
		required, optional = "name", contactFields
	case "contacts update":
		required, optional = "contact-id", "name "+contactFields
	case "notification send":
		required, optional = "title", "body schedule-at sound badge"
	default:
		return nil, fmt.Errorf("unknown app command %q", command)
	}
	fields := make(map[string]any)
	allowed := strings.Fields(required + " " + optional)
	for len(args) > 0 {
		flag, value, inline := strings.Cut(args[0], "=")
		name := strings.TrimPrefix(flag, "--")
		if command == "calendar query" {
			switch flag {
			case "--start":
				name = "from"
			case "--end":
				name = "to"
			}
		}
		if name == flag || !slices.Contains(allowed, name) {
			return nil, fmt.Errorf("invalid app %s option %q; use --%s", command, flag, strings.Join(allowed, ", --"))
		}
		key := strings.ReplaceAll(name, "-", "_")
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate app option %s", flag)
		}
		args = args[1:]
		boolean := name == "all-day" || name == "sound"
		if !inline && !boolean {
			if len(args) == 0 || strings.HasPrefix(args[0], "--") {
				return nil, fmt.Errorf("%s requires a value", flag)
			}
			value, args = args[0], args[1:]
		}
		switch name {
		case "all-day", "sound":
			if inline && value != "true" && value != "false" {
				return nil, fmt.Errorf("%s expects true or false", flag)
			}
			fields[key] = !inline || value == "true"
		case "alarm-minutes-before", "limit", "badge":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || (name == "limit" && n == 0) {
				if name == "limit" {
					return nil, fmt.Errorf("%s expects a positive integer", flag)
				}
				return nil, fmt.Errorf("%s expects a non-negative integer", flag)
			}
			fields[key] = n
		case "phone-number", "email":
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%s requires a nonempty value", flag)
			}
			key = "emails"
			if name == "phone-number" {
				key = "phone_numbers"
			}
			values, _ := fields[key].([]string)
			fields[key] = append(values, value)
		case "from", "to", "start-at", "end-at", "schedule-at":
			if _, err := time.Parse(time.RFC3339, value); err != nil {
				return nil, fmt.Errorf("%s expects an RFC3339 time with a timezone", flag)
			}
			fields[key] = value
		default:
			fields[key] = value
		}
	}
	for _, name := range strings.Fields(required) {
		value, _ := fields[strings.ReplaceAll(name, "-", "_")].(string)
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("app %s requires --%s", command, name)
		}
	}
	return fields, nil
}
