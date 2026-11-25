package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestAppDataKeepsServerConnectionGate(t *testing.T) {
	args := []string{"app", "clipboard", "read"}
	for _, tc := range []struct{ name, path, token, envSuffix string }{
		{"standalone", "/api/device-cli/app", "", ""},
		{"agent shell", "/command", "test-token", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var received []string
				auth := ""
				if tc.token != "" {
					auth = "Bearer " + tc.token
				}
				if r.Method != http.MethodPost || r.URL.Path != tc.path || r.Header.Get("Authorization") != auth || json.NewDecoder(r.Body).Decode(&received) != nil || !reflect.DeepEqual(received, args) {
					t.Errorf("unexpected request: %s %s %+v", r.Method, r.URL.Path, received)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"is_error": true, "error": "No companion app route is available for this operation."})
			}))
			defer server.Close()
			t.Setenv("AICO_DEVICE_CLI_URL", server.URL+tc.envSuffix)
			t.Setenv("AICO_DEVICE_CLI_TOKEN", tc.token)
			var stdout strings.Builder
			if err := run(server.URL, args, &stdout); err == nil || err.Error() != "No companion app route is available for this operation." {
				t.Fatalf("expected the connection error, got %v", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("failure wrote to stdout: %q", stdout.String())
			}
		})
	}
}

func TestCommandStdoutContract(t *testing.T) {
	for _, tc := range []struct {
		name, payload, want, wantError string
		args                           []string
	}{
		{"plain acknowledgement", "ok", "", "", []string{"tap", "10", "20"}},
		{"status only", `{"ok":true,"restored_from_return_entry":true}`, "", "", []string{"app", "clipboard", "write", "hello"}},
		{"single string", `{"ok":true,"event_id":"e17","restored_from_return_entry":true}`, "e17", "", []string{"app", "calendar", "create", "--title", "Event", "--start-at", "2030-01-01T09:00:00Z"}},
		{"empty array", `{"ok":true,"contacts":[]}`, "[]\n", "", []string{"app", "contacts", "query"}},
		{"multiple fields", `{"ok":true,"action":"back","binding":"keyboard"}`, "{\"action\":\"back\",\"binding\":\"keyboard\"}\n", "", []string{"back"}},
		{"status failure", `{"ok":false,"error":"could not open app","target":"Clock"}`, "", "could not open app", []string{"open", "Clock"}},
		{"standalone screenshot wrapper", `{"data":"aW1hZ2U=","action_output":"{\"ok\":true}"}`, "", "", []string{"type", "hello"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"output": tc.payload})
			}))
			defer server.Close()
			t.Setenv("AICO_DEVICE_CLI_TOKEN", "")
			var stdout strings.Builder
			err := run(server.URL, tc.args, &stdout)
			gotError := ""
			if err != nil {
				gotError = err.Error()
			}
			if got := stdout.String(); got != tc.want || gotError != tc.wantError {
				t.Fatalf("stdout = %q, error = %q; want stdout = %q, error = %q", got, gotError, tc.want, tc.wantError)
			}
		})
	}
}

func TestClipboardReadWritesOnlyVerbatimText(t *testing.T) {
	for _, tc := range []struct {
		name, payload, want string
		scoped, wantError   bool
	}{
		{"empty standalone", `{"ok":true,"text":""}`, "", false, false},
		{"multiline scoped", `{"ok":true,"text":"第一行\nsecond\n","restored_from_return_entry":true}`, "第一行\nsecond\n", true, false},
		{"missing text", `{"ok":true}`, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, token := "/api/device-cli/app", ""
			if tc.scoped {
				path, token = "/command", "test-token"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path {
					t.Errorf("path = %q, want %q", r.URL.Path, path)
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"output": tc.payload})
			}))
			defer server.Close()
			t.Setenv("AICO_DEVICE_CLI_URL", server.URL)
			t.Setenv("AICO_DEVICE_CLI_TOKEN", token)
			var stdout strings.Builder
			err := run(server.URL, []string{"app", "clipboard", "read"}, &stdout)
			if stdout.String() != tc.want || (err != nil) != tc.wantError {
				t.Fatalf("stdout = %q, err = %v; want %q, error = %t", stdout.String(), err, tc.want, tc.wantError)
			}
		})
	}
}
