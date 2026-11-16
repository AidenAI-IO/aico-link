package decision

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Gemini Decisions API wire types (POST /v1beta/decisions).

type geminiRequest struct {
	Model     string                    `json:"model"`
	Input     []geminiInput             `json:"input"`
	Questions map[string]geminiQuestion `json:"questions"`
}

type geminiInput struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	Data       string `json:"data,omitempty"`
	MIMEType   string `json:"mime_type,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

type geminiQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type geminiResponse struct {
	Model   string                  `json:"model"`
	Answers map[string]geminiAnswer `json:"answers"`
	Usage   struct {
		TotalInputTokens  int `json:"total_input_tokens"`
		TotalOutputTokens int `json:"total_output_tokens"`
		TotalTokens       int `json:"total_tokens"`
	} `json:"usage"`
}

type geminiAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// encodeGemini translates a System One request into Gemini Decisions input.
// State entries become labelled input parts: an asset reference becomes that
// image, and the rest is sent as JSON text so instructions can keep naming
// entries such as `screen`. Box questions are left out: Gemini Decisions has
// no box answer, and callers treat the missing answer as not confident.
func encodeGemini(req Request) ([]byte, error) {
	var parts []geminiInput
	text := map[string]any{}
	if state, ok := req.State.(map[string]any); ok {
		for _, key := range slices.Sorted(maps.Keys(state)) {
			if ref, isString := state[key].(string); isString {
				if asset, isAsset := req.Assets[ref]; isAsset {
					parts = append(parts,
						geminiInput{Type: "text", Text: fmt.Sprintf("`%s` is this image:", key)},
						geminiInput{
							Type:       "image",
							Data:       asset.DataBase64,
							MIMEType:   geminiImageMIME(asset.Format),
							Resolution: "high",
						})
					continue
				}
			}
			text[key] = state[key]
		}
	} else if req.State != nil {
		text["state"] = req.State
	}
	if len(text) > 0 {
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		parts = append([]geminiInput{{Type: "text", Text: string(encoded)}}, parts...)
	}
	if len(parts) == 0 {
		parts = []geminiInput{{Type: "text", Text: ""}}
	}

	ids := slices.Sorted(maps.Keys(req.Questions))
	questions := make(map[string]geminiQuestion, len(ids))
	for _, id := range ids {
		q := req.Questions[id]
		instructions, err := instructionText(q.Instructions)
		if err != nil {
			return nil, err
		}
		switch q.Type {
		case TypeNoul:
			questions[id] = geminiQuestion{Type: TypeNoul, Instructions: instructions, Criteria: geminiNoulCriteria(q.Criteria)}
		case TypeChoice:
			criteria, ok := q.Criteria.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("choice %q needs a criteria map", id)
			}
			values := slices.Sorted(maps.Keys(criteria))
			mapped := make(map[string]string, len(values))
			for _, value := range values {
				description, _ := criteria[value].(string)
				mapped[value] = description
			}
			questions[id] = geminiQuestion{Type: TypeChoice, Instructions: instructions, Criteria: mapped}
		case TypeScore:
			levels, err := geminiScoreCriteria(q.Criteria)
			if err != nil {
				return nil, fmt.Errorf("score %q: %w", id, err)
			}
			questions[id] = geminiQuestion{Type: TypeScore, Instructions: instructions, Criteria: levels}
		case TypeBox:
			// Not supported by Gemini Decisions; see above.
		default:
			return nil, fmt.Errorf("question %q type %q is not supported by Gemini decisions", id, q.Type)
		}
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("no question in the request is supported by Gemini decisions")
	}
	return json.Marshal(geminiRequest{Model: req.Model, Input: parts, Questions: questions})
}

func geminiImageMIME(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	default:
		return "image/jpeg"
	}
}

func geminiNoulCriteria(value any) any {
	criteria, ok := value.(map[string]any)
	if !ok || len(criteria) == 0 {
		return nil
	}
	out := map[string]string{}
	if text, ok := criteria["true_description"].(string); ok {
		out["true_description"] = text
	}
	if text, ok := criteria["false_description"].(string); ok {
		out["false_description"] = text
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func geminiScoreCriteria(value any) ([]string, error) {
	switch criteria := value.(type) {
	case []string:
		if len(criteria) < 2 {
			return nil, fmt.Errorf("needs at least two levels")
		}
		return criteria, nil
	case []any:
		if len(criteria) < 2 {
			return nil, fmt.Errorf("needs at least two levels")
		}
		levels := make([]string, 0, len(criteria))
		for i, level := range criteria {
			text, ok := level.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("level %d must be a non-empty string", i)
			}
			levels = append(levels, text)
		}
		return levels, nil
	default:
		return nil, fmt.Errorf("needs a criteria array")
	}
}

// decodeGemini maps answers back to System One's shape.
func decodeGemini(data []byte) (Response, error) {
	var raw geminiResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return Response{}, err
	}
	out := Response{
		Model:   raw.Model,
		Answers: map[string]Answer{},
		Usage: Usage{
			InputTokens:  raw.Usage.TotalInputTokens,
			OutputTokens: raw.Usage.TotalOutputTokens,
		},
	}
	for id, answer := range raw.Answers {
		switch answer.Type {
		case TypeNoul:
			out.Answers[id] = Answer{Type: TypeNoul, Noul: answer.Noul, Confidence: answer.Confidence}
		case TypeChoice:
			out.Answers[id] = Answer{
				Type:          TypeChoice,
				Choice:        answer.Choice,
				Confidence:    answer.Confidence,
				Probabilities: answer.Probabilities,
			}
		case TypeScore:
			out.Answers[id] = Answer{
				Type:          TypeScore,
				Score:         answer.Score,
				Confidence:    answer.Confidence,
				Probabilities: answer.Probabilities,
			}
		}
	}
	return out, nil
}
