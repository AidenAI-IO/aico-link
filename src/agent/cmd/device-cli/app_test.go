package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestAppDataKeepsServerConnectionGate(t *testing.T) {
	args := []string{"app", "clipboard", "read"}
	for _, tc := range []struct{ name, path, token string }{
		{"standalone", "/api/device-cli/app", ""},
		{"agent shell", "/command", "test-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var received []string
				if r.Method != http.MethodPost || r.URL.Path != tc.path || json.NewDecoder(r.Body).Decode(&received) != nil || !reflect.DeepEqual(received, args) {
					t.Errorf("unexpected request: %s %s %+v", r.Method, r.URL.Path, received)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"is_error": true, "error": "App commands require at least one of app_connected, app_fgs_enabled, or app_pip_enabled to be true."})
			}))
			defer server.Close()
			t.Setenv("AICO_DEVICE_CLI_URL", server.URL)
			t.Setenv("AICO_DEVICE_CLI_TOKEN", tc.token)
			if err := run(server.URL, args); err == nil || err.Error() != "App commands require at least one of app_connected, app_fgs_enabled, or app_pip_enabled to be true." {
				t.Fatalf("expected the connection error, got %v", err)
			}
		})
	}
}
