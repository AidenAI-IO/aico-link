package wifiregion

import (
	"path/filepath"
	"testing"

	"aico-agent/internal/agent"
)

func TestNormalizeRejectsUnusableCodes(t *testing.T) {
	for input, want := range map[string]string{
		"us":            "US",
		" CN ":          "CN",
		"De":            "DE",
		"00":            "00",
		"USA":           "",
		"U1":            "",
		"":              "",
		"ZZ":            "",
		"UK":            "",
		"US\nnetwork={": "",
	} {
		got, ok := Normalize(input)
		if got != want || ok != (want != "") {
			t.Errorf("Normalize(%q)=(%q,%v), want (%q,%v)", input, got, ok, want, want != "")
		}
	}
}

func TestOptionsExcludeWorldDomain(t *testing.T) {
	options := Options()
	if len(options) < 100 {
		t.Fatalf("expected the driver's country table, got %d entries", len(options))
	}
	for _, code := range options {
		if code == WorldCountry {
			t.Fatal("the permissive world domain must not be offered as a choice")
		}
	}
	if !IsSupported(WorldCountry) {
		t.Fatal("an existing configuration carrying the world domain must stay loadable")
	}
}

func TestVoteBeaconRequiresAClearMajority(t *testing.T) {
	cases := []struct {
		name  string
		votes map[string]int
		want  string
	}{
		{"single access point proves nothing", map[string]int{"DE": 1}, ""},
		{"two is still below the threshold", map[string]int{"DE": 2}, ""},
		{"three distinct radios agree", map[string]int{"DE": 3}, "DE"},
		{"clear winner over a minority", map[string]int{"DE": 7, "AT": 2}, "DE"},
		{"a tie proves less than nothing", map[string]int{"DE": 4, "AT": 4}, ""},
		{"unknown codes are discarded", map[string]int{"ZZ": 9, "DE": 3}, "DE"},
		{"the world domain never wins", map[string]int{"00": 9}, ""},
		{"no votes at all", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := VoteBeacon(tc.votes)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("VoteBeacon(%v)=(%q,%v), want %q", tc.votes, got, ok, tc.want)
			}
		})
	}
}

func TestResolveHonoursProvenancePrecedence(t *testing.T) {
	strongBeacon := map[string]int{"DE": 5}

	t.Run("a user choice is never overridden", func(t *testing.T) {
		got := Resolve(Inputs{
			Saved:           State{Country: "JP", Source: SourceUser},
			CurrentCountry:  "JP",
			ScanVotes:       strongBeacon,
			BrowserTimezone: "Europe/Berlin",
			AgentTimezone:   "Europe/Berlin",
		})
		if got.Country != "JP" || got.Source != SourceUser {
			t.Fatalf("Resolve()=%+v, want JP/user", got)
		}
	})

	t.Run("an existing deployment is never re-decided", func(t *testing.T) {
		got := Resolve(Inputs{
			Saved:          State{Country: "CN", Source: SourceExisting},
			CurrentCountry: "CN",
			ScanVotes:      strongBeacon,
		})
		if got.Country != "CN" || got.Source != SourceExisting {
			t.Fatalf("Resolve()=%+v, want CN/existing", got)
		}
	})

	t.Run("a factory default does not short-circuit detection", func(t *testing.T) {
		got := Resolve(Inputs{
			Saved:          State{Country: "CN", Source: SourceFactory},
			CurrentCountry: "CN",
			ScanVotes:      strongBeacon,
		})
		if got.Country != "DE" || got.Source != SourceBeacon {
			t.Fatalf("Resolve()=%+v, want DE/beacon", got)
		}
	})

	t.Run("beacons outrank the browser timezone", func(t *testing.T) {
		got := Resolve(Inputs{
			Saved:           State{Country: "CN", Source: SourceFactory},
			CurrentCountry:  "CN",
			ScanVotes:       strongBeacon,
			BrowserTimezone: "Asia/Tokyo",
		})
		if got.Source != SourceBeacon {
			t.Fatalf("Resolve()=%+v, want beacon", got)
		}
	})

	t.Run("the browser timezone outranks the agent timezone", func(t *testing.T) {
		got := Resolve(Inputs{
			Saved:           State{Country: "CN", Source: SourceFactory},
			CurrentCountry:  "CN",
			BrowserTimezone: "Europe/Paris",
			AgentTimezone:   "Asia/Tokyo",
		})
		if got.Country != "FR" || got.Source != SourceBrowserTZ {
			t.Fatalf("Resolve()=%+v, want FR/browser_tz", got)
		}
	})

	t.Run("UTC carries no location", func(t *testing.T) {
		got := Resolve(Inputs{
			Saved:          State{Country: "CN", Source: SourceFactory},
			CurrentCountry: "CN",
			AgentTimezone:  "UTC",
		})
		if got.Country != "CN" || got.Source != SourceUnknown {
			t.Fatalf("Resolve()=%+v, want the current value kept", got)
		}
	})

	t.Run("nothing known keeps the current setting", func(t *testing.T) {
		got := Resolve(Inputs{CurrentCountry: "CN"})
		if got.Country != "CN" || got.Source != SourceUnknown {
			t.Fatalf("Resolve()=%+v, want CN/unknown", got)
		}
	})

	t.Run("a device with nothing configured still gets a labelled default", func(t *testing.T) {
		// Refusing here would leave a device whose configuration is missing or
		// corrupt unable to join any network, recovery portal included.
		got := Resolve(Inputs{CurrentCountry: "nonsense"})
		if got.Country != FactoryCountry || got.Source != SourceFactory {
			t.Fatalf("Resolve()=%+v, want the factory default labelled as such", got)
		}
	})

	t.Run("a sidecar is preferred over the factory default", func(t *testing.T) {
		got := Resolve(Inputs{Saved: State{Country: "JP", Source: SourceFactory}})
		if got.Country != "JP" || got.Source != SourceFactory {
			t.Fatalf("Resolve()=%+v, want the recorded JP kept", got)
		}
	})
}

func TestApplicableOnlyCoversBeaconsAndOnlyWhenEnabled(t *testing.T) {
	beacon := Decision{Country: "DE", Source: SourceBeacon}
	if beacon.Applicable(false) {
		t.Fatal("beacon decisions must not apply while automatic application is off")
	}
	if !beacon.Applicable(true) {
		t.Fatal("beacon decisions must apply once enabled")
	}
	for _, source := range []Source{SourceBrowserTZ, SourceTimezone, SourceFactory, SourceUnknown} {
		if (Decision{Country: "DE", Source: source}).Applicable(true) {
			t.Fatalf("%s must never apply without an explicit confirmation", source)
		}
	}
}

// The agent's timezone list and this package's mapping must not drift apart: an
// upstream addition would otherwise silently resolve to no country.
func TestAgentTimezoneTableCoversAgentTimezones(t *testing.T) {
	supported := map[string]bool{}
	for _, timezone := range agent.SupportedTimezones() {
		supported[timezone] = true
		if timezone == utcTimezone {
			continue
		}
		country, ok := CountryForAgentTimezone(timezone)
		if !ok {
			t.Errorf("agent timezone %q has no country mapping", timezone)
			continue
		}
		if generated, found := ianaTimezoneCountry[timezone]; found && generated != country {
			t.Errorf("timezone %q maps to %q by hand but %q in the generated table", timezone, country, generated)
		}
	}
	for timezone := range agentTimezoneCountry {
		if !supported[timezone] {
			t.Errorf("mapping holds %q, which the agent no longer offers", timezone)
		}
	}
	if _, ok := CountryForAgentTimezone(utcTimezone); ok {
		t.Error("UTC must not resolve to a country")
	}
}

func TestCountryForTimezoneUsesTheGeneratedTable(t *testing.T) {
	for timezone, want := range map[string]string{
		"Europe/Berlin":      "DE",
		"America/Detroit":    "US",
		"Asia/Shanghai":      "CN",
		"Pacific/Auckland":   "NZ",
		"Europe/Busingen":    "DE",
		"UTC":                "",
		"Mars/Olympus_Mons":  "",
		"Europe":             "",
		"Antarctica/Vostok":  "",
		"America/Antigua":    "",
		"Atlantic/Bermuda":   "BM",
		"Asia/Ho_Chi_Minh":   "VN",
		"Africa/Ouagadougou": "BF",
	} {
		got, ok := CountryForTimezone(timezone)
		if got != want || ok != (want != "") {
			t.Errorf("CountryForTimezone(%q)=(%q,%v), want %q", timezone, got, ok, want)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wifi-region.json")

	state, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState on a missing file must succeed: %v", err)
	}
	if state.Source != SourceUnknown {
		t.Fatalf("missing state should be unknown, got %+v", state)
	}

	if err := SaveState(path, State{Country: "de", Source: SourceUser}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.Country != "DE" || loaded.Source != SourceUser {
		t.Fatalf("LoadState()=%+v, want DE/user", loaded)
	}

	if err := SaveState(path, State{Country: "nope", Source: SourceUser}); err == nil {
		t.Fatal("SaveState accepted an invalid country")
	}
}

func TestReconcileStateKeepsTheConfigurationFileAuthoritative(t *testing.T) {
	t.Run("a stale sidecar never rewrites the radio's country", func(t *testing.T) {
		got, changed := ReconcileState(State{Country: "DE", Source: SourceUser}, "CN")
		if !changed {
			t.Fatal("a disagreeing sidecar must be corrected")
		}
		if got.Country != "CN" {
			t.Fatalf("reconciled country=%q, want the configuration file's CN", got.Country)
		}
		if got.Source != SourceUnknown {
			t.Fatalf("reconciled source=%q, want provenance downgraded", got.Source)
		}
	})

	t.Run("a device with no sidecar predates the feature", func(t *testing.T) {
		got, changed := ReconcileState(EmptyState(), "CN")
		if !changed || got.Country != "CN" || got.Source != SourceExisting {
			t.Fatalf("ReconcileState()=%+v changed=%v, want CN/existing", got, changed)
		}
	})

	t.Run("agreement is left alone", func(t *testing.T) {
		state := State{Version: StateVersion, Country: "CN", Source: SourceUser}
		got, changed := ReconcileState(state, "CN")
		if changed {
			t.Fatalf("ReconcileState rewrote an agreeing state: %+v", got)
		}
		if got.Source != SourceUser {
			t.Fatalf("ReconcileState()=%+v, want the user's provenance kept", got)
		}
	})

	t.Run("an unusable configuration anchors nothing", func(t *testing.T) {
		state := State{Version: StateVersion, Country: "DE", Source: SourceUser}
		got, changed := ReconcileState(state, "")
		if changed || got.Country != "DE" {
			t.Fatalf("ReconcileState()=%+v changed=%v, want the sidecar untouched", got, changed)
		}
	})
}
