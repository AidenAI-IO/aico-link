package decision

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"strings"
)

// System One image limits. Images outside them are rejected, not resized, so
// screenshots are scaled here first.
const (
	maxImageSide   = 1568
	minImageSide   = 280
	maxImagePixels = 2_458_624
	jpegQuality    = 85
)

// Image is a prepared asset and the key that references it.
type Image struct {
	Ref    string
	Asset  Asset
	Width  int
	Height int
}

// PrepareJPEG decodes a base64 JPEG and scales it down to System One's limits.
// It never upscales: an image with a side below the minimum is an error, as
// the API would reject it anyway.
func PrepareJPEG(data string) (Image, error) {
	data = strings.TrimSpace(data)
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return Image{}, fmt.Errorf("decode screenshot: %w", err)
	}
	// A screenshot already within the limits is sent as is: decoding and
	// re-encoding a phone-sized JPEG costs about 250 ms on the board's CPU.
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw)); err == nil {
		if w, h := fitImage(cfg.Width, cfg.Height); w == cfg.Width && h == cfg.Height {
			if w < minImageSide || h < minImageSide {
				return Image{}, fmt.Errorf("screenshot %dx%d is below the %dpx minimum side", w, h, minImageSide)
			}
			ref, err := newRef()
			if err != nil {
				return Image{}, err
			}
			return Image{Ref: ref, Asset: Asset{Type: "image", DataBase64: data, Format: "jpg"}, Width: w, Height: h}, nil
		}
	}
	src, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return Image{}, fmt.Errorf("decode screenshot JPEG: %w", err)
	}
	bounds := src.Bounds()
	width, height := fitImage(bounds.Dx(), bounds.Dy())
	if width < minImageSide || height < minImageSide {
		return Image{}, fmt.Errorf("screenshot %dx%d is below the %dpx minimum side", width, height, minImageSide)
	}
	out := src
	if width != bounds.Dx() || height != bounds.Dy() {
		out = scaleDown(src, width, height)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return Image{}, fmt.Errorf("encode screenshot JPEG: %w", err)
	}
	ref, err := newRef()
	if err != nil {
		return Image{}, err
	}
	return Image{
		Ref:    ref,
		Asset:  Asset{Type: "image", DataBase64: base64.StdEncoding.EncodeToString(buf.Bytes()), Format: "jpg"},
		Width:  width,
		Height: height,
	}, nil
}

// fitImage returns the largest size within the side and pixel limits that
// keeps the aspect ratio.
func fitImage(width, height int) (int, int) {
	scale := 1.0
	if longest := max(width, height); longest > maxImageSide {
		scale = float64(maxImageSide) / float64(longest)
	}
	for float64(width)*scale*float64(height)*scale > maxImagePixels {
		scale *= 0.98
	}
	if scale == 1 {
		return width, height
	}
	return max(1, int(float64(width)*scale)), max(1, int(float64(height)*scale))
}

// scaleDown averages up to 2x2 samples of each destination pixel's source
// area. Screenshots shrink by well under 2x here, so that matches a full box
// filter while bounding the work on the board's CPU, with no image dependency.
func scaleDown(src image.Image, width, height int) image.Image {
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	sw, sh := bounds.Dx(), bounds.Dy()
	for y := 0; y < height; y++ {
		y0 := bounds.Min.Y + y*sh/height
		y1 := max(y0+1, bounds.Min.Y+(y+1)*sh/height)
		ystep := max(1, (y1-y0+1)/2)
		for x := 0; x < width; x++ {
			x0 := bounds.Min.X + x*sw/width
			x1 := max(x0+1, bounds.Min.X+(x+1)*sw/width)
			xstep := max(1, (x1-x0+1)/2)
			var r, g, b, n uint64
			for sy := y0; sy < y1; sy += ystep {
				for sx := x0; sx < x1; sx += xstep {
					cr, cg, cb, _ := src.At(sx, sy).RGBA()
					r, g, b, n = r+uint64(cr), g+uint64(cg), b+uint64(cb), n+1
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i] = uint8(r / n >> 8)
			dst.Pix[i+1] = uint8(g / n >> 8)
			dst.Pix[i+2] = uint8(b / n >> 8)
			dst.Pix[i+3] = 0xff
		}
	}
	return dst
}

// newRef returns a random asset key. A UUID-shaped key cannot collide with
// ordinary words in the instructions, which the API would also replace.
func newRef() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("asset reference: %w", err)
	}
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
