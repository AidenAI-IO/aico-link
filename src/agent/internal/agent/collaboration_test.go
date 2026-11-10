package agent

import (
	"context"
	"testing"
	"time"

	"aico-agent/internal/stream"
)

func TestCollaborationRecorderBarrierAndReplay(t *testing.T) {
	r := NewCollaborationRecorder("test-clock")
	defer r.Close()
	cursor := r.Cursor()
	r.Record("task.updated", "task_manager", "task-a", "turn-a", map[string]string{"phase": "created"}, &stream.Collaboration{Version: 1, GoalRevision: 1})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Barrier(ctx); err != nil {
		t.Fatal(err)
	}
	replay, _, unsubscribe := r.events.Subscribe(&cursor)
	defer unsubscribe()
	if replay.ResyncRequired || len(replay.Events) != 1 {
		t.Fatalf("replay = %+v", replay)
	}
	event := replay.Events[0]
	if event.TaskID != "task-a" || event.TurnID != "turn-a" || event.Collaboration.ClockID != "test-clock" || event.Collaboration.ObservedElapsedNS <= 0 {
		t.Fatalf("lost correlation: %+v", event)
	}
	if r.Status()["complete"] != true {
		t.Fatal("valid event invalidated recorder")
	}
	wrong := cursor
	wrong.SessionEpoch++
	replay, _, unsubscribeWrong := r.events.Subscribe(&wrong)
	defer unsubscribeWrong()
	if !replay.ResyncRequired {
		t.Fatal("wrong epoch accepted")
	}
}

func TestCollaborationRecorderEncodingFailureIsSticky(t *testing.T) {
	r := NewCollaborationRecorder("test-clock")
	defer r.Close()
	r.Record("task.updated", "task_manager", "task-a", "", make(chan int), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Barrier(ctx); err != nil {
		t.Fatal(err)
	}
	if r.Status()["complete"] != false {
		t.Fatal("encoding failure silently dropped")
	}
	r.Record("task.updated", "task_manager", "task-a", "", map[string]string{"phase": "terminal"}, nil)
	if err := r.Barrier(ctx); err != nil {
		t.Fatal(err)
	}
	if r.Status()["complete"] != false {
		t.Fatal("later success repaired missing evidence")
	}
}
