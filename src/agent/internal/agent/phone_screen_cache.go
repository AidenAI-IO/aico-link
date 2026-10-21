package agent

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aiden-agent/internal/agent/screen"
	"aiden-agent/internal/logging"
)

const phoneScreenCacheFileName = "phone_screen.json"

// phoneScreenCache remembers the last phone screen dimensions the companion
// app reported, with the capture frame size they mapped onto. The live report
// is scoped to one HID connection and kept only in memory, so an Agent restart
// while the app is closed would otherwise leave screenshots to black-border
// detection, which crops dark app content (a black Safari page) as border.
//
// An entry is written as soon as the app reports, with the frame size of the
// latest capture (filled in by the next capture when none happened yet). It is
// used only when no live report exists, the device type matches, and the frame
// has the same size; any live report replaces it.
//
// Without any report the entry is learned from black-border detection, but
// only from crops whose cut edges lie on the screen edge (a light screen), so
// a dark screen later still crops to the right aspect ratio. Only the aspect
// ratio matters: the frame service derives the orientation from each frame.
type phoneScreenCache struct {
	mu           sync.Mutex
	path         string
	loaded       bool
	entry        phoneScreenCacheEntry
	deviceTypeFn func() string
	// lastFrameWidth/Height are the latest capture frame size seen.
	lastFrameWidth, lastFrameHeight int
	// candidate and candidateCount track verified crops not yet confirmed.
	candidate      [2]int
	candidateCount int
	reprobeCount   int
}

// Entry sources: a report from the companion app, or black-border detection
// with its edges verified on the screen.
const (
	phoneScreenSourceReported = "reported"
	phoneScreenSourceDetected = "detected"
)

// phoneScreenDetectConfirmations is how many identical verified crops in a
// row confirm a detected entry, so one transitional frame is not learned.
const phoneScreenDetectConfirmations = 2

// phoneScreenReprobeEvery is how often a capture that uses a remembered entry
// also detects again, so an entry from another phone can be corrected.
const phoneScreenReprobeEvery = 20

// phoneScreenAspectTolerance is the relative aspect ratio difference below
// which a verified crop is taken to show the remembered screen.
const phoneScreenAspectTolerance = 0.03

// Phone screens are between about 4:3 and 21:9 (long side over short side).
const (
	phoneScreenMinAspect = 1.3
	phoneScreenMaxAspect = 2.35
)

type phoneScreenCacheEntry struct {
	Source      string `json:"source,omitempty"`
	DeviceType  string `json:"device_type"`
	FrameWidth  int    `json:"frame_width"`
	FrameHeight int    `json:"frame_height"`
	// Screen holds the reported dimensions, or for a detected entry the crop
	// size in frame pixels, of which only the aspect ratio is meaningful.
	Screen    screen.PhoneScreenInfo `json:"screen"`
	UpdatedAt time.Time              `json:"updated_at"`
}

var _ screen.PhoneScreenFallback = (*phoneScreenCache)(nil)

// newPhoneScreenCache stores the entry in the Agent config directory. Without
// a directory it returns nil and no fallback is used.
func newPhoneScreenCache(configDir string, deviceTypeFn func() string) *phoneScreenCache {
	if configDir == "" {
		return nil
	}
	return &phoneScreenCache{path: filepath.Join(configDir, phoneScreenCacheFileName), deviceTypeFn: deviceTypeFn}
}

func (c *phoneScreenCache) deviceTypeLocked() string {
	if c.deviceTypeFn == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.deviceTypeFn()))
}

func (c *phoneScreenCache) loadLocked() {
	if c.loaded {
		return
	}
	c.loaded = true
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var entry phoneScreenCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		logging.Warnf("agent", "screenshot", "ignore invalid %s: %v", c.path, err)
		return
	}
	c.entry = entry
}

// Remember stores a live report after it mapped onto a frame. It writes only
// when something changed, so screenshots do not rewrite the file.
func (c *phoneScreenCache) Remember(info screen.PhoneScreenInfo, frameWidth, frameHeight int) {
	if c == nil || !phoneScreenHasDimensions(info) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" {
		return
	}
	if frameWidth <= 0 || frameHeight <= 0 {
		frameWidth, frameHeight = c.lastFrameWidth, c.lastFrameHeight
	} else {
		c.lastFrameWidth, c.lastFrameHeight = frameWidth, frameHeight
	}
	c.loadLocked()
	entry := phoneScreenCacheEntry{
		Source:      phoneScreenSourceReported,
		DeviceType:  c.deviceTypeLocked(),
		FrameWidth:  frameWidth,
		FrameHeight: frameHeight,
		Screen:      info,
	}
	if current := c.entry; current.DeviceType == entry.DeviceType && current.FrameWidth == frameWidth &&
		current.FrameHeight == frameHeight && phoneScreenInfoEqual(current.Screen, info) {
		return
	}
	c.storeLocked(entry)
}

func (c *phoneScreenCache) storeLocked(entry phoneScreenCacheEntry) {
	entry.UpdatedAt = time.Now().UTC()
	c.entry = entry
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logging.Warnf("agent", "screenshot", "remember phone screen: %v", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		logging.Warnf("agent", "screenshot", "remember phone screen: %v", err)
	}
}

// ObserveCrop learns the screen aspect ratio from crops the frame service
// detected without a hint. Only a crop whose cut edges lie on the screen edge
// is trusted: on a dark page detection crops into the content, so such crops
// are ignored and never replace an entry. A trusted crop replaces the entry
// (reported or detected) only when its aspect ratio differs, as after plugging
// in another phone; the orientation does not matter.
func (c *phoneScreenCache) ObserveCrop(cropWidth, cropHeight, frameWidth, frameHeight int, edgesOnScreen bool) {
	if c == nil || !edgesOnScreen || cropWidth <= 0 || cropHeight <= 0 || frameWidth <= 0 || frameHeight <= 0 {
		return
	}
	aspect := phoneScreenAspect(float64(cropWidth), float64(cropHeight))
	if aspect < phoneScreenMinAspect || aspect > phoneScreenMaxAspect {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" {
		return
	}
	c.lastFrameWidth, c.lastFrameHeight = frameWidth, frameHeight
	c.loadLocked()
	deviceType := c.deviceTypeLocked()
	current := c.entry
	if current.DeviceType == deviceType && current.FrameWidth == frameWidth && current.FrameHeight == frameHeight {
		if w, h, ok := entryScreenDimensions(current.Screen); ok &&
			math.Abs(phoneScreenAspect(w, h)-aspect) <= phoneScreenAspectTolerance*aspect {
			c.candidateCount = 0
			return
		}
	}
	if c.candidate == [2]int{cropWidth, cropHeight} {
		c.candidateCount++
	} else {
		c.candidate, c.candidateCount = [2]int{cropWidth, cropHeight}, 1
	}
	if c.candidateCount < phoneScreenDetectConfirmations {
		return
	}
	w, h := cropWidth, cropHeight
	c.storeLocked(phoneScreenCacheEntry{
		Source:      phoneScreenSourceDetected,
		DeviceType:  deviceType,
		FrameWidth:  frameWidth,
		FrameHeight: frameHeight,
		Screen:      screen.PhoneScreenInfo{WidthPixels: &w, HeightPixels: &h},
	})
	c.candidateCount = 0
}

// phoneScreenAspect is the long side over the short side.
func phoneScreenAspect(width, height float64) float64 {
	return max(width, height) / min(width, height)
}

func entryScreenDimensions(info screen.PhoneScreenInfo) (float64, float64, bool) {
	if w, h, ok := currentPhoneScreenDimensions(info); ok {
		return w, h, true
	}
	return nativePhoneScreenDimensions(info)
}

// ShouldReprobe counts captures that use a remembered entry and asks for a
// fresh detection every phoneScreenReprobeEvery of them.
func (c *phoneScreenCache) ShouldReprobe() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reprobeCount++
	return c.reprobeCount%phoneScreenReprobeEvery == 0
}

// NoteFrame records a capture frame size and completes an entry written before
// any capture had happened.
func (c *phoneScreenCache) NoteFrame(frameWidth, frameHeight int) {
	if c == nil || frameWidth <= 0 || frameHeight <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastFrameWidth, c.lastFrameHeight = frameWidth, frameHeight
	if c.path == "" {
		return
	}
	c.loadLocked()
	if entry := c.entry; entry.FrameWidth == 0 && phoneScreenHasDimensions(entry.Screen) {
		entry.FrameWidth, entry.FrameHeight = frameWidth, frameHeight
		c.storeLocked(entry)
	}
}

// Lookup returns the remembered dimensions for a frame of this size on the
// current device type.
func (c *phoneScreenCache) Lookup(frameWidth, frameHeight int) (screen.PhoneScreenInfo, bool) {
	if c == nil {
		return screen.PhoneScreenInfo{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" {
		return screen.PhoneScreenInfo{}, false
	}
	c.loadLocked()
	entry := c.entry
	if entry.FrameWidth != frameWidth || entry.FrameHeight != frameHeight || entry.DeviceType != c.deviceTypeLocked() {
		return screen.PhoneScreenInfo{}, false
	}
	if _, _, ok := currentPhoneScreenDimensions(entry.Screen); !ok {
		if _, _, ok := nativePhoneScreenDimensions(entry.Screen); !ok {
			return screen.PhoneScreenInfo{}, false
		}
	}
	return entry.Screen, true
}

// LookupForDevice returns the remembered dimensions for the current device
// type and the frame size they were cropped from, for callers that learn the
// frame size only after capturing.
func (c *phoneScreenCache) LookupForDevice() (screen.PhoneScreenInfo, int, int, bool) {
	if c == nil {
		return screen.PhoneScreenInfo{}, 0, 0, false
	}
	c.mu.Lock()
	if c.path == "" {
		c.mu.Unlock()
		return screen.PhoneScreenInfo{}, 0, 0, false
	}
	c.loadLocked()
	frameWidth, frameHeight := c.entry.FrameWidth, c.entry.FrameHeight
	c.mu.Unlock()
	info, ok := c.Lookup(frameWidth, frameHeight)
	return info, frameWidth, frameHeight, ok
}

func phoneScreenInfoEqual(a, b screen.PhoneScreenInfo) bool {
	left, err1 := json.Marshal(a)
	right, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(left) == string(right)
}
