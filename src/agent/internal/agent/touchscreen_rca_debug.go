package agent

import (
	"aico-agent/internal/logging"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

const touchscreenRCADebugEnv = "AICO_TOUCHSCREEN_RCA_DEBUG"

var touchscreenRCADebugEnabledValue atomic.Bool

func init() {
	touchscreenRCADebugEnabledValue.Store(touchscreenRCADebugEnabledFromEnv())
}

func touchscreenRCALogf(format string, args ...any) {
	if !touchscreenRCADebugEnabledCached() {
		return
	}
	message := fmt.Sprintf("[touchscreen-rca] "+format, args...)
	message = strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(message)
	logging.Debugf("agent", "touchscreen_rca_debug", "%s", message)
}

func touchscreenRCADebugEnabledCached() bool {
	return touchscreenRCADebugEnabledValue.Load()
}

func touchscreenRCADebugEnabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(touchscreenRCADebugEnv))) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func formatTouchscreenRCAMetadata(meta *frameMetadata) string {
	if !touchscreenRCADebugEnabledCached() {
		return ""
	}
	if meta == nil {
		return "<nil>"
	}
	return fmt.Sprintf(
		"{seq:%d width:%d height:%d source_width:%d source_height:%d crop_x:%d crop_y:%d crop_width:%d crop_height:%d pixel_format:%q stride:%d bytes:%d stale:%v}",
		meta.Seq,
		meta.Width,
		meta.Height,
		meta.SourceWidth,
		meta.SourceHeight,
		meta.CropX,
		meta.CropY,
		meta.CropWidth,
		meta.CropHeight,
		meta.PixelFormat,
		meta.Stride,
		meta.Bytes,
		meta.Stale,
	)
}

func touchscreenRCAPointerMode(pc *pointerController) string {
	if pc == nil {
		return "pointer=nil"
	}
	if pc.touchscreen {
		return "touchscreen"
	}
	return "absolute_mouse"
}

func formatTouchscreenRCAFloatPtr(value *float64) string {
	if value == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%.3f", *value)
}

func formatTouchscreenRCAQuickActionMappingSummary(tool *QuickActionTool) string {
	if !touchscreenRCADebugEnabledCached() {
		return ""
	}
	if tool == nil {
		return "quick_action=nil"
	}
	if tool.touch == nil {
		return "quick_action.touch=nil"
	}
	// Note: screen field removed after MNK Provider integration
	return "quick_action.touch (MNK Provider)"
}
