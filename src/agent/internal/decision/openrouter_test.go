package decision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAskOpenRouterUsesNativeDecisions(t *testing.T) {
	for _, model := range []string{"~typesafe/jev-latest", "google/gemini-decision-1"} {
		t.Run(model, func(t *testing.T) {
			var got struct {
				Model     string              `json:"model"`
				State     json.RawMessage     `json:"state"`
				Assets    map[string]Asset    `json:"assets"`
				Questions map[string]Question `json:"questions"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/alpha/decisions" || r.Header.Get("Authorization") != "Bearer sk-or-test" {
					t.Errorf("request = %s %s, authorization = %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				where := ""
				if _, ok := got.Questions["where"]; ok {
					where = `,"where":{"type":"box","box":{"top":0.1,"bottom":0.3,"left":0.2,"right":0.6},"confidence":0.9}`
				}
				_, _ = io.WriteString(w, `{"model":"`+model+`","answers":{
                    "found":{"type":"noul","noul":0.91},
                    "mode":{"type":"choice","choice":"ascii","confidence":0.88},
                    "severity":{"type":"score","score":1.1,"confidence":0.8}`+where+`},
                    "usage":{"input_tokens":12,"output_tokens":20}}`)
			}))
			defer server.Close()
			client := &Client{Provider: ProviderOpenRouter, BaseURL: server.URL + "/api/alpha/", APIKey: "sk-or-test", Model: model, HTTP: server.Client()}
			resp, err := client.Ask(context.Background(), Request{
				State:  map[string]any{"before": "first", "screen": "second", "query": "微信"},
				Assets: map[string]Asset{"first": {Format: "jpg", DataBase64: "before"}, "second": {Format: "png", DataBase64: "after"}},
				Questions: map[string]Question{
					"found":    {Type: TypeNoul, Instructions: "Is `query` visible in `screen`?"},
					"mode":     {Type: TypeChoice, Instructions: "Which mode?", Criteria: map[string]any{"ascii": "plain", "composition": nil}},
					"severity": {Type: TypeScore, Instructions: "How messy?", Criteria: []any{"Clean", "Minor", "Major"}},
					"where":    {Type: TypeBox, Instructions: "the result in `screen`"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Model != model {
				t.Fatalf("model = %q", got.Model)
			}
			if openRouterJev(model) {
				var state map[string]any
				if err := json.Unmarshal(got.State, &state); err != nil {
					t.Fatal(err)
				}
				if state["screen"] != "second" || got.Assets["second"].DataBase64 != "after" || got.Questions["where"].Type != TypeBox {
					t.Fatalf("Jev assets or box question were lost: %+v", got)
				}
			} else {
				var state []json.RawMessage
				if err := json.Unmarshal(got.State, &state); err != nil {
					t.Fatal(err)
				}
				if len(state) != 5 || !strings.Contains(string(state[0]), "微信") || !strings.Contains(string(state[1]), "before") ||
					!strings.Contains(string(state[2]), "data:image/jpeg;base64,before") || !strings.Contains(string(state[3]), "screen") ||
					!strings.Contains(string(state[4]), "data:image/png;base64,after") || len(got.Assets) != 0 {
					t.Fatalf("non-Jev state does not have labelled top-level images: %s", got.State)
				}
				if _, ok := got.Questions["where"]; ok {
					t.Fatal("non-Jev received a box question")
				}
			}
			found, err := resp.Answer("found", TypeNoul)
			if err != nil || *found.Noul != 0.91 {
				t.Fatalf("found = %+v, %v", found, err)
			}
			mode, err := resp.Answer("mode", TypeChoice)
			if err != nil || mode.Choice != "ascii" || !mode.Confident(0.8) || mode.Confident(0.9) {
				t.Fatalf("mode = %+v, %v", mode, err)
			}
			where, err := resp.Answer("where", TypeBox)
			if openRouterJev(model) && (err != nil || !where.Box.Valid()) {
				t.Fatalf("where = %+v, %v", where, err)
			}
			if !openRouterJev(model) && err == nil {
				t.Fatal("non-Jev model produced a box answer")
			}
			if resp.Model != model || resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 20 {
				t.Fatalf("metadata = %+v", resp)
			}
		})
	}
}

func TestOpenRouterNonJevBoxOnlyDoesNotSendRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	client := &Client{Provider: ProviderOpenRouter, BaseURL: server.URL, APIKey: "key", Model: "vendor/decision", HTTP: server.Client()}
	_, err := client.Ask(context.Background(), Request{Questions: map[string]Question{"where": {Type: TypeBox, Instructions: "locate"}}})
	if err == nil || calls != 0 {
		t.Fatalf("box-only request was not rejected locally: calls=%d err=%v", calls, err)
	}
}
