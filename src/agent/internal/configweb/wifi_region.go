package configweb

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/logging"
	"aico-agent/internal/wifiregion"
)

type wifiRegionDriver interface {
	Apply(context.Context, string, string) error
	Read(context.Context, string) (string, error)
}

type systemWiFiRegionDriver struct{}

func (systemWiFiRegionDriver) Apply(ctx context.Context, interfaceName, country string) error {
	return wifiregion.ApplyDriverCountry(ctx, interfaceName, country)
}

func (systemWiFiRegionDriver) Read(ctx context.Context, interfaceName string) (string, error) {
	return wifiregion.ReadDriverCountry(ctx, interfaceName)
}

func (s *Server) setWiFiRadioCountry(ctx context.Context, country string) error {
	if err := s.wifiRegionDriver.Apply(ctx, s.options.WiFiInterface, country); err != nil {
		return err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var actual string
	var readErr error
	for {
		actual, readErr = s.wifiRegionDriver.Read(verifyCtx, s.options.WiFiInterface)
		if readErr == nil && actual == country {
			return nil
		}
		select {
		case <-verifyCtx.Done():
			if readErr != nil {
				return fmt.Errorf("read back Wi-Fi phy country %q: %w", country, readErr)
			}
			return fmt.Errorf("Wi-Fi phy country is %q after setting %q: %w", actual, country, verifyCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// wifiRegionVoteTTL bounds how long a scan result stays eligible to influence a
// decision. Country information elements describe the radios that were audible
// at scan time; a stale set should not silently steer a later connection.
const wifiRegionVoteTTL = 10 * time.Minute

// recordWiFiCountryVotes stores the tallies from the most recent scan. Scanning
// never writes configuration: it only makes a suggestion available.
func (s *Server) recordWiFiCountryVotes(votes map[string]int) {
	s.wifiRegionMu.Lock()
	defer s.wifiRegionMu.Unlock()
	s.wifiRegionVotes = votes
	s.wifiRegionVotesAt = time.Now()
}

func (s *Server) wifiCountryVotes() map[string]int {
	s.wifiRegionMu.Lock()
	defer s.wifiRegionMu.Unlock()
	if s.wifiRegionVotes == nil || time.Since(s.wifiRegionVotesAt) > wifiRegionVoteTTL {
		return nil
	}
	votes := make(map[string]int, len(s.wifiRegionVotes))
	for country, count := range s.wifiRegionVotes {
		votes[country] = count
	}
	return votes
}

// loadWiFiRegionState reads the sidecar and aligns it with the configuration
// file, which is authoritative. A disagreement means the sidecar is stale, so
// its country is corrected and its provenance downgraded — never the reverse.
func (s *Server) loadWiFiRegionState(configCountry string) wifiregion.State {
	state, err := wifiregion.LoadState(s.options.WiFiRegionStatePath)
	if err != nil {
		logging.Warnf("config_web", "wifi_region", "Wi-Fi region state unreadable, continuing without provenance: %v", err)
		// A missing sidecar identifies an older deployment; a corrupt or
		// unreadable one does not establish that provenance.
		if country, ok := wifiregion.Normalize(configCountry); ok {
			return wifiregion.State{Version: wifiregion.StateVersion, Country: country, Source: wifiregion.SourceUnknown}
		}
		return wifiregion.EmptyState()
	}
	reconciled, changed := wifiregion.ReconcileState(state, configCountry)
	if changed {
		if err := wifiregion.SaveState(s.options.WiFiRegionStatePath, reconciled); err != nil {
			logging.Warnf("config_web", "wifi_region", "persist reconciled Wi-Fi region state: %v", err)
		}
	}
	return reconciled
}

// persistWiFiRegion records provenance after the configuration file has been
// written. Failures are logged only: the radio is already configured, and
// losing the label is preferable to failing a working connection.
func (s *Server) persistWiFiRegion(decision wifiregion.Decision) {
	if decision.Country == "" {
		return
	}
	state := wifiregion.State{
		Country:    decision.Country,
		Source:     decision.Source,
		DetectedAt: time.Now().UTC(),
	}
	if decision.Source == wifiregion.SourceBeacon {
		state.Votes = s.wifiCountryVotes()
	}
	if err := wifiregion.SaveState(s.options.WiFiRegionStatePath, state); err != nil {
		logging.Warnf("config_web", "wifi_region", "persist Wi-Fi region state: %v", err)
	}
}

// wifiRegionInputs assembles the provenance chain's inputs from disk, the last
// scan, and the agent configuration.
func (s *Server) wifiRegionInputs(configCountry, browserTimezone string) wifiregion.Inputs {
	inputs := wifiregion.Inputs{
		Saved:           s.loadWiFiRegionState(configCountry),
		CurrentCountry:  configCountry,
		ScanVotes:       s.wifiCountryVotes(),
		BrowserTimezone: strings.TrimSpace(browserTimezone),
	}
	if config, err := agent.LoadRuntimeConfig(s.options.AgentConfigPath); err == nil {
		inputs.AgentTimezone = config.TimezoneOrDefault()
	}
	return inputs
}

// resolveWiFiRegion runs the chain and reports what the country should be.
//
// Only a beacon-derived answer may be adopted without asking, and only when the
// operator enabled that. Everything a detector merely suspects — timezones
// always, beacons while the switch is off — stays a suggestion for the portal,
// and the device keeps what it already has.
func (s *Server) resolveWiFiRegion(configCountry, browserTimezone string) wifiregion.Decision {
	inputs := s.wifiRegionInputs(configCountry, browserTimezone)
	// A stale or corrupt sidecar degrades to manual selection. A beacon must not
	// replace the authoritative configuration merely because its label was lost.
	if inputs.Saved.Source == wifiregion.SourceUnknown {
		if country, ok := wifiregion.Normalize(configCountry); ok {
			return wifiregion.Decision{Country: country, Source: wifiregion.SourceUnknown}
		}
	}
	decision := wifiregion.Resolve(inputs)
	switch decision.Source {
	case wifiregion.SourceUser, wifiregion.SourceExisting:
		return decision
	case wifiregion.SourceBeacon:
		if decision.Applicable(s.options.WiFiRegionAutoApplyBeacon) {
			return decision
		}
	}
	// Not adoptable: keep the configured country, or the recorded one, or the
	// labelled factory default. Falling through to the detected value here would
	// apply a suggestion that was explicitly not allowed to apply.
	if country, ok := wifiregion.Normalize(configCountry); ok {
		return wifiregion.Decision{Country: country, Source: inputs.Saved.Source}
	}
	if country, ok := wifiregion.Normalize(inputs.Saved.Country); ok {
		return wifiregion.Decision{Country: country, Source: inputs.Saved.Source}
	}
	return wifiregion.Decision{Country: wifiregion.FactoryCountry, Source: wifiregion.SourceFactory}
}

func wifiRegionValue(decision wifiregion.Decision) map[string]any {
	return map[string]any{"country": decision.Country, "source": string(decision.Source)}
}

func (s *Server) wifiRegionPayload(browserTimezone string) map[string]any {
	config, err := loadWiFiConfig(s.options.WiFiConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return map[string]any{"error": err.Error()}
	}
	inputs := s.wifiRegionInputs(config.Country, browserTimezone)
	candidates := []map[string]any{}
	for _, candidate := range wifiregion.Candidates(inputs) {
		value := wifiRegionValue(candidate)
		if candidate.Source == wifiregion.SourceBeacon {
			value["votes"] = inputs.ScanVotes
		}
		candidates = append(candidates, value)
	}
	return map[string]any{
		"current":           map[string]any{"country": config.Country, "source": string(inputs.Saved.Source)},
		"candidates":        candidates,
		"options":           wifiregion.Options(),
		"auto_apply_beacon": s.options.WiFiRegionAutoApplyBeacon,
	}
}

func (s *Server) handleWiFiRegionGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.wifiRegionPayload(""))
}

type wifiRegionResolveRequest struct {
	BrowserTimezone string `json:"browser_timezone"`
}

// handleWiFiRegionResolve returns the candidate values for the portal. The client's
// timezone arrives in the body rather than the query string so it stays out of
// access logs.
func (s *Server) handleWiFiRegionResolve(w http.ResponseWriter, r *http.Request) {
	var request wifiRegionResolveRequest
	if !readJSONBody(w, r, &request) {
		return
	}
	writeJSON(w, http.StatusOK, s.wifiRegionPayload(request.BrowserTimezone))
}

type wifiRegionUpdateRequest struct {
	Country string `json:"country"`
}

// handleWiFiRegionUpdate is the only path that records SourceUser. Intent is
// taken from the fact that this endpoint was called at all — never inferred
// from a value matching a suggestion, or from a client-side event.
func (s *Server) handleWiFiRegionUpdate(w http.ResponseWriter, r *http.Request) {
	var request wifiRegionUpdateRequest
	if !readJSONBody(w, r, &request) {
		return
	}
	country, ok := wifiregion.Normalize(request.Country)
	if !ok || country == wifiregion.WorldCountry {
		writeJSONError(w, http.StatusBadRequest, "unsupported Wi-Fi country "+request.Country)
		return
	}
	if !s.wifiOpMu.TryLock() {
		writeJSONError(w, http.StatusConflict, "another Wi-Fi configuration operation is already running")
		return
	}
	defer s.wifiOpMu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), wifiCandidateApplyTimeout)
	defer cancel()
	result := s.applyWiFiRegion(ctx, country)
	status := http.StatusOK
	if result["ok"] != true {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, result)
}

// applyWiFiRegion applies a user-confirmed country to the self-managed wiphy
// and the supplicant. The authoritative file and sidecar are committed only
// after the driver and the candidate supplicant configuration succeed.
func (s *Server) applyWiFiRegion(ctx context.Context, country string) map[string]any {
	snapshot, err := captureFileSnapshot(s.options.WiFiConfigPath)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	config, err := loadWiFiConfig(s.options.WiFiConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	if config.Country == country {
		if actual, err := s.wifiRegionDriver.Read(ctx, s.options.WiFiInterface); err == nil && actual == country {
			s.persistWiFiRegion(wifiregion.Decision{Country: country, Source: wifiregion.SourceUser})
			return map[string]any{"ok": true, "region": map[string]any{"country": country, "source": string(wifiregion.SourceUser)}, "message": "wifi region unchanged"}
		}
	}
	statusBefore := s.queryWiFiStatusContext(ctx)
	requireConnectivity := statusBefore["connected"] == true && statusBefore["ip_address"] != ""
	config.Country = country
	candidate := s.options.WiFiConfigPath + ".region-candidate"
	if err := saveWiFiConfig(candidate, config); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	removeCandidate := true
	defer func() {
		if removeCandidate {
			_ = os.Remove(candidate)
		}
	}()
	radioErr := s.setWiFiRadioCountry(ctx, country)
	apply := commandResult{ExitCode: 1}
	if radioErr == nil {
		apply = s.applyWiFiWithConnectivity(ctx, candidate, true, requireConnectivity)
	} else {
		apply.Output = []byte(radioErr.Error())
	}
	if apply.ExitCode == 0 {
		if err := saveWiFiConfig(s.options.WiFiConfigPath, config); err == nil {
			removeCandidate = false
			s.persistWiFiRegion(wifiregion.Decision{Country: country, Source: wifiregion.SourceUser})
			return map[string]any{
				"ok":          true,
				"region":      map[string]any{"country": country, "source": string(wifiregion.SourceUser)},
				"wifi_status": s.queryWiFiStatusContext(ctx),
				"message":     "wifi region saved",
			}
		} else {
			apply.ExitCode = 1
			apply.Output = append(apply.Output, []byte("\nsave Wi-Fi config: "+err.Error())...)
		}
	}
	// Rollback must proceed even if the HTTP client disconnected or the candidate
	// timed out. A new context gives the old region a bounded recovery window.
	rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancelRollback()
	rollbackCountry := snapshotWiFiCountry(snapshot)
	if rollbackCountry == "" {
		rollbackCountry = wifiregion.FactoryCountry
	}
	radioRollbackErr := s.setWiFiRadioCountry(rollbackCtx, rollbackCountry)
	restoreErr := restoreFileSnapshot(snapshot)
	rollback := commandResult{ExitCode: 0}
	if restoreErr == nil {
		if snapshot.existed {
			rollback = s.applyWiFiWithConnectivity(rollbackCtx, s.options.WiFiConfigPath, true, requireConnectivity)
		} else {
			rollback = s.stopWiFiSupplicant(rollbackCtx)
		}
	}
	result := map[string]any{
		"ok": false, "error": "failed to apply Wi-Fi region",
		"wifi_apply":    map[string]any{"exit_code": apply.ExitCode, "output": strings.TrimRight(string(apply.Output), "\r\n")},
		"wifi_rollback": map[string]any{"ok": rollback.ExitCode == 0 && restoreErr == nil && radioRollbackErr == nil, "exit_code": rollback.ExitCode},
	}
	if restoreErr != nil {
		result["disk_error"] = restoreErr.Error()
	}
	if radioRollbackErr != nil {
		result["radio_error"] = radioRollbackErr.Error()
	}
	return result
}

func snapshotWiFiCountry(snapshot fileSnapshot) string {
	if !snapshot.existed {
		return ""
	}
	for _, line := range strings.Split(string(snapshot.data), "\n") {
		line = strings.TrimSpace(line)
		if line == "network={" {
			break
		}
		if value, ok := strings.CutPrefix(line, "country="); ok {
			if country, valid := wifiregion.Normalize(value); valid {
				return country
			}
			return ""
		}
	}
	return ""
}
