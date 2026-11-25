// Package devicecli maps the device CLI protocol to the existing HTTP Tool API.
package devicecli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const Usage = `device-cli [--url URL] COMMAND
  look [--dir DIRECTORY] [--region X1 Y1 X2 Y2]
  wait [milliseconds] | home | back | enter
  tap X Y | double-tap X Y | long-press X Y
  swipe X1 Y1 X2 Y2 [milliseconds] | drag X1 Y1 X2 Y2
  drag-start X Y | drag-release X Y | type TEXT | key KEY [KEY ...]
  app type TEXT | app clipboard read | app clipboard write TEXT
  app calendar query|create|delete JSON
  app contacts query|create|update JSON
  app notification send JSON
  open APP | url URL | quick ACTION | scroll DY | move X Y
App type requires a connected, FGS, or PiP route. App data uses the supported companion routes for each action. Quote JSON as a single object argument.
For chords, use key meta+a or key meta a. Use key '+' for a literal plus.
Coordinates are integers from 0 to 1000. Quote text as one shell argument.
look saves a JPEG (default: /tmp). wait pauses for 0–30000 ms (default: 2000 ms).`

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

// The underlying bridge decoders ignore unknown JSON fields. Reject them at
// the CLI boundary so a typo cannot silently create incomplete data.
var appDataFields = map[string]string{
	"calendar query":    "from to",
	"calendar create":   "title start_at end_at all_day location notes alarm_minutes_before",
	"calendar delete":   "event_id",
	"contacts query":    "query limit",
	"contacts create":   "name phone_numbers emails organization notes",
	"contacts update":   "contact_id name phone_numbers emails organization notes",
	"notification send": "title body schedule_at sound badge",
}

// App data commands reuse the existing Bridge schemas. The verb is selected
// by the CLI, never by a field in the JSON object.
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
		allowed, valid := appDataFields[domain+" "+action]
		if !valid || len(args) != 3 {
			return nil, fmt.Errorf("unsupported app command or missing JSON object: %s %s", domain, action)
		}
		if err := json.Unmarshal([]byte(args[2]), &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("app %s %s expects a JSON object", domain, action)
		}
		for key := range fields {
			if strings.EqualFold(key, "action") {
				return nil, fmt.Errorf("omit action from JSON; use the command verb")
			}
			if key == "" || strings.Contains(key, " ") || !strings.Contains(" "+allowed+" ", " "+key+" ") {
				return nil, fmt.Errorf("unknown field %q for app %s %s; allowed fields: %s", key, domain, action, allowed)
			}
		}
	default:
		return nil, fmt.Errorf("unknown app command: %s", domain)
	}
	fields["action"] = action
	return []Call{{Tool: "bridge_" + domain, Input: fields, AppData: true}}, nil
}
