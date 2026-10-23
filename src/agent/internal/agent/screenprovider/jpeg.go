package screenprovider

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
)

// EncodeImageToJPEG re-encodes a PNG or JPEG image as JPEG. A non-positive
// quality uses DefaultJPEGQuality.
func EncodeImageToJPEG(frame []byte, quality int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil, fmt.Errorf("decode encoded image: %w", err)
	}
	if quality <= 0 {
		quality = DefaultJPEGQuality
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("jpeg encode: %w", err)
	}
	return buf.Bytes(), nil
}
