package mnk

import (
	"aico-agent/internal/agent/screen"
)

// ProviderFactory 用于根据配置创建合适的 Provider
type ProviderFactory struct {
	screenState *screen.ScreenState
}

// NewProviderFactory 创建 Provider 工厂
func NewProviderFactory(screenState *screen.ScreenState) *ProviderFactory {
	return &ProviderFactory{
		screenState: screenState,
	}
}

// CreateHIDProvider builds an HID Provider that opens new device paths.
// Production should prefer CreateHIDProviderWithDevices to share FDs with isolation.
// The error return is reserved for future validation; current construction always succeeds.
func (f *ProviderFactory) CreateHIDProvider(
	pointerDevice string,
	keyboardDevice string,
	androidKeyboardDevice string,
	touchscreen bool,
	keyboardLayout string,
) (Provider, error) {
	pointerDev := NewHIDDevice(pointerDevice)
	keyboardDev := NewHIDDevice(keyboardDevice)
	var androidKeyboardDev Device
	if androidKeyboardDevice != "" {
		androidKeyboardDev = NewHIDDevice(androidKeyboardDevice)
	}
	return f.CreateHIDProviderWithDevices(pointerDev, keyboardDev, androidKeyboardDev, touchscreen, keyboardLayout, nil)
}

// CreateHIDProviderWithDevices builds an HID provider from already-owned devices and an optional ProfileGate.
// The error return is reserved for future validation; current construction always succeeds.
func (f *ProviderFactory) CreateHIDProviderWithDevices(
	pointerDev, keyboardDev, androidKeyboardDev Device,
	touchscreen bool,
	keyboardLayout string,
	gate ProfileGate,
) (Provider, error) {
	return NewHIDProvider(
		pointerDev,
		keyboardDev,
		androidKeyboardDev,
		f.screenState,
		touchscreen,
		keyboardLayout,
		gate,
	), nil
}

// CreateADBProvider builds an ADB Provider.
// The error return is reserved for future validation; current construction always succeeds.
func (f *ProviderFactory) CreateADBProvider() (Provider, error) {
	provider := NewADBProvider(f.screenState, nil, nil)
	return provider, nil
}

// CreateHTTPProvider 创建 HTTP Provider（用于远程 HTTP 控制）
func (f *ProviderFactory) CreateHTTPProvider(baseURL string, taskID string) (Provider, error) {
	provider := NewHTTPProvider(HTTPProviderConfig{
		BaseURL: baseURL,
		TaskID:  taskID,
	})
	return provider, nil
}

// ProviderConfig 配置结构
type ProviderConfig struct {
	// Backend 类型: "hid", "adb", 或 "http"
	Backend string

	// HID 配置
	PointerDevice         string
	KeyboardDevice        string
	AndroidKeyboardDevice string
	Touchscreen           bool
	KeyboardLayout        string

	// HTTP 配置
	HTTPBaseURL string
	HTTPTaskID  string
}

// CreateProvider 根据配置创建 Provider
func (f *ProviderFactory) CreateProvider(config ProviderConfig) (Provider, error) {
	switch config.Backend {
	case "hid":
		return f.CreateHIDProvider(
			config.PointerDevice,
			config.KeyboardDevice,
			config.AndroidKeyboardDevice,
			config.Touchscreen,
			config.KeyboardLayout,
		)
	case "adb":
		return f.CreateADBProvider()
	case "http":
		return f.CreateHTTPProvider(config.HTTPBaseURL, config.HTTPTaskID)
	default:
		// 默认使用 HID
		return f.CreateHIDProvider(
			config.PointerDevice,
			config.KeyboardDevice,
			config.AndroidKeyboardDevice,
			config.Touchscreen,
			config.KeyboardLayout,
		)
	}
}
