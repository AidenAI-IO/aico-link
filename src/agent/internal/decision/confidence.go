package decision

// NoulConfident reports whether a yes probability is decisive at min: at or
// above min means yes, at or below 1-min means no, anything between is not
// decided. A noul answer carries no separate confidence.
func NoulConfident(p, min float64) (yes bool, confident bool) {
	switch {
	case p >= min:
		return true, true
	case p <= 1-min:
		return false, true
	default:
		return p >= 0.5, false
	}
}

// Confident reports whether a choice or box answer reaches min.
func (a Answer) Confident(min float64) bool {
	return a.Confidence != nil && *a.Confidence >= min
}

// ConfidenceValue is the answer's confidence, or its yes probability for a
// noul, for logging.
func (a Answer) ConfidenceValue() float64 {
	switch {
	case a.Confidence != nil:
		return *a.Confidence
	case a.Noul != nil:
		return *a.Noul
	}
	return 0
}

// Center1000 returns the box centre in the 0..1000 normalized space the
// agent's touch tools use.
func (b Box) Center1000() (float64, float64) {
	return (b.Left + b.Right) / 2 * 1000, (b.Top + b.Bottom) / 2 * 1000
}

// Valid reports whether the box is well formed and inside the image.
func (b Box) Valid() bool {
	return b.Left >= 0 && b.Top >= 0 && b.Right <= 1 && b.Bottom <= 1 && b.Left < b.Right && b.Top < b.Bottom
}
