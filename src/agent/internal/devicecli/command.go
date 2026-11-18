// Package devicecli maps the device CLI protocol to the existing HTTP Tool API.
package devicecli

import (
	"fmt"
	"strconv"
)

const Usage = `device-cli [--url URL] COMMAND
  look | wait [milliseconds] | home | back | enter
  tap X Y | double-tap X Y | long-press X Y
  swipe X1 Y1 X2 Y2 [milliseconds] | drag X1 Y1 X2 Y2
  drag-start X Y | drag-release X Y | type TEXT | key KEY [KEY ...]
  open APP | url URL | quick ACTION | scroll DY | move X Y
  utg-status | utg-goto TARGET | utg-step EDGE | utg-tag STATE LABEL
Coordinates are integers from 0 to 1000. Quote text as one shell argument.`

type Call struct {
	Tool  string
	Input map[string]any
}

func Calls(args []string) ([]Call, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("missing command")
	}
	cmd, a := args[0], args[1:]
	one := func(tool string, fields map[string]any) ([]Call, error) { return []Call{{tool, fields}}, nil }
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
			return []Call{{"touch_gesture", map[string]any{"type": "drag_start", "point": p}}, {"touch_gesture", map[string]any{"type": "drag_release", "point": q}}}, nil
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
		if len(a) == 0 {
			return one("screenshot", map[string]any{})
		}
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
			return one("keyboard_tap", map[string]any{"keys": a})
		}
	case "type":
		if len(a) == 1 {
			return one("enter_text", map[string]any{"text": a[0]})
		}
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
	case "wait":
		if len(a) <= 1 {
			v := map[string]any{}
			if len(a) == 1 {
				ms, e := strconv.Atoi(a[0])
				if e != nil || ms < 1 || ms > 30000 {
					return nil, fmt.Errorf("wait must be 1–30000 ms")
				}
				v["timeout_ms"] = ms
			}
			return one("wait_for_stable_screen", v)
		}
	}
	return nil, fmt.Errorf("invalid arguments or unknown command: %s", cmd)
}
