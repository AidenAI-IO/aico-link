package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/messages"
	"github.com/tmc/langchaingo/llms"
)

func testUTGFrame(t *testing.T, c color.Color) visualScreenshotObservation {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 8, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 8; x++ {
			m.Set(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, m, nil); err != nil {
		t.Fatal(err)
	}
	return visualScreenshotObservation{ImageBytes: out.Bytes(), MIMEType: "image/jpeg", Result: postActionScreenshotResult{screenshotResult: screenshotResult{Width: 8, Height: 12}}}
}

func TestUTGCompilesOnlyObservedNavigation(t *testing.T) {
	home := testUTGFrame(t, color.Black)
	settings := testUTGFrame(t, color.White)
	turn := &utgTurn{steps: []utgObservation{
		{frame: home},
		{args: []string{"tap", "20", "40"}, frame: settings, successful: true},
		{args: []string{"type", "private"}, frame: settings, successful: true},
		{args: []string{"back"}, frame: home, successful: true},
		{args: []string{"tap", "30", "40"}, frame: settings, successful: true},
		{args: []string{"back"}, frame: home}, // failed device call
	}}
	var labels []utgClassification
	err := json.Unmarshal([]byte(`[{"frame":0,"key":"home","home":true},{"frame":"#1","key":"settings"},{"frame":2,"key":"typed"},{"frame":3,"key":"home"},{"frame":4,"key":"settings"},{"frame":5,"key":"home"}]`), &labels)
	if err != nil {
		t.Fatal(err)
	}
	g := compileUTG(turn, labels)
	route, ok := g.path("home", "settings")
	if !ok || len(route) != 1 || strings.Join(route[0].Action, " ") != "tap 20 40" || route[0].Visits != 2 {
		t.Fatalf("raw route: %+v", route)
	}
	if len(g.Edges) != 2 {
		t.Fatalf("edges: %+v", g.Edges)
	}
	if got := g.locate(home, "settings"); got != "home" {
		t.Fatalf("matched %s", got)
	}
	encoded, _ := json.Marshal(g)
	if bytes.Contains(encoded, []byte("private")) {
		t.Fatal("text input entered graph")
	}
}

type utgBlockingModel struct {
	llms.Model
	entered chan []llms.MessageContent
	release chan struct{}
}

func (m utgBlockingModel) GenerateContent(ctx context.Context, prefix []llms.MessageContent, opts ...llms.CallOption) (*llms.ContentResponse, error) {
	m.entered <- prefix
	select {
	case <-m.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: `[{"frame":0,"key":"home","home":true}]`}}}, nil
}
func TestUTGPublishesOnlyAfterBackgroundClassification(t *testing.T) {
	dir := t.TempDir()
	s := newUTGStore(dir)
	defer s.close()
	turn := s.startTurn(context.Background())
	turn.steps = append(turn.steps, utgObservation{frame: testUTGFrame(t, color.Black)})
	if _, err := os.Stat(filepath.Join(dir, "device-utg.json")); !os.IsNotExist(err) {
		t.Fatal("graph modified before turn ended", err)
	}
	model := utgBlockingModel{entered: make(chan []llms.MessageContent, 1), release: make(chan struct{})}
	prefix := []messages.Message{{Role: messages.MessageRoleUser, Content: "task"}, {Role: messages.MessageRoleAssistant, Content: "final answer"}}
	s.finish(turn, prefix, model, nil)
	select {
	case fork := <-model.entered:
		if len(fork) != 3 || fork[1].Parts[0].(llms.TextContent).Text != "final answer" {
			t.Fatalf("fork: %+v", fork)
		}
	case <-time.After(time.Second):
		t.Fatal("background model not called")
	}
	if len(s.snapshot().Nodes) != 0 {
		t.Fatal("graph published before classifier returned")
	}
	s.mu.Lock()
	done := s.pending
	s.mu.Unlock()
	close(model.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("update did not settle")
	}
	if got := newUTGStore(dir).snapshot().Current; got != "home" {
		t.Fatalf("persisted current = %q", got)
	}
}
