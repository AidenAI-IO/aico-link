package agent

import (
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aiden-agent/internal/agent/agentpath"
	"aiden-agent/internal/agent/mnk"
	"aiden-agent/internal/agent/model"
	"aiden-agent/internal/agent/screen"
	"aiden-agent/internal/agent/screenprovider"

	langtools "github.com/tmc/langchaingo/tools"
)

// ToolSet is a fixed collection of built-in tools, keyed by name.
type ToolSet struct {
	tools                map[string]langtools.Tool
	screen               *screen.ScreenState
	screenProvider       screenprovider.Provider
	mnkProvider          mnk.Provider // MNK Provider for input control
	phoneBridge          *PhoneBridge
	phoneBridgeRestorer  *PhoneBridgeRestorer
	textInputHW          *textInputHardwareDeps
	iosKeyboardIsolation *iosKeyboardIsolationController
	// Environment bridges own the target device and cannot toggle the local
	// board's USB gadget profile, so bridge input bypasses local iOS isolation.
	iosKeyboardIsolationOptional bool
	searchOpenTool               *appSearchOpenTool
	skillInstallClient           *http.Client
}

type runtimeDeviceTypeConfigurable interface {
	SetDeviceTypeFunc(func() string)
}

// NewBuiltinToolSet returns all built-in tools. Tools are not configurable;
// everything is registered here with its runtime dependencies already wired up.
type BuiltinToolSetOption func(*builtinToolSetOptions)

type builtinToolSetOptions struct {
	waitForWakeupController     *WaitForWakeupController
	screenStable                ScreenStableDefaults
	screenState                 *screen.ScreenState
	screenProvider              screenprovider.Provider
	mnkProvider                 mnk.Provider
	shellTemporaryDirectory     string
	disableIOSKeyboardIsolation bool
}

func WithWaitForWakeupController(controller *WaitForWakeupController) BuiltinToolSetOption {
	return func(options *builtinToolSetOptions) {
		options.waitForWakeupController = controller
	}
}

func WithScreenStableDefaults(defaults ScreenStableDefaults) BuiltinToolSetOption {
	return func(options *builtinToolSetOptions) {
		options.screenStable = defaults
	}
}

func WithShellTemporaryDirectory(dir string) BuiltinToolSetOption {
	return func(options *builtinToolSetOptions) {
		options.shellTemporaryDirectory = dir
	}
}

func withIOSKeyboardIsolationDisabled(options *builtinToolSetOptions) {
	options.disableIOSKeyboardIsolation = true
}

func shouldEnableIOSKeyboardIsolation(cfg Config) bool {
	return !(cfg.EnvironmentBridge.Enabled && strings.TrimSpace(cfg.EnvironmentBridge.Endpoint) != "")
}

// WithScreenState makes the tools publish visual observations to a shared
// ScreenState. When omitted, a private state is created for backwards
// compatibility with callers that only need the tool set itself.
func WithScreenState(state *screen.ScreenState) BuiltinToolSetOption {
	return func(options *builtinToolSetOptions) {
		options.screenState = state
	}
}

func WithScreenProvider(provider screenprovider.Provider) BuiltinToolSetOption {
	return func(options *builtinToolSetOptions) {
		options.screenProvider = provider
	}
}

func WithMNKProvider(provider mnk.Provider) BuiltinToolSetOption {
	return func(options *builtinToolSetOptions) {
		options.mnkProvider = provider
	}
}

func NewBuiltinToolSet(hidCfg HIDConfig, audioCfg AudioConfig, searchCfg SearchConfig, proxyCfg ProxyConfig, options ...BuiltinToolSetOption) *ToolSet {
	return newHardwareToolSet(hidCfg, audioCfg, searchCfg, proxyCfg, options...)
}

func NewBuiltinToolSetFromConfig(cfg Config, proxyCfg ProxyConfig, options ...BuiltinToolSetOption) *ToolSet {
	cfg.EnvironmentBridge.Endpoint = strings.TrimSpace(cfg.EnvironmentBridge.Endpoint)
	defaultOptions := make([]BuiltinToolSetOption, 0, len(options)+2)
	if cfg.EnvironmentBridge.Enabled && cfg.EnvironmentBridge.Endpoint != "" {
		defaultOptions = append(defaultOptions,
			WithScreenProvider(screenprovider.NewHTTP(cfg.EnvironmentBridge.Endpoint, cfg.EnvironmentBridge.BenchmarkTaskID)),
			WithMNKProvider(mnk.NewHTTPProvider(mnk.HTTPProviderConfig{
				BaseURL: cfg.EnvironmentBridge.Endpoint,
				TaskID:  cfg.EnvironmentBridge.BenchmarkTaskID,
			})),
		)
	}
	if !shouldEnableIOSKeyboardIsolation(cfg) {
		defaultOptions = append(defaultOptions, withIOSKeyboardIsolationDisabled)
	}
	options = append(defaultOptions, options...)
	return newHardwareToolSet(cfg.HIDConfigForDevice(), cfg.Audio, cfg.Search, proxyCfg, options...)
}

func screenProviderFromRuntime(runtime *Runtime) screenprovider.Provider {
	if runtime != nil && runtime.toolSnapshot() != nil {
		if provider := runtime.toolSnapshot().ScreenProvider(); provider != nil {
			return provider
		}
	}
	socketPath := ""
	if runtime != nil {
		socketPath = runtime.ConfigSnapshot().HID.FrameSocketOrDefault()
	}
	return NewScreenCaptureClient(socketPath)
}

func newHardwareToolSet(hidCfg HIDConfig, audioCfg AudioConfig, searchCfg SearchConfig, proxyCfg ProxyConfig, options ...BuiltinToolSetOption) *ToolSet {
	toolOptions := builtinToolSetOptions{}
	for _, option := range options {
		if option != nil {
			option(&toolOptions)
		}
	}

	kbDev := NewHIDDevice(hidCfg.KeyboardDeviceOrDefault())
	androidKbDev := NewHIDDevice(hidCfg.AndroidKeyboardDeviceOrDefault())
	screen := toolOptions.screenState
	if screen == nil {
		screen = newToolScreenState()
	}
	pointer := newPointerController(hidCfg)
	var iosKeyboardIsolation *iosKeyboardIsolationController
	if !toolOptions.disableIOSKeyboardIsolation {
		iosKeyboardIsolation = newIOSKeyboardIsolationController(hidCfg, kbDev, pointer.dev, androidKbDev)
	}
	pointer.iosKeyboardIsolation = iosKeyboardIsolation
	var adbInput *ADBInputController
	if hidCfg.InputBackendADB() {
		adbInput = NewADBInputController(screen)
	}

	mnkProvider := toolOptions.mnkProvider
	if mnkProvider == nil {
		// Local HID path reuses the same device FDs as isolation.
		mnkFactory := mnk.NewProviderFactory(screen)
		var mnkErr error
		if hidCfg.InputBackendADB() {
			mnkProvider, mnkErr = mnkFactory.CreateADBProvider()
		} else {
			mnkProvider, mnkErr = mnkFactory.CreateHIDProviderWithDevices(
				asMNKDevice(pointer.dev),
				asMNKDevice(kbDev),
				asMNKDevice(androidKbDev),
				hidCfg.PointerModeOrDefault() == "touchscreen",
				hidCfg.KeyboardLayoutOrDefault(),
				newIOSKeyboardIsolationProfileGate(iosKeyboardIsolation),
			)
		}
		if mnkErr != nil {
			touchscreenRCALogf("WARNING: failed to create MNK provider: %v; keyboard/pointer tools will report module unavailable", mnkErr)
			mnkProvider = nil
		}
	}

	touchscreenRCALogf(
		"newHardwareToolSet pointer_mode=%q pointer_device=%q keyboard_device=%q keyboard_layout=%q android_keyboard_device=%q frame_socket=%q",
		hidCfg.PointerModeOrDefault(),
		hidCfg.MouseDeviceOrDefault(),
		hidCfg.KeyboardDeviceOrDefault(),
		hidCfg.KeyboardLayoutOrDefault(),
		hidCfg.AndroidKeyboardDeviceOrDefault(),
		hidCfg.FrameSocketOrDefault(),
	)
	provider := toolOptions.screenProvider
	if provider == nil {
		provider = NewScreenCaptureClient(hidCfg.FrameSocketOrDefault())
	}
	screenshot := NewScreenshotTool(provider, screen)
	screenStable := toolOptions.screenStable.Resolved()
	waitStable := NewWaitStableScreenTool(provider, screenStable, screen)
	keyboardTap := &KeyboardTapTool{mnkProvider: mnkProvider}
	keyboardText := &KeyboardTextTool{dev: kbDev, adb: adbInput, keyboardLayout: hidCfg.KeyboardLayoutOrDefault(), iosKeyboardIsolation: iosKeyboardIsolation}
	keyboardText.textProvider, _ = mnkProvider.(mnk.TextProvider)
	touchGesture := &TouchGestureTool{
		mnkProvider: mnkProvider,
		screen:      screen,
		touchscreen: hidCfg.PointerTouchscreen(),
	}
	quickAction := &QuickActionTool{keyboard: keyboardTap, touch: touchGesture, iosKeyboardIsolation: iosKeyboardIsolation}
	textInputHW := &textInputHardwareDeps{
		pointerMode:  hidCfg.PointerModeOrDefault(),
		touchGesture: touchGesture,
		keyboardTap:  keyboardTap,
		keyboardText: keyboardText,
		quickAction:  quickAction,
		screenshot:   screenshot,
	}

	tools := map[string]langtools.Tool{
		"keyboard_tap":           newPostActionStableScreenshotTool(keyboardTap, waitStable, screenshot, postActionScreenshotDelay, screenStable),
		"mouse_move":             newPostActionStableScreenshotTool(&MouseMoveTool{mnkProvider: mnkProvider}, waitStable, screenshot, postActionScreenshotDelay, screenStable),
		"mouse_scroll":           newPostActionStableScreenshotTool(&MouseScrollTool{mnkProvider: mnkProvider}, waitStable, screenshot, postActionScreenshotDelay, screenStable),
		"touch_gesture":          newPostActionStableScreenshotTool(touchGesture, waitStable, screenshot, postActionScreenshotDelay, screenStable),
		"quick_action":           newPostActionStableScreenshotTool(quickAction, waitStable, screenshot, postActionScreenshotDelay, screenStable),
		"screenshot":             screenshot,
		"wait_for_stable_screen": waitStable,
		"audio_volume":           NewAudioVolumeTool(audioCfg.SocketOrDefault()),
		"shell": &ShellTool{execution: shellExecutionConfig{
			proxy:              proxyCfg,
			temporaryDirectory: toolOptions.shellTemporaryDirectory,
		}},
		"weather":     NewWeatherTool(proxyCfg),
		"web_search":  NewWebSearchTool(searchCfg, proxyCfg),
		"wikipedia":   NewWikipediaTool(proxyCfg),
		"web_scraper": NewWebScraperTool(proxyCfg),
	}
	if toolOptions.waitForWakeupController != nil {
		tools[toolWaitForWakeup] = NewWaitForWakeupTool(toolOptions.waitForWakeupController)
	}
	// Always register human handoff tool - no callback needed for non-blocking version
	tools["request_user_action"] = NewHumanHandoffTool()

	toolSet := &ToolSet{
		tools:                        tools,
		screen:                       screen,
		screenProvider:               provider,
		mnkProvider:                  mnkProvider,
		phoneBridgeRestorer:          NewPhoneBridgeRestorer(nil, pointer),
		textInputHW:                  textInputHW,
		iosKeyboardIsolation:         iosKeyboardIsolation,
		iosKeyboardIsolationOptional: toolOptions.disableIOSKeyboardIsolation,
		skillInstallClient:           newSkillInstallHTTPClient(proxyCfg),
	}
	touchGesture.primeScreenMapping = toolSet.PrimeScreenMapping
	return toolSet
}

func newToolScreenState() *screen.ScreenState {
	return &screen.ScreenState{}
}

func (s *ToolSet) RegisterEnterTextTool(models model.Model, deviceTypeFn func() string) {
	if s == nil || s.textInputHW == nil || models == nil {
		return
	}
	engine := newTextInputEngine(*s.textInputHW, newLLMTextInputVision(models))
	bridgeTool := &textInputBridge{
		hw:       s.textInputHW,
		vision:   newLLMTextInputVision(models),
		bridgeFn: func() *PhoneBridge { return s.phoneBridge },
		restorer: s.phoneBridgeRestorer,
	}
	entryTool := &EnterTextTool{
		engine:                          engine,
		bridgeTool:                      bridgeTool,
		iosKeyboardIsolation:            s.iosKeyboardIsolation,
		allowIOSKeyboardIsolationBypass: s.iosKeyboardIsolationOptional,
	}
	entryTool.SetDeviceTypeFunc(deviceTypeFn)
	searchOpenTool := &appSearchOpenTool{
		hw:                   s.textInputHW,
		vision:               newLLMTextInputVision(models),
		deviceTypeFn:         deviceTypeFn,
		entryTool:            entryTool,
		launchDelay:          appSearchOpenLaunchDelay,
		iosKeyboardIsolation: s.iosKeyboardIsolation,
	}
	s.searchOpenTool = searchOpenTool
	s.refreshOpenAppTool()
	s.tools["enter_text"] = newPostActionScreenshotTool(entryTool, s.textInputHW.screenshot, 300*time.Millisecond)
}

func (s *ToolSet) Get(name string) (langtools.Tool, bool) {
	if s == nil {
		return nil, false
	}
	t, ok := s.tools[name]
	return t, ok
}

func (s *ToolSet) ScreenProvider() screenprovider.Provider {
	if s == nil {
		return nil
	}
	return s.screenProvider
}

// MNKProvider returns the mouse/keyboard provider used by HID tools, if configured.
func (s *ToolSet) MNKProvider() mnk.Provider {
	if s == nil {
		return nil
	}
	return s.mnkProvider
}

func mnkProviderFromRuntime(runtime *Runtime) mnk.Provider {
	if runtime != nil && runtime.toolSnapshot() != nil {
		return runtime.toolSnapshot().MNKProvider()
	}
	return nil
}

func (s *ToolSet) All() []langtools.Tool {
	if s == nil {
		return nil
	}
	names := s.Names()
	result := make([]langtools.Tool, 0, len(names))
	for _, name := range names {
		if t, ok := s.tools[name]; ok {
			result = append(result, t)
		}
	}
	return result
}

func (s *ToolSet) Names() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *ToolSet) CurrentEnvironmentHints(maxAge time.Duration) CurrentEnvironmentHints {
	if s == nil || s.screen == nil {
		return CurrentEnvironmentHints{}
	}
	width, height, age, ok := s.screen.DimensionsWithAge()
	if !ok {
		return CurrentEnvironmentHints{}
	}
	if maxAge > 0 && age > maxAge {
		return CurrentEnvironmentHints{}
	}
	return CurrentEnvironmentHints{
		ScreenshotWidth:  width,
		ScreenshotHeight: height,
	}
}

func (s *ToolSet) UpdateDeviceEnvironment(env *PhoneEnvironment) {
	if s == nil || s.screen == nil {
		return
	}
	if env == nil {
		s.screen.ClearPhoneScreenInfo()
		return
	}
	s.screen.UpdatePhoneScreenInfo(env.Screen)
}

// RegisterMemoryTools wires the memory recall/write tools. configDir locates the
// ContextManager session folder that holds conversation chunks; memoryDir is the
// filesystem memory root for the long-term, device, and episode planes.
func (s *ToolSet) RegisterMemoryTools(configDir, memoryDir string, longTermStore *LongTermMemoryStore) {
	if memoryDir == "" {
		return
	}
	// Conversation chunks are written per ContextManager session at compaction
	// time; recall scans every session folder, so no session lineage is needed.
	multiSessionStore := NewMultiSessionChunkStore(agentpath.ContextManagerSessionFolder(configDir))
	if longTermStore == nil {
		longTermStore = NewLongTermMemoryStore(filepath.Join(memoryDir, "long_term"), WithLifecycleDir(filepath.Join(memoryDir, "lifecycle")))
	}
	temporaryStore := NewLongTermMemoryStore(filepath.Join(memoryDir, "temporary"), WithLifecycleDir(filepath.Join(memoryDir, "lifecycle")))
	deviceStore := NewDeviceMemoryStore(filepath.Join(memoryDir, "device"))
	episodeStore := NewTaskEpisodeStore(filepath.Join(memoryDir, "episodes"))
	s.tools["recall_session_chunks"] = NewRecallSessionChunksTool(multiSessionStore)
	s.tools["recall_memory"] = NewRecallMemoryToolWithTemporary(longTermStore, temporaryStore)
	s.tools["save_memory"] = NewSaveMemoryTool(longTermStore)
	s.tools["forget_memory"] = NewForgetMemoryToolWithTemporary(longTermStore, temporaryStore)
	s.tools["recall_device_memory"] = NewRecallDeviceMemoryTool(deviceStore)
	s.tools["inspect_episode"] = NewInspectEpisodeTool(episodeStore)
}

func (s *ToolSet) RegisterSkillTools(skillsDir, manifestPath string, onModify ...func()) {
	s.registerSkillTools(skillsDir, manifestPath, nil, onModify...)
}

func (s *ToolSet) RegisterSkillToolsWithDeviceType(skillsDir, manifestPath string, deviceTypeFn func() string, onModify ...func()) {
	s.registerSkillTools(skillsDir, manifestPath, deviceTypeFn, onModify...)
}

func (s *ToolSet) registerSkillTools(skillsDir, manifestPath string, deviceTypeFn func() string, onModify ...func()) {
	usagePath := usagePathForManifest(manifestPath)
	listTool := NewSkillListTool(skillsDir, usagePath)
	readTool := NewSkillReadTool(skillsDir, usagePath)
	listTool.SetDeviceTypeFunc(deviceTypeFn)
	readTool.SetDeviceTypeFunc(deviceTypeFn)
	s.tools["skill_list"] = listTool
	s.tools["skill_read"] = readTool
	manageTool := NewSkillManageTool(skillsDir, manifestPath, onModify...)
	if s.skillInstallClient != nil {
		manageTool.SetHTTPClient(s.skillInstallClient)
	}
	s.tools["skill_manage"] = manageTool
	s.tools["skill_mark_used"] = NewSkillMarkUsedTool(skillsDir, usagePath)
}

func (s *ToolSet) SetRuntimeDeviceTypeFn(deviceTypeFn func() string) {
	if s == nil {
		return
	}
	if s.textInputHW != nil {
		s.textInputHW.deviceTypeFn = deviceTypeFn
	}
	for _, name := range []string{"enter_text", "keyboard_tap", "quick_action", "screenshot", "touch_gesture", "wait_for_stable_screen"} {
		tool, ok := s.tools[name]
		if !ok {
			continue
		}
		if configurable, ok := tool.(runtimeDeviceTypeConfigurable); ok {
			configurable.SetDeviceTypeFunc(deviceTypeFn)
		}
	}
	if s.searchOpenTool != nil {
		s.searchOpenTool.SetDeviceTypeFunc(deviceTypeFn)
	}
	if tool, ok := s.tools["skill_list"].(*SkillListTool); ok {
		tool.SetDeviceTypeFunc(deviceTypeFn)
	}
	if tool, ok := s.tools["skill_read"].(*SkillReadTool); ok {
		tool.SetDeviceTypeFunc(deviceTypeFn)
	}
}

func usagePathForManifest(manifestPath string) string {
	if manifestPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(manifestPath), "usage.json")
}

func (s *ToolSet) closeInputDevices() {
	if s != nil {
		if closer, ok := s.mnkProvider.(interface{ Close() }); ok {
			closer.Close()
		}
	}
}
