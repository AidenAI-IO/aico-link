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
	if got := wakeupReadyLabel(agent.Config{}); got != "MCU ASR (socket unavailable)" {
		t.Fatalf("wakeupReadyLabel() = %q, want MCU ASR (socket unavailable)", got)
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
