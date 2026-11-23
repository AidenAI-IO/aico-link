package decision

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// encodeOpenRouter uses the native Decisions envelope, not Chat Completions.
// Jev reads System One assets as written. Other providers require images in
// the top-level state array and cannot answer box questions.
func encodeOpenRouter(req Request) ([]byte, error) {
	jev := openRouterJev(req.Model)
	boxSupported := jev && openRouterJevSupportsBox(req.Model)
	questions := make(map[string]Question, len(req.Questions))
	for id, question := range req.Questions {
		if question.Type != TypeBox || boxSupported {
			questions[id] = question
		}
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("no question in the request is supported by this OpenRouter decision model")
	}
	req.Questions = questions
	if jev || len(req.Assets) == 0 {
		return json.Marshal(req)
	}
	state, ok := req.State.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("OpenRouter decision images need a labelled state map")
	}
	var parts []any
	emitted := make(map[string]bool, len(req.Assets))
	appendImage := func(label string, asset Asset) {
		parts = append(parts, label, map[string]any{
			"type": "image_url", "image_url": map[string]string{
				"url": "data:" + geminiImageMIME(asset.Format) + ";base64," + asset.DataBase64,
			},
		})
	}
	text := map[string]any{}
	for _, key := range slices.Sorted(maps.Keys(state)) {
		if ref, ok := state[key].(string); ok {
			if asset, ok := req.Assets[ref]; ok {
				appendImage(fmt.Sprintf("`%s` (asset `%s`) is this image:", key, ref), asset)
				emitted[ref] = true
				continue
			}
		}
		text[key] = state[key]
	}
	// Instructions, criteria, and nested state may refer to an asset directly.
	for _, ref := range slices.Sorted(maps.Keys(req.Assets)) {
		if !emitted[ref] {
			appendImage(fmt.Sprintf("`%s` is this image:", ref), req.Assets[ref])
		}
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

// Jev introduced box questions in 1.14; older versions retain native assets.
func openRouterJevSupportsBox(model string) bool {
	version := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(model), "~"), "typesafe/jev-")
	if version == "latest" {
		return true
	}
	majorText, minorText, ok := strings.Cut(version, ".")
	if !ok {
		return false
	}
	major, majorErr := strconv.Atoi(majorText)
	minor, minorErr := strconv.Atoi(minorText)
	return majorErr == nil && minorErr == nil && (major > 1 || major == 1 && minor >= 14)
}
