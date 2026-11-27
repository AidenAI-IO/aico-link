package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	toolReadImage = "read_image"
	// readImageMaxEdge bounds the longer side sent to the model; larger
	// images are scaled down to keep request size and decode memory low.
	readImageMaxEdge = 1568
	// readImageMaxFileBytes refuses files too large to decode on the
	// controller.
	readImageMaxFileBytes = 25 << 20
	// readImageMaxPixels refuses images whose decoded bitmap would exhaust
	// the controller's memory, checked from the header before decoding.
	readImageMaxPixels   = 40_000_000
	readImageJPEGQuality = 85
)

// ReadImageTool shows the model an image file on the controller. The
// result uses the screenshot observation shape, so the agent loop attaches the
// image to the next model request as it does for screenshots.
type ReadImageTool struct{}

func NewReadImageTool() *ReadImageTool { return &ReadImageTool{} }

func (t *ReadImageTool) Name() string { return toolReadImage }

func (t *ReadImageTool) ReturnsVisualObservation() bool { return true }

func (t *ReadImageTool) Description() string {
	return "Look at an image under /tmp or " + sharedFilesDir() + " using its absolute path, such as a screenshot saved by device-cli look, an uploaded photo, or a chart. " +
		"Input: {\"path\": \"/tmp/device-cli-example.jpg\"}. Supports PNG, JPEG, and GIF (first frame). " +
		"The tool result is ok, and the image arrives in a follow-up state message, scaled down if its longer side exceeds " + fmt.Sprint(readImageMaxEdge) + " px. " +
		"It is a file, not the connected display: do not use its coordinates for touch_gesture. Use screenshot to see the display."
}

func (t *ReadImageTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"path": stringArgSchema("Absolute image path under /tmp or "+sharedFilesDir()+".", "/tmp/device-cli-example.jpg"),
	}, "path")
}

type readImageResult struct {
	screenshotResult
	Path         string `json:"path"`
	SourceFormat string `json:"source_format"`
}

func (t *ReadImageTool) Call(ctx context.Context, input string) (string, error) {
	filename, err := parseReadImageArgs(input)
	if err != nil {
		return toolErrorResultString(ctx, CodeInvalidArguments, err.Error()), nil
	}
	if !filepath.IsAbs(filename) {
		return toolErrorResultString(ctx, CodeInvalidArguments, "image path must be absolute"), nil
	}
	data, err := readLocalImageFile(filename)
	if err != nil {
		return toolErrorResultString(ctx, CodeInvalidArguments, fmt.Sprintf("cannot read %s: %v", filename, err)), nil
	}
	result, err := encodeImageForModel(data)
	if err != nil {
		return toolErrorResultString(ctx, CodeInvalidArguments, fmt.Sprintf("cannot read %s as an image: %v", filename, err)), nil
	}
	result.Path = filename
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func parseReadImageArgs(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "{") {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		trimmed = strings.TrimSpace(args.Path)
	}
	if trimmed == "" {
		return "", errors.New("path is required")
	}
	return trimmed, nil
}

// readLocalImageFile confines each open to an allowed root. A nonblocking
// open prevents a file replaced by a FIFO from stalling the Agent.
func readLocalImageFile(path string) ([]byte, error) {
	share := sharedFilesDir()
	rootPath := share
	rel, shared := strings.CutPrefix(path, share+"/")
	if !shared {
		rootPath = "/tmp"
		var ok bool
		rel, ok = strings.CutPrefix(path, "/tmp/")
		if !ok || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("image path must be inside /tmp or %s", share)
		}
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	const flags = os.O_RDONLY | syscall.O_NONBLOCK
	var file *os.File
	if shared {
		file, err = openSharedFileWithFlags(root, rel, flags)
	} else {
		before, statErr := root.Stat(rel)
		if statErr != nil {
			return nil, statErr
		}
		if !before.Mode().IsRegular() {
			return nil, errors.New("not a regular file")
		}
		file, err = root.OpenFile(rel, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > readImageMaxFileBytes {
		return nil, fmt.Errorf("file is %d bytes; the limit is %d", info.Size(), readImageMaxFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, readImageMaxFileBytes+1))
	if len(data) > readImageMaxFileBytes {
		return nil, fmt.Errorf("file exceeds the %d byte limit", readImageMaxFileBytes)
	}
	return data, err
}

// encodeImageForModel decodes an image, scales it so its longer side is at
// most readImageMaxEdge, and re-encodes it as JPEG. An image already within the
// bound in a format every provider accepts is passed through unchanged.
func encodeImageForModel(data []byte) (readImageResult, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return readImageResult{}, errors.New("unsupported or corrupt image; supported formats are PNG, JPEG, and GIF")
	}
	if config.Width <= 0 || config.Height <= 0 {
		return readImageResult{}, errors.New("image has no pixels")
	}
	if config.Width > readImageMaxPixels/config.Height {
		return readImageResult{}, fmt.Errorf("image is %dx%d; the limit is %d pixels", config.Width, config.Height, readImageMaxPixels)
	}
	result := readImageResult{SourceFormat: format}
	result.SourceWidth, result.SourceHeight = config.Width, config.Height

	// Decode before anything is sent: DecodeConfig reads only the header, so
	// a file truncated after it would otherwise pass as a valid image.
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return readImageResult{}, errors.New("unsupported or corrupt image; supported formats are PNG, JPEG, and GIF")
	}
	// A GIF's first frame may cover only part of its logical canvas. Place it
	// at its offset so the margins and aspect ratio survive scaling.
	canvas := image.Rect(0, 0, config.Width, config.Height)
	if img.Bounds() != canvas {
		img = placeOnCanvas(img, canvas)
	}

	width, height := scaledImageSize(config.Width, config.Height, readImageMaxEdge)
	if width == config.Width && height == config.Height && (format == "jpeg" || format == "png") {
		result.Width, result.Height = width, height
		result.Format = format
		result.Size = len(data)
		result.Data = base64.StdEncoding.EncodeToString(data)
		return result, nil
	}

	scaled := scaleImageBox(img, width, height)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, scaled, &jpeg.Options{Quality: readImageJPEGQuality}); err != nil {
		return readImageResult{}, err
	}
	result.Width, result.Height = width, height
	result.Format = "jpeg"
	result.Size = out.Len()
	result.Data = base64.StdEncoding.EncodeToString(out.Bytes())
	return result, nil
}

func scaledImageSize(width, height, maxEdge int) (int, int) {
	longer := max(width, height)
	if longer <= maxEdge {
		return width, height
	}
	return max(1, width*maxEdge/longer), max(1, height*maxEdge/longer)
}

// scaleImageBox averages each destination pixel over the source pixels it
// covers, flattening transparency onto white so it does not turn black in
// JPEG. Box filtering keeps text in a downscaled photo or chart legible.
func scaleImageBox(src image.Image, width, height int) *image.RGBA {
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		y0 := bounds.Min.Y + y*srcH/height
		y1 := max(y0+1, bounds.Min.Y+(y+1)*srcH/height)
		for x := range width {
			x0 := bounds.Min.X + x*srcW/width
			x1 := max(x0+1, bounds.Min.X+(x+1)*srcW/width)
			var r, g, b, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					// Composite premultiplied color over white.
					white := 0xffff - uint64(ca)
					r += uint64(cr) + white
					g += uint64(cg) + white
					b += uint64(cb) + white
					n++
				}
			}
			dst.SetRGBA(x, y, color.RGBA{R: uint8(r / n >> 8), G: uint8(g / n >> 8), B: uint8(b / n >> 8), A: 0xff})
		}
	}
	return dst
}

// readImageObservationCaption introduces a read_image result. It names the
// file and states that it is not the connected display, so the model does not
// take touch coordinates from it.
func readImageObservationCaption(observation string) string {
	var result readImageResult
	_ = json.Unmarshal([]byte(observation), &result)
	caption := fmt.Sprintf("This image is the file %s returned by the read_image tool. It is not the connected display; do not take touch_gesture coordinates from it.", result.Path)
	if result.SourceWidth > 0 && (result.SourceWidth != result.Width || result.SourceHeight != result.Height) {
		caption += fmt.Sprintf(" The original is %dx%d px, shown scaled to %dx%d px.", result.SourceWidth, result.SourceHeight, result.Width, result.Height)
	}
	return caption
}

// readImageToolContent is the tool result kept in the transcript for a
// read_image call: the image itself travels in the follow-up message, so the
// base64 payload is dropped here.
func readImageToolContent(observation string) string {
	var result readImageResult
	if err := json.Unmarshal([]byte(observation), &result); err != nil {
		return observation
	}
	encoded, _ := json.Marshal(map[string]any{
		"ok":     true,
		"path":   result.Path,
		"width":  result.Width,
		"height": result.Height,
		"format": result.Format,
	})
	return string(encoded)
}

// placeOnCanvas draws img at its own bounds on a transparent canvas, so the
// area outside a partial frame stays empty instead of being stretched over.
func placeOnCanvas(img image.Image, canvas image.Rectangle) image.Image {
	dst := image.NewNRGBA(canvas)
	bounds := img.Bounds().Intersect(canvas)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			dst.Set(x, y, img.At(x, y))
		}
	}
	return dst
}
