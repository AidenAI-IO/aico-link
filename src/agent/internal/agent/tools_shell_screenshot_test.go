package agent

import (
	"aico-agent/internal/agent/screen"
	"bytes"
	"context"
	"fmt"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shellScreenshotEnvMap(entries []string) map[string]string {
	out := map[string]string{}
	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		out[key] = value
	}
	return out
}

func TestShellScreenshotExportWritesLatestScreenshot(t *testing.T) {
	state := &screen.ScreenState{}
	export := newShellScreenshotExport(state, t.TempDir())
	if env := export.env(); env != nil {
		t.Fatalf("env before any screenshot = %v, want none", env)
	}

	raw := solidJPEG(t, 6, 4, color.RGBA{R: 200, G: 40, B: 10, A: 255})
	state.UpdateScreenshot(raw, 6, 4)
	env := shellScreenshotEnvMap(export.env())
	if env[shellScreenshotWidthEnv] != "6" || env[shellScreenshotHeightEnv] != "4" {
		t.Fatalf("size env = %v", env)
	}
	jpegData, err := os.ReadFile(env[shellScreenshotJPEGEnv])
	if err != nil || !bytes.Equal(jpegData, raw) {
		t.Fatalf("jpeg file = %d bytes, err %v; want the screenshot bytes", len(jpegData), err)
	}
	ppm, err := os.ReadFile(env[shellScreenshotPPMEnv])
	if err != nil {
		t.Fatal(err)
	}
	header := "P6\n6 4\n255\n"
	if !strings.HasPrefix(string(ppm), header) || len(ppm) != len(header)+6*4*3 {
		t.Fatalf("ppm header/size = %q / %d", ppm[:min(len(ppm), 12)], len(ppm))
	}
	decoded, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decoded.At(5, 3).RGBA()
	last := ppm[len(ppm)-3:]
	if last[0] != byte(r>>8) || last[1] != byte(g>>8) || last[2] != byte(b>>8) {
		t.Fatalf("bottom-right pixel = %v, want the decoded screenshot pixel", last)
	}

	// A new screenshot replaces the files; an unchanged one is not rewritten.
	before, _ := os.Stat(env[shellScreenshotPPMEnv])
	export.env()
	after, _ := os.Stat(env[shellScreenshotPPMEnv])
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("unchanged screenshot was exported again")
	}
	next := solidJPEG(t, 3, 5, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	state.UpdateScreenshot(next, 3, 5)
	env = shellScreenshotEnvMap(export.env())
	if env[shellScreenshotWidthEnv] != "3" || env[shellScreenshotHeightEnv] != "5" {
		t.Fatalf("size env after a new screenshot = %v", env)
	}
	if data, _ := os.ReadFile(env[shellScreenshotJPEGEnv]); !bytes.Equal(data, next) {
		t.Fatal("jpeg file was not replaced by the new screenshot")
	}

	if info, err := os.Stat(env[shellScreenshotPPMEnv]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ppm mode = %v err %v, want 0600: screenshots can show private content", info.Mode().Perm(), err)
	}

	// A retired capture backend removes the files at once, before any later
	// shell command, and leaves no screenshot to point at.
	state.InvalidateCapture()
	for _, p := range []string{export.jpegPath(), export.ppmPath()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists right after capture invalidation (err %v)", p, err)
		}
	}
	if env := export.env(); env != nil {
		t.Fatalf("env after capture invalidation = %v, want none", env)
	}
}

func TestShellScreenshotDefaultDirIsPerProcess(t *testing.T) {
	dir := defaultShellScreenshotDir()
	if !strings.HasSuffix(dir, fmt.Sprintf("aico-agent-screenshot-%d", os.Getpid())) {
		t.Fatalf("default dir = %q, want one named for this process", dir)
	}
}

func TestShellScreenshotExportStaysPrivateOverAWiderDirectory(t *testing.T) {
	// A directory and a temporary file left by an earlier build with wider
	// modes must not expose the next screenshot.
	dir := filepath.Join(t.TempDir(), "shots")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(dir, "screen.ppm.tmp")
	if err := os.WriteFile(leftover, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := &screen.ScreenState{}
	state.UpdateScreenshot(solidJPEG(t, 6, 4, color.RGBA{A: 255}), 6, 4)
	export := newShellScreenshotExport(state, dir)
	if env := export.env(); env == nil {
		t.Fatal("no screenshot exported")
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v err %v, want 0700", info.Mode().Perm(), err)
	}
	for _, p := range []string{export.jpegPath(), export.ppmPath()} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v err %v, want 0600", p, info.Mode().Perm(), err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if name := e.Name(); name != "screen.jpg" && name != "screen.ppm" && name != "screen.ppm.tmp" {
			t.Fatalf("export left %q behind", name)
		}
	}
}

func TestShellCommandSeesScreenshotEnvironment(t *testing.T) {
	state := &screen.ScreenState{}
	state.UpdateScreenshot(solidJPEG(t, 6, 4, color.RGBA{A: 255}), 6, 4)
	execution := shellExecutionConfig{screenshot: newShellScreenshotExport(state, t.TempDir())}
	out, err := shellRunForeground(context.Background(), `printf '%s %s ' "$AICO_SCREENSHOT_WIDTH" "$AICO_SCREENSHOT_HEIGHT"; head -c 2 "$AICO_SCREENSHOT_PPM"`, "", false, execution)
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if !strings.Contains(out, "6 4 P6") {
		t.Fatalf("shell output = %q, want the screenshot size and PPM magic", out)
	}
}
