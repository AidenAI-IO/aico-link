package main

import (
	"os"
	"testing"

	"aico-agent/internal/agent"
)

func TestLegacyGPIOVoiceWakeupAllowedBoardAndEnvGate(t *testing.T) {
	originalModel := readDeviceTreeModel
	t.Cleanup(func() { readDeviceTreeModel = originalModel })

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
			os.Unsetenv(envLegacyGPIOVoiceWakeup)
			os.Unsetenv(envLegacyGPIOVoiceWakeupAlt)
			t.Cleanup(func() {
				os.Unsetenv(envLegacyGPIOVoiceWakeup)
				os.Unsetenv(envLegacyGPIOVoiceWakeupAlt)
			})
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
	os.Unsetenv(envLegacyGPIOVoiceWakeup)
	os.Unsetenv(envLegacyGPIOVoiceWakeupAlt)
	t.Setenv("AICO_MCU_SERVICE_SOCKET", t.TempDir()+"/missing.sock")
	t.Cleanup(func() {
		os.Unsetenv(envLegacyGPIOVoiceWakeup)
		os.Unsetenv(envLegacyGPIOVoiceWakeupAlt)
		os.Unsetenv("AICO_MCU_SERVICE_SOCKET")
	})

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
