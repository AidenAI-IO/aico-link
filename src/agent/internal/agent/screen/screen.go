package screen

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type PhoneScreenInfo struct {
	Width              *float64 `json:"width,omitempty"`
	Height             *float64 `json:"height,omitempty"`
	WidthPixels        *int     `json:"width_pixels,omitempty"`
	HeightPixels       *int     `json:"height_pixels,omitempty"`
	NativeWidthPixels  *int     `json:"native_width_pixels,omitempty"`
	NativeHeightPixels *int     `json:"native_height_pixels,omitempty"`
	Scale              *float64 `json:"scale,omitempty"`
	NativeScale        *float64 `json:"native_scale,omitempty"`
	Density            *float64 `json:"density,omitempty"`
	DensityDPI         *int     `json:"density_dpi,omitempty"`
	ScaledDensity      *float64 `json:"scaled_density,omitempty"`
}

type ScreenState struct {
	mu                   sync.RWMutex
	width                int
	height               int
	active               ScreenActiveArea
	phoneScreen          PhoneScreenInfo
	phoneScreenScope     string
	phoneScreenScopeFunc func() string
	updatedAt            time.Time
	screenshotJPEG       []byte
	screenshotWidth      int
	screenshotHeight     int
	screenshotUpdatedAt  time.Time
	screenshotGeneration uint64
	phoneScreenFallback  PhoneScreenFallback
}

// PhoneScreenFallback remembers reported phone screen dimensions across Agent
// restarts, for captures taken while no live report exists.
type PhoneScreenFallback interface {
	// Remember stores a report; a zero frame size means "the latest frame".
	Remember(info PhoneScreenInfo, frameWidth, frameHeight int)
	// NoteFrame records a capture frame size.
	NoteFrame(frameWidth, frameHeight int)
	// Lookup returns the dimensions remembered for this frame size.
	Lookup(frameWidth, frameHeight int) (PhoneScreenInfo, bool)
	// LookupForDevice returns the remembered dimensions and their frame size
	// (zero when no capture happened yet).
	LookupForDevice() (PhoneScreenInfo, int, int, bool)
	// ObserveCrop learns from a crop the frame service detected on its own;
	// edgesOnScreen reports that its cut edges lie on the phone screen edge.
	ObserveCrop(cropWidth, cropHeight, frameWidth, frameHeight int, edgesOnScreen bool)
	// ShouldReprobe reports whether a capture using a remembered entry should
	// also detect again without it, so a stale entry can be corrected.
	ShouldReprobe() bool
}

// SetPhoneScreenFallback installs the store used when no live report exists.
func (s *ScreenState) SetPhoneScreenFallback(fallback PhoneScreenFallback) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.phoneScreenFallback = fallback
	s.mu.Unlock()
}

// PhoneScreenFallback returns the installed store, or nil.
func (s *ScreenState) PhoneScreenFallback() PhoneScreenFallback {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phoneScreenFallback
}

type ScreenMappingState struct {
	Width     int
	Height    int
	Active    ScreenActiveArea
	UpdatedAt time.Time
}

// InvalidateCapture drops calibration and images from a retired capture/input
// backend. Phone-reported dimensions keep their existing connection scope.
func (s *ScreenState) InvalidateCapture() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.width, s.height = 0, 0
	s.active = ScreenActiveArea{}
	s.updatedAt = time.Time{}
	s.screenshotJPEG = nil
	s.screenshotWidth, s.screenshotHeight = 0, 0
	s.screenshotUpdatedAt = time.Time{}
	s.screenshotGeneration++
}

// ScreenActiveArea represents the mirrored phone touch region inside the
// captured HDMI frame. When the companion app reports the phone's original
// screen dimensions, this is the largest centered region in the frame with the
// same aspect ratio. Falling back to "visible non-black content" is only an
// approximation for when accurate phone screen info is unavailable.
type ScreenActiveArea struct {
	X      int  `json:"x"`
	Y      int  `json:"y"`
	Width  int  `json:"width"`
	Height int  `json:"height"`
	Valid  bool `json:"valid"`
}

func (s *ScreenState) Update(width, height int) {
	s.UpdateActiveArea(width, height, ScreenActiveArea{})
}

func (s *ScreenState) UpdatePhoneScreenInfo(info PhoneScreenInfo) {
	if s == nil {
		return
	}
	scope := s.currentPhoneScreenScope()
	s.mu.Lock()
	s.phoneScreen = info
	s.phoneScreenScope = scope
	s.mu.Unlock()
}

// SetPhoneScreenScopeFunc binds reported phone dimensions to a physical
// connection scope. Once the scope changes or becomes unavailable, callers no
// longer receive dimensions from the previous HID session.
func (s *ScreenState) SetPhoneScreenScopeFunc(fn func() string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.phoneScreenScopeFunc = fn
	s.mu.Unlock()
}

func (s *ScreenState) ClearPhoneScreenInfo() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.phoneScreen = PhoneScreenInfo{}
	s.phoneScreenScope = ""
	s.mu.Unlock()
}

func (s *ScreenState) PhoneScreenInfo() PhoneScreenInfo {
	if s == nil {
		return PhoneScreenInfo{}
	}
	s.mu.RLock()
	info := s.phoneScreen
	storedScope := s.phoneScreenScope
	scopeFunc := s.phoneScreenScopeFunc
	s.mu.RUnlock()
	if scopeFunc != nil {
		currentScope := strings.TrimSpace(scopeFunc())
		if storedScope == "" || currentScope == "" || currentScope != storedScope {
			return PhoneScreenInfo{}
		}
	}
	return info
}

func (s *ScreenState) currentPhoneScreenScope() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	fn := s.phoneScreenScopeFunc
	s.mu.RUnlock()
	if fn == nil {
		return ""
	}
	return strings.TrimSpace(fn())
}

func (s *ScreenState) UpdateActiveArea(width, height int, active ScreenActiveArea) {
	if width <= 0 || height <= 0 {
		return
	}
	if active.Valid {
		if active.X < 0 || active.Y < 0 || active.Width <= 0 || active.Height <= 0 || active.X+active.Width > width || active.Y+active.Height > height {
			active = ScreenActiveArea{}
		}
	}

	s.mu.Lock()
	s.width = width
	s.height = height
	s.active = active
	s.updatedAt = time.Now()
	s.mu.Unlock()
}

func (s *ScreenState) Dimensions() (width, height int, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.width <= 0 || s.height <= 0 {
		return 0, 0, false
	}
	return s.width, s.height, true
}

func (s *ScreenState) DimensionsWithAge() (width, height int, age time.Duration, ok bool) {
	width, height, _, age, ok = s.ActiveAreaWithAge()
	return width, height, age, ok
}

func (s *ScreenState) ActiveAreaWithAge() (width, height int, active ScreenActiveArea, age time.Duration, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.width <= 0 || s.height <= 0 {
		return 0, 0, ScreenActiveArea{}, 0, false
	}
	active = s.active
	if !active.Valid {
		active = ScreenActiveArea{X: 0, Y: 0, Width: s.width, Height: s.height, Valid: true}
	}
	if s.updatedAt.IsZero() {
		return s.width, s.height, active, 0, true
	}
	return s.width, s.height, active, time.Since(s.updatedAt), true
}

func (s *ScreenState) MappingState() ScreenMappingState {
	if s == nil {
		return ScreenMappingState{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ScreenMappingState{
		Width:     s.width,
		Height:    s.height,
		Active:    s.active,
		UpdatedAt: s.updatedAt,
	}
}

func (s *ScreenState) FreshActiveArea(maxAge time.Duration) bool {
	if s == nil {
		return false
	}
	_, _, active, age, ok := s.ActiveAreaWithAge()
	state := s.MappingState()
	if !ok || !active.Valid || state.UpdatedAt.IsZero() {
		return false
	}
	if maxAge > 0 && age >= maxAge {
		return false
	}
	return true
}

func (s *ScreenState) RestoreMappingState(state ScreenMappingState) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.width = state.Width
	s.height = state.Height
	s.active = state.Active
	s.updatedAt = state.UpdatedAt
}

func (s *ScreenState) UpdateScreenshot(jpegData []byte, width, height int) {
	if s == nil || len(jpegData) == 0 || width <= 1 || height <= 1 {
		return
	}
	copyData := append([]byte(nil), jpegData...)
	s.mu.Lock()
	s.screenshotJPEG = copyData
	s.screenshotWidth = width
	s.screenshotHeight = height
	s.screenshotUpdatedAt = time.Now()
	s.screenshotGeneration++
	s.mu.Unlock()
}

// ScreenshotDimensions returns the stored image's dimensions after cropping and
// capture-side scaling, without copying its bytes. Dimensions() instead returns
// the source frame used for input mapping. Capture invalidation clears both.
func (s *ScreenState) ScreenshotDimensions() (width, height int, ok bool) {
	if s == nil {
		return 0, 0, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.screenshotJPEG) == 0 || s.screenshotWidth <= 1 || s.screenshotHeight <= 1 || s.screenshotUpdatedAt.IsZero() {
		return 0, 0, false
	}
	return s.screenshotWidth, s.screenshotHeight, true
}

func (s *ScreenState) ScreenshotGeneration() uint64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.screenshotGeneration
}

func (s *ScreenState) LatestScreenshot(maxAge time.Duration) (jpegData []byte, width, height int, age time.Duration, ok bool) {
	if s == nil {
		return nil, 0, 0, 0, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.screenshotJPEG) == 0 || s.screenshotWidth <= 1 || s.screenshotHeight <= 1 || s.screenshotUpdatedAt.IsZero() {
		return nil, 0, 0, 0, false
	}
	age = time.Since(s.screenshotUpdatedAt)
	if maxAge > 0 && age >= maxAge {
		return nil, 0, 0, age, false
	}
	return append([]byte(nil), s.screenshotJPEG...), s.screenshotWidth, s.screenshotHeight, age, true
}

func (s *ScreenState) Format() string {
	if s == nil {
		return ""
	}
	width, height, active, age, ok := s.ActiveAreaWithAge()
	state := s.MappingState()
	phoneScreen := s.PhoneScreenInfo()
	phoneScreenText := phoneScreen.Format()

	rawAge := "unset"
	if !state.UpdatedAt.IsZero() {
		rawAge = fmt.Sprintf("%d", time.Since(state.UpdatedAt).Milliseconds())
	}
	effectiveAge := "unset"
	if ok {
		effectiveAge = fmt.Sprintf("%d", age.Milliseconds())
	}

	return fmt.Sprintf(
		"active_area_with_age_ok=%v effective_width=%d effective_height=%d effective_active=%s effective_age_ms=%s raw_width=%d raw_height=%d raw_active=%s raw_age_ms=%s phone_screen=%q",
		ok,
		width,
		height,
		active.Format(),
		effectiveAge,
		state.Width,
		state.Height,
		state.Active.Format(),
		rawAge,
		phoneScreenText,
	)
}

func (ps *PhoneScreenInfo) Format() string {
	parts := make([]string, 0, 8)
	if ps.Width != nil && ps.Height != nil {
		parts = append(parts, fmt.Sprintf("%.2fx%.2f pt/dp", *ps.Width, *ps.Height))
	}
	if ps.WidthPixels != nil && ps.HeightPixels != nil {
		parts = append(parts, fmt.Sprintf("%dx%d px", *ps.WidthPixels, *ps.HeightPixels))
	}
	if ps.NativeWidthPixels != nil && ps.NativeHeightPixels != nil {
		parts = append(parts, fmt.Sprintf("native=%dx%d px", *ps.NativeWidthPixels, *ps.NativeHeightPixels))
	}
	if ps.Scale != nil {
		parts = append(parts, fmt.Sprintf("scale=%.2f", *ps.Scale))
	}
	if ps.NativeScale != nil {
		parts = append(parts, fmt.Sprintf("native_scale=%.2f", *ps.NativeScale))
	}
	if ps.Density != nil {
		parts = append(parts, fmt.Sprintf("density=%.2f", *ps.Density))
	}
	if ps.DensityDPI != nil {
		parts = append(parts, fmt.Sprintf("density_dpi=%d", *ps.DensityDPI))
	}
	if ps.ScaledDensity != nil {
		parts = append(parts, fmt.Sprintf("scaled_density=%.2f", *ps.ScaledDensity))
	}
	return strings.Join(parts, ", ")
}

func (saa *ScreenActiveArea) Format() string {
	return fmt.Sprintf("{x:%d y:%d w:%d h:%d valid:%v}", saa.X, saa.Y, saa.Width, saa.Height, saa.Valid)
}
