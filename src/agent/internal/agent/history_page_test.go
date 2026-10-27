package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aico-agent/internal/agent/agentpath"
	"aico-agent/internal/agent/messages"
)

func newHistoryPageTestServer(t *testing.T, turns int) *Server {
	t.Helper()
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	manager, err := InitializeContextManager("system", agentpath.ContextManagerSessionFolder(runtime.config.ConfigDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < turns; i++ {
		for _, message := range []messages.Message{
			{Role: messages.MessageRoleUser, Content: fmt.Sprintf("question %d", i)},
			{Role: messages.MessageRoleToolCall, ToolCalls: []messages.ToolCall{{ID: fmt.Sprintf("call-%d", i), Name: "screenshot", Arguments: "{}"}}},
			{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: fmt.Sprintf("call-%d", i), Name: "screenshot", Content: "{}"}}},
			{Role: messages.MessageRoleAssistant, Content: fmt.Sprintf("answer %d", i)},
		} {
			if err := manager.AppendMessage(message); err != nil {
				t.Fatal(err)
			}
		}
	}
	runtime.contextManager = manager
	return &Server{logger: newTestLogger(), runtime: runtime}
}

func getHistoryPage(t *testing.T, server *Server, query string) HistoryPage {
	t.Helper()
	rec := httptest.NewRecorder()
	server.handleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/history?"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/history?%s status = %d body=%s", query, rec.Code, rec.Body.String())
	}
	var page HistoryPage
	if err := json.NewDecoder(rec.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	return page
}

func TestServerHistoryPagesBackwardWithStableIDs(t *testing.T) {
	server := newHistoryPageTestServer(t, 5)

	newest := getHistoryPage(t, server, "limit=6")
	if newest.SessionID == "" || newest.End != newest.Total || newest.Start != newest.Total-6 || !newest.HasMore || newest.Reset {
		t.Fatalf("newest page = %+v", newest)
	}
	if len(newest.Messages) != 6 || newest.Messages[5].Content != "answer 4" {
		t.Fatalf("newest messages = %#v", newest.Messages)
	}
	if want := fmt.Sprintf("context-backend-%d", newest.Total-1); newest.Messages[5].RequestID != want {
		t.Fatalf("newest RequestID = %q, want absolute index %q", newest.Messages[5].RequestID, want)
	}

	older := getHistoryPage(t, server, fmt.Sprintf("limit=6&before=%d&session_id=%s", newest.Start, newest.SessionID))
	if older.End != newest.Start || older.Start != newest.Start-6 || older.Reset {
		t.Fatalf("older page = %+v, want the 6 messages before %d", older, newest.Start)
	}

	full := server.webHistorySnapshot()
	byID := map[string]string{}
	for _, message := range full {
		byID[message.RequestID] = message.Content
	}
	for _, message := range append(older.Messages, newest.Messages...) {
		if content, ok := byID[message.RequestID]; !ok || content != message.Content {
			t.Fatalf("paged message %q = %q, full snapshot has %q (present=%v)", message.RequestID, message.Content, content, ok)
		}
	}

	first := getHistoryPage(t, server, fmt.Sprintf("limit=200&before=%d&session_id=%s", older.Start, older.SessionID))
	if first.Start != 0 || first.HasMore {
		t.Fatalf("first page = %+v, want start 0 without more", first)
	}
	// The system prompt is hidden from the Web view.
	for _, message := range first.Messages {
		if message.Content == "system" {
			t.Fatalf("first page exposed the system prompt: %#v", first.Messages)
		}
	}
}

func TestServerHistoryIncrementalRefresh(t *testing.T) {
	server := newHistoryPageTestServer(t, 2)
	page := getHistoryPage(t, server, "limit=50")

	unchanged := getHistoryPage(t, server, fmt.Sprintf("after=%d&session_id=%s", page.End, page.SessionID))
	if len(unchanged.Messages) != 0 || unchanged.Start != page.End || unchanged.End != page.End || unchanged.Reset {
		t.Fatalf("unchanged refresh = %+v, want an empty page at %d", unchanged, page.End)
	}

	if err := server.runtime.contextManager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "next"}); err != nil {
		t.Fatal(err)
	}
	appended := getHistoryPage(t, server, fmt.Sprintf("after=%d&session_id=%s", page.End, page.SessionID))
	if appended.Reset || appended.Start != page.End || appended.End != page.End+1 || len(appended.Messages) != 1 || appended.Messages[0].Content != "next" {
		t.Fatalf("appended refresh = %+v", appended)
	}

	behind := getHistoryPage(t, server, fmt.Sprintf("limit=2&after=1&session_id=%s", page.SessionID))
	if !behind.Reset || behind.End != behind.Total || behind.Start != behind.Total-2 {
		t.Fatalf("refresh more than a page behind = %+v, want reset to the newest page", behind)
	}
}

func TestServerHistoryResetsCursorFromAnotherSession(t *testing.T) {
	server := newHistoryPageTestServer(t, 3)
	for _, query := range []string{"after=2&session_id=stale", "before=4&session_id=stale", "after=9999"} {
		page := getHistoryPage(t, server, "limit=3&"+query)
		if !page.Reset || page.End != page.Total || page.Start != page.Total-3 {
			t.Fatalf("%s: page = %+v, want reset to the newest page", query, page)
		}
	}
}

func TestServerHistoryRejectsInvalidPageQuery(t *testing.T) {
	server := newHistoryPageTestServer(t, 1)
	for _, query := range []string{"limit=0", "limit=abc", "before=-1", "after=x", "before=1&after=1"} {
		rec := httptest.NewRecorder()
		server.handleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/history?"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", query, rec.Code)
		}
	}
}

func TestServerHistoryWithoutPageQueryKeepsFullArray(t *testing.T) {
	server := newHistoryPageTestServer(t, 60)
	rec := httptest.NewRecorder()
	server.handleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/history", nil))
	var history []Message
	if err := json.NewDecoder(rec.Body).Decode(&history); err != nil {
		t.Fatalf("legacy response must stay an array: %v", err)
	}
	if len(history) != 240 {
		t.Fatalf("legacy history length = %d, want 240", len(history))
	}
}

func TestWebHistoryTurnSnapshotStartsAtLatestUserMessage(t *testing.T) {
	server := newHistoryPageTestServer(t, 3)
	turn := server.webHistoryTurnSnapshot()
	if len(turn) != 4 || turn[0].Type != "user" || turn[0].Content != "question 2" || turn[3].Content != "answer 2" {
		t.Fatalf("turn snapshot = %#v, want only the last turn", turn)
	}
}

func TestServerHistoryResetsCursorWithoutReadableSession(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	server := &Server{logger: newTestLogger(), runtime: runtime}

	held := getHistoryPage(t, server, "after=12&session_id=s1")
	if !held.Reset || held.SessionID != "" || len(held.Messages) != 0 {
		t.Fatalf("cursor without a readable session = %+v, want an empty reset page", held)
	}
	fresh := getHistoryPage(t, server, "limit=50")
	if fresh.Reset {
		t.Fatalf("first page without a session = %+v, want no reset", fresh)
	}
}

func TestChatResultServesTheTurnCapturedAtCompletion(t *testing.T) {
	server := newHistoryPageTestServer(t, 2)
	const requestID = "done-before-next"
	server.pendingResults = map[string]*chatPendingResult{
		requestID: {done: true, turn: server.webHistoryTurnSnapshot()},
	}
	// The next request has started its turn by the time this one is polled.
	if err := server.runtime.contextManager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "next question"}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.handleChatResult(rec, httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil))
	var resp ChatResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode chat result: %v", err)
	}
	if resp.Status != "complete" || resp.Response != "answer 1" || len(resp.History) != 4 || resp.History[0].Content != "question 1" {
		t.Fatalf("chat result = %+v, want the completed turn", resp)
	}
}
