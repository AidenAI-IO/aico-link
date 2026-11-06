package main

import (
	"os"
	"testing"

	"aico-agent/internal/agent"
)

func TestLegacyGPIOVoiceWakeupAllowedBoardAndEnvGate(t *testing.T) {
	originalModel := readDeviceTreeModel
	t.Cleanup(func() { readDeviceTreeModel = originalModel })
	restoreLegacyGPIOEnv := stashEnv(t, envLegacyGPIOVoiceWakeup, envLegacyGPIOVoiceWakeupAlt)
	t.Cleanup(restoreLegacyGPIOEnv)

	cases := []struct {
		name  string
		model string
		env   *string
		want  bool
	}{
		{name: "pico zero allows", model: "Luckfox Pico Zero", want: true},
		{name: "sch v1 denies", model: "Aico Link SCH v1", want: false},
		{name: "sch v1 denies env opt-in", model: "Aico Link SCH v1", env: strPtr("1"), want: false},
		{name: "unknown denies by default", model: "Unknown Board", want: false},
		{name: "unknown allows env opt-in", model: "Unknown Board", env: strPtr("1"), want: true},
		{name: "pico zero honors explicit disable", model: "Luckfox Pico Zero", env: strPtr("0"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readDeviceTreeModel = func() string { return tc.model }
			clearEnv(t, envLegacyGPIOVoiceWakeup, envLegacyGPIOVoiceWakeupAlt)
			if tc.env != nil {
				t.Setenv(envLegacyGPIOVoiceWakeup, *tc.env)
			}
			if got := legacyGPIOVoiceWakeupAllowed(); got != tc.want {
				t.Fatalf("legacyGPIOVoiceWakeupAllowed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWakeupReadyLabelUsesGatedGPIOFallback(t *testing.T) {
	originalModel := readDeviceTreeModel
	t.Cleanup(func() { readDeviceTreeModel = originalModel })
	restore := stashEnv(t, envLegacyGPIOVoiceWakeup, envLegacyGPIOVoiceWakeupAlt, "AICO_MCU_SERVICE_SOCKET")
	t.Cleanup(restore)

	clearEnv(t, envLegacyGPIOVoiceWakeup, envLegacyGPIOVoiceWakeupAlt)
	t.Setenv("AICO_MCU_SERVICE_SOCKET", t.TempDir()+"/missing.sock")

	readDeviceTreeModel = func() string { return "Luckfox Pico Zero" }
	if got := wakeupReadyLabel(agent.Config{}); got != "GPIO 33/32" {
		t.Fatalf("wakeupReadyLabel() = %q, want GPIO 33/32", got)
	}

	readDeviceTreeModel = func() string { return "Aico Link SCH v1" }
	if got := wakeupReadyLabel(agent.Config{}); got != "GX8002 GPIO0 (J17-10)" {
		t.Fatalf("wakeupReadyLabel() = %q, want GX8002 GPIO0 (J17-10)", got)
	}
}

func TestAddGX8002GPIOWakeupUsesJ17InputOnlyOnAicoBoard(t *testing.T) {
	primary := &fakeWakeupWatcher{}
	gpio := &fakeWakeupWatcher{}
	var gpioCallback func()
	callbackCount := 0
	base := func(callback func()) (wakeupWatcher, error) {
		primary.callback = callback
		return primary, nil
	}
	newGPIO := func(pin int, edge agent.GPIOEdge, callback func()) (wakeupWatcher, error) {
		if pin != gx8002WakeupLinuxGPIO || edge != agent.GPIOEdgeRising {
			t.Fatalf("GPIO watcher configured for pin=%d edge=%q", pin, edge)
		}
		gpioCallback = callback
		return gpio, nil
	}

	watcher, err := addGX8002GPIOWakeup("Aico Link SCH v1", base, newGPIO)(func() { callbackCount++ })
	if err != nil {
		t.Fatal(err)
	}
	combined, ok := watcher.(*multiWakeupWatcher)
	if !ok || len(combined.watchers) != 2 {
		t.Fatalf("combined watcher = %#v", watcher)
	}
	if err := watcher.Start(); err != nil {
		t.Fatal(err)
	}
	gpioCallback()
	if !primary.started || !gpio.started || callbackCount != 1 {
		t.Fatalf("primary started=%v GPIO started=%v callback count=%d", primary.started, gpio.started, callbackCount)
	}
	watcher.Stop()

	called := false
	watcher, err = addGX8002GPIOWakeup("Luckfox Pico Zero", base, func(int, agent.GPIOEdge, func()) (wakeupWatcher, error) {
		called = true
		return gpio, nil
	})(func() {})
	if err != nil {
		t.Fatal(err)
	}
	if watcher != primary || called {
		t.Fatalf("non-Aico watcher = %#v, GPIO factory called=%v", watcher, called)
	}

	watcher, err = addGX8002GPIOWakeup("Aico Link SCH v1", base, func(int, agent.GPIOEdge, func()) (wakeupWatcher, error) {
		return nil, os.ErrPermission
	})(func() {})
	if err != nil {
		t.Fatalf("optional GPIO setup error = %v", err)
	}
	if watcher != primary {
		t.Fatalf("watcher after optional GPIO setup failure = %#v, want primary watcher", watcher)
	}
}

func strPtr(v string) *string { return &v }

type envSnapshot struct {
	key   string
	value string
	set   bool
}

func stashEnv(t *testing.T, keys ...string) func() {
	t.Helper()
	snaps := make([]envSnapshot, 0, len(keys))
	for _, key := range keys {
		value, set := os.LookupEnv(key)
		snaps = append(snaps, envSnapshot{key: key, value: value, set: set})
	}
	return func() {
		for _, snap := range snaps {
			if snap.set {
				os.Setenv(snap.key, snap.value)
				continue
			}
			os.Unsetenv(snap.key)
		}
	}
}

func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		os.Unsetenv(key)
	}
}
