package agent

import (
	"image"
)

// A block that keeps changing across this many consecutive frame comparisons
// is treated as continuous motion (video, carousel, clock hand, spinner).
const stableMotionPersistentComparisons = 3

// stableMotionMaxIgnoredRatio caps how much of the screen continuous motion
// may cover and still be ignored. A page transition changes most blocks in
// every frame; ignoring it would report a half-finished transition as stable.
const stableMotionMaxIgnoredRatio = 0.2

// stableQuietBlockRatio is the share of grid blocks, outside ignored motion,
// that may change between two frames while the screen still counts as quiet.
// It matches the structural progress threshold.
const stableQuietBlockRatio = screenshotProgressChangedBlockThreshold

// lumaBlockMeans is the mean luminance of each progress-grid block inside a
// region, skipping the top status area like the progress comparison.
type lumaBlockMeans struct {
	columns, rows int
	means         []float64
}

func newLumaBlockMeans(frame lumaFrame, region image.Rectangle) lumaBlockMeans {
	region = region.Intersect(image.Rect(0, 0, frame.width, frame.height))
	region.Min.Y += int(float64(region.Dy()) * screenshotProgressIgnoreTopFraction)
	width, height := region.Dx(), region.Dy()
	if !frame.valid() || width <= 0 || height <= 0 {
		return lumaBlockMeans{}
	}
	columns := min(screenshotProgressGridColumns, width)
	rows := min(screenshotProgressGridRows, height)
	means := make([]float64, 0, columns*rows)
	for row := 0; row < rows; row++ {
		y0 := region.Min.Y + height*row/rows
		y1 := region.Min.Y + height*(row+1)/rows
		for column := 0; column < columns; column++ {
			x0 := region.Min.X + width*column/columns
			x1 := region.Min.X + width*(column+1)/columns
			sum, count := 0, 0
			for y := y0; y < y1; y += tapSequenceSampleStep {
				line := frame.y[y*frame.stride:]
				for x := x0; x < x1; x += tapSequenceSampleStep {
					sum += int(line[x])
					count++
				}
			}
			mean := 0.0
			if count > 0 {
				mean = float64(sum) / float64(count)
			}
			means = append(means, mean)
		}
	}
	return lumaBlockMeans{columns: columns, rows: rows, means: means}
}

func (m lumaBlockMeans) comparable(other lumaBlockMeans) bool {
	return len(m.means) > 0 && m.columns == other.columns && m.rows == other.rows && len(m.means) == len(other.means)
}

// stableMotionTracker decides frame-to-frame quiet while ignoring small areas
// of continuous motion, so a playing video tile does not hold every wait to
// its timeout while a transition across the screen is still waited out.
type stableMotionTracker struct {
	threshold float64
	streaks   []int
}

type stableMotionComparison struct {
	// changed counts changed blocks outside ignored motion; ignored counts
	// blocks treated as continuous motion; anyChanged includes both.
	changed, ignored, total int
	anyChanged              bool
	comparable              bool
}

func (c stableMotionComparison) quiet() bool {
	return c.comparable && float64(c.changed) <= stableQuietBlockRatio*float64(c.total)
}

func (c stableMotionComparison) changedRatio() float64 {
	if c.total == 0 {
		return 1
	}
	return float64(c.changed) / float64(c.total)
}

func (t *stableMotionTracker) compare(previous, current lumaBlockMeans) stableMotionComparison {
	if !previous.comparable(current) {
		t.streaks = nil
		return stableMotionComparison{}
	}
	if len(t.streaks) != len(current.means) {
		t.streaks = make([]int, len(current.means))
	}
	changed := make([]bool, len(current.means))
	persistent := 0
	result := stableMotionComparison{total: len(current.means), comparable: true}
	for i := range current.means {
		diff := current.means[i] - previous.means[i]
		if diff < 0 {
			diff = -diff
		}
		if diff > t.threshold {
			changed[i] = true
			result.anyChanged = true
			t.streaks[i]++
			if t.streaks[i] >= stableMotionPersistentComparisons {
				persistent++
			}
		} else {
			t.streaks[i] = 0
		}
	}
	ignoreMotion := float64(persistent) <= stableMotionMaxIgnoredRatio*float64(result.total)
	for i, blockChanged := range changed {
		switch {
		case ignoreMotion && t.streaks[i] >= stableMotionPersistentComparisons:
			result.ignored++
		case blockChanged:
			result.changed++
		}
	}
	return result
}
