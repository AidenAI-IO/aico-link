package agent

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"

	"aiden-agent/internal/agent/screenprovider"
)

func convertFrameToRGB(meta *frameMetadata, frame []byte) ([]byte, error) {
	w := int(meta.Width)
	h := int(meta.Height)
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("invalid dimensions: %dx%d", w, h)
	}

	pixels := w * h
	rgb := make([]byte, pixels*3)

	switch meta.PixelFormat {
	case "jpeg", "jpg", "png":
		return decodeEncodedImageToRGB(meta.PixelFormat, frame, w, h)
	case "nv12", "nv16":
		isNV12 := meta.PixelFormat == "nv12"
		yPlaneSize := pixels
		uvPlaneSize := pixels / 2
		if !isNV12 {
			uvPlaneSize = pixels
		}
		if len(frame) < yPlaneSize+uvPlaneSize {
			return nil, fmt.Errorf("frame too small for %s: %d < %d", meta.PixelFormat, len(frame), yPlaneSize+uvPlaneSize)
		}
		yPlane := frame[:yPlaneSize]
		uvPlane := frame[yPlaneSize:]
		for y := 0; y < h; y++ {
			uvRow := y
			if isNV12 {
				uvRow = y / 2
			}
			for x := 0; x < w; x++ {
				yIdx := y*w + x
				uvIdx := uvRow*w + (x & ^1)
				yuvToRGB(yPlane[yIdx], uvPlane[uvIdx], uvPlane[uvIdx+1], rgb[yIdx*3:])
			}
		}

	case "yuyv", "uyvy":
		if w%2 != 0 || len(frame) < pixels*2 {
			return nil, fmt.Errorf("frame too small for %s: %d < %d", meta.PixelFormat, len(frame), pixels*2)
		}
		src := 0
		dst := 0
		for src+4 <= len(frame) && dst+6 <= len(rgb) {
			var y0, u, y1, v byte
			if meta.PixelFormat == "uyvy" {
				u, y0, v, y1 = frame[src], frame[src+1], frame[src+2], frame[src+3]
			} else {
				y0, u, y1, v = frame[src], frame[src+1], frame[src+2], frame[src+3]
			}
			src += 4
			yuvToRGB(y0, u, v, rgb[dst:])
			yuvToRGB(y1, u, v, rgb[dst+3:])
			dst += 6
		}

	default:
		return nil, fmt.Errorf("unsupported pixel format: %s", meta.PixelFormat)
	}

	return rgb, nil
}

func decodeEncodedImageToRGB(format string, frame []byte, width, height int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", format, err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != width || bounds.Dy() != height {
		return nil, fmt.Errorf("%s dimensions mismatch: decoded %dx%d, metadata %dx%d", format, bounds.Dx(), bounds.Dy(), width, height)
	}

	rgb := make([]byte, width*height*3)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			r, g, b, _ := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			offset := (y*width + x) * 3
			rgb[offset] = byte(r >> 8)
			rgb[offset+1] = byte(g >> 8)
			rgb[offset+2] = byte(b >> 8)
		}
	}
	return rgb, nil
}

func yuvToRGB(y, u, v byte, dst []byte) {
	c := int(y) - 16
	if c < 0 {
		c = 0
	}
	d := int(u) - 128
	e := int(v) - 128
	c298 := c * 298
	dst[0] = clampByte((c298 + 409*e + 128) >> 8)
	dst[1] = clampByte((c298 - 100*d - 208*e + 128) >> 8)
	dst[2] = clampByte((c298 + 516*d + 128) >> 8)
}

func clampByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// encodeJPEG encodes RGB data to JPEG at the given quality (1-100).
func encodeJPEG(rgb []byte, w, h int, quality int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 3
			img.SetRGBA(x, y, color.RGBA{R: rgb[i], G: rgb[i+1], B: rgb[i+2], A: 255})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("jpeg encode: %w", err)
	}
	return buf.Bytes(), nil
}

func encodeFrameAsJPEG(meta *frameMetadata, frame []byte, quality int) ([]byte, error) {
	if meta == nil {
		return nil, fmt.Errorf("missing frame metadata")
	}
	if quality <= 0 {
		quality = screenshotJPEGQuality
	}
	if meta.PixelFormat == "jpeg" || meta.PixelFormat == "jpg" {
		return append([]byte(nil), frame...), nil
	}
	if meta.PixelFormat == "png" {
		return screenprovider.EncodeImageToJPEG(frame, quality)
	}

	rgb, err := convertFrameToRGB(meta, frame)
	if err != nil {
		return nil, err
	}
	return encodeJPEG(rgb, int(meta.Width), int(meta.Height), quality)
}
