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
		{[]string{"calendar", "delete", "--event-id", "e1"}, "bridge_calendar", "delete"},
		{[]string{"contacts", "create", "--name", "Alice", "--email", "alice@example.test"}, "bridge_contacts", "create"},
		{[]string{"notification", "send", "--title", "Reminder", "--sound=false"}, "bridge_notification", "send"},
		{[]string{"notification", "query", `{}`}, "", ""},
		{[]string{"notification", "send", `{"Action":"query"}`}, "", ""},
		{[]string{"calendar", "query", `null`}, "", ""},
		{[]string{"contacts", "create", `{"name":"Alice","phone":"123"}`}, "", ""},
		{[]string{"notification", "send", `{"title":"Reminder","time":"2030-01-01T09:00:00+08:00"}`}, "", ""},
		{[]string{"calendar", "query", `{"start_at":"2030-01-01T09:00:00+08:00"}`}, "", ""},
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

func TestCalendarCreateFlags(t *testing.T) {
	calls, err := Calls([]string{"app", "calendar", "create", "--title", "Team sync", "--start-at=2026-11-23T09:00:00+08:00", "--alarm-minutes-before", "10", "--all-day"})
	if err != nil || len(calls) != 1 {
		t.Fatalf("create flags = %+v, %v", calls, err)
	}
	in := calls[0].Input
	if in["action"] != "create" || in["title"] != "Team sync" || in["start_at"] != "2026-11-23T09:00:00+08:00" || in["alarm_minutes_before"] != 10 || in["all_day"] != true {
		t.Fatalf("unexpected bridge input: %+v", in)
	}
}

func TestAppFlagsPreserveBridgeTypes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"contacts", "query", "--query", "Alice", "--limit=5"}, map[string]any{"action": "query", "query": "Alice", "limit": 5}},
		{[]string{"contacts", "update", "--contact-id", "c1", "--phone-number", "+15550100", "--phone-number=+15550101", "--email", "a@example.test", "--email=b@example.test"}, map[string]any{"action": "update", "contact_id": "c1", "phone_numbers": []string{"+15550100", "+15550101"}, "emails": []string{"a@example.test", "b@example.test"}}},
		{[]string{"notification", "send", "--title", "Reminder", "--body", "Check calendar", "--schedule-at", "2030-01-01T09:00:00+08:00", "--sound=false", "--badge", "0"}, map[string]any{"action": "send", "title": "Reminder", "body": "Check calendar", "schedule_at": "2030-01-01T09:00:00+08:00", "sound": false, "badge": 0}},
		{[]string{"contacts", "query", "--limit", "0"}, nil},
		{[]string{"contacts", "update", "--name", "Alice"}, nil},
		{[]string{"notification", "send", "--title", "Reminder", "--sound=maybe"}, nil},
		{[]string{"notification", "send", "--title", "Reminder", "--schedule-at", "tomorrow"}, nil},
	} {
		calls, err := Calls(append([]string{"app"}, tc.args...))
		if tc.want == nil {
			if err == nil {
				t.Errorf("accepted invalid args %q", tc.args)
			}
			continue
		}
		if err != nil || len(calls) != 1 || !reflect.DeepEqual(calls[0].Input, tc.want) {
			t.Errorf("args %q: got %+v, %v; want %v", tc.args, calls, err, tc.want)
		}
	}
}
