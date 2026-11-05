package agent

import (
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/logging"
	"bytes"
	"fmt"
	"image"
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
		dir = filepath.Join(os.TempDir(), "aico-agent-screenshot")
	}
	return &shellScreenshotExport{screen: state, dir: dir}
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
	// Each file is replaced in one rename, so a command reading it while the
	// next screenshot is exported never sees a partial image.
	if err := writeFileAtomic(e.jpegPath(), jpegData, 0o644); err != nil {
		return err
	}
	return writeFileAtomic(e.ppmPath(), encodePPM(img), 0o644)
}

// encodePPM returns img as binary PPM (P6): a text header "P6\n<w> <h>\n255\n"
// followed by width*height RGB triples, row by row from the top left.
func encodePPM(img image.Image) []byte {
	bounds := img.Bounds()
	header := fmt.Sprintf("P6\n%d %d\n255\n", bounds.Dx(), bounds.Dy())
	out := make([]byte, 0, len(header)+bounds.Dx()*bounds.Dy()*3)
	out = append(out, header...)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			out = append(out, byte(r>>8), byte(g>>8), byte(b>>8))
		}
	}
	return out
}
