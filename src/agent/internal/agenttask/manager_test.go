package agenttask

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"aico-agent/internal/stream"
)

type fakeRunner struct {
	mu           sync.Mutex
	started      chan string
	release      chan struct{}
	result       string
	err          error
	cancelResult string
}

type recordedRun struct {
	prompt            string
	steerProvider     func(context.Context) (SteerMessage, bool)
	interruptProvider func() <-chan struct{}
	acknowledgeSteer  func(SteerMessage)
}

type steerAwareRunner struct {
	started chan recordedRun
	results chan string
}

func (r *steerAwareRunner) Run(ctx context.Context, prompt string) (string, error) {
	r.started <- recordedRun{
		prompt:            prompt,
		steerProvider:     SteerProviderFromContext(ctx),
		interruptProvider: SteerInterruptFromContext(ctx),
		acknowledgeSteer:  SteerAcknowledgerFromContext(ctx),
	}
	select {
	case result := <-r.results:
		return result, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (r *fakeRunner) Run(ctx context.Context, prompt string) (string, error) {
	if r.started != nil {
		r.started <- prompt
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return r.cancelResult, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result, r.err
}

func TestManagerCompletesQueuedTask(t *testing.T) {
	runner := &fakeRunner{started: make(chan string, 1), release: make(chan struct{}), result: "done"}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()

	task, err := manager.Create("operate the device")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusQueued {
		t.Fatalf("initial status = %q, want queued", task.Status)
	}
	select {
	case got := <-runner.started:
		if got != task.Prompt {
			t.Fatalf("runner input = %q, want %q", got, task.Prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	close(runner.release)
	waitForStatus(t, manager, task.ID, StatusCompleted)

	terminal := waitForTerminalTasks(t, manager)
	if len(terminal) != 1 || terminal[0].ID != task.ID || terminal[0].Result != "done" {
		t.Fatalf("terminal tasks = %+v", terminal)
	}
}

func TestManagerCancelsRunningTask(t *testing.T) {
	// A cancelled run keeps its account of effects that already happened.
	partial := CancelledResult([]CompletedStep{{Tool: "bridge_clipboard", Input: `{"action":"write"}`, Output: "ok"}})
	runner := &fakeRunner{started: make(chan string, 1), release: make(chan struct{}), cancelResult: partial}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()

	task, err := manager.Create("long task")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	cancelled, _, err := manager.Cancel(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != StatusCancelling {
		t.Fatalf("cancel status = %q, want cancelling", cancelled.Status)
	}
	waitForStatus(t, manager, task.ID, StatusCancelled)
	terminal := waitForTerminalTasks(t, manager)
	if len(terminal) != 1 || terminal[0].Result != partial ||
		!strings.Contains(partial, "not undone") || !strings.Contains(partial, "bridge_clipboard") {
		t.Fatalf("cancelled result = %+v", terminal)
	}
	if CancelledResult(nil) != "" {
		t.Fatal("a run with no completed steps must report no result")
	}
}

func TestManagerMarksRunnerFailure(t *testing.T) {
	manager := newManager(&fakeRunner{err: errors.New("boom")}, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("fail")
	if err != nil {
		t.Fatal(err)
	}
	got := waitForStatus(t, manager, task.ID, StatusFailed)
	if got.Error != "boom" {
		t.Fatalf("error = %q, want boom", got.Error)
	}
}

func TestManagerCancelsQueuedTaskWithoutRunningIt(t *testing.T) {
	runner := &fakeRunner{started: make(chan string, 2), release: make(chan struct{})}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()

	first, err := manager.Create("first")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first task did not start")
	}
	second, err := manager.Create("second")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Cancel(second.ID); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, second.ID, StatusCancelled)
	close(runner.release)
	waitForStatus(t, manager, first.ID, StatusCompleted)

	select {
	case prompt := <-runner.started:
		t.Fatalf("cancelled queued task unexpectedly ran: %q", prompt)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestManagerOutstandingTracksInFlightAndUndeliveredWork(t *testing.T) {
	runner := &fakeRunner{started: make(chan string, 2), release: make(chan struct{})}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()

	first, err := manager.Create("first")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first task did not start")
	}
	second, err := manager.Create("second")
	if err != nil {
		t.Fatal(err)
	}

	// Work in flight is outstanding, newest first, so a caller checking before
	// starting something new sees the duplicate it just created.
	outstanding := manager.Outstanding()
	if len(outstanding) != 2 || outstanding[0].ID != second.ID || outstanding[1].ID != first.ID {
		t.Fatalf("outstanding = %+v, want %s then %s", outstanding, second.ID, first.ID)
	}

	// A finished task stays outstanding until its result has actually been
	// handed to the foreground: the caller must not start the same work again
	// while the result is still on its way.
	close(runner.release)
	waitForStatus(t, manager, first.ID, StatusCompleted)
	waitForStatus(t, manager, second.ID, StatusCompleted)
	if outstanding := manager.Outstanding(); len(outstanding) != 2 {
		t.Fatalf("outstanding before delivery = %+v, want both results", outstanding)
	}

	claimed := manager.DrainTerminalTasks()
	if outstanding := manager.Outstanding(); len(outstanding) != 2 {
		t.Fatalf("outstanding while claimed by foreground = %+v, want both results", outstanding)
	}
	delivering := manager.BeginTaskUpdateDelivery(claimed)
	manager.CompleteTaskUpdateDelivery(delivering)
	if outstanding := manager.Outstanding(); len(outstanding) != 0 {
		t.Fatalf("outstanding after delivery = %+v, want none", outstanding)
	}
}

func TestManagerRestoresUndeliveredTerminalTasks(t *testing.T) {
	manager := newManager(&fakeRunner{result: "done"}, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("task")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, task.ID, StatusCompleted)
	terminal := waitForTerminalTasks(t, manager)
	manager.RestoreTerminalTasks(terminal)
	restored := waitForTerminalTasks(t, manager)
	if len(restored) != 1 || restored[0].ID != task.ID {
		t.Fatalf("restored tasks = %+v", restored)
	}
}

func TestManagerCancelSuppressesQueuedTerminalDelivery(t *testing.T) {
	manager := newManager(&fakeRunner{result: "done"}, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("task")
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForStatus(t, manager, task.ID, StatusCompleted)
	if _, cleared, err := manager.Cancel(task.ID); err != nil {
		t.Fatal(err)
	} else if !cleared {
		t.Fatal("queued terminal delivery was not reported as cleared")
	}
	if got, _ := manager.Query(task.ID); got.Status != StatusCompleted {
		t.Fatalf("cancel changed completed task status to %q", got.Status)
	}
	if deliverable := manager.BeginTaskUpdateDelivery([]Task{completed}); len(deliverable) != 0 {
		t.Fatalf("cancelled terminal delivery is still deliverable: %+v", deliverable)
	}
	if terminal := manager.DrainTerminalTasks(); len(terminal) != 0 {
		t.Fatalf("cancelled terminal delivery remained queued: %+v", terminal)
	}
}

func TestManagerCancelSuppressesClaimedTerminalDelivery(t *testing.T) {
	manager := newManager(&fakeRunner{result: "done"}, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("task")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, task.ID, StatusCompleted)
	claimed := waitForTerminalTasks(t, manager)
	if _, cleared, err := manager.Cancel(task.ID); err != nil {
		t.Fatal(err)
	} else if !cleared {
		t.Fatal("claimed terminal delivery was not reported as cleared")
	}
	if deliverable := manager.BeginTaskUpdateDelivery(claimed); len(deliverable) != 0 {
		t.Fatalf("cancelled claimed delivery is still deliverable: %+v", deliverable)
	}
	manager.RestoreTerminalTasks(claimed)
	if terminal := manager.DrainTerminalTasks(); len(terminal) != 0 {
		t.Fatalf("cancelled claimed delivery was restored: %+v", terminal)
	}
}

func TestManagerCancelDuringTerminalDeliveryPreventsRestore(t *testing.T) {
	manager := newManager(&fakeRunner{result: "done"}, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("task")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, task.ID, StatusCompleted)
	claimed := waitForTerminalTasks(t, manager)
	delivering := manager.BeginTaskUpdateDelivery(claimed)
	if len(delivering) != 1 || delivering[0].ID != task.ID {
		t.Fatalf("delivering tasks = %+v, want %s", delivering, task.ID)
	}
	if _, cleared, err := manager.Cancel(task.ID); err != nil {
		t.Fatal(err)
	} else if cleared {
		t.Fatal("delivery already in progress was reported as pending and cleared")
	}
	manager.RestoreTerminalTasks(delivering)
	if terminal := manager.DrainTerminalTasks(); len(terminal) != 0 {
		t.Fatalf("cancelled in-progress delivery was restored: %+v", terminal)
	}
}

func TestManagerCancelSuppressesOnlyMatchingClaimedDelivery(t *testing.T) {
	manager := newManager(&fakeRunner{result: "done"}, 4, time.Now)
	defer manager.Close()
	first, err := manager.Create("first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create("second")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, first.ID, StatusCompleted)
	waitForStatus(t, manager, second.ID, StatusCompleted)
	claimed := waitForTerminalTasks(t, manager)
	if _, _, err := manager.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	delivering := manager.BeginTaskUpdateDelivery(claimed)
	if len(delivering) != 1 || delivering[0].ID != second.ID {
		t.Fatalf("deliverable tasks = %+v, want only %s", delivering, second.ID)
	}
}

func TestManagerUpdateQueuedTaskUsesLatestGoal(t *testing.T) {
	runner := &fakeRunner{started: make(chan string, 2), release: make(chan struct{})}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()
	first, err := manager.Create("block worker")
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	task, err := manager.Create("old goal")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := manager.Update(task.ID, "new goal")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Prompt != "new goal" || updated.Status != StatusQueued {
		t.Fatalf("updated task = %+v", updated)
	}
	close(runner.release)
	waitForStatus(t, manager, first.ID, StatusCompleted)
	select {
	case prompt := <-runner.started:
		if prompt != "new goal" {
			t.Fatalf("runner prompt = %q, want new goal", prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("updated queued task did not run")
	}
}

func TestManagerUpdateRunningTaskPublishesSteer(t *testing.T) {
	runner := &steerAwareRunner{started: make(chan recordedRun, 1), results: make(chan string, 1)}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("old goal")
	if err != nil {
		t.Fatal(err)
	}
	run := <-runner.started
	interrupt := run.interruptProvider()
	if _, err := manager.Update(task.ID, "new goal"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-interrupt:
	case <-time.After(time.Second):
		t.Fatal("update did not interrupt the running task")
	}
	steer, ok := run.steerProvider(context.Background())
	if !ok || !strings.Contains(steer.Content, "new goal") {
		t.Fatalf("steer = %q, ok=%v, want updated goal", steer.Content, ok)
	}
	peekedAgain, ok := run.steerProvider(context.Background())
	if !ok || peekedAgain.ID != steer.ID {
		t.Fatalf("steer was consumed before acknowledgement: first=%+v second=%+v ok=%v", steer, peekedAgain, ok)
	}
	run.acknowledgeSteer(steer)
	if _, ok := run.steerProvider(context.Background()); ok {
		t.Fatal("acknowledged steer remained pending")
	}
	runner.results <- "updated result"
	completed := waitForStatus(t, manager, task.ID, StatusCompleted)
	if completed.Prompt != "new goal" || completed.Result != "updated result" {
		t.Fatalf("completed task = %+v", completed)
	}
}

func TestManagerRunningUpdatesAreLatestWins(t *testing.T) {
	runner := &steerAwareRunner{started: make(chan recordedRun, 1), results: make(chan string, 1)}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("old goal")
	if err != nil {
		t.Fatal(err)
	}
	run := <-runner.started
	interrupt := run.interruptProvider()
	if _, err := manager.Update(task.ID, "intermediate goal"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update(task.ID, "final goal"); err != nil {
		t.Fatal(err)
	}
	<-interrupt
	steer, ok := run.steerProvider(context.Background())
	if !ok || !strings.Contains(steer.Content, "complete new goal:\nfinal goal") {
		t.Fatalf("latest steer = %q, ok=%v", steer.Content, ok)
	}
	run.acknowledgeSteer(steer)
	runner.results <- "done"
	waitForStatus(t, manager, task.ID, StatusCompleted)
}

type revisionRunner struct {
	started chan string
	release chan struct{}
	mu      sync.Mutex
	runs    int
}

func (r *revisionRunner) Run(ctx context.Context, prompt string) (string, error) {
	r.mu.Lock()
	r.runs++
	run := r.runs
	r.mu.Unlock()
	r.started <- prompt
	if run == 1 {
		select {
		case <-r.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "stale result", nil
	}
	return "fresh result", nil
}

func TestManagerRerunsWhenUpdateWasNotConsumed(t *testing.T) {
	runner := &revisionRunner{started: make(chan string, 2), release: make(chan struct{})}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()
	sink := &phaseSink{}
	manager.SetEventSink(sink)
	task, err := manager.Create("old goal")
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	if _, err := manager.Update(task.ID, "new goal"); err != nil {
		t.Fatal(err)
	}
	close(runner.release)
	select {
	case prompt := <-runner.started:
		if !strings.Contains(prompt, "new goal") {
			t.Fatalf("rerun prompt = %q, want updated goal", prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("task did not rerun for the unconsumed update")
	}
	completed := waitForStatus(t, manager, task.ID, StatusCompleted)
	if completed.Result != "fresh result" {
		t.Fatalf("task completed with stale result: %+v", completed)
	}
	// The rerun applied revision 2; observers waiting for it must see goal_applied.
	if applied := sink.appliedRevisions("goal_applied"); len(applied) != 1 || applied[0] != 2 {
		t.Fatalf("goal_applied revisions = %v, want [2]", applied)
	}
}

type phaseSink struct {
	mu     sync.Mutex
	events []stream.Event
}

func (s *phaseSink) Emit(_ context.Context, event stream.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *phaseSink) appliedRevisions(phase string) []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var revisions []uint64
	for _, event := range s.events {
		if payload, ok := event.Payload.(map[string]any); ok && payload["phase"] == phase {
			revisions = append(revisions, event.Collaboration.AppliedRevision)
		}
	}
	return revisions
}

type userActionRunner struct {
	started chan string
}

func (r *userActionRunner) Run(ctx context.Context, prompt string) (string, error) {
	r.started <- prompt
	if prompt == "open the app" {
		handler := UserActionHandlerFromContext(ctx)
		if handler == nil {
			return "", errors.New("user action handler missing")
		}
		handler(UserAction{Reason: "authentication", Details: "Login is required", SuggestedAction: "Sign in on the device"})
	}
	return "done", nil
}

func TestManagerPausesAndContinuesAfterUserAction(t *testing.T) {
	runner := &userActionRunner{started: make(chan string, 2)}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()

	task, err := manager.Create("open the app")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case prompt := <-runner.started:
		if prompt != task.Prompt {
			t.Fatalf("first prompt = %q, want %q", prompt, task.Prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}

	var paused Task
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		paused, _ = manager.Query(task.ID)
		if paused.Status == StatusRunning && paused.PendingUserAction != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if paused.Status != StatusRunning || paused.PendingUserAction == nil {
		t.Fatalf("paused task = %+v", paused)
	}
	if _, err := manager.Continue(task.ID, "用户已完成登录并回到首页"); err != nil {
		t.Fatal(err)
	}
	select {
	case prompt := <-runner.started:
		if prompt != "用户已完成登录并回到首页" {
			t.Fatalf("continuation prompt = %q", prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("task did not resume")
	}
	completed := waitForStatus(t, manager, task.ID, StatusCompleted)
	if completed.Result != "done" || completed.PendingUserAction != nil {
		t.Fatalf("completed task = %+v", completed)
	}
}

func TestManagerUpdatePreservesQueuedUserActionContinuation(t *testing.T) {
	const (
		taskID      = "task-paused"
		oldGoal     = "open the app"
		newGoal     = "open settings after login"
		userMessage = "用户已完成登录并回到首页"
	)
	manager := &Manager{
		now: time.Now,
		tasks: map[string]*entry{
			taskID: {
				task:         Task{ID: taskID, Prompt: oldGoal, Status: StatusRunning},
				nextPrompt:   userMessage,
				resumeQueued: true,
			},
		},
	}

	if _, err := manager.Update(taskID, newGoal); err != nil {
		t.Fatal(err)
	}
	got := manager.tasks[taskID].nextPrompt
	if !strings.HasPrefix(got, userMessage+"\n\n") {
		t.Fatalf("queued prompt lost user-action continuation: %q", got)
	}
	if !strings.Contains(got, "complete new goal:\n"+newGoal) {
		t.Fatalf("queued prompt lost updated goal: %q", got)
	}
}

func TestManagerCancelsPausedUserActionTask(t *testing.T) {
	runner := &userActionRunner{started: make(chan string, 1)}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("open the app")
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	waitForPendingUserAction(t, manager, task.ID)
	if _, _, err := manager.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, task.ID, StatusCancelled)
}

// A task that paused for a user action and then reached a terminal state must
// not carry the stale action. Otherwise the foreground is told to perform an
// action for finished work, and session teardown misclassifies the terminal
// update as an action request, which silently drops it.
func TestManagerCancelledPausedTaskClearsPendingUserAction(t *testing.T) {
	runner := &userActionRunner{started: make(chan string, 1)}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("open the app")
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	waitForPendingUserAction(t, manager, task.ID)
	cancelled, _, err := manager.Cancel(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.PendingUserAction != nil {
		t.Fatalf("cancel snapshot still carries pending user action: %+v", cancelled.PendingUserAction)
	}
	terminal := waitForTerminalTasks(t, manager)
	if len(terminal) != 1 || terminal[0].Status != StatusCancelled {
		t.Fatalf("terminal = %+v", terminal)
	}
	if terminal[0].PendingUserAction != nil {
		t.Fatalf("terminal task still carries pending user action: %+v", terminal[0].PendingUserAction)
	}
	if queried, _ := manager.Query(task.ID); queried.PendingUserAction != nil {
		t.Fatalf("queried task still carries pending user action: %+v", queried.PendingUserAction)
	}
	// A restored terminal update must stay terminal and stay deliverable.
	manager.RestoreTerminalTasks(terminal)
	restored := waitForTerminalTasks(t, manager)
	if len(restored) != 1 || restored[0].ID != task.ID {
		t.Fatalf("restored = %+v", restored)
	}
	if restored[0].PendingUserAction != nil {
		t.Fatalf("restored task carries pending user action: %+v", restored[0].PendingUserAction)
	}
}

func TestManagerUpdatePausedTaskClearsOldActionAndResumes(t *testing.T) {
	runner := &userActionRunner{started: make(chan string, 2)}
	manager := newManager(runner, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("open the app")
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	waitForPendingUserAction(t, manager, task.ID)
	claimedAction := manager.DrainUserActionTasks()
	updated, err := manager.Update(task.ID, "open settings instead")
	if err != nil {
		t.Fatal(err)
	}
	if updated.PendingUserAction != nil || updated.Prompt != "open settings instead" {
		t.Fatalf("updated paused task = %+v", updated)
	}
	if deliverable := manager.BeginTaskUpdateDelivery(claimedAction); len(deliverable) != 0 {
		t.Fatalf("superseded user action is still deliverable: %+v", deliverable)
	}
	select {
	case prompt := <-runner.started:
		if !strings.Contains(prompt, "complete new goal:\nopen settings instead") {
			t.Fatalf("resumed prompt = %q", prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("updated paused task did not resume")
	}
	completed := waitForStatus(t, manager, task.ID, StatusCompleted)
	if completed.PendingUserAction != nil || completed.Prompt != "open settings instead" {
		t.Fatalf("completed updated task = %+v", completed)
	}
}

func TestManagerTerminalStateDoesNotConsume(t *testing.T) {
	manager := newManager(&fakeRunner{result: "done"}, 4, time.Now)
	defer manager.Close()
	task, err := manager.Create("task")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, task.ID, StatusCompleted)
	pending, seq := manager.TerminalState()
	if len(pending) != 1 || pending[0].ID != task.ID {
		t.Fatalf("pending = %+v", pending)
	}
	if seq != 1 {
		t.Fatalf("sequence = %d, want 1", seq)
	}
	// Peeking must leave the update for the foreground session to drain.
	if again, _ := manager.TerminalState(); len(again) != 1 {
		t.Fatalf("second peek = %+v", again)
	}
	if terminal := waitForTerminalTasks(t, manager); len(terminal) != 1 {
		t.Fatalf("terminal = %+v", terminal)
	}
	pending, seq = manager.TerminalState()
	if len(pending) != 0 {
		t.Fatalf("pending after drain = %+v", pending)
	}
	if seq != 1 {
		t.Fatalf("sequence after drain = %d, want 1", seq)
	}
}

func TestManagerTerminalTasksUseCompletionTimeThenID(t *testing.T) {
	earlier := time.Date(2026, time.September, 22, 10, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Second)
	manager := &Manager{
		tasks: map[string]*entry{
			"later-b": {task: Task{ID: "later-b", Status: StatusCompleted, CompletedAt: &later}},
			"earlier": {task: Task{ID: "earlier", Status: StatusCompleted, CompletedAt: &earlier}},
			"later-a": {task: Task{ID: "later-a", Status: StatusCompleted, CompletedAt: &later}},
		},
		terminalNotifications: map[string]terminalNotificationState{
			"later-b": terminalNotificationQueued,
			"earlier": terminalNotificationQueued,
			"later-a": terminalNotificationQueued,
		},
	}

	want := []string{"earlier", "later-a", "later-b"}
	assertOrder := func(name string, tasks []Task) {
		t.Helper()
		if len(tasks) != len(want) {
			t.Fatalf("%s() returned %d tasks, want %d: %+v", name, len(tasks), len(want), tasks)
		}
		for i, task := range tasks {
			if task.ID != want[i] {
				t.Fatalf("%s()[%d].ID = %q, want %q; tasks = %+v", name, i, task.ID, want[i], tasks)
			}
		}
	}

	pending, _ := manager.TerminalState()
	assertOrder("TerminalState", pending)
	assertOrder("DrainTerminalTasks", manager.DrainTerminalTasks())
}

func TestManagerSignalsWakeAndSequencePerTerminalTask(t *testing.T) {
	manager := newManager(&fakeRunner{result: "done"}, 4, time.Now)
	defer manager.Close()
	if seq := manager.TerminalSequence(); seq != 0 {
		t.Fatalf("initial sequence = %d, want 0", seq)
	}
	task, err := manager.Create("task")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, task.ID, StatusCompleted)
	select {
	case <-manager.WakeNotifications():
	case <-time.After(time.Second):
		t.Fatal("terminal task did not signal a standby wake")
	}
	if seq := manager.TerminalSequence(); seq != 1 {
		t.Fatalf("sequence = %d, want 1", seq)
	}
	// Draining the wake signal must not steal the foreground's drain signal.
	terminal := waitForTerminalTasks(t, manager)
	if len(terminal) != 1 || terminal[0].ID != task.ID {
		t.Fatalf("terminal = %+v", terminal)
	}
}

func TestManagerWakeSignalCoalescesWithoutAdvancingForDelivery(t *testing.T) {
	manager := newManager(&fakeRunner{result: "done"}, 4, time.Now)
	defer manager.Close()
	for _, prompt := range []string{"first", "second"} {
		if _, err := manager.Create(prompt); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if manager.TerminalSequence() == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if seq := manager.TerminalSequence(); seq != 2 {
		t.Fatalf("sequence = %d, want 2", seq)
	}
	select {
	case <-manager.WakeNotifications():
	case <-time.After(time.Second):
		t.Fatal("terminal tasks did not signal a standby wake")
	}
	// One coalesced signal is enough: a second read must not report new work.
	select {
	case <-manager.WakeNotifications():
		t.Fatal("wake signal did not coalesce")
	default:
	}
}

func waitForPendingUserAction(t *testing.T, manager *Manager, taskID string) Task {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if task, ok := manager.Query(taskID); ok && task.PendingUserAction != nil {
			return task
		}
		time.Sleep(time.Millisecond)
	}
	task, _ := manager.Query(taskID)
	t.Fatalf("task did not pause for user action: %+v", task)
	return Task{}
}

func waitForStatus(t *testing.T, manager *Manager, taskID string, want Status) Task {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if task, ok := manager.Query(taskID); ok && task.Status == want {
			return task
		}
		time.Sleep(time.Millisecond)
	}
	task, _ := manager.Query(taskID)
	t.Fatalf("task status = %q, want %q", task.Status, want)
	return Task{}
}

func waitForTerminalTasks(t *testing.T, manager *Manager) []Task {
	t.Helper()
	select {
	case <-manager.TerminalNotifications():
		return manager.DrainTerminalTasks()
	case <-time.After(time.Second):
		t.Fatal("terminal task notification timeout")
		return nil
	}
}
