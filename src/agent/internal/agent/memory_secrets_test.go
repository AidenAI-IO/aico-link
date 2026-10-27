package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMemorySensitiveKind(t *testing.T) {
	cases := map[string]memorySensitiveKind{
		"":             memorySensitiveNone,
		"none":         memorySensitiveNone,
		"credential":   memorySensitiveCredential,
		"secret":       memorySensitiveCredential,
		"payment_card": memorySensitiveCard,
		"password":     memorySensitiveCredential,
	}
	for value, want := range cases {
		if got := parseMemorySensitiveKind(value); got != want {
			t.Errorf("parseMemorySensitiveKind(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestUserRequestQuoted(t *testing.T) {
	user := []string{"我的保险箱密码是 7316，帮我记住它。", "Also, please remember my PIN!"}
	for _, quote := range []string{"帮我记住它", "帮我记住它。", "please remember my pin", "保险箱密码是7316，帮我记住"} {
		if !userRequestQuoted(quote, user...) {
			t.Errorf("quote %q not found", quote)
		}
	}
	for _, quote := range []string{"", "7316", "帮我永久保存密码", "remember it forever"} {
		if userRequestQuoted(quote, user...) {
			t.Errorf("quote %q accepted", quote)
		}
	}
}

func saveMemoryForTest(t *testing.T, ctx context.Context, tool *SaveMemoryTool, input string) map[string]string {
	t.Helper()
	out, err := tool.Call(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	return result
}

func userRunContext(input string) context.Context {
	recorder := NewEpisodeRecorder(MemoryRetrieveRequest{Input: input}, MemoryContext{})
	return WithEpisodeRecorder(context.Background(), recorder)
}

func TestSaveMemoryToolRefusesSensitiveWithoutQuotedRequest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "long_term")
	tool := NewSaveMemoryTool(NewLongTermMemoryStore(root))
	ctx := userRunContext("我的银行卡密码是 520731，下次付款要用")
	for name, input := range map[string]string{
		"no request":       `{"type":"fact","content":"用户的银行卡密码是 520731","sensitive":"credential"}`,
		"invented request": `{"type":"fact","content":"用户的银行卡密码是 520731","sensitive":"credential","user_request":"帮我记住密码"}`,
		"value as request": `{"type":"fact","content":"用户的银行卡密码是 520731","sensitive":"credential","user_request":"520731"}`,
	} {
		if result := saveMemoryForTest(t, ctx, tool, input); result["status"] != "refused" {
			t.Errorf("%s: result = %v, want refused", name, result)
		}
	}
	// Without a recorder there is no user text to quote from.
	if result := saveMemoryForTest(t, context.Background(), tool, `{"type":"fact","content":"PIN 7316","sensitive":"credential","user_request":"记住PIN"}`); result["status"] != "refused" {
		t.Errorf("no recorder: result = %v, want refused", result)
	}
	if files, _ := filepath.Glob(filepath.Join(root, "memories", "*.md")); len(files) != 0 {
		t.Fatalf("stored %v, want nothing", files)
	}
}

func TestSaveMemoryToolKeepsRequestedSecretOutOfBrowsing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "long_term")
	store := NewLongTermMemoryStore(root)
	tool := NewSaveMemoryTool(store)
	ctx := userRunContext("保险箱密码是 7316，帮我记住")
	result := saveMemoryForTest(t, ctx, tool, `{"type":"profile","title":"保险箱密码","content":"用户的保险箱密码是 7316","tags":["保险箱"],"sensitive":"credential","user_request":"帮我记住"}`)
	if result["status"] != "saved" {
		t.Fatalf("result = %v, want saved", result)
	}
	data, err := os.ReadFile(filepath.Join(root, "memories", result["id"]+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sensitive: true") {
		t.Fatalf("memory file lacks the sensitive flag:\n%s", data)
	}

	recall := NewRecallMemoryTool(store)
	browse, err := recall.Call(context.Background(), `{"types":["profile","preference","rule","fact"],"limit":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(browse, "7316") {
		t.Fatalf("browse recall exposed the secret: %s", browse)
	}
	topical, err := recall.Call(context.Background(), `{"tags":["保险箱"],"limit":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(topical, "7316") || !strings.Contains(topical, `"sensitive":true`) {
		t.Fatalf("topical recall = %s, want the sensitive memory", topical)
	}

	profile, err := store.ProfilePrompt(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(profile, "7316") {
		t.Fatalf("profile prompt exposed the secret: %q", profile)
	}
}

func TestSaveMemoryToolAcceptsRequestFromSteer(t *testing.T) {
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	tool := NewSaveMemoryTool(store)
	recorder := NewEpisodeRecorder(MemoryRetrieveRequest{Input: "等一下"}, MemoryContext{})
	// The request may arrive as a steer while the run is in progress.
	recorder.RecordEvent(TaskEpisodeEvent{Type: "steer", Role: "user", Content: "门禁卡密码 2690，先帮我记一下"})
	ctx := WithEpisodeRecorder(context.Background(), recorder)
	result := saveMemoryForTest(t, ctx, tool, `{"type":"fact","content":"门禁卡密码 2690","sensitive":"credential","user_request":"先帮我记一下"}`)
	if result["status"] != "saved" {
		t.Fatalf("result = %v, want saved", result)
	}
}

func TestSaveMemoryToolSavesOrdinaryMemoryWithoutRequestQuote(t *testing.T) {
	tool := NewSaveMemoryTool(NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term")))
	result := saveMemoryForTest(t, context.Background(), tool, `{"type":"profile","content":"用户希望回复尽量简短"}`)
	if result["status"] != "saved" || result["note"] != "" {
		t.Fatalf("result = %v, want an ordinary save", result)
	}
}

func TestDecodeSaveMemoryRequestAcceptsBooleanSensitive(t *testing.T) {
	for input, want := range map[string]memorySensitiveKind{
		`{"type":"fact","content":"x","sensitive":false}`: memorySensitiveNone,
		`{"type":"fact","content":"x","sensitive":true}`:  memorySensitiveCredential,
		`{"type":"fact","content":"x","sensitive":null}`:  memorySensitiveNone,
	} {
		req, err := decodeSaveMemoryRequest(input)
		if err != nil {
			t.Fatalf("decode %s: %v", input, err)
		}
		if got := parseMemorySensitiveKind(req.Sensitive); got != want {
			t.Errorf("%s: kind = %q, want %q", input, got, want)
		}
	}
}
