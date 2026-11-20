package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyServiceSeverity(t *testing.T) {
	cases := []struct {
		active, result string
		restarts       int
		want           string
	}{
		{"active", "success", 0, "ok"},
		{"inactive", "success", 0, "ok"},
		{"active", "success", 3, "degraded"},
		{"activating", "success", 0, "degraded"},
		{"failed", "exit-code", 1, "failed"},
		{"inactive", "start-limit-hit", 5, "failed"},
		{"active", "signal", 0, "failed"},
	}
	for _, tc := range cases {
		if got := classifyServiceSeverity(tc.active, tc.result, tc.restarts); got != tc.want {
			t.Fatalf("classify(%q,%q,%d)=%q want %q", tc.active, tc.result, tc.restarts, got, tc.want)
		}
	}
}

func TestParseSystemctlShowBlocks(t *testing.T) {
	raw := "" +
		"Id=aico-agent.service\n" +
		"ActiveState=active\n" +
		"SubState=running\n" +
		"Result=success\n" +
		"NRestarts=1\n" +
		"MainPID=42\n" +
		"\n" +
		"Id=aico-frame.service\n" +
		"ActiveState=failed\n" +
		"SubState=dead\n" +
		"Result=exit-code\n" +
		"NRestarts=4\n" +
		"MainPID=0\n"
	blocks := parseSystemctlShowBlocks(raw)
	if blocks["aico-agent.service"]["MainPID"] != "42" {
		t.Fatalf("agent block: %#v", blocks["aico-agent.service"])
	}
	if blocks["aico-frame.service"]["ActiveState"] != "failed" {
		t.Fatalf("frame block: %#v", blocks["aico-frame.service"])
	}
}

func TestCollectCoreServiceStatusesUsesSystemctl(t *testing.T) {
	original := systemctlShow
	t.Cleanup(func() { systemctlShow = original })
	systemctlShow = func(units ...string) (string, error) {
		if len(units) != len(coreServiceSpecs) {
			t.Fatalf("units=%v", units)
		}
		var b strings.Builder
		for i, unit := range units {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("Id=" + unit + "\nLoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nNRestarts=0\nMainPID=0\n")
		}
		return b.String(), nil
	}
	snapshot := collectCoreServiceStatuses()
	if len(snapshot.Services) != len(coreServiceSpecs) {
		t.Fatalf("services=%d", len(snapshot.Services))
	}
	if snapshot.Services[0].Severity != "ok" || snapshot.Services[0].ActiveState != "active" {
		t.Fatalf("first service: %#v", snapshot.Services[0])
	}
}

func TestCollectCoreServiceStatusesNotFoundUnit(t *testing.T) {
	original := systemctlShow
	t.Cleanup(func() { systemctlShow = original })
	systemctlShow = func(units ...string) (string, error) {
		var b strings.Builder
		for i, unit := range units {
			if i > 0 {
				b.WriteByte('\n')
			}
			if unit == "aico-frame.service" {
				b.WriteString("Id=" + unit + "\nLoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\nNRestarts=0\nMainPID=0\n")
				continue
			}
			b.WriteString("Id=" + unit + "\nLoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nNRestarts=0\nMainPID=0\n")
		}
		return b.String(), nil
	}
	snapshot := collectCoreServiceStatuses()
	var frame *coreServiceStatus
	for i := range snapshot.Services {
		if snapshot.Services[i].Unit == "aico-frame.service" {
			frame = &snapshot.Services[i]
			break
		}
	}
	if frame == nil {
		t.Fatal("missing frame service")
	}
	if frame.Severity != "degraded" || frame.LoadError != "unit not found" {
		t.Fatalf("not-found frame: %#v", frame)
	}
}

func TestCollectCoreServiceStatusesSystemctlError(t *testing.T) {
	original := systemctlShow
	t.Cleanup(func() { systemctlShow = original })
	systemctlShow = func(units ...string) (string, error) {
		return "", os.ErrPermission
	}
	snapshot := collectCoreServiceStatuses()
	if len(snapshot.Services) != len(coreServiceSpecs) {
		t.Fatalf("services=%d", len(snapshot.Services))
	}
	for _, service := range snapshot.Services {
		if service.Severity != "degraded" {
			t.Fatalf("severity=%q want degraded for %#v", service.Severity, service)
		}
		if service.LoadError == "" {
			t.Fatalf("expected load_error for %#v", service)
		}
	}
}

func TestReadServiceIncidentEventsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AICO_DIAGNOSTICS_DIR", dir)
	path := filepath.Join(dir, "service-events.jsonl")
	content := "" +
		`{"ts":"2026-01-01T00:00:01Z","unit":"aico-frame.service","severity":"failed"}` + "\n" +
		`{"ts":"2026-01-01T00:00:02Z","unit":"aico-audio.service","severity":"failed"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := readServiceIncidentEvents(10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%d", len(events))
	}
	if events[0]["unit"] != "aico-audio.service" {
		t.Fatalf("newest first: %#v", events[0])
	}
	filtered, err := readServiceIncidentEvents(10, "aico-frame")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0]["unit"] != "aico-frame.service" {
		t.Fatalf("unit filter: %#v", filtered)
	}
}

func TestReadServiceIncidentEventsTailWindow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AICO_DIAGNOSTICS_DIR", dir)
	path := filepath.Join(dir, "service-events.jsonl")
	var b strings.Builder
	// Prefix padding larger than the default read window forces a seek + partial first line drop.
	b.WriteString(strings.Repeat("x", diagnosticsEventsReadWindow+64))
	b.WriteString("partial-garbage\n")
	b.WriteString(`{"ts":"2026-01-01T00:00:01Z","unit":"aico-frame.service","severity":"failed"}` + "\n")
	b.WriteString(`{"ts":"2026-01-01T00:00:02Z","unit":"aico-audio.service","severity":"failed"}` + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := readServiceIncidentEvents(10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%d want 2", len(events))
	}
	if events[0]["unit"] != "aico-audio.service" {
		t.Fatalf("newest first: %#v", events[0])
	}
}

func TestServiceDiagnosticsHTTP(t *testing.T) {
	original := systemctlShow
	t.Cleanup(func() { systemctlShow = original })
	systemctlShow = func(units ...string) (string, error) {
		return "Id=aico-agent.service\nLoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nNRestarts=0\nMainPID=0\n", nil
	}
	server := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/diagnostics/services", server.handleServiceDiagnostics)
	mux.HandleFunc("/api/diagnostics/incidents", server.handleServiceIncidents)
	mux.HandleFunc("/api/diagnostics/incident-log", server.handleServiceIncidentLog)

	dir := t.TempDir()
	t.Setenv("AICO_DIAGNOSTICS_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "last"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "service-events.jsonl"), []byte(
		`{"ts":"2026-01-01T00:00:01Z","unit":"aico-frame.service","severity":"failed"}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "last", "aico-frame.service"), []byte("boom\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/diagnostics/services", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("services status=%d body=%s", w.Code, w.Body.String())
	}
	var services serviceDiagnosticsSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &services); err != nil {
		t.Fatal(err)
	}
	if len(services.Services) == 0 {
		t.Fatal("expected services")
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/diagnostics/incidents?limit=5", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("incidents status=%d", w.Code)
	}
	var incidents map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &incidents); err != nil {
		t.Fatal(err)
	}
	events, _ := incidents["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events=%v", incidents["events"])
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/diagnostics/incident-log?unit=aico-frame.service", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("incident-log status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("body=%s", w.Body.String())
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/diagnostics/incident-log?unit=../etc/passwd", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("path escape status=%d", w.Code)
	}
}
