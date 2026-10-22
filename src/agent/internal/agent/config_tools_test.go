package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeConfigEditor struct {
	snapshot json.RawMessage
	edit     ConfigEdit
	editErr  error
	patches  []string
	commits  []bool
}

func (f *fakeConfigEditor) Snapshot(string) (json.RawMessage, error) { return f.snapshot, nil }

func (f *fakeConfigEditor) Edit(_ string, patch json.RawMessage, commit bool) (ConfigEdit, error) {
	f.patches = append(f.patches, string(patch))
	f.commits = append(f.commits, commit)
	return f.edit, f.editErr
}

type configToolHarness struct {
	editor   *fakeConfigEditor
	reloads  []uint64
	reload   func(uint64) (ConfigApplyStatus, error)
	logged   int
	get      *ConfigGetTool
	update   *ConfigUpdateTool
	statusFn func() ConfigApplyStatus
}

func newConfigToolHarness(editor *fakeConfigEditor) *configToolHarness {
	h := &configToolHarness{editor: editor}
	h.reload = func(uint64) (ConfigApplyStatus, error) {
		return ConfigApplyStatus{State: "pending", Pending: true}, nil
	}
	h.statusFn = func() ConfigApplyStatus { return ConfigApplyStatus{State: "applied", Applied: true} }
	backend := &configToolBackend{
		editor: editor,
		path:   func() string { return "/userdata/agent/agent.toml" },
		status: func() ConfigApplyStatus { return h.statusFn() },
		reload: func(revision uint64) (ConfigApplyStatus, error) {
			h.reloads = append(h.reloads, revision)
			return h.reload(revision)
		},
		log: func([]selfConfigChange, uint64, string) { h.logged++ },
	}
	h.get = &ConfigGetTool{backend: backend}
	h.update = &ConfigUpdateTool{backend: backend}
	return h
}

func decodeToolResult(t *testing.T, output string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	return result
}

func TestConfigUpdateRejectsSettingsOutsideAllowlist(t *testing.T) {
	for _, input := range []string{
		`{"settings":{"model_providers.openai.base_url":"https://evil.example"},"dry_run":false}`,
		`{"settings":{"hid.pointer_mode":"relative"},"dry_run":false}`,
		`{"settings":{"model.provider":"other"},"dry_run":false}`,
		`{"settings":{"tts":{"speed":0.8}},"dry_run":false}`,
	} {
		editor := &fakeConfigEditor{snapshot: json.RawMessage(`{}`)}
		h := newConfigToolHarness(editor)
		if _, err := h.update.Call(context.Background(), input); err == nil {
			t.Fatalf("Call(%s) error = nil, want rejection", input)
		}
		if len(editor.patches) != 0 {
			t.Fatalf("Call(%s) reached the editor with %v", input, editor.patches)
		}
	}
}

func TestConfigUpdateRejectsWrongValueTypes(t *testing.T) {
	for _, input := range []string{
		`{"settings":{"tts.speed":"fast"}}`,
		`{"settings":{"agent.silence_ms":1200.5}}`,
		`{"settings":{"agent.voice_followup_enabled":"yes"}}`,
		`{"settings":{"agent.timezone":"  "}}`,
		`{"settings":{"agent.timezone":null}}`,
		`{"settings":{}}`,
	} {
		editor := &fakeConfigEditor{snapshot: json.RawMessage(`{}`)}
		if _, err := newConfigToolHarness(editor).update.Call(context.Background(), input); err == nil {
			t.Fatalf("Call(%s) error = nil, want rejection", input)
		}
	}
}

func TestConfigUpdateSavesWithoutDryRun(t *testing.T) {
	editor := &fakeConfigEditor{
		snapshot: json.RawMessage(`{"tts":{"speed":1}}`),
		edit:     ConfigEdit{Config: json.RawMessage(`{"tts":{"speed":0.8}}`), ChangedPaths: []string{"voice_settings.classic.tts.speed"}, Apply: ApplyLive, Revision: 3},
	}
	h := newConfigToolHarness(editor)
	output, err := h.update.Call(context.Background(), `{"settings":{"tts.speed":0.8}}`)
	if err != nil {
		t.Fatal(err)
	}
	result := decodeToolResult(t, output)
	if result["state"] != "pending" || result["dry_run"] != false {
		t.Fatalf("result = %v, want a saved change pending application", result)
	}
	if !strings.Contains(result["note"].(string), "change it back") {
		t.Fatalf("note = %v, want it to tell the user how to undo", result["note"])
	}
	if len(editor.commits) != 1 || !editor.commits[0] || len(h.reloads) != 1 {
		t.Fatalf("commits = %v reloads = %v, want one persisted edit and one reload", editor.commits, h.reloads)
	}
}

func TestConfigUpdateDryRunPreviewsWithoutSaving(t *testing.T) {
	editor := &fakeConfigEditor{
		snapshot: json.RawMessage(`{"tts":{"speed":1}}`),
		edit:     ConfigEdit{Config: json.RawMessage(`{"tts":{"speed":0.8}}`), ChangedPaths: []string{"voice_settings.classic.tts.speed"}, Apply: ApplyLive},
	}
	h := newConfigToolHarness(editor)
	output, err := h.update.Call(context.Background(), `{"settings":{"tts.speed":0.8},"dry_run":true}`)
	if err != nil {
		t.Fatal(err)
	}
	result := decodeToolResult(t, output)
	if result["state"] != "preview" || result["dry_run"] != true {
		t.Fatalf("result = %v, want an unsaved preview", result)
	}
	if len(editor.commits) != 1 || editor.commits[0] {
		t.Fatalf("commits = %v, want one validate-only edit", editor.commits)
	}
	if editor.patches[0] != `{"config":{"tts":{"speed":0.8}}}` {
		t.Fatalf("patch = %s", editor.patches[0])
	}
	if len(h.reloads) != 0 {
		t.Fatalf("dry run reloaded %v", h.reloads)
	}
	changes := result["changes"].([]any)
	change := changes[0].(map[string]any)
	if change["setting"] != "tts.speed" || change["from"] != 1.0 || change["to"] != 0.8 {
		t.Fatalf("change = %v", change)
	}
}

func TestConfigUpdateCommitQueuesReload(t *testing.T) {
	editor := &fakeConfigEditor{
		snapshot: json.RawMessage(`{"agent":{"timezone":"Asia/Shanghai","silence_ms":800}}`),
		edit: ConfigEdit{
			Config:       json.RawMessage(`{"agent":{"timezone":"Asia/Tokyo","silence_ms":1500}}`),
			ChangedPaths: []string{"basic_settings.language_timezone.timezone", "voice_settings.vad.silence_ms"},
			Apply:        ApplyLive,
			Revision:     42,
		},
	}
	h := newConfigToolHarness(editor)
	output, err := h.update.Call(context.Background(), `{"settings":{"agent.timezone":"Asia/Tokyo","agent.silence_ms":1500},"dry_run":false}`)
	if err != nil {
		t.Fatal(err)
	}
	result := decodeToolResult(t, output)
	if result["state"] != "pending" {
		t.Fatalf("state = %v, want pending", result["state"])
	}
	if !strings.Contains(result["note"].(string), "current task") {
		t.Fatalf("note = %v, want it to say the change applies after this task", result["note"])
	}
	if len(editor.commits) != 1 || !editor.commits[0] {
		t.Fatalf("commits = %v, want one persisted edit", editor.commits)
	}
	if editor.patches[0] != `{"config":{"agent":{"silence_ms":1500,"timezone":"Asia/Tokyo"}}}` {
		t.Fatalf("patch = %s", editor.patches[0])
	}
	if len(h.reloads) != 1 || h.reloads[0] != 42 || h.logged != 1 {
		t.Fatalf("reloads = %v logged = %d, want revision 42 reloaded and logged once", h.reloads, h.logged)
	}
}

func TestConfigUpdateTreatsSupersededRevisionAsPending(t *testing.T) {
	editor := &fakeConfigEditor{
		snapshot: json.RawMessage(`{"tts":{"speed":1}}`),
		edit:     ConfigEdit{Config: json.RawMessage(`{"tts":{"speed":0.8}}`), ChangedPaths: []string{"voice_settings.classic.tts.speed"}, Apply: ApplyLive, Revision: 7},
	}
	h := newConfigToolHarness(editor)
	h.reload = func(uint64) (ConfigApplyStatus, error) {
		return ConfigApplyStatus{}, ErrConfigRevisionStale
	}
	output, err := h.update.Call(context.Background(), `{"settings":{"tts.speed":0.8},"dry_run":false}`)
	if err != nil {
		t.Fatal(err)
	}
	if state := decodeToolResult(t, output)["state"]; state != "pending" {
		t.Fatalf("state = %v, want pending", state)
	}
}

func TestConfigUpdateReportsReloadFailure(t *testing.T) {
	editor := &fakeConfigEditor{
		snapshot: json.RawMessage(`{"tts":{"speed":1}}`),
		edit:     ConfigEdit{Config: json.RawMessage(`{"tts":{"speed":0.8}}`), ChangedPaths: []string{"voice_settings.classic.tts.speed"}, Apply: ApplyLive, Revision: 7},
	}
	h := newConfigToolHarness(editor)
	h.reload = func(uint64) (ConfigApplyStatus, error) {
		return ConfigApplyStatus{}, errors.New("config file unavailable")
	}
	output, err := h.update.Call(context.Background(), `{"settings":{"tts.speed":0.8},"dry_run":false}`)
	if err != nil {
		t.Fatal(err)
	}
	if state := decodeToolResult(t, output)["state"]; state != "saved_not_applied" {
		t.Fatalf("state = %v, want saved_not_applied", state)
	}
}

func TestConfigUpdateReportsIgnoredSettingButStillReloads(t *testing.T) {
	// No TTS provider: the speed is written yet resolves to zero.
	editor := &fakeConfigEditor{
		snapshot: json.RawMessage(`{"tts":{"speed":0}}`),
		edit:     ConfigEdit{Config: json.RawMessage(`{"tts":{"speed":0}}`), ChangedPaths: []string{"voice_settings.classic.tts.speed"}, Apply: ApplyLive, Revision: 9},
	}
	h := newConfigToolHarness(editor)
	output, err := h.update.Call(context.Background(), `{"settings":{"tts.speed":0.8},"dry_run":false}`)
	if err != nil {
		t.Fatal(err)
	}
	result := decodeToolResult(t, output)
	ignored, _ := result["ignored"].([]any)
	if len(ignored) != 1 || ignored[0] != "tts.speed" {
		t.Fatalf("ignored = %v, want [tts.speed]", result["ignored"])
	}
	if len(h.reloads) != 1 {
		t.Fatalf("reloads = %v, want the written file reloaded", h.reloads)
	}
}

func TestConfigUpdateUnchangedSkipsReload(t *testing.T) {
	editor := &fakeConfigEditor{
		snapshot: json.RawMessage(`{"tts":{"speed":0.8}}`),
		edit:     ConfigEdit{Config: json.RawMessage(`{"tts":{"speed":0.8}}`), Apply: ApplyLive},
	}
	h := newConfigToolHarness(editor)
	output, err := h.update.Call(context.Background(), `{"settings":{"tts.speed":0.8},"dry_run":false}`)
	if err != nil {
		t.Fatal(err)
	}
	if state := decodeToolResult(t, output)["state"]; state != "unchanged" {
		t.Fatalf("state = %v, want unchanged", state)
	}
	if len(h.reloads) != 0 || h.logged != 0 {
		t.Fatalf("reloads = %v logged = %d, want none", h.reloads, h.logged)
	}
}

func TestConfigGetReturnsOnlyAllowlistedSettingsAndApplyStatus(t *testing.T) {
	editor := &fakeConfigEditor{snapshot: json.RawMessage(`{
		"tts":{"speed":1.1,"provider":"voice"},
		"agent":{"timezone":"Asia/Shanghai","prompt":"secret instructions"},
		"model_providers":{"openai":{"type":"openai","has_api_key":true,"base_url":"https://api.example"}},
		"telemetry":{"secret_key":"sk-telemetry"}
	}`)}
	h := newConfigToolHarness(editor)
	h.statusFn = func() ConfigApplyStatus { return ConfigApplyStatus{State: "pending", Pending: true} }
	output, err := h.get.Call(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"sk-telemetry", "api.example", "secret instructions", "provider"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("config_get output leaks %q: %s", leaked, output)
		}
	}
	result := decodeToolResult(t, output)
	settings := result["settings"].(map[string]any)
	if len(settings) != len(selfConfigSettings) || settings["tts.speed"] != 1.1 || settings["agent.timezone"] != "Asia/Shanghai" {
		t.Fatalf("settings = %v", settings)
	}
	if settings["live_activity.enabled"] != nil {
		t.Fatalf("absent setting = %v, want null", settings["live_activity.enabled"])
	}
	status := result["apply_status"].(map[string]any)
	if status["state"] != "pending" || status["pending"] != true {
		t.Fatalf("apply_status = %v", status)
	}
}

func TestConfigToolsStayOffHTTP(t *testing.T) {
	editor := &fakeConfigEditor{snapshot: json.RawMessage(`{}`)}
	h := newConfigToolHarness(editor)
	for _, spec := range []ToolSpec{NewToolSpec(h.get), NewToolSpec(h.update)} {
		if spec.HTTPExposed || !spec.AgentExposed {
			t.Fatalf("%s: http=%v agent=%v, want agent-only", spec.Name, spec.HTTPExposed, spec.AgentExposed)
		}
	}
}
