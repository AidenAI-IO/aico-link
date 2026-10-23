package agent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiden-agent/internal/agent/agentpath"
	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/mcu"
	"aiden-agent/internal/routines"
	langtools "github.com/tmc/langchaingo/tools"
)

type scheduleTestMCU struct{ timer *mcu.Timer }

func (f *scheduleTestMCU) GetState(context.Context) (*mcu.State, error) {
	return &mcu.State{Generation: "g"}, nil
}
func (f *scheduleTestMCU) GetWakeupTimer(context.Context) (*mcu.Timer, error) {
	if f.timer == nil {
		return &mcu.Timer{}, nil
	}
	return f.timer, nil
}
func (f *scheduleTestMCU) SetWakeupTimer(_ context.Context, s int64, _, _ string) (*mcu.Timer, error) {
	f.timer = &mcu.Timer{Active: true, RemainingS: uint32(s)}
	return f.timer, nil
}
func (f *scheduleTestMCU) CancelWakeupTimer(context.Context, string, string) (*mcu.Timer, error) {
	f.timer = &mcu.Timer{}
	return f.timer, nil
}
func (f *scheduleTestMCU) EventsSince(ctx context.Context, _, _ string, _, _ int) (*mcu.EventPage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestScheduleToolCreateListUpdateCancel(t *testing.T) {
	configDir := t.TempDir()
	config := Config{ConfigDir: configDir, InputMode: "realtime", VoiceModel: VoiceModelConfig{UseBackendAgent: true}}
	session, err := contextmanager.NewContextManager(agentpath.ContextManagerSessionFolder(configDir), "system")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := routines.Open(filepath.Join(configDir, "routines", "state.json"), &scheduleTestMCU{}, func(context.Context, routines.Routine) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	tool := &ScheduleTool{manager: manager, runtime: &Runtime{config: config}, now: time.Now}
	output, err := tool.Call(context.Background(), `{"action":"create","task":"Check the shipment","delay_s":60,"interval_s":3600}`)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Result     routines.Routine `json:"result"`
		TimerError string           `json:"timer_error"`
	}
	if err := json.Unmarshal([]byte(output), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Result.SessionID != session.GetSessionID() || saved.Result.IntervalS != 3600 || saved.TimerError != "" {
		t.Fatalf("saved=%+v", saved)
	}
	output, err = tool.Call(context.Background(), `{"action":"list"}`)
	if err != nil || !strings.Contains(output, saved.Result.ID) {
		t.Fatalf("list=%s err=%v", output, err)
	}
	for _, input := range []map[string]any{
		{"action": "update", "id": saved.Result.ID, "task": "Check again", "delay_s": 120},
		{"action": "cancel", "id": saved.Result.ID},
	} {
		encoded, _ := json.Marshal(input)
		if _, err := tool.Call(context.Background(), string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := manager.List()
	if items[0].Status != "cancelled" || items[0].Task != "Check again" || items[0].IntervalS != 0 {
		t.Fatalf("items=%+v", items)
	}
}

func TestScheduleToolRejectsInvalidInput(t *testing.T) {
	manager, err := routines.Open(filepath.Join(t.TempDir(), "state.json"), &scheduleTestMCU{}, func(context.Context, routines.Routine) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	tool := &ScheduleTool{manager: manager, runtime: &Runtime{config: Config{ConfigDir: t.TempDir()}}, now: func() time.Time { return time.Now() }}
	for _, input := range []string{
		`{"action":"create","task":"x","delay_s":0}`,
		`{"action":"create","task":"x","delay_s":1,"run_at":"2026-10-23T10:00:00Z"}`,
		`{"action":"create","task":"x","delay_s":864001}`,
		`{"action":"create","task":"x","delay_s":1,"unknown":true}`,
		`{"action":"create","task":"x","delay_s":1} {}`,
		`{"action":"nope"}`,
	} {
		if _, err := tool.Call(context.Background(), input); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}

func TestScheduleToolRequiresBackendForRealtime(t *testing.T) {
	manager, err := routines.Open(filepath.Join(t.TempDir(), "state.json"), &scheduleTestMCU{}, func(context.Context, routines.Routine) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	tool := &ScheduleTool{manager: manager, runtime: &Runtime{config: Config{ConfigDir: t.TempDir(), InputMode: "realtime"}}, now: time.Now}
	_, err = tool.Call(context.Background(), `{"action":"create","task":"x","delay_s":10}`)
	if err == nil || !strings.Contains(err.Error(), "backend agent") {
		t.Fatalf("error=%v", err)
	}
}

func TestStartRoutinesRegistersToolAndPreservesMCUError(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("AIDEN_MCU_SERVICE_SOCKET", filepath.Join(configDir, "missing.sock"))
	runtime := NewRuntimeWithDeps(Config{ConfigDir: configDir}, nil, nil, &ToolSet{tools: map[string]langtools.Tool{}}, NewSkillIndex())
	defer runtime.Close()
	if err := runtime.StartRoutines(); err != nil {
		t.Fatal(err)
	}
	first := runtime.routines
	if err := runtime.StartRoutines(); err != nil || first != runtime.routines {
		t.Fatal("startup not idempotent")
	}
	tool, ok := runtime.Tool("schedule")
	if !ok {
		t.Fatal("schedule tool not installed")
	}
	if _, err := contextmanager.NewContextManager(agentpath.ContextManagerSessionFolder(configDir), "system"); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Call(context.Background(), `{"action":"create","task":"Check it","delay_s":10}`); err == nil {
		t.Fatal("reported success without MCU")
	}
}

func TestScheduleOwnsManualMCUTimerHTTP(t *testing.T) {
	manager, err := routines.Open(filepath.Join(t.TempDir(), "state.json"), &scheduleTestMCU{}, func(context.Context, routines.Routine) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	routine, err := manager.Save(context.Background(), routines.Routine{Task: "check", SessionID: "s_original", RunAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s := &Server{runtime: &Runtime{routines: manager}, mcuRequest: func(context.Context, mcu.Request) (mcu.Response, error) {
		calls++
		return mcu.Response{Status: "OK", Timer: &mcu.Timer{}}, nil
	}}
	for _, method := range []string{"PUT", "DELETE"} {
		r := httptest.NewRequest(method, "/api/mcu/wakeup-timer", strings.NewReader(`{"generation":"g","request_id":"manual","delay_s":10}`))
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.handleMCU(w, r)
		if w.Code != 409 {
			t.Fatalf("%s status=%d body=%s", method, w.Code, w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatal("manual request changed scheduler timer")
	}
	if _, err := manager.Cancel(context.Background(), routine.ID); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PUT", "/api/mcu/wakeup-timer", strings.NewReader(`{"generation":"g","request_id":"manual","delay_s":10}`))
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.handleMCU(w, r)
	if w.Code != 200 || calls != 1 {
		t.Fatalf("released timer status=%d calls=%d", w.Code, calls)
	}
}
