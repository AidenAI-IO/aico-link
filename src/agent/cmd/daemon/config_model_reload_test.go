package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"aico-agent/internal/agent"
)

// A model-settings save must wait for the current Agent task, but not for a
// conversation that can keep listening and accepting turns indefinitely.
func TestInputLifecycleModelReloadKeepsActiveVoiceSession(t *testing.T) {
	for _, mode := range []string{"stt", "realtime"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce, firstRequest sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			provider := func(model, key string, hold bool) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Model string `json:"model"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if body.Model != model || r.Header.Get("Authorization") != "Bearer "+key {
						t.Errorf("provider received stale model or credentials: model=%q", body.Model)
					}
					if hold {
						firstRequest.Do(func() { close(entered); <-release })
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "test", "object": "chat.completion",
						"choices": []any{map[string]any{
							"index": 0, "finish_reason": "stop",
							"message": map[string]any{"role": "assistant", "content": model},
						}},
					})
				}))
			}
			oldProvider := provider("first", "old-key", true)
			defer oldProvider.Close()
			newProvider := provider("second", "new-key", false)
			defer newProvider.Close()

			cfg := agent.DefaultConfig()
			cfg.ConfigDir = t.TempDir()
			cfg.InputMode, cfg.STT.Provider, cfg.TTS.Provider = mode, "", ""
			cfg.VoiceModel.UseBackendAgent = mode == "realtime"
			cfg.QuickCapture.Enabled = new(bool)
			cfg.Model = agent.ModelConfig{
				Provider: "openai", Model: "first", APIKey: "old-key",
				BaseURL: oldProvider.URL + "/v1", APIMode: "chat_completions",
				ContextWindow: 32768, ModelMaxOutputTokens: 1024,
			}
			manager := agent.NewModelManager(cfg.Model, agent.ProxyConfig{})
			runtime := agent.NewRuntimeWithDeps(cfg, manager, agent.NewMemoryManager(""),
				agent.NewBuiltinToolSet(cfg.HID, cfg.Audio, cfg.Search, agent.ProxyConfig{}), agent.NewSkillIndex())
			server := agent.NewServer(runtime, ":0")
			controller := newInputLifecycle(runtime, server)
			controller.newDialog = func(agent.Config) (*agent.AudioDialog, error) { return nil, nil }
			started := make(chan struct{}, 2)
			controller.runVoice = func(_ agent.Config, _ *agent.AudioDialog, shutdown chan os.Signal, _ <-chan struct{}) {
				started <- struct{}{}
				// Model the inside of an active session: graceful reload is only
				// observed by the outer wakeup loop after this session returns.
				<-shutdown
			}
			var runDone <-chan struct{}
			defer func() {
				unblock()
				if runDone != nil {
					<-runDone
				}
				controller.StopForShutdown()
				runtime.StopConfigReloads()
				controller.Close()
				server.Close()
				runtime.Close()
			}()
			if err := controller.Start(cfg); err != nil {
				t.Fatal(err)
			}
			<-started
			voiceStop, voiceDone := controller.stop, controller.done
			prepared := make(chan struct{}, 1)
			runtime.SetConfigPreparer(func(ctx context.Context, next agent.Config) (func(bool), error) {
				finish, err := controller.Prepare(ctx, next)
				prepared <- struct{}{}
				return finish, err
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type runResult struct {
				result agent.RunResult
				err    error
			}
			first := make(chan runResult, 1)
			firstDone := make(chan struct{})
			runDone = firstDone
			go func() {
				defer close(firstDone)
				result, err := runtime.Run(ctx, agent.RunRequest{Input: "before model switch"})
				first <- runResult{result, err}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("first task did not reach the original provider")
			}
			next := cfg
			next.Model.Provider, next.Model.Model, next.Model.APIKey = "openrouter", "second", "new-key"
			next.Model.BaseURL = newProvider.URL + "/v1"
			runtime.QueueConfig(next, 42)
			select {
			case <-voiceStop:
				t.Fatal("model-only save tried to drain the active voice session")
			case <-prepared:
			case <-ctx.Done():
				t.Fatal("model preparation remained blocked by the voice session")
			}
			if status := runtime.ConfigApplyStatus(); !status.Pending || runtime.ConfigSnapshot().Model.Provider != "openai" {
				t.Fatalf("model switched before the current task finished: %+v", status)
			}
			unblock()
			answer := <-first
			if answer.err != nil || !strings.Contains(answer.result.Output, "first") {
				t.Fatalf("in-flight task = %+v", answer)
			}
			for runtime.ConfigApplyStatus().Pending {
				select {
				case <-ctx.Done():
					t.Fatal("model reload did not finish while the voice session remained open")
				case <-time.After(time.Millisecond):
				}
			}
			if status := runtime.ConfigApplyStatus(); !status.Applied || status.AppliedRevision != 42 {
				t.Fatalf("model reload failed: %+v", status)
			}
			result, err := runtime.Run(ctx, agent.RunRequest{Input: "after model switch"})
			if err != nil || !strings.Contains(result.Output, "second") {
				t.Fatalf("next task did not use the new provider: output=%q err=%v", result.Output, err)
			}
			select {
			case <-voiceDone:
				t.Fatal("model switch closed the voice session")
			case <-started:
				t.Fatal("model switch restarted the voice loop")
			default:
			}
		})
	}
}
