package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise the actual async export and shutdown path against a local OTLP
// receiver. Only benchmark workers should wait for telemetry before exit.
func TestRuntimeCloseTelemetryIsolation(t *testing.T) {
	for _, environment := range []string{"benchmark", "production", ""} {
		t.Run("environment="+environment, func(t *testing.T) {
			testRuntimeCloseTelemetry(t, environment)
		})
	}
}

func testRuntimeCloseTelemetry(t *testing.T, environment string) {
	t.Helper()
	episode, fixtureDir, prompts := langfuseLiveEpisodeFixture(t)
	configDir := t.TempDir()
	episodeDir := EpisodeDirectory(filepath.Join(configDir, "memory", "episodes"), episode)
	if err := os.MkdirAll(filepath.Dir(episodeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixtureDir, episodeDir); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	scored := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	releaseUpload := func() { once.Do(func() { close(release) }) }
	var mu sync.Mutex
	var bodies [][]byte
	scoreCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/public/otel/v1/traces":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		case "/api/public/scores":
			mu.Lock()
			scoreCount++
			mu.Unlock()
			scored <- struct{}{}
		default:
			t.Errorf("unexpected request (screenshots are disabled): %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	defer releaseUpload()
	runtime := &Runtime{config: Config{
		ConfigDir: configDir,
		Telemetry: TelemetryConfig{
			Enabled: boolPtr(true), BaseURL: server.URL,
			PublicKey: "pk-test", SecretKey: "sk-test",
			UploadScreenshots: boolPtr(false), UploadTimeoutSec: 5,
			Environment: environment,
		},
	}}
	runtime.exportEpisodeBestEffort(episode, &telemetryPromptCapture{calls: prompts})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("export did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	if environment == "benchmark" {
		select {
		case err := <-closed:
			t.Fatalf("Close returned before telemetry was accepted: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		releaseUpload()
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Close did not finish after upload")
		}
		if _, ok := runtime.telemetryExports.begin(); ok {
			runtime.telemetryExports.done()
			t.Fatal("export accepted after shutdown")
		}
	} else {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("ordinary Agent shutdown waited for telemetry")
		}
		releaseUpload()
	}
	select {
	case <-scored:
	case <-time.After(3 * time.Second):
		t.Fatal("export did not finish after upload was released")
	}
	mu.Lock()
	defer mu.Unlock()
	var spans []otlpSpanPayload
	for _, body := range bodies {
		spans = append(spans, decodeOTLPSpans(t, body)...)
	}
	root := otlpSpanByName(t, spans, "agent-run")
	if root.TraceID != telemetryTraceID(episode.ID) {
		t.Fatalf("trace ID %s does not match the experiment correlation ID", root.TraceID)
	}
	if got := root.attributeString(t, "langfuse.observation.output"); !strings.Contains(got, episode.Outcome.FinalAnswer) {
		t.Fatalf("missing final response: %s", got)
	}
	for _, name := range []string{"agent-response", "screenshot"} {
		span := otlpSpanByName(t, spans, name)
		for _, field := range []string{"input", "output"} {
			if span.attributeString(t, "langfuse.observation."+field) == "" {
				t.Fatalf("%s missing %s", name, field)
			}
		}
		if span.TraceID != root.TraceID || span.ParentSpanID == "" {
			t.Fatalf("%s missing trace hierarchy", name)
		}
	}
	if scoreCount != 1 {
		t.Fatalf("score requests = %d, want 1", scoreCount)
	}
}
