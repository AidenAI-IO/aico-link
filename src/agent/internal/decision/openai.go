package decision

import (
	"encoding/json"
	"fmt"
	"sort"
)

// OpenAI Decisions API wire types (POST /v1/decisions).

type openAIRequest struct {
	Model     string           `json:"model"`
	Input     []openAIMessage  `json:"input"`
	Questions []openAIQuestion `json:"questions"`
}

type openAIMessage struct {
	Role    string        `json:"role"`
	Content []openAIInput `json:"content"`
}

type openAIInput struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type openAIQuestion struct {
	Type         string         `json:"type"`
	Name         string         `json:"name"`
	Instructions string         `json:"instructions"`
	Choices      []openAIChoice `json:"choices,omitempty"`
}

type openAIChoice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type openAIResponse struct {
	Model   string `json:"model"`
	Answers []struct {
		Type          string   `json:"type"`
		Name          string   `json:"name"`
		Probability   *float64 `json:"probability"`
		Choice        string   `json:"choice"`
		Confidence    *float64 `json:"confidence"`
		Probabilities []struct {
			Value       any     `json:"value"`
			Probability float64 `json:"probability"`
		} `json:"probabilities"`
	} `json:"answers"`
	Usage Usage `json:"usage"`
}

// encodeOpenAI translates a System One request. State entries become labelled
// input parts: an entry whose value is an asset reference becomes that image,
// and the rest is sent as JSON text, so instructions can keep naming entries
// such as `screen`. Box questions are left out: OpenAI has no box answer, and
// callers treat the missing answer as not confident.
func encodeOpenAI(req Request) ([]byte, error) {
	var parts []openAIInput
	text := map[string]any{}
	if state, ok := req.State.(map[string]any); ok {
		keys := make([]string, 0, len(state))
		for key := range state {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if ref, isString := state[key].(string); isString {
				if asset, isAsset := req.Assets[ref]; isAsset {
					parts = append(parts,
						openAIInput{Type: "input_text", Text: fmt.Sprintf("`%s` is this image:", key)},
						openAIInput{Type: "input_image", ImageURL: "data:image/jpeg;base64," + asset.DataBase64})
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
		parts = append([]openAIInput{{Type: "input_text", Text: string(encoded)}}, parts...)
	}

	ids := make([]string, 0, len(req.Questions))
	for id := range req.Questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	questions := make([]openAIQuestion, 0, len(ids))
	for _, id := range ids {
		q := req.Questions[id]
		instructions, err := instructionText(q.Instructions)
		if err != nil {
			return nil, err
		}
		switch q.Type {
		case TypeNoul:
			questions = append(questions, openAIQuestion{Type: "predicate", Name: id, Instructions: instructions})
		case TypeChoice:
			criteria, ok := q.Criteria.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("choice %q needs a criteria map", id)
			}
			values := make([]string, 0, len(criteria))
			for value := range criteria {
				values = append(values, value)
			}
			sort.Strings(values)
			choices := make([]openAIChoice, 0, len(values))
			for _, value := range values {
				description, _ := criteria[value].(string)
				choices = append(choices, openAIChoice{Value: value, Description: description})
			}
			questions = append(questions, openAIQuestion{Type: "choice", Name: id, Instructions: instructions, Choices: choices})
		case TypeBox:
			// Not supported by OpenAI; see above.
		default:
			return nil, fmt.Errorf("question %q type %q is not supported by OpenAI decisions", id, q.Type)
		}
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("no question in the request is supported by OpenAI decisions")
	}
	return json.Marshal(openAIRequest{
		Model:     req.Model,
		Input:     []openAIMessage{{Role: "user", Content: parts}},
		Questions: questions,
	})
}

func instructionText(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

// decodeOpenAI maps answers back to System One's shape. A refusal, or any
// answer type without a counterpart, is left out and reads as missing.
func decodeOpenAI(data []byte) (Response, error) {
	var raw openAIResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return Response{}, err
	}
	out := Response{Model: raw.Model, Answers: map[string]Answer{}, Usage: raw.Usage}
	for _, answer := range raw.Answers {
		switch answer.Type {
		case "predicate":
			out.Answers[answer.Name] = Answer{Type: TypeNoul, Noul: answer.Probability}
		case "choice":
			probabilities := make(map[string]float64, len(answer.Probabilities))
			for _, p := range answer.Probabilities {
				probabilities[fmt.Sprint(p.Value)] = p.Probability
			}
			out.Answers[answer.Name] = Answer{Type: TypeChoice, Choice: answer.Choice, Confidence: answer.Confidence, Probabilities: probabilities}
		}
	}
	return out, nil
}
