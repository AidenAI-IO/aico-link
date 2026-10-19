package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func androidSearchAlwaysOpen(context.Context, screenshotResult) (androidSearchCheck, error) {
	return androidSearchCheck{SearchOpen: true}, nil
}

type androidSearchEntryHarness struct {
	cfg          appSearchOpenFlowConfig
	engine       *textInputEngine
	keys         *recordingTextInputTool
	touch        *recordingTextInputTool
	keyboardText *recordingTextInputTool
	checks       int
}

// newAndroidSearchEntryHarness answers the screenshot checks in order; the
// last answer repeats.
func newAndroidSearchEntryHarness(store *androidSearchEntryStore, phoneID string, answers ...androidSearchCheck) *androidSearchEntryHarness {
	h := &androidSearchEntryHarness{
		keys:         &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		touch:        &recordingTextInputTool{name: "touch_gesture", out: "ok"},
		keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
	}
	hw := &textInputHardwareDeps{
		touchGesture: h.touch,
		keyboardText: h.keyboardText,
		keyboardTap:  h.keys,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}
	vision := &stubTextInputVision{}
	h.engine = newTextInputEngineWithSleep(*hw, vision, testNoWaitSleep)
	h.cfg = appSearchOpenFlowConfig{
		hw:            hw,
		vision:        vision,
		platform:      "android",
		sleep:         testNoWaitSleep,
		searchEntries: store,
		phoneIDFn:     func() string { return phoneID },
		checkSearchOpenFn: func(context.Context, screenshotResult) (androidSearchCheck, error) {
			answer := answers[len(answers)-1]
			if h.checks < len(answers) {
				answer = answers[h.checks]
			}
			h.checks++
			return answer, nil
		},
	}
	return h
}

func TestOpenAndroidSystemSearchTriesEntriesAndRemembersTheWorkingOne(t *testing.T) {
	store := &androidSearchEntryStore{}
	h := newAndroidSearchEntryHarness(store, "phone-1",
		androidSearchCheck{Reason: "notification shade"},
		androidSearchCheck{Reason: "home screen"},
		androidSearchCheck{SearchField: &focusPointArgs{X: 500, Y: 90}, Reason: "app drawer"},
		androidSearchCheck{SearchOpen: true},
	)

	outcome, err := openAndroidSystemSearch(context.Background(), h.cfg, h.engine)
	if err != nil {
		t.Fatalf("openAndroidSystemSearch() error = %v", err)
	}
	wantKeys := [][]string{
		{"KEYCODE_HOME"},
		{"KEYCODE_BACK"}, {"KEYCODE_HOME"}, {"backspace"},
		{"KEYCODE_BACK"}, {"KEYCODE_HOME"},
	}
	if got := tappedKeys(t, h.keys); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("keyboard_tap = %v, want %v", got, wantKeys)
	}
	if got := typedTexts(t, h.keyboardText); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("keyboard_text = %q, want the type-to-search letter", got)
	}
	// Swipe down, swipe up into the drawer, then tap its search field.
	if len(h.touch.calls) != 3 || !strings.Contains(h.touch.calls[2], `"tap"`) {
		t.Fatalf("touch_gesture = %v, want two swipes and a search field tap", h.touch.calls)
	}
	if h.checks != 4 {
		t.Fatalf("screenshot checks = %d, want 4", h.checks)
	}
	if last := outcome.steps[len(outcome.steps)-1]; last != "opened system search via "+androidSearchEntryAppDrawer {
		t.Fatalf("steps = %q", outcome.steps)
	}
	if got := store.preferred("phone-1"); got != androidSearchEntryAppDrawer {
		t.Fatalf("remembered entry for phone-1 = %q", got)
	}
	if got := store.preferred("other-phone"); got != androidSearchEntryAppDrawer {
		t.Fatalf("fallback entry for an unknown phone = %q, want the last working entry", got)
	}
}

func TestOpenAndroidSystemSearchStartsWithRememberedEntry(t *testing.T) {
	store := &androidSearchEntryStore{}
	if err := store.remember("phone-1", androidSearchEntryTypeOnHome); err != nil {
		t.Fatal(err)
	}
	h := newAndroidSearchEntryHarness(store, "phone-1", androidSearchCheck{SearchOpen: true})

	if _, err := openAndroidSystemSearch(context.Background(), h.cfg, h.engine); err != nil {
		t.Fatalf("openAndroidSystemSearch() error = %v", err)
	}
	if got := tappedKeys(t, h.keys); !reflect.DeepEqual(got, [][]string{{"KEYCODE_HOME"}, {"backspace"}}) {
		t.Fatalf("keyboard_tap = %v, want only the remembered type-to-search entry", got)
	}
	if len(h.touch.calls) != 0 || h.checks != 1 {
		t.Fatalf("touch_gesture = %v, checks = %d; want no swipe and one check", h.touch.calls, h.checks)
	}
}

func TestOpenAndroidSystemSearchReportsWhenNoEntryWorks(t *testing.T) {
	store := &androidSearchEntryStore{}
	h := newAndroidSearchEntryHarness(store, "phone-1", androidSearchCheck{Reason: "notification shade"})

	_, err := openAndroidSystemSearch(context.Background(), h.cfg, h.engine)
	if err == nil {
		t.Fatal("openAndroidSystemSearch() error = nil, want failure")
	}
	for _, id := range []string{androidSearchEntrySwipeDown, androidSearchEntryTypeOnHome, androidSearchEntryAppDrawer} {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("error %q does not list tried entry %s", err, id)
		}
	}
	if got := store.preferred("phone-1"); got != "" {
		t.Fatalf("remembered entry = %q, want none after failure", got)
	}
}

func TestAndroidSearchEntryStorePersistsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), androidSearchEntriesFileName)
	store := &androidSearchEntryStore{path: path}
	if err := store.remember("phone-1", androidSearchEntryAppDrawer); err != nil {
		t.Fatalf("remember() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("entries file not written: %v", err)
	}

	reloaded := &androidSearchEntryStore{path: path}
	if got := reloaded.preferred("phone-1"); got != androidSearchEntryAppDrawer {
		t.Fatalf("reloaded entry = %q", got)
	}
	if got := reloaded.preferred(""); got != androidSearchEntryAppDrawer {
		t.Fatalf("entry for an unidentified phone = %q, want the last working entry", got)
	}
}
