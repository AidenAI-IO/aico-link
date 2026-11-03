package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/executor"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/logging"
)

var (
	errRunPreempted = errors.New("another run took over the backend runtime")
	errRunPanicked  = errors.New("backend run panicked")
)

const pendingBackendRunFile = ".pending-run.json"

// runNotices belongs to the serialized execution, not to its HTTP/audio output.
// Its manager accessor follows pruning/compaction revisions. Persistence must
// remain usable after the execution context has already been canceled.
type runNotices struct {
	manager  func() *contextmanager.ContextManager
	journal  *pendingBackendRun
	terminal bool
	finished bool
}

type pendingBackendRun struct {
	RunID     string            `json:"run_id"`
	SessionID string            `json:"session_id"`
	Completed bool              `json:"completed,omitempty"`
	Notice    *messages.Message `json:"notice,omitempty"`
}

func interruptNotice(reason, detail string) messages.Message {
	return messages.Message{
		Role:    messages.MessageRoleNotice,
		Content: fmt.Sprintf("Interrupt [%s]: %s Execution may have partially changed the device. Check the recorded tool results and current state before resuming or repeating actions.", reason, detail),
	}
}

func (n *runNotices) stop(reason, detail string) error {
	if n.terminal {
		return nil
	}
	message := interruptNotice(reason, detail)
	manager := n.manager()
	if n.journal != nil {
		message.Content += "\nRun: " + n.journal.RunID
		n.journal.SessionID = manager.GetSessionID()
		n.journal.Notice = &message
		if err := savePendingBackendRun(manager.GetSessionFolder(), n.journal); err != nil {
			return err
		}
	}
	if err := manager.AppendMessage(message); err != nil {
		return fmt.Errorf("persist interrupt notice: %w", err)
	}
	n.terminal = true
	return nil
}

// Called directly as a defer so panic unwinding cannot mark a run completed.
// Re-panic after recording: this does not change the caller's panic policy.
func (n *runNotices) finishOnReturn(ctx context.Context, runErr *error) {
	if value := recover(); value != nil {
		if err := n.finish(ctx, errRunPanicked); err != nil {
			logging.Errorf("agent", "interrupt", "persist panic notice: %v", err)
		}
		panic(value)
	}
	if err := n.finish(ctx, *runErr); err != nil {
		*runErr = errors.Join(*runErr, err)
	}
}

func (n *runNotices) finish(ctx context.Context, runErr error) error {
	if n.finished {
		return nil
	}
	n.finished = true
	if runErr != nil && !n.terminal {
		reason, detail := runInterruptionReason(ctx, runErr)
		if err := n.stop(reason, detail); err != nil {
			return err // Keep the journal for recovery before the next run.
		}
	}
	if n.journal == nil {
		return nil
	}
	folder := n.manager().GetSessionFolder()
	// A completed marker prevents a failed unlink from turning successful work
	// or an intentional pause into a restart interruption on the next request.
	n.journal.Completed = true
	if err := savePendingBackendRun(folder, n.journal); err != nil {
		return err
	}
	return removePendingBackendRun(folder)
}

func runInterruptionReason(ctx context.Context, err error) (string, string) {
	switch {
	case errors.Is(err, errRunPanicked):
		return "panic", "Execution stopped unexpectedly because of a runtime panic; task completion is not confirmed."
	case errors.Is(context.Cause(ctx), errRunPreempted):
		return "preempted", "Another request took over the runtime before this run completed."
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded", "This run stopped because its deadline was exceeded; task completion is not confirmed."
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return "canceled", "This run was canceled before completion (user cancellation or runtime shutdown); task completion is not confirmed."
	default:
		var modelErr *executor.LLMCallError
		if errors.As(err, &modelErr) {
			return "model_error", "A model request failed and this run ended without completing its response."
		}
		return "execution_error", "This run ended because of an execution or context-processing error; task completion is not confirmed."
	}
}

func startRunNotices(manager func() *contextmanager.ContextManager, runID string) (*runNotices, error) {
	n := &runNotices{manager: manager, journal: &pendingBackendRun{
		RunID: runID, SessionID: manager().GetSessionID(),
	}}
	if err := savePendingBackendRun(manager().GetSessionFolder(), n.journal); err != nil {
		return nil, err
	}
	return n, nil
}

// recoverPendingBackendRunManager is called under Runtime's run gate, before
// adding another user input or rotating the conversation. A hard kill cannot
// execute a defer, so an unfinished journal is resolved here instead. It returns
// the context to continue using; recovery of an unrelated conversation must not
// replace the active conversation.
func recoverPendingBackendRunManager(folder string, live *contextmanager.ContextManager) (*contextmanager.ContextManager, error) {
	data, err := os.ReadFile(filepath.Join(folder, pendingBackendRunFile))
	if os.IsNotExist(err) {
		return live, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pending backend run: %w", err)
	}
	var pending pendingBackendRun
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("decode pending backend run: %w", err)
	}
	if pending.Completed {
		return live, removePendingBackendRun(folder)
	}
	if pending.RunID == "" || !safeRunSessionID(pending.SessionID) {
		return nil, errors.New("invalid pending backend run identity")
	}
	manager := live
	if manager == nil {
		sessionID := contextmanager.CurrentSessionID(folder)
		if sessionID == "" {
			sessionID = pending.SessionID
		}
		if _, err := os.Stat(filepath.Join(folder, sessionID+".jsonl")); os.IsNotExist(err) {
			return nil, removePendingBackendRun(folder)
		} else if err != nil {
			return nil, err
		}
		manager, err = contextmanager.LoadContextManagerFromSessionID(folder, sessionID)
		if err != nil {
			return nil, fmt.Errorf("load interrupted context: %w", err)
		}
	}
	activeManager := manager
	resumePending := false
	// Walk lineage without mutating a different live context. A cleared/rotated
	// history must stay clear.
	ancestor := manager
	seen := map[string]bool{}
	for ancestor.GetSessionID() != pending.SessionID {
		parent := ancestor.GetParentSessionID()
		if parent == pending.SessionID {
			break
		}
		if parent == "" || seen[parent] {
			// Clearing history can leave a journal after removing its transcript.
			if _, err := os.Stat(filepath.Join(folder, pending.SessionID+".jsonl")); os.IsNotExist(err) {
				return nil, removePendingBackendRun(folder)
			} else if err != nil {
				return nil, err
			}
			manager, err = contextmanager.LoadContextManagerFromSessionID(folder, pending.SessionID)
			// Only repair the old transcript; keep the active conversation.
			break
		}
		if !safeRunSessionID(parent) {
			return nil, errors.New("invalid interrupted context lineage")
		}
		seen[parent] = true
		next, loadErr := contextmanager.LoadContextManagerFromSessionID(folder, parent)
		if loadErr != nil {
			// A damaged intermediate compaction revision must not strand every
			// later run. Fall back to the pending session, which is the last
			// confirmed transcript for this run. Keep the existence check first
			// so history clearing does not recreate a deleted conversation.
			pendingPath := filepath.Join(folder, pending.SessionID+".jsonl")
			if _, statErr := os.Stat(pendingPath); os.IsNotExist(statErr) {
				return nil, removePendingBackendRun(folder)
			} else if statErr != nil {
				return nil, statErr
			}
			manager, err = contextmanager.LoadContextManagerFromSessionID(folder, pending.SessionID)
			if err != nil {
				return nil, fmt.Errorf("load pending interrupted context: %w", err)
			}
			resumePending = true
			break
		}
		ancestor = next
	}
	if err != nil {
		return nil, fmt.Errorf("load interrupted context lineage: %w", err)
	}
	// Do not recreate a transcript intentionally removed by history clearing.
	if _, err := os.Stat(filepath.Join(folder, manager.GetSessionID()+".jsonl")); os.IsNotExist(err) {
		return nil, removePendingBackendRun(folder)
	} else if err != nil {
		return nil, err
	}
	message := interruptNotice("agent_restart", "The previous backend run has no recorded end; the process may have stopped or restarted. Task completion is unknown.")
	message.Content += "\nRun: " + pending.RunID
	if pending.Notice != nil {
		message = *pending.Notice
	}
	noticeExists := false
	for _, existing := range manager.CloneMessageList() {
		if existing.Role == messages.MessageRoleNotice && existing.Content == message.Content {
			noticeExists = true
			break
		}
	}
	if !noticeExists {
		if err := manager.AppendMessage(message); err != nil {
			return nil, fmt.Errorf("recover interrupted backend run: %w", err)
		}
	}
	if resumePending {
		// Keep the active session and its on-disk pointer in sync before removing
		// the journal, so another restart retains the recovered context.
		if err := activeManager.Activate(manager); err != nil {
			return nil, fmt.Errorf("activate recovered backend context: %w", err)
		}
	}
	return activeManager, removePendingBackendRun(folder)
}

func safeRunSessionID(id string) bool {
	return strings.HasPrefix(id, "s_") && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func savePendingBackendRun(folder string, pending *pendingBackendRun) error {
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(folder, ".pending-run-*")
	if err != nil {
		return fmt.Errorf("create pending backend run: %w", err)
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write pending backend run: %w", err)
	}
	if err := os.Rename(file.Name(), filepath.Join(folder, pendingBackendRunFile)); err != nil {
		return fmt.Errorf("install pending backend run: %w", err)
	}
	return syncDir(folder)
}

func removePendingBackendRun(folder string) error {
	err := os.Remove(filepath.Join(folder, pendingBackendRunFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(folder)
}
