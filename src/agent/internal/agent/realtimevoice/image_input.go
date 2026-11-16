package realtimevoice

import (
	"context"
	"encoding/base64"
	"strings"
)

// imageInputModelPrefixes lists the realtime models that accept an image as
// part of the conversation, after a tool result or in replayed history.
// Models that only take images as streamed video frames or input buffers
// (Gemini 3.1 Flash Live, Qwen Omni, MOSS Live) are not listed: their images
// cannot follow a tool result.
var imageInputModelPrefixes = []string{
	"gpt-realtime",    // OpenAI: input_image content in a user message
	"gemini-3.8-live", // Gemini: inlineData parts in a function response or turn
}

// SupportsImageInput reports whether the realtime model selected for provider
// accepts images in the conversation. An empty model is the provider default.
func SupportsImageInput(provider, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if index := strings.LastIndex(model, "/"); index >= 0 {
		model = model[index+1:]
	}
	if model == "" {
		switch normalizeProviderName(provider) {
		case ProviderOpenAI:
			model = DefaultOpenAIRealtimeModel
		case ProviderGemini:
			model = DefaultGeminiLiveModel
		}
	}
	for _, prefix := range imageInputModelPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// Image is an image sent to a realtime model with the caption that explains
// what it is.
type Image struct {
	Caption  string
	MIMEType string
	Data     []byte
}

func (i Image) dataURL() string {
	return "data:" + i.mimeType() + ";base64," + base64.StdEncoding.EncodeToString(i.Data)
}

func (i Image) mimeType() string {
	if mimeType := strings.TrimSpace(i.MIMEType); mimeType != "" {
		return mimeType
	}
	return "image/jpeg"
}

// ImageToolResultSender is an optional session capability for providers that
// can show the model the images a tool returned. Callers use it only for a
// model SupportsImageInput reports; other models get a text tool result.
type ImageToolResultSender interface {
	SendToolResultWithImages(ctx context.Context, id, output string, images []Image) error
}
