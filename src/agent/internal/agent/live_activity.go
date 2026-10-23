package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aico-agent/internal/agent/speech"
)

const (
	LiveActivityStatusRunning   = "running"
	LiveActivityStatusReady     = "ready"
	LiveActivityStatusNeedsApp  = "needs_app"
	LiveActivityStatusCompleted = "completed"
	LiveActivityStatusFailed    = "failed"
	LiveActivityStatusCanceled  = "canceled"

	LiveActivityPhasePlanning    = "planning"
	LiveActivityPhaseObserving   = "observing"
	LiveActivityPhaseActing      = "acting"
	LiveActivityPhasePhoneBridge = "phone_bridge"
	LiveActivityPhaseWaitingApp  = "waiting_app"
	LiveActivityPhaseWaitingUser = "waiting_user"
	LiveActivityPhaseVerifying   = "verifying"
	LiveActivityPhaseAnswering   = "answering"

	liveActivityFinalStateRetention = 5 * time.Minute
	liveActivityLocalNotifyInterval = 750 * time.Millisecond
	liveActivityLocalNotifyTimeout  = time.Second
)

type LiveActivityState struct {
	RequestID     string     `json:"request_id"`
	PhoneID       string     `json:"phone_id,omitempty"`
	Locale        string     `json:"locale,omitempty"`
	Status        string     `json:"status"`
	Phase         string     `json:"phase,omitempty"`
	TaskTitle     string     `json:"task_title"`
	CurrentStep   string     `json:"current_step"`
	CurrentAction string     `json:"current_action,omitempty"`
	CurrentTarget string     `json:"current_target,omitempty"`
	CurrentApp    string     `json:"current_app,omitempty"`
	LastToolName  string     `json:"last_tool_name,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	ToolStatus    string     `json:"tool_status,omitempty"`
	ToolStartedAt *time.Time `json:"tool_started_at,omitempty"`
	Progress      float64    `json:"progress,omitempty"`
	ShowsProgress bool       `json:"shows_progress"`
	CanStop       bool       `json:"can_stop"`
	RequiresApp   bool       `json:"requires_app,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
}

type LiveActivityManager struct {
	disabled           atomic.Bool
	mu                 sync.Mutex
	states             map[string]LiveActivityState
	activeRequestID    string
	logger             *Logger
	locale             atomic.Value
	localNotifyMu      sync.RWMutex
	localNotifier      func(context.Context, string) error
	localNotifyQueue   chan struct{}
	localNotifyStarted bool
}

// liveActivityText defines localized strings for Live Activity display.
type liveActivityText struct {
	AicoTask                         string
	ProcessingRequest                string
	AnalyzingTask                    string
	PlanningNextStep                 string
	PleaseTakeOverOnPhone            string
	OpenCompanionAppToContinue       string
	LaunchRequestSentVerifyingScreen string
	UpdatingPlanFromUserInput        string
	Completed                        string
	Failed                           string
	Canceled                         string
	OpeningApp                       string
	OpeningAppWithTarget             string
	CreatingCalendarEvent            string
	CheckingCalendar                 string
	DeletingCalendarEvent            string
	UpdatingCalendar                 string
	CheckingContacts                 string
	CreatingContact                  string
	UpdatingContact                  string
	CheckingNotifications            string
	SendingNotification              string
	UsingClipboard                   string
	ReadingClipboard                 string
	WritingClipboard                 string
	PreparingAnswer                  string
	PlanningPrefix                   string
	Browser                          string
	Messages                         string
	Using                            string
	CheckingScreen                   string
	WaitingForScreen                 string
	OpeningLink                      string
	ControllingPhone                 string
	MovingPointer                    string
	Scrolling                        string
	TypingText                       string
	PressingKeys                     string
	UsingNotifications               string
	Searching                        string
	AdjustingAudio                   string
	CheckingInformation              string
	RecallingContext                 string
	UpdatingMemory                   string
	UsingSkills                      string
	WaitingForUserInput              string
	ScreenChecked                    string
	ScreenReady                      string
	AppOpened                        string
	LinkOpened                       string
	ActionSentCheckingResult         string
	FinishedPrefix                   string
	Finished                         string
	ProblemWhilePrefix               string
	ToolFailed                       string
	OpeningPrefix                    string
	OpeningWebpage                   string
	OpeningMessageComposer           string
	OpeningEmailComposer             string
	OpeningPhone                     string
	Mail                             string
	Phone                            string
	PreparingToSwitchApps            string
	PreparingToOpenTargetApp         string
	ActionSentWaitingScreenStable    string
	ReadingScreenAfterAction         string
}

func NewLiveActivityManager(cfg LiveActivityConfig, locale string, logger *Logger) *LiveActivityManager {
	if !cfg.EnabledOrDefault() {
		return nil
	}
	return newReloadableLiveActivityManager(cfg, locale, logger)
}

func newReloadableLiveActivityManager(cfg LiveActivityConfig, locale string, logger *Logger) *LiveActivityManager {
	manager := &LiveActivityManager{
		states: make(map[string]LiveActivityState),
		logger: logger,
	}
	manager.locale.Store(normalizeVoiceNotificationLocale(locale))
	manager.disabled.Store(!cfg.EnabledOrDefault())
	return manager
}

// currentLocale returns the manager's normalized locale ("" when unset), so the
// phone can localize its own Live Activity labels to match the agent's setting.
func (m *LiveActivityManager) currentLocale() string {
	if m == nil {
		return ""
	}
	locale, _ := m.locale.Load().(string)
	return locale
}

// localizeToolProgressContent converts hardcoded English progress messages to localized text.
func (m *LiveActivityManager) localizeToolProgressContent(content string) string {
	if m == nil || content == "" {
		return content
	}
	text := m.getLiveActivityText()
	switch content {
	case "Preparing to switch apps; restoring the companion app":
		return text.PreparingToSwitchApps
	case "Preparing to switch apps; opening the target app":
		return text.PreparingToOpenTargetApp
	case "Action sent; waiting for the screen to stabilize":
		return text.ActionSentWaitingScreenStable
	case "Reading the screen after the action":
		return text.ReadingScreenAfterAction
	default:
		return content
	}
}

// getLiveActivityText returns localized text based on the manager's locale.
func (m *LiveActivityManager) getLiveActivityText() liveActivityText {
	locale := ""
	if m != nil {
		locale, _ = m.locale.Load().(string)
	}
	return getLiveActivityTextForLocale(locale)
}

// getLiveActivityTextForLocale returns localized text for a given locale.
func getLiveActivityTextForLocale(locale string) liveActivityText {
	if locale == "" || strings.HasPrefix(locale, "en") {
		// English (default)
		return liveActivityText{
			AicoTask:                         "Aico task",
			ProcessingRequest:                "Processing request",
			AnalyzingTask:                    "Analyzing the task and preparing the next action",
			PlanningNextStep:                 "Planning next step",
			PleaseTakeOverOnPhone:            "Please take over on the phone",
			OpenCompanionAppToContinue:       "Open the companion app to continue",
			LaunchRequestSentVerifyingScreen: "Launch request sent; verifying the target screen",
			UpdatingPlanFromUserInput:        "Updating plan from user input",
			Completed:                        "Completed",
			Failed:                           "Failed",
			Canceled:                         "Canceled",
			OpeningApp:                       "Opening app",
			OpeningAppWithTarget:             "Opening %s",
			CreatingCalendarEvent:            "Creating calendar event",
			CheckingCalendar:                 "Checking calendar",
			DeletingCalendarEvent:            "Deleting calendar event",
			UpdatingCalendar:                 "Updating calendar",
			CheckingContacts:                 "Checking contacts",
			CreatingContact:                  "Creating contact",
			UpdatingContact:                  "Updating contact",
			CheckingNotifications:            "Checking notifications",
			SendingNotification:              "Sending notification",
			UsingClipboard:                   "Using clipboard",
			ReadingClipboard:                 "Reading clipboard",
			WritingClipboard:                 "Writing clipboard",
			PreparingAnswer:                  "Preparing answer",
			PlanningPrefix:                   "Planning: ",
			Browser:                          "Browser",
			Messages:                         "Messages",
			Using:                            "Using",
			CheckingScreen:                   "Checking the screen",
			WaitingForScreen:                 "Waiting for the screen",
			OpeningLink:                      "Opening link",
			ControllingPhone:                 "Controlling the phone",
			MovingPointer:                    "Moving pointer",
			Scrolling:                        "Scrolling",
			TypingText:                       "Typing text",
			PressingKeys:                     "Pressing keys",
			UsingNotifications:               "Using notifications",
			Searching:                        "Searching",
			AdjustingAudio:                   "Adjusting audio",
			CheckingInformation:              "Checking information",
			RecallingContext:                 "Recalling context",
			UpdatingMemory:                   "Updating memory",
			UsingSkills:                      "Using skills",
			WaitingForUserInput:              "Waiting for user input",
			ScreenChecked:                    "Screen checked",
			ScreenReady:                      "Screen is ready",
			AppOpened:                        "App opened",
			LinkOpened:                       "Link opened",
			ActionSentCheckingResult:         "Action sent; checking result",
			FinishedPrefix:                   "Finished: ",
			Finished:                         "Finished",
			ProblemWhilePrefix:               "Problem while ",
			ToolFailed:                       "Tool failed",
			OpeningPrefix:                    "Opening ",
			OpeningWebpage:                   "Opening webpage",
			OpeningMessageComposer:           "Opening message composer",
			OpeningEmailComposer:             "Opening email composer",
			OpeningPhone:                     "Opening phone",
			Mail:                             "Mail",
			Phone:                            "Phone",
			PreparingToSwitchApps:            "Preparing to switch apps; restoring the companion app",
			PreparingToOpenTargetApp:         "Preparing to switch apps; opening the target app",
			ActionSentWaitingScreenStable:    "Action sent; waiting for the screen to stabilize",
			ReadingScreenAfterAction:         "Reading the screen after the action",
		}
	}
	// Simplified Chinese
	return liveActivityText{
		AicoTask:                         "Aico 任务",
		ProcessingRequest:                "处理请求中",
		AnalyzingTask:                    "正在分析任务并准备下一步操作",
		PlanningNextStep:                 "规划下一步",
		PleaseTakeOverOnPhone:            "请在手机上继续操作",
		OpenCompanionAppToContinue:       "打开配套应用以继续",
		LaunchRequestSentVerifyingScreen: "启动请求已发送；正在验证目标屏幕",
		UpdatingPlanFromUserInput:        "根据用户输入更新计划",
		Completed:                        "已完成",
		Failed:                           "失败",
		Canceled:                         "已取消",
		OpeningApp:                       "打开应用",
		OpeningAppWithTarget:             "正在打开%s",
		CreatingCalendarEvent:            "创建日历事件",
		CheckingCalendar:                 "查看日历",
		DeletingCalendarEvent:            "删除日历事件",
		UpdatingCalendar:                 "更新日历",
		CheckingContacts:                 "查看联系人",
		CreatingContact:                  "创建联系人",
		UpdatingContact:                  "更新联系人",
		CheckingNotifications:            "查看通知",
		SendingNotification:              "发送通知",
		UsingClipboard:                   "使用剪贴板",
		ReadingClipboard:                 "读取剪贴板",
		WritingClipboard:                 "写入剪贴板",
		PreparingAnswer:                  "准备回答",
		PlanningPrefix:                   "规划：",
		Browser:                          "浏览器",
		Messages:                         "信息",
		Using:                            "使用",
		CheckingScreen:                   "正在查看屏幕",
		WaitingForScreen:                 "等待屏幕稳定",
		OpeningLink:                      "打开链接",
		ControllingPhone:                 "控制手机",
		MovingPointer:                    "移动指针",
		Scrolling:                        "滚动",
		TypingText:                       "输入文本",
		PressingKeys:                     "按键",
		UsingNotifications:               "使用通知",
		Searching:                        "搜索中",
		AdjustingAudio:                   "调整音量",
		CheckingInformation:              "查看信息",
		RecallingContext:                 "回忆上下文",
		UpdatingMemory:                   "更新记忆",
		UsingSkills:                      "使用技能",
		WaitingForUserInput:              "等待用户输入",
		ScreenChecked:                    "已查看屏幕",
		ScreenReady:                      "屏幕就绪",
		AppOpened:                        "应用已打开",
		LinkOpened:                       "链接已打开",
		ActionSentCheckingResult:         "操作已发送；正在确认结果",
		FinishedPrefix:                   "已完成：",
		Finished:                         "已完成",
		ProblemWhilePrefix:               "出现问题：",
		ToolFailed:                       "工具失败",
		OpeningPrefix:                    "正在打开",
		OpeningWebpage:                   "正在打开网页",
		OpeningMessageComposer:           "正在打开短信编辑",
		OpeningEmailComposer:             "正在打开邮件编辑",
		OpeningPhone:                     "正在打开拨号",
		Mail:                             "邮件",
		Phone:                            "电话",
		PreparingToSwitchApps:            "即将跳转，正在尝试唤回配套应用",
		PreparingToOpenTargetApp:         "即将跳转，正在打开目标应用",
		ActionSentWaitingScreenStable:    "操作已发送，正在等待页面稳定",
		ReadingScreenAfterAction:         "正在读取操作后的屏幕",
	}
}

func (m *LiveActivityManager) SetLocalUpdateNotifier(notifier func(context.Context, string) error) {
	if m == nil {
		return
	}
	m.localNotifyMu.Lock()
	m.localNotifier = notifier
	queue := m.localNotifyQueue
	start := false
	if notifier != nil && queue == nil {
		queue = make(chan struct{}, 1)
		m.localNotifyQueue = queue
	}
	if notifier != nil && !m.localNotifyStarted {
		m.localNotifyStarted = true
		start = true
	}
	m.localNotifyMu.Unlock()
	if start && queue != nil {
		go m.runLocalUpdateNotifier(queue)
	}
}

func (m *LiveActivityManager) enqueueLocalUpdate() {
	if m == nil {
		return
	}
	m.localNotifyMu.RLock()
	queue := m.localNotifyQueue
	m.localNotifyMu.RUnlock()
	if queue == nil {
		return
	}
	select {
	case queue <- struct{}{}:
	default:
	}
}

func (m *LiveActivityManager) runLocalUpdateNotifier(queue <-chan struct{}) {
	var lastAttempt time.Time
	for range queue {
		if wait := liveActivityLocalNotifyInterval - time.Since(lastAttempt); !lastAttempt.IsZero() && wait > 0 {
			timer := time.NewTimer(wait)
			<-timer.C
		}
		m.localNotifyMu.RLock()
		notifier := m.localNotifier
		m.localNotifyMu.RUnlock()
		if notifier == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), liveActivityLocalNotifyTimeout)
		err := notifier(ctx, "live_activity")
		cancel()
		lastAttempt = time.Now()
		if m.logger == nil {
			continue
		}
		if err != nil {
			m.logger.Debug("live activity: local BLE update wake unavailable: %v", err)
		} else {
			m.logger.Debug("live activity: local BLE update wake delivered")
		}
	}
}

func (m *LiveActivityManager) StartTask(requestID, title string, phoneIDs ...string) *LiveActivityState {
	if m == nil || m.disabled.Load() || strings.TrimSpace(requestID) == "" {
		return nil
	}
	m.logger.Info("Starting live activity task: %s, %s", requestID, title)
	phoneID := ""
	if len(phoneIDs) > 0 {
		phoneID = firstNonEmptyString(phoneIDs)
	}
	phoneID = strings.TrimSpace(phoneID)
	now := time.Now()
	text := m.getLiveActivityText()
	state := LiveActivityState{
		RequestID:     strings.TrimSpace(requestID),
		PhoneID:       phoneID,
		Locale:        m.currentLocale(),
		Status:        LiveActivityStatusRunning,
		Phase:         LiveActivityPhasePlanning,
		TaskTitle:     truncateLiveActivityText(firstNonEmptyString([]string{title, text.AicoTask}), 80),
		CurrentStep:   text.ProcessingRequest,
		CurrentAction: "process",
		ToolStatus:    "processing",
		Progress:      0.05,
		ShowsProgress: true,
		CanStop:       true,
		StartedAt:     now,
		UpdatedAt:     now,
	}
	m.mu.Lock()
	m.states[state.RequestID] = state
	m.activeRequestID = state.RequestID
	m.mu.Unlock()
	m.publish(state.RequestID, false)
	return &state
}

func (m *LiveActivityManager) UpdateFromRunEvent(requestID string, event RunEvent) *LiveActivityState {
	if m == nil || strings.TrimSpace(requestID) == "" {
		return nil
	}
	m.mu.Lock()
	requestID = strings.TrimSpace(requestID)
	state, ok := m.states[requestID]
	if !ok || state.EndedAt != nil {
		m.mu.Unlock()
		return nil
	}
	text := m.getLiveActivityText()
	switch event.Type {
	case "role_output", "assistant_output":
		state.Status = LiveActivityStatusRunning
		state.ShowsProgress = true
		state.RequiresApp = false
		state.LastError = ""
		state.LastToolName = ""
		state.ToolStatus = "processing"
		state.CurrentAction = "process"
		state.ToolStartedAt = nil
		state.Phase = liveActivityPhaseFromRole(event.Content)
		if step := truncateLiveActivityText(liveActivityStepFromRoleOutput(event, text), 120); step != "" {
			state.CurrentStep = step
		}
	case runEventReasoningDelta:
		if strings.TrimSpace(event.ReasoningContent) == "" {
			m.mu.Unlock()
			return &state
		}
		// Raw reasoning is not a display summary. Use a stage-level fallback
		// rather than exposing the model's reasoning stream.
		state.Status = LiveActivityStatusRunning
		state.Phase = LiveActivityPhasePlanning
		state.CurrentAction = "think"
		state.ToolStatus = "thinking"
		state.ToolStartedAt = nil
		state.LastToolName = ""
		state.LastError = ""
		state.RequiresApp = false
		state.CurrentStep = text.AnalyzingTask
	case runEventReasoningReset:
		state.Status = LiveActivityStatusRunning
		state.Phase = LiveActivityPhasePlanning
		state.CurrentAction = "process"
		state.ToolStatus = "processing"
		state.ToolStartedAt = nil
		state.LastToolName = ""
		state.LastError = ""
		state.RequiresApp = false
		state.CurrentStep = text.ProcessingRequest
	case runEventToolProgress:
		state.Status = LiveActivityStatusRunning
		toolStatus := firstNonEmptyString([]string{event.ToolStatus, "running"})
		if toolStatus == "running" {
			if state.ToolStatus == "running" && state.ToolStartedAt != nil {
				// Keep the timestamp from the initial tool call while progress
				// updates continue for the same tool.
			} else {
				now := event.Timestamp
				if now.IsZero() {
					now = time.Now()
				}
				state.ToolStartedAt = &now
			}
		} else {
			state.ToolStartedAt = nil
		}
		state.ToolStatus = toolStatus
		localizedContent := m.localizeToolProgressContent(event.Content)
		state.CurrentStep = truncateLiveActivityText(firstNonEmptyString([]string{
			localizedContent,
			state.CurrentStep,
		}), 120)
		state.RequiresApp = state.ToolStatus == "waiting_app"
		if state.ToolStatus == "verifying" {
			state.Phase = LiveActivityPhaseVerifying
			state.CurrentAction = "verify_result"
		} else if state.ToolStatus == "preparing" {
			state.Phase = LiveActivityPhasePhoneBridge
		} else if state.ToolStatus == "waiting_app" {
			state.Status = LiveActivityStatusNeedsApp
			state.Phase = LiveActivityPhaseWaitingApp
			state.CurrentAction = "open_aico"
		}
		state.LastError = ""
	case runEventToolCall:
		toolStatus := liveActivityToolCallStatus(event, text)
		state.Status = firstNonEmptyString([]string{toolStatus.status, LiveActivityStatusRunning})
		state.Phase = toolStatus.phase
		state.CurrentAction = toolStatus.action
		state.CurrentTarget = truncateLiveActivityText(toolStatus.target, 80)
		state.RequiresApp = toolStatus.requiresApp
		state.ShowsProgress = toolStatus.status != LiveActivityStatusNeedsApp
		state.LastError = ""
		state.LastToolName = strings.TrimSpace(event.ToolName)
		state.ToolStatus = firstNonEmptyString([]string{toolStatus.status, "running"})
		if toolStatus.phase == LiveActivityPhaseWaitingUser {
			state.ToolStatus = "waiting_user"
		}
		startedAt := event.Timestamp
		if startedAt.IsZero() {
			startedAt = time.Now()
		}
		state.ToolStartedAt = &startedAt
		if app := toolStatus.app; app != "" {
			state.CurrentApp = truncateLiveActivityText(app, 40)
		}
		stepCandidates := []string{
			event.Content,
			toolStatus.step,
			formatToolStep(text.Using, event.ToolName),
		}
		if toolStatus.status == LiveActivityStatusNeedsApp {
			stepCandidates[0], stepCandidates[1] = stepCandidates[1], stepCandidates[0]
		}
		state.CurrentStep = truncateLiveActivityText(firstNonEmptyString(stepCandidates), 120)
		state.Progress = bumpLiveActivityProgress(state.Progress)
	case "tool_result":
		hasError := liveActivityEventHasError(event)
		state.LastToolName = strings.TrimSpace(event.ToolName)
		resultAction := liveActivityToolCallStatus(event, text).action
		if resultAction != "" {
			state.CurrentAction = resultAction
		}
		if !hasError && strings.EqualFold(strings.TrimSpace(event.ToolName), toolUserActionStep) {
			state.Status = LiveActivityStatusNeedsApp
			state.Phase = LiveActivityPhaseWaitingUser
			state.CurrentAction = "request_user_input"
			state.CurrentTarget = ""
			state.RequiresApp = false
			state.ShowsProgress = false
			state.LastError = ""
			state.ToolStatus = "waiting_user"
			state.ToolStartedAt = nil
			state.CurrentStep = truncateLiveActivityText(firstNonEmptyString([]string{
				liveActivityHumanHandoffStep(event.Content),
				liveActivityHumanHandoffStep(event.ToolInput),
				text.PleaseTakeOverOnPhone,
			}), 120)
		} else if hasError {
			errText := liveActivityEventErrorText(event)
			state.LastError = truncateLiveActivityText(errText, 160)
			if liveActivityResultNeedsApp(event, errText) {
				state.Status = LiveActivityStatusNeedsApp
				state.Phase = LiveActivityPhaseWaitingApp
				state.CurrentAction = "open_aico"
				state.CurrentStep = text.OpenCompanionAppToContinue
				state.RequiresApp = true
				state.ShowsProgress = false
				state.ToolStatus = "waiting_app"
				state.ToolStartedAt = nil
			} else {
				state.Status = LiveActivityStatusRunning
				state.Phase = liveActivityToolResultPhase(event.ToolName)
				state.CurrentAction = "recover"
				state.CurrentStep = truncateLiveActivityText(liveActivityToolErrorStep(event.ToolName, text), 120)
				state.RequiresApp = false
				state.ShowsProgress = true
				state.ToolStatus = "failed"
				state.ToolStartedAt = nil
			}
		} else {
			state.Status = LiveActivityStatusRunning
			state.Phase = liveActivityToolResultPhase(event.ToolName)
			state.CurrentAction = "verify_result"
			state.RequiresApp = false
			state.ShowsProgress = true
			state.LastError = ""
			state.ToolStatus = "succeeded"
			state.ToolStartedAt = nil
			state.CurrentStep = truncateLiveActivityText(liveActivityToolResultStep(event.ToolName, text), 120)
			if event.ToolName == toolOpenApp || event.ToolName == toolOpenURL || event.ToolName == toolBridgeOpenApp {
				// An accepted launch (even with a captured image) does not prove
				// the target app is on screen. The next model observation does.
				state.ToolStatus = "verifying"
				state.CurrentStep = text.LaunchRequestSentVerifyingScreen
			}
			state.Progress = bumpLiveActivityProgress(state.Progress)
		}
	case "steer":
		state.Status = LiveActivityStatusRunning
		state.Phase = LiveActivityPhasePlanning
		state.CurrentAction = "steer"
		state.CurrentStep = text.UpdatingPlanFromUserInput
		state.RequiresApp = false
		state.ShowsProgress = true
		state.LastError = ""
		state.ToolStatus = "processing"
		state.ToolStartedAt = nil
	default:
		m.mu.Unlock()
		return &state
	}
	state.UpdatedAt = time.Now()
	m.states[requestID] = state
	m.activeRequestID = requestID
	m.mu.Unlock()
	m.publish(requestID, false)
	return &state
}

func (m *LiveActivityManager) CompleteTask(requestID, output string) *LiveActivityState {
	if state := m.pauseForHumanHandoff(requestID, output); state != nil {
		return state
	}
	text := m.getLiveActivityText()
	return m.finishTask(requestID, LiveActivityStatusCompleted, firstNonEmptyString([]string{
		truncateLiveActivityText(output, 120),
		text.Completed,
	}), "")
}

func (m *LiveActivityManager) pauseForHumanHandoff(requestID, output string) *LiveActivityState {
	if m == nil || strings.TrimSpace(requestID) == "" {
		return nil
	}
	requestID = strings.TrimSpace(requestID)
	text := m.getLiveActivityText()
	m.mu.Lock()
	state, ok := m.states[requestID]
	if !ok || (state.Phase != LiveActivityPhaseWaitingUser && !strings.EqualFold(state.LastToolName, toolUserActionStep)) {
		m.mu.Unlock()
		return nil
	}
	state.Status = LiveActivityStatusNeedsApp
	state.Phase = LiveActivityPhaseWaitingUser
	state.CurrentAction = "request_user_input"
	state.CurrentTarget = ""
	state.ToolStatus = "waiting_user"
	state.ToolStartedAt = nil
	state.CurrentStep = truncateLiveActivityText(firstNonEmptyString([]string{
		output,
		state.CurrentStep,
		text.PleaseTakeOverOnPhone,
	}), 120)
	state.Progress = 0
	state.ShowsProgress = false
	state.CanStop = false
	state.RequiresApp = false
	state.UpdatedAt = time.Now()
	state.EndedAt = nil
	m.states[requestID] = state
	m.activeRequestID = requestID
	m.mu.Unlock()
	m.publish(requestID, false)
	return &state
}

func (m *LiveActivityManager) FailTask(requestID, message string) *LiveActivityState {
	text := m.getLiveActivityText()
	return m.finishTask(requestID, LiveActivityStatusFailed, text.Failed, truncateLiveActivityText(message, 160))
}

func (m *LiveActivityManager) CancelTask(requestID string) *LiveActivityState {
	text := m.getLiveActivityText()
	return m.finishTask(requestID, LiveActivityStatusCanceled, text.Canceled, "")
}

func (m *LiveActivityManager) finishTask(requestID, status, step, errText string) *LiveActivityState {
	if m == nil || strings.TrimSpace(requestID) == "" {
		return nil
	}
	now := time.Now()
	m.mu.Lock()
	state, ok := m.states[requestID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	state.Status = status
	state.Phase = liveActivityFinalPhase(status)
	state.CurrentStep = truncateLiveActivityText(step, 120)
	state.CurrentAction = status
	state.CurrentTarget = ""
	state.LastError = errText
	if status == LiveActivityStatusCompleted {
		state.ToolStatus = "succeeded"
	} else if status == LiveActivityStatusCanceled {
		state.ToolStatus = ""
	} else {
		state.ToolStatus = status
	}
	state.ToolStartedAt = nil
	state.Progress = 1
	state.LastToolName = ""
	state.ShowsProgress = false
	state.CanStop = false
	state.RequiresApp = false
	state.UpdatedAt = now
	state.EndedAt = &now
	m.states[requestID] = state
	m.activeRequestID = requestID
	m.mu.Unlock()
	m.publish(requestID, true)
	m.scheduleCleanup(requestID, now, liveActivityFinalStateRetention)
	return &state
}

func (m *LiveActivityManager) Snapshot(requestID string) *LiveActivityState {
	if m == nil {
		return nil
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil
	}
	m.mu.Lock()
	state, ok := m.states[requestID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return &state
}

func (m *LiveActivityManager) SnapshotActive() *LiveActivityState {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeRequestID != "" {
		if state, ok := m.states[m.activeRequestID]; ok {
			return &state
		}
	}
	var latest *LiveActivityState
	for _, state := range m.states {
		candidate := state
		if latest == nil || candidate.UpdatedAt.After(latest.UpdatedAt) {
			latest = &candidate
		}
	}
	return latest
}

func (m *LiveActivityManager) SnapshotActiveForPhone(phoneID string) *LiveActivityState {
	if m == nil {
		return nil
	}
	phoneID = strings.TrimSpace(phoneID)
	if phoneID == "" {
		return m.SnapshotActive()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeRequestID != "" {
		if state, ok := m.states[m.activeRequestID]; ok && liveActivityStateMatchesPhoneID(state, phoneID) {
			return &state
		}
	}
	var latest *LiveActivityState
	for _, state := range m.states {
		if !liveActivityStateMatchesPhoneID(state, phoneID) {
			continue
		}
		candidate := state
		if latest == nil || candidate.UpdatedAt.After(latest.UpdatedAt) {
			latest = &candidate
		}
	}
	return latest
}

func (m *LiveActivityManager) publish(requestID string, final bool) {
	if m == nil {
		return
	}
	if m.logger != nil {
		m.logger.Info("Publishing live activity locally: %s, %t", requestID, final)
	}
	m.enqueueLocalUpdate()
}

func (m *LiveActivityManager) scheduleCleanup(requestID string, endedAt time.Time, after time.Duration) {
	if m == nil || after <= 0 {
		return
	}
	time.AfterFunc(after, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		state, ok := m.states[requestID]
		if !ok || state.EndedAt == nil || !state.EndedAt.Equal(endedAt) {
			return
		}
		delete(m.states, requestID)
		if m.activeRequestID == requestID {
			m.activeRequestID = ""
		}
	})
}

func liveActivityStateMatchesPhoneID(state LiveActivityState, phoneID string) bool {
	phoneID = strings.TrimSpace(phoneID)
	if phoneID == "" {
		return true
	}
	statePhoneID := strings.TrimSpace(state.PhoneID)
	return statePhoneID == "" || statePhoneID == phoneID
}

func bumpLiveActivityProgress(current float64) float64 {
	if current <= 0 {
		return 0.1
	}
	next := current + 0.08
	if next > 0.92 {
		return 0.92
	}
	return next
}

func formatToolStep(prefix, tool string) string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return prefix
	}
	return prefix + " " + tool
}

type liveActivityToolStatus struct {
	status      string
	phase       string
	action      string
	step        string
	app         string
	target      string
	requiresApp bool
}

func liveActivityPhaseFromRole(content string) string {
	if speech.ExtractText(content) != "" {
		return LiveActivityPhaseAnswering
	}
	return LiveActivityPhasePlanning
}

func liveActivityToolCallStatus(event RunEvent, text liveActivityText) liveActivityToolStatus {
	tool := strings.ToLower(strings.TrimSpace(event.ToolName))
	target := liveActivityTargetFromToolCall(event)
	status := liveActivityToolStatus{
		phase:  LiveActivityPhaseActing,
		action: normalizedLiveActivityAction(tool),
		step:   liveActivityToolCallStep(tool, text),
		target: target,
	}
	switch tool {
	case "screenshot":
		status.phase = LiveActivityPhaseObserving
		status.action = "observe_screen"
	case "wait_for_stable_screen":
		status.phase = LiveActivityPhaseObserving
		status.action = "wait_for_screen"
	case toolOpenApp:
		status.action = "open_app"
		status.app = liveActivityAppFromToolCall(event, text)
		if status.step == "" {
			status.step = text.OpeningApp
		}
		if target != "" {
			status.step = fmt.Sprintf(text.OpeningAppWithTarget, target)
		}
	case toolOpenURL:
		status.phase = LiveActivityPhasePhoneBridge
		status.action = "open_url"
		status.requiresApp = true
		status.app = liveActivityOpenURLApp(target, text)
		status.step = liveActivityOpenURLCallStep(target, text)
	case toolBridgeOpenApp:
		status.phase = LiveActivityPhasePhoneBridge
		status.action = "open_app"
		status.requiresApp = true
		status.app = liveActivityAppFromToolCall(event, text)
		status.step = text.OpeningApp
	case toolBridgeClipboard:
		status.phase = LiveActivityPhasePhoneBridge
		status.action = "clipboard"
		status.requiresApp = true
		status.step = liveActivityClipboardStep(event.ToolInput, text)
	case toolBridgeCalendar:
		status.phase = LiveActivityPhasePhoneBridge
		status.action = "calendar"
		status.requiresApp = true
		status.step = liveActivityActionStep(event.ToolInput, map[string]string{
			"create": text.CreatingCalendarEvent,
			"query":  text.CheckingCalendar,
			"delete": text.DeletingCalendarEvent,
		}, text.UpdatingCalendar)
	case toolBridgeContacts:
		status.phase = LiveActivityPhasePhoneBridge
		status.action = "contacts"
		status.requiresApp = true
		status.step = liveActivityActionStep(event.ToolInput, map[string]string{
			"query":  text.CheckingContacts,
			"create": text.CreatingContact,
			"update": text.UpdatingContact,
		}, text.CheckingContacts)
	case toolBridgeNotification:
		payload, _ := liveActivityJSONObject(event.ToolInput)
		if strings.EqualFold(liveActivityString(payload, "action"), "query") {
			status.phase = LiveActivityPhaseVerifying
			status.action = "notification"
			status.step = text.CheckingNotifications
		} else {
			status.phase = LiveActivityPhasePhoneBridge
			status.action = "notification"
			status.requiresApp = true
			status.step = text.SendingNotification
		}
	case "request_user_action":
		status.status = LiveActivityStatusNeedsApp
		status.phase = LiveActivityPhaseWaitingUser
		status.action = "request_user_input"
		status.step = firstNonEmptyString([]string{
			liveActivityHumanHandoffStep(event.ToolInput),
			text.PleaseTakeOverOnPhone,
		})
	case "touch_gesture", "quick_action":
		status.action = "control_phone"
	case "mouse_move":
		status.action = "move_pointer"
	case "mouse_scroll":
		status.action = "scroll"
	case "keyboard_text", "enter_text":
		status.action = "type_text"
	case "keyboard_tap":
		status.action = "press_keys"
	case "web_search", "wikipedia", "web_scraper":
		status.action = "search"
	case "weather":
		status.action = "check_information"
	}
	if status.step == "" {
		status.step = formatToolStep(text.Using, tool)
	}
	return status
}

func liveActivityToolResultPhase(tool string) string {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "screenshot", "wait_for_stable_screen":
		return LiveActivityPhaseVerifying
	case toolOpenURL, toolBridgeOpenApp, toolBridgeClipboard, toolBridgeCalendar, toolBridgeContacts, toolBridgeNotification:
		return LiveActivityPhasePhoneBridge
	case "request_user_action":
		return LiveActivityPhaseWaitingUser
	default:
		return LiveActivityPhaseVerifying
	}
}

func liveActivityFinalPhase(status string) string {
	switch status {
	case LiveActivityStatusCompleted:
		return LiveActivityPhaseAnswering
	case LiveActivityStatusFailed:
		return "failed"
	case LiveActivityStatusCanceled:
		return "canceled"
	default:
		return ""
	}
}

func liveActivityEventHasError(event RunEvent) bool {
	if event.ToolError != nil || event.IsError {
		return true
	}
	payload, ok := liveActivityJSONObject(event.Content)
	if !ok {
		return false
	}
	if okValue, ok := payload["ok"].(bool); ok && !okValue {
		return true
	}
	return false
}

func liveActivityEventErrorText(event RunEvent) string {
	if event.ToolError != nil && strings.TrimSpace(event.ToolError.Message) != "" {
		return strings.TrimSpace(event.ToolError.Message)
	}
	payload, ok := liveActivityJSONObject(event.Content)
	if ok {
		if value, ok := payload["error"].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return strings.TrimSpace(event.Content)
}

func liveActivityResultNeedsApp(event RunEvent, errText string) bool {
	if !liveActivityToolRequiresApp(event.ToolName) {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(errText))
	for _, marker := range []string{
		"phone bridge not connected",
		"not connected",
		"connection closed",
		"command timeout",
		"write command",
		"websocket",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func liveActivityToolRequiresApp(tool string) bool {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case toolOpenURL, toolBridgeOpenApp, toolBridgeClipboard, toolBridgeCalendar, toolBridgeContacts, toolBridgeNotification:
		return true
	default:
		return false
	}
}

func liveActivityClipboardStep(input string, text liveActivityText) string {
	payload, ok := liveActivityJSONObject(input)
	if !ok {
		return text.UsingClipboard
	}
	switch strings.ToLower(strings.TrimSpace(liveActivityString(payload, "action"))) {
	case "read":
		return text.ReadingClipboard
	case "write":
		return text.WritingClipboard
	default:
		return text.UsingClipboard
	}
}

func liveActivityActionStep(input string, labels map[string]string, fallback string) string {
	payload, ok := liveActivityJSONObject(input)
	if !ok {
		return fallback
	}
	action := strings.ToLower(strings.TrimSpace(liveActivityString(payload, "action")))
	if label := strings.TrimSpace(labels[action]); label != "" {
		return label
	}
	return fallback
}

func liveActivityHumanHandoffStep(input string) string {
	payload, ok := liveActivityJSONObject(input)
	if !ok {
		return ""
	}
	return firstNonEmptyString([]string{
		liveActivityString(payload, "suggested_action"),
		liveActivityString(payload, "details"),
		liveActivityString(payload, "message"),
	})
}

func liveActivityTargetFromToolCall(event RunEvent) string {
	payload, ok := liveActivityJSONObject(event.ToolInput)
	if !ok {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(event.ToolName)) {
	case toolOpenApp, toolBridgeOpenApp:
		return firstNonEmptyString([]string{liveActivityString(payload, "app"), liveActivityString(payload, "name")})
	case toolOpenURL:
		return liveActivityString(payload, "url")
	case toolBridgeCalendar:
		return firstNonEmptyString([]string{
			liveActivityString(payload, "title"),
			liveActivityString(payload, "from"),
			liveActivityString(payload, "event_id"),
		})
	case toolBridgeContacts:
		return firstNonEmptyString([]string{
			liveActivityString(payload, "name"),
			liveActivityString(payload, "query"),
			liveActivityString(payload, "contact_id"),
		})
	case toolBridgeNotification:
		return liveActivityString(payload, "title")
	case "weather":
		return liveActivityString(payload, "location")
	case "web_search", "wikipedia", "web_scraper":
		return firstNonEmptyString([]string{
			liveActivityString(payload, "query"),
			liveActivityString(payload, "url"),
		})
	default:
		return ""
	}
}

func liveActivityJSONObject(input string) (map[string]interface{}, bool) {
	input = strings.TrimSpace(input)
	if input == "" || !strings.HasPrefix(input, "{") {
		return nil, false
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		return nil, false
	}
	return payload, true
}

func liveActivityString(payload map[string]interface{}, key string) string {
	if value, ok := payload[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func normalizedLiveActivityAction(tool string) string {
	tool = strings.ToLower(strings.TrimSpace(tool))
	if tool == "" {
		return "use_tool"
	}
	var builder strings.Builder
	for _, r := range tool {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '_' || r == '-':
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "use_tool"
	}
	return builder.String()
}

func liveActivityStepFromRoleOutput(event RunEvent, text liveActivityText) string {
	role := strings.ToLower(strings.TrimSpace(event.Role))
	content := strings.TrimSpace(event.Content)
	if content != "" && strings.HasPrefix(content, "{") {
		if step := liveActivityStepFromJSONRoleOutput(content, text); step != "" {
			return step
		}
	}
	switch role {
	case "agent":
		return text.ProcessingRequest
	default:
		if content != "" && !strings.HasPrefix(content, "{") && !strings.HasPrefix(content, "[") {
			return content
		}
		return text.ProcessingRequest
	}
}

func liveActivityStepFromJSONRoleOutput(content string, text liveActivityText) string {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return ""
	}
	if _, ok := payload["final_answer"]; ok {
		return text.PreparingAnswer
	}
	for _, key := range []string{"current_step", "summary", "reason"} {
		if value, ok := payload[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	if plan, ok := payload["plan"].([]interface{}); ok && len(plan) > 0 {
		if first, ok := plan[0].(string); ok && strings.TrimSpace(first) != "" {
			return text.PlanningPrefix + strings.TrimSpace(first)
		}
	}
	return ""
}

func liveActivityOpenURLKind(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.HasPrefix(value, "http://"), strings.HasPrefix(value, "https://"):
		return "web"
	case strings.HasPrefix(value, "sms:"):
		return "sms"
	case strings.HasPrefix(value, "mailto:"):
		return "email"
	case strings.HasPrefix(value, "tel:"):
		return "phone"
	default:
		return "link"
	}
}

func liveActivityOpenURLApp(value string, text liveActivityText) string {
	switch liveActivityOpenURLKind(value) {
	case "web":
		return text.Browser
	case "sms":
		return text.Messages
	case "email":
		return text.Mail
	case "phone":
		return text.Phone
	default:
		return ""
	}
}

func liveActivityOpenURLCallStep(value string, text liveActivityText) string {
	switch liveActivityOpenURLKind(value) {
	case "web":
		if value = strings.TrimSpace(value); value != "" {
			return text.OpeningPrefix + value
		}
		return text.OpeningWebpage
	case "sms":
		return text.OpeningMessageComposer
	case "email":
		return text.OpeningEmailComposer
	case "phone":
		return text.OpeningPhone
	default:
		return text.OpeningLink
	}
}

func liveActivityToolCallStep(tool string, text liveActivityText) string {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "screenshot":
		return text.CheckingScreen
	case "wait_for_stable_screen":
		return text.WaitingForScreen
	case toolOpenApp:
		return text.OpeningApp
	case toolOpenURL:
		return text.OpeningLink
	case "touch_gesture", "quick_action":
		return text.ControllingPhone
	case "mouse_move":
		return text.MovingPointer
	case "mouse_scroll":
		return text.Scrolling
	case "keyboard_text", "enter_text":
		return text.TypingText
	case "keyboard_tap":
		return text.PressingKeys
	case toolBridgeClipboard:
		return text.UsingClipboard
	case toolBridgeCalendar:
		return text.UpdatingCalendar
	case toolBridgeContacts:
		return text.CheckingContacts
	case toolBridgeNotification:
		return text.UsingNotifications
	case "web_search", "wikipedia", "web_scraper":
		return text.Searching
	case "audio_volume":
		return text.AdjustingAudio
	case "weather":
		return text.CheckingInformation
	case "recall_memory", "recall_session_chunks", "recall_device_memory", "inspect_episode":
		return text.RecallingContext
	case "save_memory", "forget_memory":
		return text.UpdatingMemory
	case "skill_list", "skill_read", "skill_manage", "skill_mark_used":
		return text.UsingSkills
	case "request_user_action":
		return text.WaitingForUserInput
	default:
		return ""
	}
}

func liveActivityToolResultStep(tool string, text liveActivityText) string {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "screenshot":
		return text.ScreenChecked
	case "wait_for_stable_screen":
		return text.ScreenReady
	case toolOpenApp:
		return text.AppOpened
	case toolOpenURL:
		return text.LinkOpened
	case "touch_gesture", "quick_action", "mouse_move", "mouse_scroll", "keyboard_tap", "keyboard_text", "enter_text":
		return text.ActionSentCheckingResult
	case "request_user_action":
		return text.WaitingForUserInput
	default:
		if step := liveActivityToolCallStep(tool, text); step != "" {
			return text.FinishedPrefix + step
		}
		return formatToolStep(text.Finished, tool)
	}
}

func liveActivityToolErrorStep(tool string, text liveActivityText) string {
	if step := liveActivityToolCallStep(tool, text); step != "" {
		return text.ProblemWhilePrefix + strings.ToLower(step)
	}
	return formatToolStep(text.ToolFailed, tool)
}

func liveActivityAppFromToolCall(event RunEvent, text liveActivityText) string {
	tool := strings.ToLower(strings.TrimSpace(event.ToolName))
	if tool != toolOpenApp && tool != toolOpenURL && tool != toolBridgeOpenApp {
		return ""
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(event.ToolInput)), &payload); err != nil {
		return ""
	}
	if value, ok := payload["app"].(string); ok {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	if value, ok := payload["name"].(string); ok {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	if value, ok := payload["url"].(string); ok && strings.TrimSpace(value) != "" {
		return liveActivityOpenURLApp(value, text)
	}
	return ""
}

func truncateLiveActivityText(s string, limit int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if limit <= 0 || len(runes) <= limit {
		return s
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "..."
}

func isCancelableLiveActivityStatus(status string) bool {
	switch status {
	case LiveActivityStatusRunning, LiveActivityStatusNeedsApp:
		return true
	default:
		return false
	}
}

func (m *LiveActivityManager) Reconfigure(cfg LiveActivityConfig, locale string) {
	if m == nil {
		return
	}
	m.locale.Store(normalizeVoiceNotificationLocale(locale))
	m.disabled.Store(!cfg.EnabledOrDefault())
	if !cfg.EnabledOrDefault() {
		m.mu.Lock()
		running := make([]string, 0, len(m.states))
		for requestID, state := range m.states {
			if isCancelableLiveActivityStatus(state.Status) {
				running = append(running, requestID)
			}
		}
		m.mu.Unlock()
		for _, requestID := range running {
			m.CancelTask(requestID)
		}
	}
}
