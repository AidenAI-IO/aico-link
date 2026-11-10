package agenttask

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"aico-agent/internal/stream"
)

type Status string

const (
	StatusCreated    Status = "created"
	StatusQueued     Status = "queued"
	StatusRunning    Status = "running"
	StatusCancelling Status = "cancelling"
	StatusCancelled  Status = "cancelled"
	StatusFailed     Status = "failed"
	StatusCompleted  Status = "completed"
)

const (
	defaultQueueSize = 64
	maxResultRunes   = 8000
)

// terminal reports whether the task has finished and will not change state
// again. cancelling is not terminal: the runtime still owns the task until it
// returns from context cancellation.
func (s Status) terminal() bool {
	switch s {
	case StatusCancelled, StatusFailed, StatusCompleted:
		return true
	default:
		return false
	}
}

type Task struct {
	ID                string      `json:"id"`
	Prompt            string      `json:"prompt"`
	Status            Status      `json:"status"`
	Result            string      `json:"result,omitempty"`
	Error             string      `json:"error,omitempty"`
	CreatedAt         time.Time   `json:"created_at"`
	UpdatedAt         time.Time   `json:"updated_at"`
	StartedAt         *time.Time  `json:"started_at,omitempty"`
	CompletedAt       *time.Time  `json:"completed_at,omitempty"`
	PendingUserAction *UserAction `json:"pending_user_action,omitempty"`
	GoalRevision      uint64      `json:"-"` // Snapshot identity for observer-side result correlation.
}

type UserAction struct {
	Reason          string `json:"reason"`
	Details         string `json:"details"`
	SuggestedAction string `json:"suggested_action,omitempty"`
}

type userActionHandlerContextKey struct{}
type steerControlContextKey struct{}

type SteerMessage struct {
	ID        string
	Content   string
	Timestamp time.Time
	Revision  uint64
}

type steerControl struct {
	provider    func(context.Context) (SteerMessage, bool)
	interrupt   func() <-chan struct{}
	acknowledge func(SteerMessage)
}

func WithUserActionHandler(ctx context.Context, handler func(UserAction)) context.Context {
	return context.WithValue(ctx, userActionHandlerContextKey{}, handler)
}

func UserActionHandlerFromContext(ctx context.Context) func(UserAction) {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(userActionHandlerContextKey{}).(func(UserAction))
	return h
}

func withSteerControl(
	ctx context.Context,
	provider func(context.Context) (SteerMessage, bool),
	interrupt func() <-chan struct{},
	acknowledge func(SteerMessage),
) context.Context {
	return context.WithValue(ctx, steerControlContextKey{}, steerControl{
		provider:    provider,
		interrupt:   interrupt,
		acknowledge: acknowledge,
	})
}

func SteerProviderFromContext(ctx context.Context) func(context.Context) (SteerMessage, bool) {
	if ctx == nil {
		return nil
	}
	control, _ := ctx.Value(steerControlContextKey{}).(steerControl)
	return control.provider
}

func SteerInterruptFromContext(ctx context.Context) func() <-chan struct{} {
	if ctx == nil {
		return nil
	}
	control, _ := ctx.Value(steerControlContextKey{}).(steerControl)
	return control.interrupt
}

func SteerAcknowledgerFromContext(ctx context.Context) func(SteerMessage) {
	if ctx == nil {
		return nil
	}
	control, _ := ctx.Value(steerControlContextKey{}).(steerControl)
	return control.acknowledge
}

// Runner is the narrow boundary between task orchestration and an agent
// implementation. The manager does not depend on the legacy agent package.
// A run stopped by cancellation may still return text, normally from
// CancelledResult, describing work it completed before stopping.
type Runner interface {
	Run(context.Context, string) (string, error)
}

// CompletedStep is one successful tool call made by a run.
type CompletedStep struct {
	Tool   string
	Input  string
	Output string
}

const (
	maxCancelledSteps     = 20
	maxCancelledStepRunes = 300
)

// CancelledResult describes the successful steps a cancelled run completed,
// so the user can be told which effects already happened. It returns an empty
// string when nothing completed.
func CancelledResult(steps []CompletedStep) string {
	if len(steps) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("Stopped before finishing. These steps had already completed and were not undone:")
	if omitted := len(steps) - maxCancelledSteps; omitted > 0 {
		fmt.Fprintf(&out, "\n- (%d earlier steps omitted)", omitted)
		steps = steps[omitted:]
	}
	for _, step := range steps {
		fmt.Fprintf(&out, "\n- %s", step.Tool)
		if input := limitText(step.Input, maxCancelledStepRunes); input != "" {
			fmt.Fprintf(&out, " input=%s", input)
		}
		if output := limitText(step.Output, maxCancelledStepRunes); output != "" {
			fmt.Fprintf(&out, " output=%s", output)
		}
	}
	return out.String()
}

type Observation struct {
	Task            Task
	Phase           string
	GoalRevision    uint64
	AppliedRevision uint64
	Sequence        uint64
	StreamSink      stream.EventSink
	Cause           Cause
}

type Cause struct {
	TurnID     string
	ResponseID string
	CallID     string
}

type causeContextKey struct{}

func WithCause(ctx context.Context, cause Cause) context.Context {
	return context.WithValue(ctx, causeContextKey{}, cause)
}

func CauseFromContext(ctx context.Context) Cause {
	cause, _ := ctx.Value(causeContextKey{}).(Cause)
	return cause
}

type taskContextKey struct{}

func TaskObservationFromContext(ctx context.Context) (Observation, bool) {
	snapshot, ok := ctx.Value(taskContextKey{}).(func() Observation)
	if !ok {
		return Observation{}, false
	}
	return snapshot(), true
}

// ObservationSnapshot is read-only and never claims a result notification.
func (m *Manager) ObservationSnapshot(taskID string) Observation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item := m.tasks[taskID]
	if item == nil {
		return Observation{}
	}
	return Observation{Task: item.task, GoalRevision: item.goalRevision,
		AppliedRevision: item.appliedRevision, StreamSink: m.streamSink}
}

type entry struct {
	task            Task
	cancel          context.CancelFunc
	nextPrompt      string
	actionNotified  bool
	resumeQueued    bool
	goalRevision    uint64
	appliedRevision uint64
	pendingSteer    *SteerMessage
	steerSignal     chan struct{}
}

type terminalNotificationState uint8

const (
	terminalNotificationQueued terminalNotificationState = iota + 1
	terminalNotificationClaimed
	terminalNotificationDelivering
)

// Manager serializes background work and keeps create, cancel, and query
// operations independent from task execution latency.
type Manager struct {
	runner Runner
	now    func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	queue  chan string
	wg     sync.WaitGroup

	mu                    sync.RWMutex
	tasks                 map[string]*entry
	terminalNotifications map[string]terminalNotificationState
	terminalSeq           uint64
	terminalChanged       chan struct{}
	wakeChanged           chan struct{}
	actionChanged         chan struct{}
	closed                bool
	observationSequence   uint64
	streamSink            stream.EventSink // Optional nonblocking observer; never a network sink.
}

// SetEventSink installs a nonblocking in-process recorder before tasks are submitted.
// Emit must only copy/enqueue: it is invoked under the manager lock to preserve
// the state transition's observation time. The recorder owns overflow reporting.
func (m *Manager) SetEventSink(sink stream.EventSink) {
	m.mu.Lock()
	m.streamSink = sink
	m.mu.Unlock()
}

func (m *Manager) observeLocked(item *entry, phase string, cause ...Cause) {
	m.observationSequence++
	observation := Observation{Task: item.task, Phase: phase, GoalRevision: item.goalRevision,
		AppliedRevision: item.appliedRevision, Sequence: m.observationSequence, StreamSink: m.streamSink}
	if len(cause) > 0 {
		observation.Cause = cause[0]
	}
	if observation.StreamSink == nil {
		return
	}
	payload := map[string]any{"phase": observation.Phase, "status": observation.Task.Status, "task": observation.Task}
	identity := stream.EventIdentity{ConversationID: "agent", SessionEpoch: 1, TaskID: observation.Task.ID,
		TurnID: observation.Cause.TurnID, ResponseID: observation.Cause.ResponseID, CallID: observation.Cause.CallID}
	event := stream.NewEvent(identity, stream.EventTaskUpdated, payload)
	event.Origin = "task_manager"
	event.Visibility = stream.VisibilityDiagnostic
	event.Collaboration = &stream.Collaboration{Version: 1, SourceSequence: observation.Sequence,
		Actor: "task_manager", GoalRevision: observation.GoalRevision, AppliedRevision: observation.AppliedRevision}
	// The benchmark recorder only copies/enqueues here. Its queue owns overflow
	// reporting and draining; no I/O or subscriber callbacks run under this lock.
	_ = observation.StreamSink.Emit(context.Background(), event)
}

func NewManager(runner Runner) *Manager {
	return newManager(runner, defaultQueueSize, time.Now)
}

func newManager(runner Runner, queueSize int, now func() time.Time) *Manager {
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		runner:                runner,
		now:                   now,
		ctx:                   ctx,
		cancel:                cancel,
		queue:                 make(chan string, queueSize),
		tasks:                 make(map[string]*entry),
		terminalNotifications: make(map[string]terminalNotificationState),
		terminalChanged:       make(chan struct{}, 1),
		wakeChanged:           make(chan struct{}, 1),
		actionChanged:         make(chan struct{}, 1),
	}
	m.wg.Add(1)
	go m.worker()
	return m
}

func (m *Manager) Create(prompt string) (Task, error) { return m.CreateWithCause(prompt, Cause{}) }

func (m *Manager) CreateWithCause(prompt string, cause Cause) (Task, error) {
	if m == nil {
		return Task{}, errors.New("agent task manager is unavailable")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Task{}, errors.New("task is required")
	}
	now := m.now().UTC()
	task := Task{
		ID:           "task_" + uuid.NewString(),
		Prompt:       prompt,
		Status:       StatusCreated,
		GoalRevision: 1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Task{}, errors.New("agent task manager is closed")
	}
	task.Status = StatusQueued
	m.tasks[task.ID] = &entry{task: task, goalRevision: 1}
	select {
	case m.queue <- task.ID:
		m.observeLocked(m.tasks[task.ID], "created", cause)
		m.observeLocked(m.tasks[task.ID], "queued", cause)
		m.mu.Unlock()
		return task, nil
	default:
		delete(m.tasks, task.ID)
		m.mu.Unlock()
		return Task{}, errors.New("agent task queue is full")
	}
}

func (m *Manager) Query(taskID string) (Task, bool) {
	if m == nil {
		return Task{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.tasks[strings.TrimSpace(taskID)]
	if !ok {
		return Task{}, false
	}
	return item.task, true
}

// Outstanding returns the work a caller must consider before starting
// something new: tasks that have not finished, plus finished tasks whose result
// has not been delivered to the foreground yet. Finished tasks that were
// already delivered are left out because the foreground holds their result in
// its own context, so asking for the same work again is a genuine new request.
// Tasks are ordered newest first.
func (m *Manager) Outstanding() []Task {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tasks []Task
	for id, item := range m.tasks {
		if item.task.Status.terminal() {
			if _, waiting := m.terminalNotifications[id]; !waiting {
				continue
			}
		}
		tasks = append(tasks, item.task)
	}
	slices.SortFunc(tasks, func(a, b Task) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(b.ID, a.ID)
	})
	return tasks
}

func (m *Manager) Cancel(taskID string) (Task, bool, error) {
	if m == nil {
		return Task{}, false, errors.New("agent task manager is unavailable")
	}
	taskID = strings.TrimSpace(taskID)
	m.mu.Lock()
	item, ok := m.tasks[taskID]
	if !ok {
		m.mu.Unlock()
		return Task{}, false, errors.New("agent task not found")
	}
	var cancel context.CancelFunc
	var notificationCleared bool
	switch item.task.Status {
	case StatusCreated, StatusQueued:
		m.finishLocked(item, StatusCancelled, "", "")
	case StatusRunning:
		if item.task.PendingUserAction != nil || item.cancel == nil {
			m.finishLocked(item, StatusCancelled, "", "")
			break
		}
		item.task.Status = StatusCancelling
		item.task.UpdatedAt = m.now().UTC()
		m.observeLocked(item, "cancelling")
		cancel = item.cancel
	case StatusCancelling, StatusCancelled, StatusFailed, StatusCompleted:
		// Cancellation is idempotent for tasks already stopping or terminal. For a
		// terminal task it also means "do not announce this result" while delivery
		// is still owned by the manager or claimed by a foreground session.
		if item.task.Status.terminal() {
			notificationCleared = m.suppressTerminalNotificationLocked(taskID)
		}
	}
	task := item.task
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return task, notificationCleared, nil
}

func (m *Manager) Update(taskID, prompt string) (Task, error) {
	return m.UpdateWithCause(taskID, prompt, Cause{})
}

func (m *Manager) UpdateWithCause(taskID, prompt string, cause Cause) (Task, error) {
	if m == nil {
		return Task{}, errors.New("agent task manager is unavailable")
	}
	taskID, prompt = strings.TrimSpace(taskID), strings.TrimSpace(prompt)
	if prompt == "" {
		return Task{}, errors.New("task is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.tasks[taskID]
	if !ok {
		return Task{}, errors.New("agent task not found")
	}
	oldPrompt := item.task.Prompt
	switch item.task.Status {
	case StatusCreated, StatusQueued:
		m.updateGoalLocked(item, prompt)
	case StatusRunning:
		if item.task.PendingUserAction != nil {
			select {
			case m.queue <- taskID:
				m.updateGoalLocked(item, prompt)
				item.task.PendingUserAction = nil
				item.actionNotified = false
				item.nextPrompt = formatUpdatedTaskPrompt(oldPrompt, prompt)
				item.resumeQueued = true
			default:
				return Task{}, errors.New("agent task queue is full")
			}
			break
		}
		m.updateGoalLocked(item, prompt)
		if item.resumeQueued || item.cancel == nil {
			nextPrompt := formatUpdatedTaskPrompt(oldPrompt, prompt)
			if item.nextPrompt != "" {
				nextPrompt = item.nextPrompt + "\n\n" + nextPrompt
			}
			item.nextPrompt = nextPrompt
			break
		}
		message := SteerMessage{
			ID:        fmt.Sprintf("%s_goal_%d", taskID, item.goalRevision),
			Content:   formatUpdatedTaskPrompt(oldPrompt, prompt),
			Timestamp: item.task.UpdatedAt,
			Revision:  item.goalRevision,
		}
		if item.pendingSteer == nil && item.steerSignal != nil {
			close(item.steerSignal)
		}
		item.pendingSteer = &message
	case StatusCancelling:
		return Task{}, errors.New("agent task is cancelling")
	case StatusCancelled, StatusFailed, StatusCompleted:
		return Task{}, errors.New("agent task is already finished")
	default:
		return Task{}, fmt.Errorf("agent task cannot be updated from status %q", item.task.Status)
	}
	m.observeLocked(item, "goal_updated", cause)
	return item.task, nil
}

func (m *Manager) Continue(taskID, userMessage string) (Task, error) {
	if m == nil {
		return Task{}, errors.New("agent task manager is unavailable")
	}
	taskID, userMessage = strings.TrimSpace(taskID), strings.TrimSpace(userMessage)
	if userMessage == "" {
		return Task{}, errors.New("user_message is required")
	}
	m.mu.Lock()
	item, ok := m.tasks[taskID]
	if !ok {
		m.mu.Unlock()
		return Task{}, errors.New("agent task not found")
	}
	if item.task.Status != StatusRunning || item.task.PendingUserAction == nil {
		m.mu.Unlock()
		return Task{}, errors.New("agent task is not waiting for user action")
	}
	select {
	case m.queue <- taskID:
		item.task.PendingUserAction = nil
		item.actionNotified = false
		item.nextPrompt = userMessage
		item.resumeQueued = true
		item.task.UpdatedAt = m.now().UTC()
		m.observeLocked(item, "resumed")
		task := item.task
		m.mu.Unlock()
		return task, nil
	default:
		m.mu.Unlock()
		return Task{}, errors.New("agent task queue is full")
	}
}

func (m *Manager) TerminalNotifications() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.terminalChanged
}

// WakeNotifications signals once per new terminal task update. A standby
// consumer uses it to activate the foreground so a result is announced without
// waiting for the user to speak first. It is deliberately separate from
// TerminalNotifications: consuming a wake signal must never steal the drain
// signal from the foreground session that owns result delivery.
func (m *Manager) WakeNotifications() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.wakeChanged
}

// TerminalSequence counts terminal task updates produced so far. A standby
// consumer uses it as a watermark: a session records the sequence it is about
// to consume, so a batch activates the foreground once while an update the
// session never received can still be announced later.
func (m *Manager) TerminalSequence() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.terminalSeq
}

func (m *Manager) UserActionNotifications() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.actionChanged
}

func (m *Manager) DrainUserActionTasks() []Task {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []Task
	for _, item := range m.tasks {
		if item.task.Status == StatusRunning && item.task.PendingUserAction != nil && !item.actionNotified {
			item.actionNotified = true
			result = append(result, item.task)
		}
	}
	return result
}

// RestoreUserActionTasks makes user-action notifications eligible for delivery
// again when a foreground session ends before it could tell the user.
func (m *Manager) RestoreUserActionTasks(tasks []Task) {
	if m == nil || len(tasks) == 0 {
		return
	}
	ids := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		ids[task.ID] = struct{}{}
	}
	m.mu.Lock()
	changed := false
	for id := range ids {
		if item, ok := m.tasks[id]; ok && item.task.Status == StatusRunning && item.task.PendingUserAction != nil {
			item.actionNotified = false
			changed = true
		}
	}
	if changed {
		select {
		case m.actionChanged <- struct{}{}:
		default:
		}
	}
	m.mu.Unlock()
}

// PendingUserActionTasks returns all running tasks waiting for user action and
// makes them eligible for notification in a future foreground session.
func (m *Manager) PendingUserActionTasks() []Task {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []Task
	for _, item := range m.tasks {
		if item.task.Status == StatusRunning && item.task.PendingUserAction != nil {
			item.actionNotified = false
			result = append(result, item.task)
		}
	}
	return result
}

// DrainTerminalTasks claims all queued terminal updates for one foreground
// session. A claim remains outstanding until CompleteTaskUpdateDelivery
// acknowledges it, and RestoreTerminalTasks releases it if the session ends.
func (m *Manager) DrainTerminalTasks() []Task {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Task, 0, len(m.terminalNotifications))
	for id, state := range m.terminalNotifications {
		if state != terminalNotificationQueued {
			continue
		}
		item, ok := m.tasks[id]
		if !ok || !item.task.Status.terminal() {
			delete(m.terminalNotifications, id)
			continue
		}
		m.terminalNotifications[id] = terminalNotificationClaimed
		result = append(result, item.task)
	}
	sortTerminalTasks(result)
	return result
}

// BeginTaskUpdateDelivery resolves claimed snapshots against current manager
// state immediately before the foreground injects them. Cancel and Update can
// invalidate a snapshot after it was drained, so callers must only format the
// returned tasks.
func (m *Manager) BeginTaskUpdateDelivery(tasks []Task) []Task {
	if m == nil || len(tasks) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Task, 0, len(tasks))
	for _, snapshot := range tasks {
		item, ok := m.tasks[snapshot.ID]
		if !ok {
			continue
		}
		if snapshot.Status.terminal() {
			if m.terminalNotifications[snapshot.ID] != terminalNotificationClaimed {
				continue
			}
			m.terminalNotifications[snapshot.ID] = terminalNotificationDelivering
			result = append(result, item.task)
			continue
		}
		if item.task.Status == StatusRunning && item.task.PendingUserAction != nil && item.actionNotified {
			result = append(result, item.task)
		}
	}
	return result
}

// CompleteTaskUpdateDelivery acknowledges updates after the foreground has
// successfully submitted the response request to its realtime provider.
func (m *Manager) CompleteTaskUpdateDelivery(tasks []Task) {
	if m == nil || len(tasks) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, snapshot := range tasks {
		if snapshot.Status.terminal() {
			if m.terminalNotifications[snapshot.ID] == terminalNotificationDelivering {
				delete(m.terminalNotifications, snapshot.ID)
			}
			continue
		}
		// User-action updates remain marked as notified until the next session or
		// until the task changes state; they need no separate terminal-style ack.
	}
}

// TerminalState returns the terminal updates still waiting for delivery
// together with the sequence they were observed at, without consuming them.
// Both values are read under one lock so a standby consumer cannot combine an
// emptiness check with a sequence from a different moment: activating for a
// batch that is already gone would open an empty foreground session.
func (m *Manager) TerminalState() ([]Task, uint64) {
	if m == nil {
		return nil, 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Task, 0, len(m.terminalNotifications))
	for id, state := range m.terminalNotifications {
		if state != terminalNotificationQueued {
			continue
		}
		if item, ok := m.tasks[id]; ok && item.task.Status.terminal() {
			result = append(result, item.task)
		}
	}
	sortTerminalTasks(result)
	return result, m.terminalSeq
}

// RestoreTerminalTasks makes updates claimable again when a foreground session
// ends before it can inject them.
func (m *Manager) RestoreTerminalTasks(tasks []Task) {
	if m == nil || len(tasks) == 0 {
		return
	}
	m.mu.Lock()
	restored := false
	for _, task := range tasks {
		state := m.terminalNotifications[task.ID]
		if state != terminalNotificationClaimed && state != terminalNotificationDelivering {
			continue
		}
		item, ok := m.tasks[task.ID]
		if !ok || !item.task.Status.terminal() {
			delete(m.terminalNotifications, task.ID)
			continue
		}
		m.terminalNotifications[task.ID] = terminalNotificationQueued
		restored = true
	}
	if restored {
		m.signalTerminalLocked()
	}
	m.mu.Unlock()
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	for _, item := range m.tasks {
		switch item.task.Status {
		case StatusCreated, StatusQueued:
			m.finishLocked(item, StatusCancelled, "", "")
		case StatusRunning:
			if item.task.PendingUserAction != nil || item.cancel == nil {
				m.finishLocked(item, StatusCancelled, "", "")
				continue
			}
			item.task.Status = StatusCancelling
			item.task.UpdatedAt = m.now().UTC()
			if item.cancel != nil {
				item.cancel()
			}
		}
	}
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case taskID := <-m.queue:
			m.runTask(taskID)
		}
	}
}

func (m *Manager) runTask(taskID string) {
	for {
		m.mu.Lock()
		item, ok := m.tasks[taskID]
		if !ok || (item.task.Status != StatusQueued && (item.task.Status != StatusRunning || item.task.PendingUserAction != nil)) {
			m.mu.Unlock()
			return
		}
		if m.runner == nil {
			m.finishLocked(item, StatusFailed, "", "background agent is unavailable")
			m.mu.Unlock()
			return
		}
		taskCtx, cancel := context.WithCancel(m.ctx)
		startedAt := m.now().UTC()
		item.cancel = cancel
		item.resumeQueued = false
		item.task.Status = StatusRunning
		alreadyStarted := item.task.StartedAt != nil
		if item.task.StartedAt == nil {
			item.task.StartedAt = &startedAt
		}
		item.task.UpdatedAt = startedAt
		prompt := item.nextPrompt
		if prompt == "" {
			prompt = item.task.Prompt
		}
		item.nextPrompt = ""
		// A rerun that starts from a newer goal applies that revision just as a
		// steer would; observers waiting for goal_applied must see it.
		advanced := alreadyStarted && item.goalRevision > item.appliedRevision
		item.appliedRevision = item.goalRevision
		item.pendingSteer = nil
		item.steerSignal = make(chan struct{})
		m.observeLocked(item, "running")
		if advanced {
			m.observeLocked(item, "goal_applied")
		}
		m.mu.Unlock()

		runCtx := context.WithValue(taskCtx, taskContextKey{}, func() Observation { return m.ObservationSnapshot(taskID) })
		runCtx = WithUserActionHandler(runCtx, func(action UserAction) {
			m.mu.Lock()
			if current, exists := m.tasks[taskID]; exists && current.task.Status == StatusRunning && !current.resumeQueued {
				current.task.PendingUserAction = &action
				current.actionNotified = false
				current.task.UpdatedAt = m.now().UTC()
				m.observeLocked(current, "user_action_required")
				select {
				case m.actionChanged <- struct{}{}:
				default:
				}
			}
			m.mu.Unlock()
		})
		runCtx = withSteerControl(runCtx, func(context.Context) (SteerMessage, bool) {
			return m.pendingSteerMessage(taskID)
		}, func() <-chan struct{} {
			return m.steerInterrupt(taskID)
		}, func(message SteerMessage) {
			m.acknowledgeSteer(taskID, message)
		})
		result, err := m.runner.Run(runCtx, prompt)
		cancel()

		m.mu.Lock()
		item, ok = m.tasks[taskID]
		if !ok {
			m.mu.Unlock()
			return
		}
		item.cancel = nil
		if item.task.Status == StatusCancelled || item.task.Status == StatusFailed || item.task.Status == StatusCompleted {
			m.mu.Unlock()
			return
		}
		if item.task.Status == StatusCancelling || errors.Is(err, context.Canceled) {
			// Cancellation cannot undo side effects, so keep the runner's account
			// of what it already did for the foreground to report.
			m.finishLocked(item, StatusCancelled, limitText(result, maxResultRunes), "")
			m.mu.Unlock()
			return
		}
		if item.resumeQueued {
			m.mu.Unlock()
			return
		}
		if item.pendingSteer != nil || item.appliedRevision < item.goalRevision {
			item.task.PendingUserAction = nil
			item.actionNotified = false
			item.pendingSteer = nil
			item.nextPrompt = formatUpdatedTaskPrompt("", item.task.Prompt)
			m.mu.Unlock()
			continue
		}
		if item.task.PendingUserAction != nil {
			m.mu.Unlock()
			return
		}
		if err != nil {
			m.finishLocked(item, StatusFailed, "", err.Error())
			m.mu.Unlock()
			return
		}
		m.finishLocked(item, StatusCompleted, limitText(result, maxResultRunes), "")
		m.mu.Unlock()
		return
	}
}

func (m *Manager) finishLocked(item *entry, status Status, result, taskError string) {
	now := m.now().UTC()
	item.task.Status = status
	item.task.Result = result
	item.task.Error = taskError
	item.task.UpdatedAt = now
	item.task.CompletedAt = &now
	// A terminal task is never waiting for a user action. Clearing the field
	// keeps the snapshot honest in both directions: the foreground is not asked
	// to perform an action for work that already ended, and session teardown can
	// classify a restored update as terminal instead of dropping it as an
	// action request for a task that is no longer running.
	item.task.PendingUserAction = nil
	item.actionNotified = false
	item.pendingSteer = nil
	m.terminalSeq++
	m.terminalNotifications[item.task.ID] = terminalNotificationQueued
	m.observeLocked(item, "terminal")
	m.signalTerminalLocked()
	m.signalWakeLocked()
}

func (m *Manager) pendingSteerMessage(taskID string) (SteerMessage, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.tasks[taskID]
	if !ok || item.task.Status != StatusRunning || item.pendingSteer == nil {
		return SteerMessage{}, false
	}
	return *item.pendingSteer, true
}

func (m *Manager) acknowledgeSteer(taskID string, message SteerMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.tasks[taskID]
	if !ok || item.task.Status != StatusRunning {
		return
	}
	if message.Revision > item.appliedRevision {
		item.appliedRevision = message.Revision
		m.observeLocked(item, "goal_applied")
	}
	if item.pendingSteer != nil && item.pendingSteer.ID == message.ID {
		item.pendingSteer = nil
		item.steerSignal = make(chan struct{})
	}
}

func (m *Manager) updateGoalLocked(item *entry, prompt string) {
	item.task.Prompt = prompt
	item.goalRevision++
	item.task.GoalRevision = item.goalRevision
	item.task.UpdatedAt = m.now().UTC()
}

func (m *Manager) steerInterrupt(taskID string) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.tasks[taskID]
	if !ok || item.task.Status != StatusRunning {
		return nil
	}
	if item.steerSignal == nil {
		item.steerSignal = make(chan struct{})
	}
	return item.steerSignal
}

func (m *Manager) suppressTerminalNotificationLocked(taskID string) bool {
	state, ok := m.terminalNotifications[taskID]
	if !ok {
		return false
	}
	delete(m.terminalNotifications, taskID)
	return state == terminalNotificationQueued || state == terminalNotificationClaimed
}

func sortTerminalTasks(tasks []Task) {
	slices.SortFunc(tasks, func(a, b Task) int {
		if order := a.CompletedAt.Compare(*b.CompletedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func formatUpdatedTaskPrompt(oldPrompt, newPrompt string) string {
	var output strings.Builder
	output.WriteString("The task goal has been updated. Replace the previous goal with this complete new goal:\n")
	output.WriteString(strings.TrimSpace(newPrompt))
	if oldPrompt = strings.TrimSpace(oldPrompt); oldPrompt != "" {
		fmt.Fprintf(&output, "\n\nPrevious goal (no longer authoritative):\n%s", oldPrompt)
	}
	return output.String()
}

func (m *Manager) signalTerminalLocked() {
	select {
	case m.terminalChanged <- struct{}{}:
	default:
	}
}

func (m *Manager) signalWakeLocked() {
	select {
	case m.wakeChanged <- struct{}{}:
	default:
	}
}

func limitText(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…"
}
