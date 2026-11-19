package agent

import (
	"bytes"
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

// Bound the number of operations dispatched by one shell call.
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

// renderDeviceCLISheet composes annotated presentation copies of captured frames.
func renderDeviceCLISheet(frames []deviceCLIFrame, actions []deviceCLIAction) ([]byte, image.Rectangle) {
	type panel struct {
		src    image.Image
		action int
	}
	var panels []panel
	var cell image.Point
	for _, frame := range frames {
		src, _, err := image.Decode(bytes.NewReader(frame.visual.ImageBytes))
		if err != nil || src.Bounds().Empty() {
			continue
		}
		panels = append(panels, panel{src, frame.action})
		cell.X = max(cell.X, src.Bounds().Dx())
		cell.Y = max(cell.Y, src.Bounds().Dy())
	}
	if len(panels) == 0 {
		return nil, image.Rectangle{}
	}
	const gutter = 4
	// Pick the grid that preserves the most detail within the image budget;
	// break ties with the least empty space. Portrait chains usually use one row.
	cols, rows, bestScale := 1, len(panels), 0.0
	for c := 1; c <= len(panels); c++ {
		r := (len(panels) + c - 1) / c
		scale := min(1.0, float64(1920-gutter*(c-1))/float64(cell.X*c), float64(1080-gutter*(r-1))/float64(cell.Y*r))
		if scale > bestScale || (scale == bestScale && c*r < cols*rows) {
			cols, rows, bestScale = c, r, scale
		}
	}
	cellSize := image.Pt(max(1, int(float64(cell.X)*bestScale)), max(1, int(float64(cell.Y)*bestScale)))
	sheet := image.NewRGBA(image.Rect(0, 0, cellSize.X*cols+gutter*(cols-1), cellSize.Y*rows+gutter*(rows-1)))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.RGBA{19, 25, 32, 255}), image.Point{}, draw.Src)
	var rect image.Rectangle
	for i, panel := range panels {
		origin := image.Pt(i%cols*(cellSize.X+gutter), i/cols*(cellSize.Y+gutter))
		size := panel.src.Bounds().Size()
		w, h := max(1, int(float64(size.X)*bestScale)), max(1, int(float64(size.Y)*bestScale))
		rect = image.Rectangle{Min: origin, Max: origin.Add(image.Pt(w, h))}
		if size == rect.Size() {
			draw.Draw(sheet, rect, panel.src, panel.src.Bounds().Min, draw.Src)
		} else {
			xdraw.ApproxBiLinear.Scale(sheet, rect, panel.src, panel.src.Bounds(), draw.Src, nil)
		}
		if panel.action >= 0 && panel.action < len(actions) {
			action := actions[panel.action]
			if action.name == "drag-release" && action.pointCount == 1 && panel.action > 0 {
				start := actions[panel.action-1]
				if start.name == "drag-start" && start.pointCount == 1 && !start.failed {
					action.points = [2]image.Point{start.points[0], action.points[0]}
					action.pointCount = 2
				}
			}
			markDeviceCLIPanel(sheet.SubImage(rect).(*image.RGBA), action, panel.action+1)
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, sheet, &jpeg.Options{Quality: 86}); err != nil {
		return nil, image.Rectangle{}
	}
	return out.Bytes(), rect
}

func markDeviceCLIPanel(img *image.RGBA, action deviceCLIAction, number int) {
	bounds := img.Bounds()
	dark := color.RGBA{19, 25, 32, 255}
	light := color.RGBA{255, 255, 255, 255}
	origin := bounds.Min.Add(image.Pt(4, 4))
	if action.pointCount > 0 {
		pixel := func(p image.Point) image.Point {
			return bounds.Min.Add(image.Pt((p.X*(bounds.Dx()-1)+500)/1000, (p.Y*(bounds.Dy()-1)+500)/1000))
		}
		start := pixel(action.points[0])
		end := start
		if action.pointCount == 2 {
			end = pixel(action.points[1])
			drawDeviceCLIArrow(img, start, end)
		}
		drawMarkerRing(img, start.X, start.Y, 8, 2, dark)
		drawMarkerRing(img, start.X, start.Y, 6, 2, light)
		origin = end.Add(image.Pt(9, -15))
	}
	label := strconv.Itoa(number)
	if action.failed {
		label += "!"
	}
	face := basicfont.Face7x13
	w, h := font.MeasureString(face, label).Ceil()+6, 15
	origin.X = max(bounds.Min.X, min(origin.X, bounds.Max.X-w))
	origin.Y = max(bounds.Min.Y, min(origin.Y, bounds.Max.Y-h))
	badge := image.Rectangle{Min: origin, Max: origin.Add(image.Pt(w, h))}
	draw.Draw(img, badge, image.NewUniform(light), image.Point{}, draw.Src)
	draw.Draw(img, badge.Inset(1), image.NewUniform(dark), image.Point{}, draw.Src)
	d := font.Drawer{Dst: img, Src: image.NewUniform(light), Face: face, Dot: fixed.P(origin.X+3, origin.Y+11)}
	d.DrawString(label)
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
