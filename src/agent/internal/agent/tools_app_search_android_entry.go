package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aiden-agent/internal/logging"
)

// Android launchers open app search in different ways: a mid-screen swipe
// down opens search on some brands and the notification shade on others. The
// search flow tries each known entry, checks the screenshot, and remembers the
// entry that worked for the connected phone so later searches start with it.

const androidSearchEntriesFileName = "android_search_entries.json"

const (
	androidSearchEntrySwipeDown  = "home_swipe_down"
	androidSearchEntryTypeOnHome = "home_type_to_search"
	androidSearchEntryAppDrawer  = "app_drawer_search"
)

// androidSearchEntryLastKey holds the entry that last worked on any phone. It
// is tried first when the connected phone is not identified yet: one board
// usually serves one phone, and the screenshot check catches a wrong guess.
const androidSearchEntryLastKey = "_last"

const (
	androidSearchHomeDelay    = 700 * time.Millisecond
	androidSearchGestureDelay = 700 * time.Millisecond
	androidSearchBackDelay    = 500 * time.Millisecond
)

type androidSearchEntry struct {
	id   string
	open func(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine) error
	// tapField taps the search input the check reports when the entry shows
	// one without focusing it, such as the search bar of an app drawer.
	tapField bool
}

var androidSearchEntries = []androidSearchEntry{
	{
		id: androidSearchEntrySwipeDown,
		open: func(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine) error {
			// Start below the status bar so the swipe reaches the launcher
			// instead of the notification shade.
			return androidSearchHomeThenSwipe(ctx, cfg, engine, 400, 750)
		},
	},
	{
		id: androidSearchEntryTypeOnHome,
		open: func(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine) error {
			if err := androidSearchGoHome(ctx, engine); err != nil {
				return err
			}
			// Launchers with type-to-search open search on the first letter
			// from a hardware keyboard; Backspace leaves the field empty.
			if err := engine.typeASCII(ctx, "a"); err != nil {
				return err
			}
			if err := engine.sleepFor(ctx, androidSearchGestureDelay); err != nil {
				return err
			}
			return engine.tapKeys(ctx, []string{"backspace"})
		},
	},
	{
		id: androidSearchEntryAppDrawer,
		open: func(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine) error {
			return androidSearchHomeThenSwipe(ctx, cfg, engine, 900, 300)
		},
		tapField: true,
	},
}

func androidSearchGoHome(ctx context.Context, engine *textInputEngine) error {
	if err := engine.tapKeys(ctx, []string{"KEYCODE_HOME"}); err != nil {
		return err
	}
	return engine.sleepFor(ctx, androidSearchHomeDelay)
}

func androidSearchHomeThenSwipe(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine, startY, endY int) error {
	if err := androidSearchGoHome(ctx, engine); err != nil {
		return err
	}
	out, err := cfg.hw.touchGesture.Call(ctx, jsonString(map[string]any{
		"type":  "swipe",
		"start": map[string]any{"x": 500, "y": startY},
		"end":   map[string]any{"x": 500, "y": endY},
		"speed": 2500,
	}))
	if err != nil {
		return err
	}
	if err := interpretTextInputToolOutput(out); err != nil {
		return err
	}
	return engine.sleepFor(ctx, androidSearchGestureDelay)
}

// androidSearchEntryOrder puts the remembered entry first and keeps the rest
// in their default order.
func androidSearchEntryOrder(preferred string) []androidSearchEntry {
	order := make([]androidSearchEntry, 0, len(androidSearchEntries))
	for _, entry := range androidSearchEntries {
		if entry.id == preferred {
			order = append(order, entry)
		}
	}
	for _, entry := range androidSearchEntries {
		if entry.id != preferred {
			order = append(order, entry)
		}
	}
	return order
}

type androidSearchCheck struct {
	SearchOpen  bool            `json:"search_open"`
	SearchField *focusPointArgs `json:"search_field"`
	Reason      string          `json:"reason"`
}

type androidSearchEntryOutcome struct {
	steps    []string
	vlmCalls int
}

// openAndroidSystemSearch opens launcher search with the first entry the
// screenshot check confirms and remembers it for the connected phone.
func openAndroidSystemSearch(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine) (androidSearchEntryOutcome, error) {
	var outcome androidSearchEntryOutcome
	store := cfg.searchEntries
	if store == nil {
		store = globalAndroidSearchEntries
	}
	phoneID := ""
	if cfg.phoneIDFn != nil {
		phoneID = strings.TrimSpace(cfg.phoneIDFn())
	}
	order := androidSearchEntryOrder(store.preferred(phoneID))
	tried := make([]string, 0, len(order))
	for index, entry := range order {
		if index > 0 {
			// Close what the previous entry opened, such as the notification
			// shade, before the next entry presses Home.
			if err := engine.tapKeys(ctx, []string{"KEYCODE_BACK"}); err != nil {
				return outcome, err
			}
			if err := engine.sleepFor(ctx, androidSearchBackDelay); err != nil {
				return outcome, err
			}
		}
		tried = append(tried, entry.id)
		if err := entry.open(ctx, cfg, engine); err != nil {
			outcome.steps = append(outcome.steps, fmt.Sprintf("search entry %s failed", entry.id))
			return outcome, err
		}
		check, calls, err := checkAndroidSearchOpen(ctx, cfg, engine)
		outcome.vlmCalls += calls
		if err != nil {
			outcome.steps = append(outcome.steps, fmt.Sprintf("check search entry %s failed", entry.id))
			return outcome, err
		}
		if !check.SearchOpen && entry.tapField && check.SearchField != nil {
			if err := tapSearchOpenResult(ctx, cfg.hw, *check.SearchField); err != nil {
				outcome.steps = append(outcome.steps, fmt.Sprintf("tap search field for %s failed", entry.id))
				return outcome, err
			}
			if err := engine.sleepFor(ctx, androidSearchGestureDelay); err != nil {
				return outcome, err
			}
			check, calls, err = checkAndroidSearchOpen(ctx, cfg, engine)
			outcome.vlmCalls += calls
			if err != nil {
				outcome.steps = append(outcome.steps, fmt.Sprintf("check search entry %s failed", entry.id))
				return outcome, err
			}
		}
		if check.SearchOpen {
			if err := store.remember(phoneID, entry.id); err != nil {
				logging.Warnf("agent", "app_search", "remember android search entry failed: %v", err)
			}
			logging.Infof("agent", "app_search", "android search opened entry=%s phone_id=%q", entry.id, phoneID)
			outcome.steps = append(outcome.steps, fmt.Sprintf("opened system search via %s", entry.id))
			return outcome, nil
		}
		logging.Infof("agent", "app_search", "android search entry=%s did not open search: %s", entry.id, check.Reason)
		outcome.steps = append(outcome.steps, fmt.Sprintf("search entry %s did not open search", entry.id))
	}
	return outcome, fmt.Errorf("could not open Android launcher search; tried %s", strings.Join(tried, ", "))
}

func checkAndroidSearchOpen(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine) (androidSearchCheck, int, error) {
	shot, err := engine.captureScreenshot(ctx)
	if err != nil {
		return androidSearchCheck{}, 0, err
	}
	if cfg.checkSearchOpenFn != nil {
		check, err := cfg.checkSearchOpenFn(ctx, shot)
		return check, 0, err
	}
	modelVision, ok := cfg.vision.(*llmTextInputVision)
	if !ok || modelVision == nil {
		return androidSearchCheck{}, 0, fmt.Errorf("android search check vision is not configured")
	}
	raw, err := modelVision.visionDecisionJSON(ctx, "android_search_entry", buildAndroidSearchCheckPrompt(), []string{"search_open"}, shot)
	if err != nil {
		return androidSearchCheck{}, 1, err
	}
	var check androidSearchCheck
	if err := json.Unmarshal([]byte(raw), &check); err != nil {
		return androidSearchCheck{}, 1, fmt.Errorf("parse android search check: %w", err)
	}
	return check, 1, nil
}

func buildAndroidSearchCheckPrompt() string {
	return strings.TrimSpace(`Analyze this Android phone screenshot taken right after trying to open the launcher's app search.
Decide whether a search input is open and ready for typing an app name.
Return JSON only:
{
  "search_open": true,
  "search_field": {"x": 500, "y": 80},
  "reason": "launcher search page with active input"
}

Rules:
- search_open=true only when a launcher or system search page is showing with its search input active: a text cursor, a highlighted or expanded search field, or a search page listing apps or suggestions. A hardware keyboard is connected, so the on-screen keyboard may be hidden.
- search_open=false for the notification shade or quick settings, the plain home screen, an app drawer whose search field is not active, a web browser, or any other app.
- search_field: when search_open=false but an inactive app search field is visible, such as the search bar at the top of an app drawer, give its center in normalized 0-1000 coordinates. Otherwise return null.
- Keep reason short and concrete.`)
}

// androidSearchEntryStore remembers the working search entry per phone in the
// Agent config directory. Without a path it only keeps entries in memory.
type androidSearchEntryStore struct {
	mu      sync.Mutex
	path    string
	loaded  bool
	entries map[string]string
}

type androidSearchEntriesFile struct {
	Version int               `json:"version"`
	Entries map[string]string `json:"entries"`
}

var globalAndroidSearchEntries = &androidSearchEntryStore{}

func configureAndroidSearchEntryStore(configDir string) {
	path := ""
	if configDir != "" {
		path = filepath.Join(configDir, androidSearchEntriesFileName)
	}
	globalAndroidSearchEntries.mu.Lock()
	defer globalAndroidSearchEntries.mu.Unlock()
	globalAndroidSearchEntries.path = path
	globalAndroidSearchEntries.loaded = false
	globalAndroidSearchEntries.entries = nil
}

func (s *androidSearchEntryStore) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.entries = map[string]string{}
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var file androidSearchEntriesFile
	if err := json.Unmarshal(data, &file); err != nil {
		logging.Warnf("agent", "app_search", "ignore invalid %s: %v", s.path, err)
		return
	}
	for key, id := range file.Entries {
		s.entries[key] = id
	}
}

// preferred returns the entry remembered for phoneID, or the entry that last
// worked when the phone is unknown or has no entry yet.
func (s *androidSearchEntryStore) preferred(phoneID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if phoneID != "" {
		if id, ok := s.entries[phoneID]; ok {
			return id
		}
	}
	return s.entries[androidSearchEntryLastKey]
}

func (s *androidSearchEntryStore) remember(phoneID, entryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	changed := s.entries[androidSearchEntryLastKey] != entryID
	s.entries[androidSearchEntryLastKey] = entryID
	if phoneID != "" && s.entries[phoneID] != entryID {
		s.entries[phoneID] = entryID
		changed = true
	}
	if !changed || s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(androidSearchEntriesFile{Version: 1, Entries: s.entries}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0o644)
}
