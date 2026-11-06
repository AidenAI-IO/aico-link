package agent

import (
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/logging"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Shell commands see the screenshot the model saw last, as files named by
// these environment variables. Pixel coordinates in the files are the
// screenshot's own, so a value measured there converts to touch coordinates
// with the same width and height the model was told.
const (
	shellScreenshotJPEGEnv   = "AICO_SCREENSHOT_JPEG"
	shellScreenshotPPMEnv    = "AICO_SCREENSHOT_PPM"
	shellScreenshotWidthEnv  = "AICO_SCREENSHOT_WIDTH"
	shellScreenshotHeightEnv = "AICO_SCREENSHOT_HEIGHT"
)

// shellScreenshotExport writes the latest screenshot for shell commands. The
// PPM copy is uncompressed RGB, readable with only the Python standard
// library, since the controller has no image libraries.
type shellScreenshotExport struct {
	screen *screen.ScreenState
	dir    string

	mu         sync.Mutex
	generation uint64
	width      int
	height     int
	exported   bool
}

func newShellScreenshotExport(state *screen.ScreenState, dir string) *shellScreenshotExport {
	if state == nil {
		return nil
	}
	if dir == "" {
		dir = defaultShellScreenshotDir()
	}
	e := &shellScreenshotExport{screen: state, dir: dir}
	// Drop the files as soon as the screenshot is retired, not at the next
	// shell command. A rebuilt tool set replaces this hook with its own
	// exporter, which uses the same directory.
	state.SetCaptureInvalidatedHook(e.clear)
	return e
}

// defaultShellScreenshotDir is private to this process, so two Agent
// processes sharing a temp directory never read or remove each other's
// screenshot.
func defaultShellScreenshotDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("aico-agent-screenshot-%d", os.Getpid()))
}

// removeShellScreenshotExports deletes this process's default export
// directory, for Agent shutdown.
func removeShellScreenshotExports() {
	os.RemoveAll(defaultShellScreenshotDir())
}

// clear removes the exported files and forgets them.
func (e *shellScreenshotExport) clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	os.Remove(e.jpegPath())
	os.Remove(e.ppmPath())
	e.exported = false
}

func (e *shellScreenshotExport) jpegPath() string { return filepath.Join(e.dir, "screen.jpg") }
func (e *shellScreenshotExport) ppmPath() string  { return filepath.Join(e.dir, "screen.ppm") }

// env exports the latest screenshot when it changed since the last export and
// returns the variables naming it. It returns nil when there is no
// screenshot or it cannot be written, so a shell command never waits on or
// fails because of it.
func (e *shellScreenshotExport) env() []string {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	generation := e.screen.ScreenshotGeneration()
	if !e.exported || generation != e.generation {
		data, width, height, _, ok := e.screen.LatestScreenshot(0)
		if !ok {
			// No current screenshot: make sure nothing reads a stale screen.
			os.Remove(e.jpegPath())
			os.Remove(e.ppmPath())
			e.exported = false
			e.generation = generation
			return nil
		}
		if err := e.write(data); err != nil {
			logging.Warnf("agent", "shell_screenshot", "export screenshot for shell: %v", err)
			e.exported = false
			return nil
		}
		e.generation, e.width, e.height, e.exported = generation, width, height, true
	}
	return []string{
		shellScreenshotJPEGEnv + "=" + e.jpegPath(),
		shellScreenshotPPMEnv + "=" + e.ppmPath(),
		shellScreenshotWidthEnv + "=" + strconv.Itoa(e.width),
		shellScreenshotHeightEnv + "=" + strconv.Itoa(e.height),
	}
}

func (e *shellScreenshotExport) write(jpegData []byte) error {
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return fmt.Errorf("decode screenshot: %w", err)
	}
	// Screenshots can show private content, so only the Agent's own user
	// may read them. MkdirAll keeps the mode of a directory that already
	// exists, so tighten it explicitly.
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(e.dir, 0o700); err != nil {
		return err
	}
	if err := writeScreenshotFile(e.jpegPath(), jpegData); err != nil {
		return err
	}
	return writeScreenshotFile(e.ppmPath(), encodePPM(img))
}

// writeScreenshotFile replaces path in one rename, so a command reading it
// while the next screenshot is exported never sees a partial image. The
// temporary file is unique and created 0600, so neither a leftover from an
// interrupted write nor a concurrent export can widen its permissions.
func writeScreenshotFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// encodePPM returns img as binary PPM (P6): a text header "P6\n<w> <h>\n255\n"
// followed by width*height RGB triples, row by row from the top left.
func encodePPM(img image.Image) []byte {
	bounds := img.Bounds()
	header := fmt.Sprintf("P6\n%d %d\n255\n", bounds.Dx(), bounds.Dy())
	out := make([]byte, 0, len(header)+bounds.Dx()*bounds.Dy()*3)
	out = append(out, header...)
	// Screenshots decode as YCbCr; converting its planes directly avoids an
	// interface call per pixel, which matters on the controller's slow CPU.
	if ycc, ok := img.(*image.YCbCr); ok {
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				yi, ci := ycc.YOffset(x, y), ycc.COffset(x, y)
				r, g, b := color.YCbCrToRGB(ycc.Y[yi], ycc.Cb[ci], ycc.Cr[ci])
				out = append(out, r, g, b)
			}
		}
		return out
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			out = append(out, byte(r>>8), byte(g>>8), byte(b>>8))
		}
	}
	return out
}
