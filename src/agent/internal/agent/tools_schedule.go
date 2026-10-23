package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aiden-agent/internal/agent/agentpath"
	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/mcu"
	"aiden-agent/internal/routines"
)

// StartRoutines installs the schedule tool before the daemon accepts requests.
// Dependency-injected and benchmark runtimes do not start hardware consumers.
func (r *Runtime) StartRoutines() error {
	if r.routines != nil {
		return nil
	}
	cfg := r.ConfigSnapshot()
	if cfg.ConfigDir == "" || r.tools == nil {
		return errors.New("routines require a configured runtime directory and tools")
	}
	m, err := routines.Open(filepath.Join(cfg.ConfigDir, "routines", "state.json"),
		mcu.Client{Socket: strings.TrimSpace(os.Getenv("AIDEN_MCU_SERVICE_SOCKET"))},
		func(ctx context.Context, routine routines.Routine) (string, error) {
			prompt := fmt.Sprintf("Scheduled task %s is due (scheduled for %s). Continue this task in the existing conversation, checking prior results and current device state before acting. Execute this occurrence now; do not create another schedule for it.\n\n%s", routine.ID, routine.RunAt.Format(time.RFC3339), routine.Task)
			result, err := r.Run(ctx, RunRequest{Input: prompt, WaitForIdle: true, ResumeSessionID: routine.SessionID, AsyncEpisodeMaintenance: true})
			return result.Output, err
		})
	if err != nil {
		return err
	}
	r.routines = m
	r.tools.tools["schedule"] = &ScheduleTool{manager: m, runtime: r, now: time.Now}
	m.Start()
	return nil
}

type ScheduleTool struct {
	manager *routines.Manager
	runtime *Runtime
	now     func() time.Time
}

func (t *ScheduleTool) Name() string { return "schedule" }

func (t *ScheduleTool) Description() string {
	return "Create, list, update, or cancel scheduled tasks and repeating routines. The MCU wakes the device and the task resumes in this same conversation. Use create with a complete task and either delay_s or run_at (RFC3339 with timezone). interval_s=0 runs once; a positive interval repeats from run_at, skipping missed occurrences. Use update with id and the complete replacement task and timing. List before creating to avoid duplicates. Check timer_error: a saved routine is not armed while that field is nonempty. Cancel stops future occurrences and requests cancellation of an active run."
}

func (t *ScheduleTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"action":     stringArgSchema("Required: create, list, update, or cancel."),
		"id":         stringArgSchema("Routine ID for update or cancel."),
		"task":       stringArgSchema("Complete task to execute when due, with enough detail to continue this conversation."),
		"run_at":     stringArgSchema("First execution time in RFC3339 with timezone; use either run_at or delay_s."),
		"delay_s":    minIntegerArgSchema("Seconds from now until first execution; use either delay_s or run_at. Maximum 604800.", 1),
		"interval_s": minIntegerArgSchema("Repeat interval in seconds (1..604800), or 0/omitted for one execution.", 0),
	}, "action")
}

func (t *ScheduleTool) Call(ctx context.Context, input string) (string, error) {
	var args struct {
		Action    string `json:"action"`
		ID        string `json:"id"`
		Task      string `json:"task"`
		RunAt     string `json:"run_at"`
		DelayS    int64  `json:"delay_s"`
		IntervalS int64  `json:"interval_s"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return "", err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", errors.New("expected one JSON object")
	}
	if t.manager == nil {
		return "", errors.New("routines are unavailable")
	}
	var result any
	switch args.Action {
	case "list":
		result, _ = t.manager.List()
	case "cancel":
		r, err := t.manager.Cancel(ctx, args.ID)
		if err != nil {
			return "", err
		}
		result = r
	case "create", "update":
		if (args.Action == "create" && args.ID != "") || (args.Action == "update" && args.ID == "") {
			return "", errors.New("provide id only for update or cancel")
		}
		if args.DelayS < 0 || args.DelayS > 604800 || (args.DelayS == 0) == (args.RunAt == "") {
			return "", errors.New("provide exactly one of delay_s (1..604800) or run_at")
		}
		cfg := t.runtime.ConfigSnapshot()
		if cfg.InputModeOrDefault() == "realtime" && !cfg.VoiceModel.UseBackendAgent {
			return "", errors.New("scheduled execution requires the backend agent to be enabled")
		}
		at := t.now().Add(time.Duration(args.DelayS) * time.Second)
		if args.RunAt != "" {
			var err error
			at, err = time.Parse(time.RFC3339, args.RunAt)
			if err != nil {
				return "", fmt.Errorf("run_at must include a timezone: %w", err)
			}
		}
		sessionID := contextmanager.CurrentSessionID(agentpath.ContextManagerSessionFolder(cfg.ConfigDir))
		r, err := t.manager.Save(ctx, routines.Routine{ID: args.ID, Task: args.Task, SessionID: sessionID, RunAt: at, IntervalS: args.IntervalS})
		if err != nil {
			return "", err
		}
		result = r
	default:
		return "", errors.New("action must be create, list, update, or cancel")
	}
	_, timerErr := t.manager.List()
	return encodeToolJSON(map[string]any{"result": result, "timer_error": timerErr})
}
