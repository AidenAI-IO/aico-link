package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/messages"
	"aico-agent/internal/decision"
	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
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
	encoded, _ := json.Marshal(g)
	if bytes.Contains(encoded, []byte("private")) {
		t.Fatal("text input entered graph")
	}
}

type utgBlockingModel struct {
	llms.Model
	entered chan []llms.MessageContent
	release chan struct{}
	labels  string
}

func (m utgBlockingModel) GenerateContent(ctx context.Context, prefix []llms.MessageContent, opts ...llms.CallOption) (*llms.ContentResponse, error) {
	m.entered <- prefix
	select {
	case <-m.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	labels := m.labels
	if labels == "" {
		labels = `[{"frame":0,"key":"home","home":true}]`
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: labels}}}, nil
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

func TestUTGReplayStopsOnUnexpectedDestination(t *testing.T) {
	home := testUTGFrame(t, color.Black)
	settings := testUTGFrame(t, color.White)
	unknown := testUTGFrame(t, color.RGBA{R: 210, A: 255})
	graph := compileUTG(&utgTurn{steps: []utgObservation{
		{frame: home}, {args: []string{"tap", "300", "400"}, frame: settings, successful: true},
	}}, []utgClassification{{Frame: 0, Key: "home", Home: true}, {Frame: 1, Key: "settings"}})
	for _, tc := range []struct {
		name                string
		start, finish       visualScreenshotObservation
		probabilities       []float64
		expectedCalls       int
		expectedError       string
		rejectedDestination bool
	}{
		{name: "model confirms visually different source", start: unknown, finish: settings, probabilities: []float64{.97, .99}, expectedCalls: 1},
		{name: "model rejects exact source image", start: home, finish: settings, probabilities: []float64{.03}, expectedError: "source"},
		{name: "uncertain source", start: home, finish: settings, probabilities: []float64{.56}, expectedError: "source"},
		{name: "disabled decision model", start: home, finish: settings, expectedError: "enabled decision model"},
		{name: "wrong destination", start: home, finish: unknown, probabilities: []float64{.99, .01}, expectedCalls: 1, expectedError: "TRAP", rejectedDestination: true},
		{name: "uncertain destination", start: home, finish: unknown, probabilities: []float64{.99, .46}, expectedCalls: 1, expectedError: "TRAP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var advisor *decisionAdvisor
			asks := 0
			if len(tc.probabilities) > 0 {
				advisor = newUTGTestAdvisor(t, func(req decision.Request) decision.Response {
					if len(req.Assets) != 2 || len(req.Questions) != 1 {
						t.Errorf("expected two images and one question: %d, %d", len(req.Assets), len(req.Questions))
					}
					if asks >= len(tc.probabilities) {
						t.Error("unexpected extra decision")
						return decision.Response{}
					}
					p := tc.probabilities[asks]
					asks++
					return decision.Response{Answers: map[string]decision.Answer{"reference_1": {Type: decision.TypeNoul, Noul: &p}}}
				})
			}
			active := tc.start
			response := func(v visualScreenshotObservation, changed bool) string {
				result := v.Result
				result.Data = base64.StdEncoding.EncodeToString(v.ImageBytes)
				result.ScreenChanged = &changed
				b, _ := json.Marshal(result)
				return string(b)
			}
			r := &Runtime{tools: &ToolSet{decider: advisor, tools: map[string]langtools.Tool{
				"screenshot": utgReplayTestTool{"screenshot", func(string) string { return response(active, false) }},
				"touch_gesture": utgReplayTestTool{"touch_gesture", func(input string) string {
					calls++
					var payload struct{ Point struct{ X, Y int } }
					if json.Unmarshal([]byte(input), &payload) != nil || payload.Point.X != 300 || payload.Point.Y != 400 {
						t.Errorf("wrong physical action: %s", input)
					}
					active = tc.finish
					return response(active, true)
				}},
			}}}
			turn := &utgTurn{graph: graph}
			ctx := context.WithValue(context.Background(), utgTurnKey{}, turn)
			capture := &deviceCLICapture{}
			result, err := r.runUTGCommand(ctx, &Server{runtime: r}, []string{"utg-step", "1"}, capture)
			if tc.expectedError == "" {
				if err != nil || result.Output != "Confirmed state: settings" {
					t.Fatalf("result %q, err %v", result.Output, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.expectedError) {
				t.Fatalf("error = %v", err)
			}
			if asks != len(tc.probabilities) {
				t.Fatalf("decision calls = %d, want %d", asks, len(tc.probabilities))
			}
			if calls != tc.expectedCalls {
				t.Fatalf("device calls = %d, want %d", calls, tc.expectedCalls)
			}
			if len(capture.actions) != 1+tc.expectedCalls || capture.actions[0].name != "look" || (tc.expectedCalls > 0 && capture.actions[1].name != "tap") {
				t.Fatalf("missing UTG sub-operations: %+v", capture.actions)
			}
			if tc.rejectedDestination {
				if len(turn.mismatches) != 1 || turn.mismatches[0].edge.ID != 1 || turn.mismatches[0].frame != len(turn.steps)-1 {
					t.Fatalf("missing replay evidence: %+v", turn.mismatches)
				}
			} else if len(turn.mismatches) != 0 {
				t.Fatalf("unexpected replay evidence: %+v", turn.mismatches)
			}
			if len(capture.frames) == 0 {
				t.Fatal("did not preserve final screenshot")
			}
		})
	}
}

type utgReplayTestTool struct {
	name string
	fn   func(string) string
}

func (s utgReplayTestTool) Name() string        { return s.name }
func (s utgReplayTestTool) Description() string { return s.name }
func (s utgReplayTestTool) Call(_ context.Context, input string) (string, error) {
	return s.fn(input), nil
}

func newUTGTestAdvisor(t *testing.T, answer func(decision.Request) decision.Response) *decisionAdvisor {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req decision.Request
		if r.Method != http.MethodPost || r.URL.Path != "/systemone" || json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("invalid decision request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(answer(req))
	}))
	t.Cleanup(endpoint.Close)
	advisor := &decisionAdvisor{}
	advisor.setConfig(func() DecisionModelConfig {
		return DecisionModelConfig{Enabled: true, APIKey: "test-key", BaseURL: endpoint.URL}
	})
	return advisor
}

func TestUTGDiscoveryRejectsAmbiguousModelResults(t *testing.T) {
	frame := testUTGFrame(t, color.Black)
	nodes := []utgNode{{Key: "launcher/home", Thumbnail: frame.ImageBytes}, {Key: "launcher/other", Thumbnail: frame.ImageBytes}}
	for _, tc := range []struct {
		name string
		ps   [2]float64
		want string
	}{
		{"single match", [2]float64{.93, .06}, "launcher/home"},
		{"two matches", [2]float64{.93, .91}, ""},
		{"unresolved alternative", [2]float64{.93, .45}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			advisor := newUTGTestAdvisor(t, func(req decision.Request) decision.Response {
				if len(req.Assets) != 3 || len(req.Questions) != 2 {
					t.Error("discovery must send both reference images")
				}
				return decision.Response{Answers: map[string]decision.Answer{
					"reference_1": {Type: decision.TypeNoul, Noul: &tc.ps[0]}, "reference_2": {Type: decision.TypeNoul, Noul: &tc.ps[1]},
				}}
			})
			if got, err := advisor.matchUTG(context.Background(), frame, nodes); err != nil || got.key != tc.want {
				t.Fatalf("match=%q err=%v, want %q", got.key, err, tc.want)
			}
		})
	}
}

func TestUTGAutoHomeRequiresModelConfirmation(t *testing.T) {
	home := testUTGFrame(t, color.Black)
	target := testUTGFrame(t, color.White)
	changedHome := testUTGFrame(t, color.Gray{Y: 42})
	graph := utgGraph{
		Nodes: []utgNode{{Key: "home", Home: true, Thumbnail: home.ImageBytes}, {Key: "target", Thumbnail: target.ImageBytes}},
		Edges: []utgEdge{{ID: 1, From: "home", To: "target", Action: []string{"tap", "300", "400"}}},
	}
	for _, tc := range []struct {
		name           string
		homeConfidence float64
		wantTap        bool
	}{
		{"clock change accepted", .98, true},
		{"wrong launcher page rejected", .04, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asks := 0
			advisor := newUTGTestAdvisor(t, func(req decision.Request) decision.Response {
				asks++
				answers := map[string]decision.Answer{}
				for id := range req.Questions {
					p := .01
					if asks == 2 {
						p = tc.homeConfidence
					} else if asks == 3 {
						p = .99
					}
					answers[id] = decision.Answer{Type: decision.TypeNoul, Noul: &p}
				}
				return decision.Response{Answers: answers}
			})
			shot := func(v visualScreenshotObservation) string {
				payload := v.Result
				payload.Data = base64.StdEncoding.EncodeToString(v.ImageBytes)
				raw, _ := json.Marshal(payload)
				return string(raw)
			}
			homeCalls, tapCalls := 0, 0
			r := &Runtime{tools: &ToolSet{decider: advisor, tools: map[string]langtools.Tool{
				"screenshot":    deviceCLITestScreenshot{output: shot(testUTGFrame(t, color.Gray{Y: 200}))},
				"quick_action":  utgReplayTestTool{"quick_action", func(string) string { homeCalls++; return shot(changedHome) }},
				"touch_gesture": utgReplayTestTool{"touch_gesture", func(string) string { tapCalls++; return shot(target) }},
			}}}
			ctx := context.WithValue(context.Background(), utgTurnKey{}, &utgTurn{graph: graph})
			res, err := r.runUTGCommand(ctx, &Server{runtime: r}, []string{"utg-goto", "target"}, &deviceCLICapture{})
			if homeCalls != 1 || (tapCalls == 1) != tc.wantTap || tapCalls > 1 {
				t.Fatalf("home calls=%d tap calls=%d", homeCalls, tapCalls)
			}
			if tc.wantTap && (err != nil || res.Output != "Confirmed state: target") || !tc.wantTap && err == nil {
				t.Fatalf("output=%q err=%v", res.Output, err)
			}
		})
	}
}

func TestUTGBackgroundQuarantinesConfirmedWrongDestination(t *testing.T) {
	for _, tc := range []struct {
		name, labels string
		quarantined  bool
	}{
		{"different landing state", `[{"frame":0,"key":"home"},{"frame":1,"key":"root"}]`, true},
		{"unknown landing state", `[{"frame":0,"key":"home"}]`, false},
		{"models disagree", `[{"frame":0,"key":"home"},{"frame":1,"key":"bluetooth"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newUTGStore(t.TempDir())
			defer store.close()
			shortcut := utgEdge{ID: 15, From: "home", To: "bluetooth", Action: []string{"tap", "144", "690"}, Visits: 5}
			store.graph = utgGraph{
				Nodes: []utgNode{{Key: "home"}, {Key: "root"}, {Key: "bluetooth"}},
				Edges: []utgEdge{{ID: 6, From: "home", To: "root", Action: shortcut.Action}, {ID: 9, From: "root", To: "bluetooth", Action: []string{"tap", "500", "565"}}, shortcut},
			}
			frame := testUTGFrame(t, color.White)
			turn := store.startTurn(context.Background())
			turn.steps = []utgObservation{{frame: frame}, {frame: frame, args: shortcut.Action, successful: true}}
			turn.mismatches = []utgReplayMismatch{{edge: shortcut, frame: 1}}
			model := utgBlockingModel{entered: make(chan []llms.MessageContent, 1), release: make(chan struct{}), labels: tc.labels}
			store.finish(turn, nil, model, nil)
			select {
			case <-model.entered:
			case <-time.After(time.Second):
				t.Fatal("background classifier did not start")
			}
			if plan, _ := store.snapshot().path("home", "bluetooth"); len(plan) != 1 || plan[0].ID != 15 {
				t.Fatalf("changed before background confirmation: %+v", plan)
			}
			store.mu.Lock()
			done := store.pending
			store.mu.Unlock()
			close(model.release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("background update did not finish")
			}
			g := newUTGStore(filepath.Dir(store.path)).snapshot()
			plan, _ := g.path("home", "bluetooth")
			if !tc.quarantined {
				if len(plan) != 1 || plan[0].ID != 15 {
					t.Fatalf("inconclusive evidence removed edge: %+v", plan)
				}
				return
			}
			if len(plan) != 2 || plan[0].ID != 6 || plan[1].ID != 9 || g.Edges[2].MismatchState != "root" {
				t.Fatalf("shortcut still usable or evidence missing: %+v", g.Edges)
			}
			// A later observation of the originally expected result must not silently
			// restore a shortcut whose outcome depends on which page the app resumes.
			g = compileUTG(&utgTurn{graph: g, steps: turn.steps}, []utgClassification{{Frame: 0, Key: "home"}, {Frame: 1, Key: "bluetooth"}})
			if plan, _ := g.path("home", "bluetooth"); len(plan) != 2 || g.Edges[2].Visits != 5 {
				t.Fatalf("quarantined edge was relearned: %+v", g.Edges)
			}
			ctx := context.WithValue(context.Background(), utgTurnKey{}, &utgTurn{graph: g})
			r := &Runtime{}
			if _, err := r.runUTGCommand(ctx, &Server{runtime: r}, []string{"utg-step", "15"}, &deviceCLICapture{}); err == nil || !strings.Contains(err.Error(), "disabled") {
				t.Fatalf("quarantined edge accepted for manual replay: %v", err)
			}
		})
	}
}

func TestUTGGotoRelocatesAfterFailedVerification(t *testing.T) {
	frame := testUTGFrame(t, color.Black)
	// Make root the fifth candidate (the first in the second model batch).
	nodes := []utgNode{{Key: "home", Home: true}, {Key: "target"}, {Key: "other1"}, {Key: "other2"}, {Key: "root"}}
	for i := range nodes {
		nodes[i].Thumbnail = frame.ImageBytes
	}
	edges := []utgEdge{
		{ID: 15, From: "home", To: "target", Action: []string{"tap", "144", "690"}},
		{ID: 1, From: "home", To: "other1"}, {ID: 2, From: "home", To: "other2"},
		{ID: 6, From: "home", To: "root", Action: []string{"tap", "145", "690"}},
		{ID: 9, From: "root", To: "target", Action: []string{"tap", "500", "565"}},
	}
	for _, tc := range []struct {
		name          string
		first, second int
		taps          string
		wantMismatch  bool
		wantError     string
	}{
		{name: "alternate landing in second batch", second: 1, taps: "144,500", wantMismatch: true},
		{name: "stayed on source", first: 1, taps: "144,145,500", wantMismatch: true},
		{name: "expected landing confirmed on second check", first: 2, taps: "144"},
		{name: "unknown landing", taps: "144", wantError: "cannot relocate"},
		{name: "ambiguous across batches", first: 2, second: 1, taps: "144", wantError: "cannot relocate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asks := 0
			advisor := newUTGTestAdvisor(t, func(req decision.Request) decision.Response {
				asks++
				pick := 1
				switch asks {
				case 1:
					pick = 2 // target, Home, root
				case 2:
					pick = 0 // uncertain at the expected destination
				case 3:
					pick = tc.first
				case 4:
					pick = tc.second
				}
				wantQuestions := map[int]int{1: 3, 2: 1, 3: 4, 4: 1}
				if want, ok := wantQuestions[asks]; ok && len(req.Questions) != want {
					t.Errorf("request %d: expected %d reference images, got %d", asks, want, len(req.Questions))
				}
				answers := map[string]decision.Answer{}
				for id := range req.Questions {
					p := .01
					if asks == 2 {
						p = .6
					} else if id == fmt.Sprintf("reference_%d", pick) {
						p = .99
					}
					answers[id] = decision.Answer{Type: decision.TypeNoul, Noul: &p}
				}
				return decision.Response{Answers: answers}
			})
			payload := frame.Result
			payload.Data = base64.StdEncoding.EncodeToString(frame.ImageBytes)
			encoded, _ := json.Marshal(payload)
			var taps []string
			r := &Runtime{tools: &ToolSet{decider: advisor, tools: map[string]langtools.Tool{
				"screenshot": deviceCLITestScreenshot{output: string(encoded)},
				"touch_gesture": utgReplayTestTool{"touch_gesture", func(input string) string {
					var p struct{ Point struct{ X int } }
					_ = json.Unmarshal([]byte(input), &p)
					taps = append(taps, strconv.Itoa(p.Point.X))
					return string(encoded)
				}},
			}}}
			turn := &utgTurn{graph: utgGraph{Nodes: nodes, Edges: edges}}
			ctx := context.WithValue(context.Background(), utgTurnKey{}, turn)
			capture := &deviceCLICapture{}
			res, err := r.runUTGCommand(ctx, &Server{runtime: r}, []string{"utg-goto", "target"}, capture)
			if tc.wantError == "" {
				if err != nil || res.Output != "Confirmed state: target" {
					t.Fatalf("output=%q, error=%v", res.Output, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || len(capture.last.ImageBytes) == 0 {
				t.Fatalf("missing fallback error or frame: %v", err)
			}
			if got := strings.Join(taps, ","); got != tc.taps {
				t.Fatalf("taps=%s, want %s", got, tc.taps)
			}
			if (len(turn.mismatches) == 1) != tc.wantMismatch {
				t.Fatalf("mismatches=%+v", turn.mismatches)
			}
			if path, _ := turn.graph.path("home", "target"); len(path) != 1 || path[0].ID != 15 {
				t.Fatalf("goto changed persisted graph snapshot: %+v", path)
			}
		})
	}
}

func TestUTGRefreshesLegacyReferenceFromNewFrame(t *testing.T) {
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 450, 1000)), nil); err != nil {
		t.Fatal(err)
	}
	frame := visualScreenshotObservation{ImageBytes: raw.Bytes()}
	legacy := utgImage(frame, 320)
	in := utgGraph{Nodes: []utgNode{{Key: "home", Thumbnail: legacy}}}
	out := compileUTG(&utgTurn{graph: in, steps: []utgObservation{{frame: frame}}}, []utgClassification{{Frame: 0, Key: "home"}})
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out.Nodes[0].Thumbnail))
	if err != nil || cfg.Height != 480 || cfg.Width != 216 {
		t.Fatalf("reference dimensions=%+v, error=%v", cfg, err)
	}
}
