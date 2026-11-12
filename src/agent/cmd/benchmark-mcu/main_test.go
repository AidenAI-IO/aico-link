package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aico-agent/internal/mcu"
	"aico-agent/internal/routines"
)

// The emulator must satisfy the real routines manager and MCU client: a
// schedule arms a real countdown, cancellation disarms it, and a due routine is
// woken and executed. Unavailable hardware stays an explicit error.
func TestEmulatorArmsRealScheduleTimerAndWakesDueRoutine(t *testing.T) {
	// Unix socket paths are length-limited, so avoid t.TempDir's long names.
	dir, err := os.MkdirTemp("", "mcu")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "mcu.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, socket) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if info, err := os.Stat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("emulator did not serve its socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client := mcu.Client{Socket: socket}
	ran := make(chan routines.Routine, 1)
	manager, err := routines.Open(filepath.Join(dir, "state.json"), client,
		func(_ context.Context, r routines.Routine) (string, error) { ran <- r; return "done", nil })
	if err != nil {
		t.Fatal(err)
	}
	manager.Start()
	defer manager.Close()

	later, err := manager.Save(ctx, routines.Routine{Task: "pay rent", SessionID: "s", RunAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, timerErr := manager.List(); timerErr != "" {
		t.Fatalf("schedule was not armed: %s", timerErr)
	}
	timer, err := client.GetWakeupTimer(ctx)
	if err != nil || !timer.Active || timer.RemainingS < 3590 || timer.RemainingS > 3600 {
		t.Fatalf("timer = %+v, err = %v", timer, err)
	}
	if _, err := manager.Cancel(ctx, later.ID); err != nil {
		t.Fatal(err)
	}
	if timer, err := client.GetWakeupTimer(ctx); err != nil || timer.Active {
		t.Fatalf("cancelled routine left timer = %+v, err = %v", timer, err)
	}

	soon, err := manager.Save(ctx, routines.Routine{Task: "call mom", SessionID: "s", RunAt: time.Now().Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ran:
		if r.ID != soon.ID {
			t.Fatalf("woke %s, want %s", r.ID, soon.ID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RTC wakeup did not run the due routine")
	}

	if _, err := client.Do(ctx, mcu.Request{Op: "get_led", LED: "red"}); err == nil {
		t.Fatal("emulator claimed LED hardware")
	}
	state, err := client.GetState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Poweroff(ctx, state.Generation, "off"); err == nil {
		t.Fatal("emulator accepted poweroff")
	}
}
