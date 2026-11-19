package decision

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// encodeOpenRouter uses the native Decisions envelope, not Chat Completions.
// Jev reads System One assets as written. Other providers require images in
// the top-level state array and cannot answer box questions.
func encodeOpenRouter(req Request) ([]byte, error) {
	if openRouterJev(req.Model) {
		return json.Marshal(req)
	}
	questions := make(map[string]Question, len(req.Questions))
	for id, question := range req.Questions {
		if question.Type != TypeBox {
			questions[id] = question
		}
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("no question in the request is supported by this OpenRouter decision model")
	}
	req.Questions = questions
	if len(req.Assets) == 0 {
		return json.Marshal(req)
	}
	state, ok := req.State.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("OpenRouter decision images need a labelled state map")
	}
	var parts []any
	text := map[string]any{}
	for _, key := range slices.Sorted(maps.Keys(state)) {
		if ref, ok := state[key].(string); ok {
			if asset, ok := req.Assets[ref]; ok {
				parts = append(parts, fmt.Sprintf("`%s` is this image:", key), map[string]any{
					"type": "image_url", "image_url": map[string]string{
						"url": "data:" + geminiImageMIME(asset.Format) + ";base64," + asset.DataBase64,
					},
				})
				continue
			}
		}
		text[key] = state[key]
	}
	if len(text) > 0 {
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		parts = append([]any{string(encoded)}, parts...)
	}
	req.State, req.Assets = parts, nil
	return json.Marshal(req)
}

func openRouterJev(model string) bool {
	model = strings.TrimPrefix(strings.TrimSpace(model), "~")
	return strings.HasPrefix(model, "typesafe/jev-") && model != "typesafe/jev-router"
}
