package agent

import (
	"context"
	"image/color"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

func TestUTGResetClearsPersistedGraphAndDiscardsPriorLearning(t *testing.T) {
	dir := t.TempDir()
	store := newUTGStore(dir)
	defer store.close()
	store.graph = utgGraph{Nodes: []utgNode{{Key: "old"}}, Edges: []utgEdge{{ID: 1}}, Current: "old"}
	if err := store.save(store.graph); err != nil {
		t.Fatal(err)
	}
	server := &Server{runtime: &Runtime{utg: store}}
	reset := func(origin string) int {
		req := httptest.NewRequest(http.MethodDelete, "http://board:8080/api/debug/utg", nil)
		req.Header.Set("Origin", origin)
		res := httptest.NewRecorder()
		server.handleUTG(res, req)
		return res.Code
	}
	if code := reset("http://other.example"); code != http.StatusForbidden || store.snapshot().Current != "old" {
		t.Fatalf("cross-origin reset was not rejected: status=%d", code)
	}
	makeTurn := func() *utgTurn {
		turn := store.startTurn(context.Background())
		turn.steps = []utgObservation{{frame: testUTGFrame(t, color.Black)}}
		return turn
	}
	prior, queued, unfinished := makeTurn(), makeTurn(), makeTurn()
	model := utgResetTestModel{entered: make(chan context.Context, 2), release: make(chan struct{})}
	store.finish(prior, nil, model, nil)
	var jobCtx context.Context
	select {
	case jobCtx = <-model.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("classifier did not start")
	}
	store.finish(queued, nil, model, nil)
	store.mu.Lock()
	oldJobs := store.pending
	store.mu.Unlock()
	if code := reset("http://board:8080"); code != http.StatusNoContent {
		t.Fatalf("reset status=%d", code)
	}
	if jobCtx.Err() != context.Canceled {
		t.Fatal("reset did not cancel running classifier")
	}
	if g := newUTGStore(dir).snapshot(); len(g.Nodes) != 0 || len(g.Edges) != 0 || g.Current != "" {
		t.Fatalf("reset did not persist an empty graph: %+v", g)
	}
	store.finish(makeTurn(), nil, utgBlockingModel{entered: make(chan []llms.MessageContent, 1), release: closedUTGTestChannel(), labels: `[{"frame":0,"key":"new"}]`}, nil)
	// Complete the old model even though its context was canceled, to exercise the publication guard.
	close(model.release)
	select {
	case <-oldJobs:
	case <-time.After(2 * time.Second):
		t.Fatal("old jobs did not exit")
	}
	select {
	case <-model.entered:
		t.Fatal("queued classifier started after reset")
	default:
	}
	store.finish(unfinished, nil, model, nil)
	store.mu.Lock()
	newJob := store.pending
	store.mu.Unlock()
	if newJob != nil {
		select {
		case <-newJob:
		case <-time.After(2 * time.Second):
			t.Fatal("new job did not settle")
		}
	}
	for _, g := range []utgGraph{store.snapshot(), newUTGStore(dir).snapshot()} {
		if len(g.Edges) != 0 || len(g.Nodes) != 1 || g.Current != "new" {
			t.Fatalf("unexpected graph after reset and relearn: %+v", g)
		}
	}
}

func closedUTGTestChannel() chan struct{} { c := make(chan struct{}); close(c); return c }

type utgResetTestModel struct {
	llms.Model
	entered chan context.Context
	release chan struct{}
}

func (m utgResetTestModel) GenerateContent(ctx context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.entered <- ctx
	<-m.release
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: `[{"frame":0,"key":"stale"}]`}}}, nil
}
