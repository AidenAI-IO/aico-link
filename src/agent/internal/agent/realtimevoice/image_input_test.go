package realtimevoice

import (
	"encoding/json"
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
	// A replayed message whose text is only its image caption keeps the
	// caption and sends no empty text part.
	captionOnly := ContextItem{Type: "message", Role: "user", Images: []Image{image}}
	replayed, _ := json.Marshal(openAIContextItemPayload(captionOnly, ""))
	if want := `{"content":[{"text":"screen","type":"input_text"},{"image_url":"data:image/jpeg;base64,anBlZw==","type":"input_image"}],"role":"user","type":"message"}`; string(replayed) != want {
		t.Fatalf("replayed openai message = %s, want %s", replayed, want)
	}
	geminiReplay, _ := json.Marshal(geminiMessageParts(captionOnly))
	if want := `[{"text":"screen"},{"inlineData":{"data":"anBlZw==","mimeType":"image/jpeg"}}]`; string(geminiReplay) != want {
		t.Fatalf("replayed gemini turn = %s, want %s", geminiReplay, want)
	}
}
