package realtimevoice

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSupportsImageInput(t *testing.T) {
	for _, tc := range []struct {
		provider, model string
		want            bool
	}{
		{ProviderOpenAI, "", true},
		{ProviderOpenAI, "gpt-realtime-2", true},
		{ProviderGemini, "models/gemini-3.8-live", true},
		{ProviderGemini, Gemini38ThinkingModel, true},
		{ProviderGemini, "", false},
		{ProviderGemini, DefaultGeminiLiveModel, false},
		{ProviderQwen, "", false},
		{ProviderXAI, "", false},
		{ProviderMoss, "", false},
	} {
		if got := SupportsImageInput(tc.provider, tc.model); got != tc.want {
			t.Errorf("SupportsImageInput(%q, %q) = %t, want %t", tc.provider, tc.model, got, tc.want)
		}
	}
}

// Images follow a tool result in each provider's own wire shape: inlineData
// parts of the Gemini function response, and an input_image user message
// after the OpenAI function_call_output.
func TestImageWirePayloads(t *testing.T) {
	image := Image{Caption: "screen", MIMEType: "image/jpeg", Data: []byte("jpeg")}
	gemini, _ := json.Marshal(geminiFunctionResponse("call_1", "screenshot", `"ok"`, []Image{image}))
	if want := `{"id":"call_1","name":"screenshot","parts":[{"inlineData":{"data":"anBlZw==","mimeType":"image/jpeg"}}],"response":{"image_caption":"screen","result":"ok"}}`; string(gemini) != want {
		t.Fatalf("gemini function response = %s, want %s", gemini, want)
	}
	openAI, _ := json.Marshal(openAIImageContent([]Image{image}))
	if want := `[{"text":"screen","type":"input_text"},{"image_url":"data:image/jpeg;base64,anBlZw==","type":"input_image"}]`; string(openAI) != want {
		t.Fatalf("openai image content = %s, want %s", openAI, want)
	}
	replayed, _ := json.Marshal(openAIContextItemPayload(ContextItem{Type: "message", Role: "user", Content: "hi", Images: []Image{image}}, ""))
	if !strings.Contains(string(replayed), `"input_image"`) {
		t.Fatalf("replayed user message = %s, want its image", replayed)
	}
}
