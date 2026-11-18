package agent

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"strconv"

	"aico-agent/internal/devicecli"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Bound the log and legend together, rather than silently omitting operations
// from the image. The screenshot count and UTG log have independent limits.
const maxDeviceCLIActions = 64

type deviceCLIAction struct {
	name       string
	points     [2]image.Point
	pointCount int
	failed     bool
}

func newDeviceCLIAction(command string, call devicecli.Call) deviceCLIAction {
	action := deviceCLIAction{name: command}
	// Record coordinates from the validated HTTP payload, never from shell text.
	// A composite drag logs the dispatched phases separately, so an aborted drag
	// cannot show a release that was never sent.
	if command == "drag" {
		if call.Input["type"] == "drag_start" {
			action.name = "drag-start"
		} else {
			action.name = "drag-release"
		}
	}
	switch call.Tool {
	case "touch_gesture":
		for _, field := range []string{"point", "start", "end"} {
			if p, ok := call.Input[field].(map[string]int); ok && action.pointCount < len(action.points) {
				action.points[action.pointCount] = image.Pt(p["x"], p["y"])
				action.pointCount++
			}
		}
	case "mouse_move":
		x, xOK := call.Input["x"].(int)
		y, yOK := call.Input["y"].(int)
		if xOK && yOK {
			action.points[0] = image.Pt(x, y)
			action.pointCount = 1
		}
	}
	return action
}

func (a deviceCLIAction) label(number int) string {
	label := fmt.Sprintf("%02d %s", number, a.name)
	for _, p := range a.points[:a.pointCount] {
		label += fmt.Sprintf(" %d,%d", p.X, p.Y)
	}
	if a.failed {
		label += " [failed]"
	}
	return label
}

// Annotate a presentation copy only. The original frame, screen dimensions,
// and UTG image signature stay untouched. The ledger contains no text inputs,
// URLs, keyboard payloads, or other non-coordinate arguments.
func annotateDeviceCLIFrame(v visualScreenshotObservation, actions []deviceCLIAction) (visualScreenshotObservation, string) {
	src, _, err := image.Decode(bytes.NewReader(v.ImageBytes))
	if err != nil || src.Bounds().Empty() || len(actions) == 0 {
		return v, ""
	}
	screen := image.Rect(0, 0, src.Bounds().Dx(), src.Bounds().Dy())
	face := basicfont.Face7x13
	const padding, rowHeight, headerHeight = 8, 16, 24
	labels := make([]string, len(actions))
	colWidth := 0
	for i, action := range actions {
		labels[i] = action.label(i + 1)
		colWidth = max(colWidth, font.MeasureString(face, labels[i]).Ceil()+2*padding)
	}
	const heading = "DEVICE CLI / ACTIONS IN ORDER"
	// Keep the device frame readable on long chains and the composite within
	// the same height budget as plain CLI frames.
	minColumns := (len(actions) + 31) / 32
	fitScreen := func(columns int) (image.Rectangle, int) {
		rows := (len(actions) + columns - 1) / columns
		ledgerHeight := headerHeight + rows*rowHeight + padding
		height := min(screen.Dy(), 1080-ledgerHeight)
		return image.Rect(0, 0, max(1, screen.Dx()*height/screen.Dy()), height), ledgerHeight
	}
	columns := minColumns
	for candidate := minColumns + 1; candidate <= min(len(actions), screen.Dx()/colWidth); candidate++ {
		area, _ := fitScreen(candidate)
		if candidate*colWidth <= area.Dx() {
			columns = candidate
		}
	}
	fitted, ledgerHeight := fitScreen(columns)
	screen = fitted
	width := max(screen.Dx(), columns*colWidth, font.MeasureString(face, heading).Ceil()+2*padding)
	annotated := image.NewRGBA(image.Rect(0, 0, width, screen.Dy()+ledgerHeight))
	dark := image.NewUniform(color.RGBA{19, 25, 32, 255})
	light := image.NewUniform(color.RGBA{255, 255, 255, 255})
	draw.Draw(annotated, annotated.Bounds(), dark, image.Point{}, draw.Src)
	if screen.Size() == src.Bounds().Size() {
		draw.Draw(annotated, screen, src, src.Bounds().Min, draw.Src)
	} else {
		xdraw.ApproxBiLinear.Scale(annotated, screen, src, src.Bounds(), draw.Src, nil)
	}
	d := font.Drawer{Dst: annotated, Src: light, Face: face}
	d.Dot = fixed.P(padding, screen.Dy()+16)
	d.DrawString(heading)
	for i, label := range labels {
		d.Dot = fixed.P(padding+i%columns*colWidth, screen.Dy()+headerHeight+11+i/columns*rowHeight)
		d.DrawString(label)
	}
	layer := annotated.SubImage(screen).(*image.RGBA)
	const markRadius = 8
	positions := make([]image.Point, len(actions))
	badges := make([]image.Rectangle, 0, len(actions))
	pixel := func(p image.Point) image.Point {
		return image.Pt((p.X*(screen.Dx()-1)+500)/1000, (p.Y*(screen.Dy()-1)+500)/1000)
	}
	for i, a := range actions {
		if a.pointCount == 0 {
			continue
		}
		start := pixel(a.points[0])
		end := start
		if a.pointCount == 2 {
			end = pixel(a.points[1])
			drawDeviceCLIArrow(layer, start, end)
		}
		if a.name == "drag-release" && i > 0 && actions[i-1].name == "drag-start" && !actions[i-1].failed && actions[i-1].pointCount > 0 {
			drawDeviceCLIArrow(layer, pixel(actions[i-1].points[0]), end)
		}
		drawMarkerRing(layer, start.X, start.Y, markRadius, 2, dark.C)
		drawMarkerRing(layer, start.X, start.Y, markRadius-2, 2, light.C)
		positions[i] = end
	}
	// Draw numbers above all lines and try nearby slots for repeated taps.
	d.Dst = layer
	for i, a := range actions {
		if a.pointCount == 0 {
			continue
		}
		p := positions[i]
		label := strconv.Itoa(i + 1)
		w, h := font.MeasureString(face, label).Ceil()+6, 15
		badge := image.Rectangle{}
	findSlot:
		for distance := markRadius + 2; distance <= markRadius+62; distance += 16 {
			for _, offset := range []image.Point{{X: distance, Y: -h}, {X: -distance - w, Y: -h}, {X: distance, Y: 0}, {X: -distance - w, Y: 0}, {X: -w / 2, Y: distance}, {X: -w / 2, Y: -distance - h}} {
				origin := p.Add(offset)
				origin.X = max(0, min(origin.X, screen.Max.X-w))
				origin.Y = max(0, min(origin.Y, screen.Max.Y-h))
				candidate := image.Rectangle{Min: origin, Max: origin.Add(image.Pt(w, h))}.Intersect(screen)
				if badge.Empty() {
					badge = candidate
				}
				overlaps := false
				for _, used := range badges {
					if candidate.Overlaps(used) {
						overlaps = true
						break
					}
				}
				if !overlaps {
					badge = candidate
					break findSlot
				}
			}
		}
		badges = append(badges, badge)
		draw.Draw(layer, badge, light, image.Point{}, draw.Src)
		draw.Draw(layer, badge.Inset(1), dark, image.Point{}, draw.Src)
		d.Dot = fixed.P(badge.Min.X+3, badge.Min.Y+11)
		d.DrawString(label)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, annotated, &jpeg.Options{Quality: 86}); err != nil {
		return v, ""
	}
	v.ImageBytes = out.Bytes()
	v.MIMEType = "image/jpeg"
	// Keep the normalized coordinate target unambiguous when the ledger changes
	// the overall image aspect ratio. Preserve stability/change status too.
	status := v.Result
	status.Width, status.Height = 0, 0
	caption := fmt.Sprintf(" Device screen: top-left %d×%d px of this %d×%d px image. Normalize against only that region; do not include margins or the action ledger. Numbered marks and arrows are action history, not UI targets.", screen.Dx(), screen.Dy(), width, annotated.Bounds().Dy())
	if notes := screenshotObservationStatusSummary("shell", status); notes != "" {
		caption += " " + notes
	}
	return v, caption
}

func drawDeviceCLIArrow(img *image.RGBA, from, to image.Point) {
	dx, dy := float64(to.X-from.X), float64(to.Y-from.Y)
	length := math.Hypot(dx, dy)
	if length == 0 {
		return
	}
	ux, uy := dx/length, dy/length
	wing := func(sign float64) image.Point {
		return image.Pt(to.X+int(math.Round(-9*ux-sign*5*uy)), to.Y+int(math.Round(-9*uy+sign*5*ux)))
	}
	for _, stroke := range []struct {
		radius int
		ink    color.RGBA
	}{{2, color.RGBA{19, 25, 32, 255}}, {0, color.RGBA{255, 255, 255, 255}}} {
		for _, line := range [][2]image.Point{{from, to}, {to, wing(1)}, {to, wing(-1)}} {
			delta := line[1].Sub(line[0])
			n := max(absCLI(delta.X), absCLI(delta.Y), 1)
			for i := 0; i <= n; i++ {
				p := line[0].Add(image.Pt(delta.X*i/n, delta.Y*i/n))
				r := stroke.radius
				for y := p.Y - r; y <= p.Y+r; y++ {
					for x := p.X - r; x <= p.X+r; x++ {
						img.SetRGBA(x, y, stroke.ink)
					}
				}
			}
		}
	}
}

func absCLI(x int) int { return max(x, -x) }
