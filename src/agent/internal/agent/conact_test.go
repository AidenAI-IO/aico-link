package agent

import (
	"context"
	"encoding/json"
	"image"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/executor"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/model"

	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestConactFoldPreservesFactsAndSourceRevision(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	m, err := contextmanager.NewContextManager(root, "system")
	if err != nil {
		t.Fatal(err)
	}
	shot := &stubTool{name: "screenshot", output: terminationPolicyScreenshotObservation(t, 120, 240, image.Rectangle{})}
	c := newConactRun(&Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"screenshot": shot}}})
	add := func(msg ...messages.Message) {
		t.Helper()
		if err := m.AppendMessages(msg); err != nil {
			t.Fatal(err)
		}
	}
	add(messages.Message{Role: messages.MessageRoleUser, Content: "original request", Attachments: []messages.Attachment{{FilePath: "user.jpg", MIMEType: "image/jpeg"}}})
	c.rebase(m)
	const input = `{"subtask_name":"Read order","extracted_facts":["ID 00017 / 报告.pdf"],"next_goal":"Switch app"}`
	for i := 0; i < 2; i++ {
		if got, err := c.fold(ctx, m, input); err != nil || !strings.HasPrefix(got, "error:") || len(shot.inputs) != i {
			t.Fatalf("fold without a new action: %q %v", got, err)
		}
		add(messages.Message{Role: messages.MessageRoleState, Content: "temporary observation"})
		id := m.GetSessionID()
		c.observe(ToolResult{deviceCalls: []deviceCLIToolEvidence{{Tool: "touch_gesture"}}})

		if got, err := c.fold(ctx, m, input); err != nil || got != "Subtask folded." {
			t.Fatalf("fold: %q %v", got, err)
		}
		if id == m.GetSessionID() {
			t.Fatal("fold should activate a revision")
		}
		parent, err := contextmanager.LoadContextManagerFromSessionID(root, id)
		if err != nil {
			t.Fatal(err)
		}
		old := parent.CloneMessageList()
		if old[len(old)-1].Content != "temporary observation" {
			t.Fatal("original transcript was overwritten")
		}
	}
	list := m.CloneMessageList()
	if list[1].Content != "original request" || len(list[1].Attachments) != 1 {
		t.Fatal("user input was not retained")
	}
	count := 0
	for _, msg := range list {
		if msg.Content == "temporary observation" {
			t.Fatal("active subtask survived the fold")
		}
		for _, a := range msg.Attachments {
			if a.Source == messages.AttachmentSourceScreenshotObservation {
				count++
			}
		}
	}
	if count != 1 || !strings.Contains(list[len(list)-3].Content, "ID 00017 / 报告.pdf") || !strings.Contains(list[len(list)-1].Content, "ID 00017 / 报告.pdf") {
		t.Fatalf("expected exact facts from both folds and one screen; got %d screens", count)
	}
}

func TestConactFallbackOnlyPrunesScreenshotObservations(t *testing.T) {
	m, err := contextmanager.NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatal(err)
	}
	c := newConactRun(&Runtime{})
	c.rebase(m)
	for i := 0; i < conactImageTrigger; i++ {
		if err := m.AppendMessage(messages.Message{Role: messages.MessageRoleState, Content: "untouched", Attachments: []messages.Attachment{{FilePath: "user", MIMEType: "image/jpeg"}, {FilePath: "screen", MIMEType: "image/jpeg", Source: messages.AttachmentSourceScreenshotObservation}}}); err != nil {
			t.Fatal(err)
		}
		id := m.GetSessionID()
		if err := c.fallback(m); err != nil {
			t.Fatal(err)
		}
		if (id != m.GetSessionID()) != (i == conactImageTrigger-1) {
			t.Fatalf("unexpected rewrite at frame %d", i+1)
		}
	}
	list := m.CloneMessageList()
	for i, msg := range list[1:] {
		want := 1
		if i >= conactImageTrigger-conactImageRetain {
			want++
		}
		if msg.Content != "untouched" || len(msg.Attachments) != want || msg.Attachments[0].FilePath != "user" {
			t.Fatalf("wrong retained data at frame %d", i+1)
		}
	}
}

type conactBudgetModel struct{ calls int }

func (*conactBudgetModel) CallOptions() []chains.ChainCallOption { return nil }
func (*conactBudgetModel) Spec() model.ModelSpec                 { return model.ModelSpec{} }
func (*conactBudgetModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	panic("unexpected Call")
}
func (m *conactBudgetModel) GenerateContent(_ context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.calls++
	choice := &llms.ContentChoice{Content: "Done"}
	if m.calls == 1 {
		choice.Content = ""
		choice.ToolCalls = []llms.ToolCall{{ID: "search", Type: "function", FunctionCall: &llms.FunctionCall{Name: conactSearch, Arguments: `{}`}}}
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{choice}}, nil
}
func TestConactReadDoesNotSpendIterationAndDeniedFoldIsError(t *testing.T) {
	ctx := context.Background()
	m, err := contextmanager.NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatal(err)
	}
	mod := &conactBudgetModel{}
	loop := NewAgentLoop(mod, RoleProfile{}, 1, nil, nil, m)
	loop.conact = newConactRun(&Runtime{})
	if got, err := loop.Run(ctx, "test"); err != nil || got != "Done" || mod.calls != 2 {
		t.Fatalf("memory turn spent action allowance: %q %v, calls=%d", got, err, mod.calls)
	}
	res := executeToolCall(ctx, ToolCallExecution{Specs: NewToolSpecs(loop.conact.tools(executor.NewLLMExecutor(nil, m))), Action: schema.AgentAction{Tool: conactFold, ToolInput: `{"subtask_name":"scan","extracted_facts":["fact"],"next_goal":"exit"}`}})
	if !res.Result.IsError() {
		t.Fatal("denied fold was treated as success")
	}
}

func TestSkillUTGPreservesIntentAndRejectsUnsupportedEvidence(t *testing.T) {
	before := utgFingerprint{}
	after := utgFingerprint{Hash: 0xffff}
	step := utgStep{Before: before, After: after, OK: true, Kind: "tap", Pointer: true}
	steps := []utgStep{step, step, {Before: before, After: before, OK: true, Kind: "tap", Pointer: true}, step}
	var p utgProposal
	if err := json.Unmarshal([]byte(`{
		"states":[
		{"label":"calendar_editor","summary":"Calendar editing form","step":1,"side":"before"},{"label":"calendar_month","summary":"Calendar monthly grid","step":1,"side":"after"},
		{"label":"calendar_editor","summary":"Calendar editing form","step":2,"side":"before"},{"label":"calendar_month","summary":"Calendar monthly grid","step":2,"side":"after"},
		{"label":"calendar_editor","summary":"Calendar editing form","step":3,"side":"before"},{"label":"calendar_month","summary":"Calendar monthly grid","step":3,"side":"after"}],
		"edges":[{"from":"calendar_editor","to":"calendar_month","intent":"save","guide":"Tap the Save button","step":1},
		{"from":"calendar_editor","to":"calendar_month","intent":"cancel","guide":"Close the draft editor","step":2},
		{"from":"calendar_editor","to":"calendar_month","intent":"nav","guide":"Open the monthly grid","step":3}],
		"pitfalls":[{"label":"calendar_editor","guide":"Avoid tapping inactive controls","failed":3,"recovered":4}]}`), &p); err != nil {
		t.Fatal(err)
	}
	g := skillUTG{}
	if !g.merge(p, steps) || len(g.Edges) != 2 {
		t.Fatalf("expected two verified parallel intents: %+v", g.Edges)
	}
	if got := g.route("calendar_editor", "calendar_month", ""); len(got) != 0 {
		t.Fatalf("default route suggests a mutating action: %+v", got)
	}
	for _, intent := range []string{"commit", "cancel"} {
		if got := g.route("calendar_editor", "calendar_month", intent); len(got) != 1 || got[0].Intent != intent {
			t.Fatalf("wrong route for %s: %+v", intent, got)
		}
	}
	if g.Nodes["calendar_editor"].Pitfall == "" {
		t.Fatal("verified no-progress recovery was lost")
	}
	if got := g.match(before); got != "" {
		t.Fatalf("ambiguous screen must not auto-match: %q", got)
	}
	file := filepath.Join(t.TempDir(), "utg.json")
	if err := saveSkillUTG(file, g); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSkillUTG(file)
	if err != nil || len(loaded.route("calendar_editor", "calendar_month", "cancel")) != 1 {
		t.Fatalf("persisted route lost: %v", err)
	}
}

type utgReflectionModel struct {
	model.Model
	seen    []messages.Message
	content string
	started chan struct{}
	resume  chan struct{}
}

func (*utgReflectionModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	panic("unexpected text-only call")
}
func (*utgReflectionModel) GenerateContent(context.Context, []llms.MessageContent, ...llms.CallOption) (*llms.ContentResponse, error) {
	panic("expected native message list")
}
func (m *utgReflectionModel) GenerateContentFromMessageList(ctx context.Context, list []messages.Message, opts ...llms.CallOption) (*llms.ContentResponse, error) {
	close(m.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.resume:
	}
	m.seen = list
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: m.content}}}, nil
}

func TestSkillUTGReflectionForkDoesNotAppendOrLearnHiddenFrames(t *testing.T) {
	ctx := context.Background()
	manager, err := contextmanager.NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{config: Config{
		ConfigDir:         t.TempDir(),
		Device:            DeviceConfig{DeviceType: "Android"},
		EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true, Endpoint: "http://bridge.invalid"},
	}}
	c := newConactRun(rt)
	if want := filepath.Join(rt.config.ConfigDir, "memory", "skill-utg", "android.json"); c.path != want {
		t.Fatalf("bridge run graph path = %q, want %q", c.path, want)
	}
	step := utgStep{After: utgFingerprint{Hash: 0xffff}, Kind: "tap", OK: true}
	c.steps = []utgStep{step, step}
	attachment := messages.Attachment{Source: messages.AttachmentSourceScreenshotObservation, MIMEType: "image/jpeg", FilePath: "screen.jpg"}
	if err := manager.AppendMessages([]messages.Message{
		{Role: messages.MessageRoleUser, Content: "Open the settings", Attachments: []messages.Attachment{attachment}},
		{Role: messages.MessageRoleState, Content: "Skill-UTG evidence step 1 (after) [previous-run]", Attachments: []messages.Attachment{attachment}},
		{Role: messages.MessageRoleState, Content: "Skill-UTG evidence step 2 (after) [" + c.evidenceTag + "]", Attachments: []messages.Attachment{attachment}},
		{Role: messages.MessageRoleAssistant, Content: "Settings opened."},
	}); err != nil {
		t.Fatal(err)
	}
	mod := &utgReflectionModel{started: make(chan struct{}), resume: make(chan struct{}), content: `{"states":[
		{"label":"hidden_screen","summary":"Unknown earlier screen","step":1,"side":"before"},
		{"label":"phone_home","summary":"Phone home screen","step":2,"side":"before"},
		{"label":"phone_settings","summary":"Phone settings screen","step":2,"side":"after"}],
		"edges":[{"from":"phone_home","to":"phone_settings","intent":"open","guide":"Open the Settings app","step":2}]}`}
	count := len(manager.CloneMessageList())
	rt.models = mod
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.startReflection(turnCtx, manager, nil)
	cancel()
	waitCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	defer rt.episodeMaintenance.closeAndWait(waitCtx)
	select {
	case <-mod.started:
	case <-waitCtx.Done():
		t.Fatal("background reflection did not start")
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "A later request"}); err != nil {
		t.Fatal(err)
	}
	close(mod.resume)
	if err := rt.episodeMaintenance.closeAndWait(waitCtx); err != nil {
		t.Fatal(err)
	}
	if len(manager.CloneMessageList()) != count+1 || len(mod.seen) != count+1 || mod.seen[count-1].Content != "Settings opened." || strings.Contains(mod.seen[count].Content, "step 1: tap") {
		t.Fatalf("reflection must fork, using only current evidence, saw %d messages", len(mod.seen))
	}
	g := newConactRun(rt).graph
	if g.Nodes["hidden_screen"] != nil || len(g.route("phone_home", "phone_settings", "nav")) != 1 {
		t.Fatalf("expected the next bridge run to load only the visible transition: %+v", g)
	}
}
