package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"aiden-agent/internal/agent"
)

func TestInputLifecycleGPIOFailureCanRetrySameConfig(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	cfg.Model.Provider, cfg.Model.Model = "fake", "fake"
	cfg.InputMode, cfg.TTS.Provider = "text", ""
	cfg.QuickCapture.Enabled = new(bool)
	r, err := agent.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := agent.NewServer(r, ":0")
	defer s.Close()
	c := newInputLifecycle(r, s)
	if err := c.Start(cfg); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r.SetConfigPreparer(c.Prepare)
	failed := true
	watcher := &fakeWakeupWatcher{}
	c.newWatcher = func(int, func()) (wakeupWatcher, error) {
		if failed {
			return nil, errors.New("GPIO unavailable")
		}
		return watcher, nil
	}
	next := cfg
	enabled := true
	next.QuickCapture.Enabled = &enabled
	next.QuickCapture.GPIOPin = 3
	if err := r.ApplyConfigSnapshot(next); err == nil {
		t.Fatal("failed GPIO marked applied")
	}
	if r.ConfigSnapshot().QuickCapture.EnabledOrDefault() {
		t.Fatal("failed config committed")
	}
	failed = false
	if err := r.ApplyConfigSnapshot(next); err != nil {
		t.Fatal(err)
	}
	if !watcher.started || c.quick != watcher || !r.ConfigSnapshot().QuickCapture.EnabledOrDefault() {
		t.Fatal("same config did not retry GPIO")
	}
}

func TestInputLifecycleReloadWaitsForSessionAndKeepsServer(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	cfg.Model.Provider = "fake"
	cfg.Model.Model = "fake"
	cfg.InputMode = "text"
	cfg.TTS.Provider = ""
	cfg.QuickCapture.Enabled = new(bool)
	runtime, err := agent.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server := agent.NewServer(runtime, ":0")
	defer server.Close()
	controller := newInputLifecycle(runtime, server)
	sessionDone := make(chan struct{})
	started := make(chan string, 4)
	controller.newDialog = func(agent.Config) (*agent.AudioDialog, error) { return nil, nil }
	controller.runVoice = func(cfg agent.Config, _ *agent.AudioDialog, shutdown chan os.Signal, stop <-chan struct{}) {
		started <- cfg.InputMode
		select {
		case <-stop:
			if cfg.InputMode == "text" {
				<-sessionDone
			}
		case <-shutdown:
		}
	}
	if err := controller.Start(cfg); err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	<-started
	next := cfg
	next.InputMode = "realtime"
	type result struct {
		finish func(bool)
		err    error
	}
	prepared := make(chan result, 1)
	go func() { finish, err := controller.Prepare(context.Background(), next); prepared <- result{finish, err} }()
	select {
	case <-prepared:
		t.Fatal("reload did not wait for session")
	case <-time.After(15 * time.Millisecond):
	}
	close(sessionDone)
	answer := <-prepared
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	answer.finish(true)
	select {
	case mode := <-started:
		if mode != "realtime" {
			t.Fatalf("mode=%s", mode)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement loop did not start")
	}
	if controller.server != server || controller.runtime != runtime {
		t.Fatal("replaced persistent server or runtime")
	}
}

func TestInputLifecycleFailedReplacementRestoresOldLoop(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	cfg.Model.Provider = "fake"
	cfg.Model.Model = "fake"
	cfg.InputMode = "text"
	cfg.TTS.Provider = ""
	cfg.QuickCapture.Enabled = new(bool)
	runtime, err := agent.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server := agent.NewServer(runtime, ":0")
	defer server.Close()
	controller := newInputLifecycle(runtime, server)
	started := make(chan string, 4)
	controller.newDialog = func(cfg agent.Config) (*agent.AudioDialog, error) {
		if cfg.InputMode == "stt" {
			return nil, errors.New("VAD unavailable")
		}
		return nil, nil
	}
	controller.runVoice = func(cfg agent.Config, _ *agent.AudioDialog, shutdown chan os.Signal, stop <-chan struct{}) {
		started <- cfg.InputMode
		select {
		case <-stop:
		case <-shutdown:
		}
	}
	if err := controller.Start(cfg); err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	<-started
	next := cfg
	next.InputMode = "stt"
	if _, err := controller.Prepare(context.Background(), next); err == nil {
		t.Fatal("expected failed VAD construction")
	}
	if mode := <-started; mode != "text" {
		t.Fatalf("restored %s, want text", mode)
	}
}

func TestInputLifecycleMissingVADHelperKeepsTextLoop(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	cfg.Model.Provider = "fake"
	cfg.InputMode = "text"
	cfg.TTS.Provider = ""
	cfg.QuickCapture.Enabled = new(bool)
	runtime, err := agent.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server := agent.NewServer(runtime, ":0")
	defer server.Close()
	controller := newInputLifecycle(runtime, server)
	if err := controller.Start(cfg); err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	next := cfg
	next.InputMode = "stt"
	next.VADHelperPath = "/missing-vad-helper"
	if _, err := controller.Prepare(context.Background(), next); err == nil {
		t.Fatal("missing helper accepted")
	}
	select {
	case <-controller.done:
		t.Fatal("old text loop was not restored")
	default:
	}
	if controller.cfg.InputMode != "text" {
		t.Fatal("failed preparation changed mode")
	}
}

func TestInputLifecycleTimeoutCancelRestartsVoiceLoop(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	cfg.Model.Provider = "fake"
	cfg.Model.Model = "fake"
	cfg.InputMode = "text"
	cfg.TTS.Provider = ""
	cfg.QuickCapture.Enabled = new(bool)
	runtime, err := agent.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server := agent.NewServer(runtime, ":0")
	defer server.Close()
	controller := newInputLifecycle(runtime, server)
	started := make(chan string, 4)
	stopped := make(chan struct{})
	sessionDone := make(chan struct{})
	var stopOnce sync.Once
	controller.newDialog = func(agent.Config) (*agent.AudioDialog, error) { return nil, nil }
	controller.runVoice = func(_ agent.Config, _ *agent.AudioDialog, shutdown chan os.Signal, stop <-chan struct{}) {
		started <- "text"
		select {
		case <-stop:
		case <-shutdown:
		}
		stopOnce.Do(func() { close(stopped) })
		<-sessionDone
	}
	if err := controller.Start(cfg); err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	<-started
	next := cfg
	next.InputMode = "realtime"
	ctx, cancel := context.WithCancel(context.Background())
	prepared := make(chan error, 1)
	go func() { _, err := controller.Prepare(ctx, next); prepared <- err }()
	<-stopped
	// A plain cancellation (for example the apply timeout) must roll back to
	// the old voice loop so the runtime keeps serving.
	cancel()
	close(sessionDone)
	select {
	case err := <-prepared:
		if err != context.Canceled {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not return")
	}
	select {
	case mode := <-started:
		if mode != "text" {
			t.Fatalf("restored %s", mode)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout rollback did not restart the old loop")
	}
}

func TestInputLifecycleShutdownCancelStopsVoiceWithoutRestart(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	cfg.Model.Provider = "fake"
	cfg.Model.Model = "fake"
	cfg.InputMode = "text"
	cfg.TTS.Provider = ""
	cfg.QuickCapture.Enabled = new(bool)
	runtime, err := agent.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server := agent.NewServer(runtime, ":0")
	defer server.Close()
	controller := newInputLifecycle(runtime, server)
	started := make(chan string, 4)
	stopped := make(chan struct{})
	sessionDone := make(chan struct{})
	controller.newDialog = func(agent.Config) (*agent.AudioDialog, error) { return nil, nil }
	controller.runVoice = func(_ agent.Config, _ *agent.AudioDialog, _ chan os.Signal, stop <-chan struct{}) {
		started <- "text"
		<-stop
		close(stopped)
		<-sessionDone
	}
	if err := controller.Start(cfg); err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	<-started
	next := cfg
	next.InputMode = "realtime"
	ctx, cancel := context.WithCancel(context.Background())
	prepared := make(chan error, 1)
	go func() { _, err := controller.Prepare(ctx, next); prepared <- err }()
	<-stopped
	// Mirror the daemon shutdown order: quiesce, cancel the reload worker,
	// then close the lifecycle.
	controller.StopForShutdown()
	cancel()
	close(sessionDone)
	select {
	case err := <-prepared:
		if err != context.Canceled {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown cancel did not return")
	}
	select {
	case mode := <-started:
		t.Fatalf("voice loop restarted during shutdown: %s", mode)
	case <-time.After(50 * time.Millisecond):
	}
	controller.Close()
	select {
	case <-controller.done:
	default:
		t.Fatal("voice loop was left running")
	}
}

// TestVoiceConfigCoversVoiceLoopFields keeps the voice-restart comparison in
// sync with agent.Config. A field added to Config must either be copied by
// voiceConfig (so the drained voice loop restarts with it) or be exempt with
// a reason, otherwise the running loop silently keeps the old value.
func TestVoiceConfigCoversVoiceLoopFields(t *testing.T) {
	exempt := map[string]string{
		"DeviceTypeOverride":         "CLI-only; already folded into Device before comparison",
		"Model":                      "shared model manager is replaced by Runtime at Agent task boundaries",
		"ModelProviders":             "resolved into Model at config load time",
		"TTSProviders":               "resolved into TTS / the shared TTS provider manager",
		"STTProviders":               "resolved into STT at config load time",
		"VoiceModelProviders":        "resolved into VoiceModel at config load time",
		"FrameService":               "frame service restart only",
		"Storage":                    "storage monitor and manager are reconfigured in place",
		"VoiceNotifications":         "notification manager is reconfigured in place",
		"QuickCapture":               "watcher replaced separately; TTL read at capture time",
		"Log":                        "process-wide logger; retention applied on save",
		"OTA":                        "not read by the voice loop",
		"EnvironmentBridge":          "process-only, set from CLI flags",
		"Benchmark":                  "process-only, set from CLI flags",
		"ForceSimpleLoop":            "process-only, set from CLI flags",
		"SkillMergeModel":            "process-only injected model (interface)",
		"LiveActivity":               "server-side manager is reconfigured in place",
		"MaxIterations":              "per-run value read from the runtime snapshot",
		"TerminationPolicy":          "per-run value read from the runtime snapshot",
		"ContextPruneThreshold":      "per-run value read from the runtime snapshot",
		"ContextCompactionThreshold": "per-run value read from the runtime snapshot",
		"ScreenshotKeepN":            "per-run value read from the runtime snapshot",
		"ScreenshotPruneInterval":    "per-run value read from the runtime snapshot",
		"SkillsDirs":                 "skills reload is flagged separately",
		"BundledSkillsDir":           "skills reload is flagged separately",
		"Telemetry":                  "per-run value read from the runtime snapshot",
		"ConfigDir":                  "fixed for the process; apply rejects changes",
		"MCPEnabled":                 "HTTP /mcp gated from runtime snapshot; not used by voice loop",
		"MCPShellEnabled":            "HTTP /mcp gated from runtime snapshot; not used by voice loop",
		"MCPConfigWebBaseURL":        "HTTP /mcp gated from runtime snapshot; not used by voice loop",
	}
	filledValue := fillNonZeroValue(t, reflect.TypeOf(agent.Config{}))
	filled := filledValue.Interface().(agent.Config)
	got := reflect.ValueOf(voiceConfig(filled))
	cfgType := reflect.TypeOf(agent.Config{})
	for i := 0; i < cfgType.NumField(); i++ {
		field := cfgType.Field(i)
		name := field.Name
		if field.Type.Kind() == reflect.Interface {
			if _, ok := exempt[name]; !ok {
				t.Errorf("interface field %s cannot be verified automatically; add it to the exempt list with a reason", name)
			}
			continue
		}
		if _, ok := exempt[name]; ok {
			if reflect.DeepEqual(got.Field(i).Interface(), filledValue.Field(i).Interface()) {
				t.Errorf("field %s is exempt but voiceConfig still copies it; remove the exemption", name)
			}
			continue
		}
		if !reflect.DeepEqual(got.Field(i).Interface(), filledValue.Field(i).Interface()) {
			t.Errorf("Config field %s is not copied by voiceConfig; the running voice loop keeps the old value. Copy it in voiceConfig or add it to the exempt list with a reason", name)
		}
	}
	for name := range exempt {
		if _, ok := cfgType.FieldByName(name); !ok {
			t.Errorf("exempt list references unknown Config field %s", name)
		}
	}
}

// fillNonZeroValue builds a value of typ with every settable field non-zero so
// field copies can be compared. Interface fields stay nil.
func fillNonZeroValue(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	switch typ.Kind() {
	case reflect.String:
		v := reflect.New(typ)
		v.Elem().SetString("filled")
		return v.Elem()
	case reflect.Bool:
		v := reflect.New(typ)
		v.Elem().SetBool(true)
		return v.Elem()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v := reflect.New(typ)
		v.Elem().SetInt(1)
		return v.Elem()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v := reflect.New(typ)
		v.Elem().SetUint(1)
		return v.Elem()
	case reflect.Float32, reflect.Float64:
		v := reflect.New(typ)
		v.Elem().SetFloat(1)
		return v.Elem()
	case reflect.Ptr:
		v := reflect.New(typ.Elem())
		v.Elem().Set(fillNonZeroValue(t, typ.Elem()))
		return v
	case reflect.Slice:
		s := reflect.MakeSlice(typ, 1, 1)
		s.Index(0).Set(fillNonZeroValue(t, typ.Elem()))
		return s
	case reflect.Map:
		m := reflect.MakeMap(typ)
		m.SetMapIndex(fillNonZeroValue(t, typ.Key()), fillNonZeroValue(t, typ.Elem()))
		return m
	case reflect.Struct:
		v := reflect.New(typ).Elem()
		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).PkgPath == "" {
				v.Field(i).Set(fillNonZeroValue(t, typ.Field(i).Type))
			}
		}
		return v
	case reflect.Interface:
		// No implementation can be fabricated; such fields stay nil and the
		// coverage test requires them to be exempt.
		return reflect.Zero(typ)
	default:
		t.Fatalf("cannot fill type %s", typ)
		return reflect.Zero(typ)
	}
}
