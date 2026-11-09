package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"strconv"
)

// uiGroundingElement is one Set-of-Mark element reported to the model, in
// screenshot pixels: the coordinate space touch_gesture takes.
type uiGroundingElement struct {
	ID     int    `json:"id"`
	Center [2]int `json:"center"`
	Box    [4]int `json:"box"`
}

// groundedScreenshotOutput is the screenshot tool's action output when
// grounding was requested. It is the tool content the model reads.
type groundedScreenshotOutput struct {
	OK             bool                 `json:"ok"`
	Elements       []uiGroundingElement `json:"elements"`
	GroundingError string               `json:"grounding_error,omitempty"`
}

var somColors = []color.RGBA{
	{0x00, 0xE5, 0xFF, 0xFF},
	{0xFF, 0xD7, 0x40, 0xFF},
	{0xFF, 0x6B, 0xD6, 0xFF},
	{0x80, 0xFF, 0x72, 0xFF},
}

// groundScreenshot detects interface components on a JPEG screenshot and
// returns the JPEG with numbered Set-of-Mark boxes drawn on it, plus the
// elements in the same pixel space and numbering.
func groundScreenshot(ctx context.Context, grounder uiGrounder, jpegData []byte) ([]byte, []uiGroundingElement, error) {
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return nil, nil, fmt.Errorf("decode screenshot: %w", err)
	}
	canvas := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(canvas, canvas.Bounds(), img, img.Bounds().Min, draw.Src)
	width, height := canvas.Bounds().Dx(), canvas.Bounds().Dy()

	rgb := make([]byte, width*height*3)
	for i, j := 0, 0; i < len(canvas.Pix); i, j = i+4, j+3 {
		copy(rgb[j:j+3], canvas.Pix[i:i+3])
	}
	boxes, err := grounder.Detect(ctx, rgb, width, height)
	if err != nil {
		return nil, nil, err
	}

	elements := make([]uiGroundingElement, len(boxes))
	rects := make([]image.Rectangle, len(boxes))
	for i, box := range boxes {
		rect := image.Rect(
			int(math.Floor(box.X1)), int(math.Floor(box.Y1)),
			int(math.Ceil(box.X2)), int(math.Ceil(box.Y2)),
		).Intersect(canvas.Bounds())
		rects[i] = rect
		elements[i] = uiGroundingElement{
			ID:     i + 1,
			Center: [2]int{int(math.Round((box.X1 + box.X2) / 2)), int(math.Round((box.Y1 + box.Y2) / 2))},
			Box:    [4]int{rect.Min.X, rect.Min.Y, rect.Max.X, rect.Max.Y},
		}
	}
	// Outlines first, then tags, so later outlines do not cross earlier labels.
	for i, rect := range rects {
		drawSoMOutline(canvas, rect, somColors[i%len(somColors)])
	}
	for i, rect := range rects {
		drawSoMTag(canvas, rect, strconv.Itoa(i+1), somColors[i%len(somColors)])
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: screenshotJPEGQuality}); err != nil {
		return nil, nil, fmt.Errorf("encode grounded screenshot: %w", err)
	}
	return out.Bytes(), elements, nil
}

func drawSoMOutline(img *image.RGBA, rect image.Rectangle, c color.RGBA) {
	const thickness = 2
	if rect.Empty() {
		return
	}
	for t := 0; t < thickness; t++ {
		inner := rect.Inset(t)
		if inner.Empty() {
			return
		}
		for x := inner.Min.X; x < inner.Max.X; x++ {
			img.SetRGBA(x, inner.Min.Y, c)
			img.SetRGBA(x, inner.Max.Y-1, c)
		}
		for y := inner.Min.Y; y < inner.Max.Y; y++ {
			img.SetRGBA(inner.Min.X, y, c)
			img.SetRGBA(inner.Max.X-1, y, c)
		}
	}
}

// somDigits is a 3x5 bitmap font; each row's three low bits are its pixels.
var somDigits = [10][5]uint8{
	{7, 5, 5, 5, 7}, {2, 6, 2, 2, 7}, {7, 1, 7, 4, 7}, {7, 1, 7, 1, 7}, {5, 5, 7, 1, 1},
	{7, 4, 7, 1, 7}, {7, 4, 7, 5, 7}, {7, 1, 1, 1, 1}, {7, 5, 7, 5, 7}, {7, 5, 7, 1, 7},
}

// drawSoMTag draws the element number at the box's inner top-left corner,
// shrinking it to fit small boxes and moving it above a box too small to
// hold even the smallest tag.
func drawSoMTag(img *image.RGBA, rect image.Rectangle, label string, c color.RGBA) {
	if rect.Empty() {
		return
	}
	const padding = 1
	scale := 3
	tagSize := func(scale int) (int, int) {
		return len(label)*4*scale - scale + 2*padding, 5*scale + 2*padding
	}
	w, h := tagSize(scale)
	for scale > 1 && (w > rect.Dx()-4 || h > rect.Dy()-4) {
		scale--
		w, h = tagSize(scale)
	}
	origin := rect.Min.Add(image.Pt(2, 2))
	if w > rect.Dx()-4 || h > rect.Dy()-4 {
		origin = image.Pt(rect.Min.X, rect.Min.Y-h)
	}
	bounds := img.Bounds()
	origin.X = min(max(origin.X, bounds.Min.X), bounds.Max.X-w)
	origin.Y = min(max(origin.Y, bounds.Min.Y), bounds.Max.Y-h)
	tag := image.Rect(origin.X, origin.Y, origin.X+w, origin.Y+h).Intersect(bounds)
	draw.Draw(img, tag, image.NewUniform(c), image.Point{}, draw.Src)

	ink := color.RGBA{0x11, 0x11, 0x11, 0xFF}
	x := origin.X + padding
	for _, r := range label {
		glyph := somDigits[r-'0']
		for row := 0; row < 5; row++ {
			for col := 0; col < 3; col++ {
				if glyph[row]&(4>>col) == 0 {
					continue
				}
				dot := image.Rect(x+col*scale, origin.Y+padding+row*scale, x+(col+1)*scale, origin.Y+padding+(row+1)*scale)
				draw.Draw(img, dot.Intersect(bounds), image.NewUniform(ink), image.Point{}, draw.Src)
			}
		}
		x += 4 * scale
	}
}

func marshalGroundedScreenshotOutput(elements []uiGroundingElement, groundingErr error) string {
	output := groundedScreenshotOutput{OK: true, Elements: elements}
	if output.Elements == nil {
		output.Elements = []uiGroundingElement{}
	}
	if groundingErr != nil {
		output.GroundingError = groundingErr.Error()
	}
	data, _ := json.Marshal(output)
	return string(data)
}
