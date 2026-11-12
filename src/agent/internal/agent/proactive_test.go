package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aico-agent/internal/agent/messages"
	"aico-agent/internal/stream"
)

func TestDeliverProactiveMessageRoutesByAppState(t *testing.T) {
	cases := []struct {
		name         string
		setup        func(*PhoneBridge)
		phoneAnswers bool
		want         string
	}{
		{
			name: "foreground app shows it in chat",
			setup: func(b *PhoneBridge) {
				b.connected = true
				b.platform = "ios"
				b.appState = "active"
			},
			want: ProactiveDeliveryChat,
		},
		{
			name: "background app with a route gets a notification",
			setup: func(b *PhoneBridge) {
				b.platform = "ios"
				b.appState = "background"
				b.pipBridgeEnabled = true
				b.pipBridgeSeen = true
			},
			phoneAnswers: true,
			want:         ProactiveDeliveryNotification,
		},
		{
			name: "background app without a route defers",
			setup: func(b *PhoneBridge) {
				b.platform = "ios"
				b.appState = "background"
			},
			want: ProactiveDeliveryDeferred,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge := newPhoneBridgeForTest()
			defer bridge.queue.Stop()
			bridge.mu.Lock()
			tc.setup(bridge)
			bridge.mu.Unlock()

			runtime := NewRuntimeWithDeps(
				withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
				&testModelResolver{model: &scriptedModel{}},
				NewMemoryManager(""),
				NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
				NewSkillIndex(),
			)
			runtime.phoneBridge = bridge
			server := newServerForTest(runtime)

			var notified []BridgeCommand
			done := make(chan struct{})
			if tc.phoneAnswers {
				go func() {
					defer close(done)
					for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
						for _, cmd := range bridge.queue.Poll("ios", 10) {
							notified = append(notified, cmd)
							_ = bridge.queue.SubmitResult(BridgeCommandResponse{ID: cmd.ID, Method: cmd.Type, Data: json.RawMessage(`{"notification_id":"n1"}`)})
							return
						}
					}
				}()
			} else {
				close(done)
			}

			cursor := server.semanticEvents.Cursor()
			result, err := server.DeliverProactiveMessage(context.Background(), "  Your package arrived.  ")
			<-done
			if err != nil {
				t.Fatalf("DeliverProactiveMessage() error = %v", err)
			}
			if result.Delivery != tc.want {
				t.Fatalf("delivery = %q (error %q), want %q", result.Delivery, result.Error, tc.want)
			}
			if tc.phoneAnswers {
				var payload map[string]any
				if len(notified) != 1 || json.Unmarshal(notified[0].Payload, &payload) != nil || payload["body"] != "Your package arrived." {
					t.Fatalf("notification commands = %+v, want one carrying the message", notified)
				}
			}

			// The message is in the displayed history as a proactive message,
			// and stays a plain assistant message for the model.
			history := server.webHistorySnapshot()
			last := history[len(history)-1]
			if last.Type != messageTypeProactive || last.Content != "Your package arrived." || last.RequestID != result.Message.RequestID {
				t.Fatalf("last history message = %+v, result = %+v", last, result.Message)
			}
			stored := runtime.contextManager.MessageListDump().Messages
			if tail := stored[len(stored)-1]; tail.Role != messages.MessageRoleAssistant || !tail.Proactive {
				t.Fatalf("stored message = %+v, want proactive assistant", tail)
			}

			replay, _, unsubscribe := server.semanticEvents.Subscribe(&cursor)
			unsubscribe()
			if len(replay.Events) != 1 || replay.Events[0].Kind != stream.EventMessageAppended {
				t.Fatalf("semantic events = %+v, want one message.appended", replay.Events)
			}
		})
	}
}

func TestAppendProactiveMessageWaitsForRunningTask(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	unlock, err := runtime.lockRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := runtime.AppendProactiveMessage(context.Background(), "later")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("AppendProactiveMessage() returned %v while a task held the run lock", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AppendProactiveMessage() after the task error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AppendProactiveMessage() did not resume after the task released the run lock")
	}
}
