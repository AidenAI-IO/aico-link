package devicecli

import (
	"reflect"
	"testing"
)

func TestKeyChord(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"meta+a"}, []string{"meta", "a"}},
		{[]string{"ctrl+shift", "tab"}, []string{"ctrl", "shift", "tab"}},
		{[]string{"ctrl", "a"}, []string{"ctrl", "a"}},
		{[]string{"+"}, []string{"+"}},
		{[]string{"ctrl++a"}, nil},
		{[]string{"ctrl+"}, nil},
		{[]string{"+a"}, nil},
	} {
		calls, err := Calls(append([]string{"key"}, tc.args...))
		if tc.want == nil {
			if err == nil || len(calls) != 0 {
				t.Errorf("%q: malformed chord made a call: %+v", tc.args, calls)
			}
			continue
		}
		if err != nil || len(calls) != 1 || calls[0].Tool != "keyboard_tap" || !reflect.DeepEqual(calls[0].Input["keys"], tc.want) {
			t.Errorf("%q: calls=%+v, err=%v; want one keyboard_tap with %q", tc.args, calls, err, tc.want)
		}
	}
}

func TestTypeRoutes(t *testing.T) {
	const text = "中文\nhello"
	for _, tc := range []struct {
		args  []string
		route string
	}{
		{[]string{"type", text}, TextRouteDevice},
		{[]string{"app", "type", text}, TextRouteApp},
		{[]string{"app"}, ""},
		{[]string{"app", "tap", "100", "200"}, ""},
	} {
		calls, err := Calls(tc.args)
		if tc.route == "" {
			if err == nil || len(calls) != 0 {
				t.Errorf("%q should be rejected", tc.args)
			}
			continue
		}
		if err != nil || len(calls) != 1 || calls[0].Tool != "enter_text" || !reflect.DeepEqual(calls[0].Input, map[string]any{"text": text, "route": tc.route}) {
			t.Errorf("%q mapped to %+v, err=%v", tc.args, calls, err)
		}
	}
}

func TestAppDataCommandRestrictsAction(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		tool, action string
	}{
		{[]string{"clipboard", "write", ""}, "bridge_clipboard", "write"},
		{[]string{"calendar", "delete", `{"event_id":"e1"}`}, "bridge_calendar", "delete"},
		{[]string{"contacts", "create", `{"name":"Alice","emails":["alice@example.test"]}`}, "bridge_contacts", "create"},
		{[]string{"notification", "send", `{"title":"Reminder","sound":false}`}, "bridge_notification", "send"},
		{[]string{"notification", "query", `{}`}, "", ""},
		{[]string{"notification", "send", `{"Action":"query"}`}, "", ""},
		{[]string{"calendar", "query", `null`}, "", ""},
	} {
		calls, err := Calls(append([]string{"app"}, tc.args...))
		if tc.tool == "" {
			if err == nil {
				t.Errorf("unexpectedly accepted %q: %+v", tc.args, calls)
			}
			continue
		}
		if err != nil || len(calls) != 1 || !calls[0].AppData || calls[0].Tool != tc.tool || calls[0].Input["action"] != tc.action {
			t.Errorf("unexpected mapping for %q: %+v, %v", tc.args, calls, err)
			continue
		}
		if tc.tool == "bridge_clipboard" && calls[0].Input["text"] != "" {
			t.Error("clipboard clear must contain an explicit empty string")
		}
	}
}

func TestParseWaitDuration(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ms   int
	}{
		{nil, 2000},
		{[]string{"0"}, 0},
		{[]string{"1200"}, 1200},
		{[]string{"30000"}, 30000},
		{[]string{"-1"}, -1},
		{[]string{"30001"}, -1},
		{[]string{"1s"}, -1},
		{[]string{"1", "2"}, -1},
	} {
		delay, err := ParseWaitDuration(tc.args)
		if tc.ms < 0 {
			if err == nil {
				t.Errorf("%q: wanted an error", tc.args)
			}
		} else if err != nil || delay.Milliseconds() != int64(tc.ms) {
			t.Errorf("%q: delay=%v, err=%v; want %d ms", tc.args, delay, err, tc.ms)
		}
	}
}
