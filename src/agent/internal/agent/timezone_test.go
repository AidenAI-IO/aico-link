package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimezoneDefaultsToUTC(t *testing.T) {
	if got := (Config{}).TimezoneOrDefault(); got != "UTC" {
		t.Fatalf("TimezoneOrDefault() = %q, want UTC", got)
	}
	if got := DefaultConfig().Timezone; got != "UTC" {
		t.Fatalf("DefaultConfig().Timezone = %q, want UTC", got)
	}
}

func TestLoadConfigReadsGroupedTimezone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	source := "[basic_settings.language_timezone]\nlocale = \"en-US\"\ntimezone = \"Asia/Shanghai\"\n\n[model_settings.model]\nprovider = \"fake\"\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Timezone != "Asia/Shanghai" {
		t.Fatalf("Timezone = %q, want Asia/Shanghai", cfg.Timezone)
	}
}

func TestConfigRejectsUnsupportedTimezone(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Timezone = "Mars/Olympus"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported timezone") {
		t.Fatalf("Validate() error = %v, want unsupported timezone", err)
	}
}

func TestConfiguredTimeUsesSelectedTimezone(t *testing.T) {
	utc := time.Date(2026, time.September, 14, 18, 30, 0, 0, time.UTC)
	shanghai := configuredTime(utc, "Asia/Shanghai")
	if got := shanghai.Format("2006-01-02 15:04 -0700"); got != "2026-09-15 02:30 +0800" {
		t.Fatalf("configuredTime() = %q", got)
	}
}

func TestApplyTimezoneSetsPortableChildProcessTimezone(t *testing.T) {
	previous, hadPrevious := os.LookupEnv("TZ")
	t.Cleanup(func() {
		if hadPrevious {
			_ = os.Setenv("TZ", previous)
		} else {
			_ = os.Unsetenv("TZ")
		}
	})
	if err := ApplyTimezone(Config{Timezone: "Asia/Shanghai"}); err != nil {
		t.Fatalf("ApplyTimezone() error = %v", err)
	}
	if got := os.Getenv("TZ"); got != "CST-8" {
		t.Fatalf("TZ = %q, want CST-8", got)
	}
}

func TestDeviceStateIncludesConfiguredTimezoneAndTime(t *testing.T) {
	originalNow := promptNow
	promptNow = func() time.Time { return time.Date(2026, time.September, 14, 18, 30, 0, 0, time.UTC) }
	t.Cleanup(func() { promptNow = originalNow })
	state := newDeviceStateUpdater(Config{Timezone: "Asia/Tokyo"}).UpdateState()
	if got := state["controller_timezone"]; got != "Asia/Tokyo" {
		t.Fatalf("controller_timezone = %q, want Asia/Tokyo", got)
	}
	if got, want := state["controller_time"], "2026-09-15 03:30:00 +09:00 (Tuesday)"; got != want {
		t.Fatalf("controller_time = %q, want %q", got, want)
	}
}
