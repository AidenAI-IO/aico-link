package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"

	"aico-agent/internal/devicecli"
)

func saveScreenshot(data string, opts devicecli.LookOptions) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", fmt.Errorf("decode screenshot data: %w", err)
	}
	if opts.Region != nil {
		screen, err := jpeg.Decode(bytes.NewReader(raw))
		if err != nil {
			return "", fmt.Errorf("decode screenshot JPEG: %w", err)
		}
		b, region := screen.Bounds(), *opts.Region
		// Round outward so even a small nonempty normalized rectangle has pixels.
		area := image.Rect(region.Min.X*b.Dx()/1000, region.Min.Y*b.Dy()/1000,
			(region.Max.X*b.Dx()+999)/1000, (region.Max.Y*b.Dy()+999)/1000).Add(b.Min)
		crop := image.NewRGBA(image.Rectangle{Max: area.Size()})
		draw.Draw(crop, crop.Bounds(), screen, area.Min, draw.Src)
		var cropped bytes.Buffer
		if err := jpeg.Encode(&cropped, crop, &jpeg.Options{Quality: 90}); err != nil {
			return "", fmt.Errorf("encode cropped screenshot: %w", err)
		}
		raw = cropped.Bytes()
	} else if _, err := jpeg.DecodeConfig(bytes.NewReader(raw)); err != nil {
		return "", fmt.Errorf("invalid screenshot JPEG: %w", err)
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "device-cli-*.jpg")
	if err != nil {
		return "", fmt.Errorf("create screenshot file: %w", err)
	}
	_, err = file.Write(raw)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("save screenshot: %w", err)
	}
	return file.Name(), nil
}
