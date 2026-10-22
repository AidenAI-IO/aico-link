// Package decision is a client for decision models: TypeSafe's System One API
// (Jev) and OpenAI's Decisions API (GPT-6 Luna). Both answer typed questions
// about a state: a choice among named options, a yes/no probability (noul),
// or a score; Jev also answers a box in an image. Neither generates text, so
// callers keep their own fallback for anything they cannot answer confidently.
//
// Requests are written in System One's shape; the OpenAI backend translates
// them and leaves out box questions, which it cannot answer.
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds one request, retries included.
	DefaultTimeout = 15 * time.Second

	imagesHeader   = "TypeSafe-Danger-Zone-Images"
	maxRetries     = 2
	retryBaseDelay = 400 * time.Millisecond
	maxErrorBody   = 512
)

// Question types accepted by System One.
const (
	TypeChoice = "choice"
	TypeNoul   = "noul"
	TypeScore  = "score"
	TypeBox    = "box"
)

// Question is one typed question. Instructions and Criteria may be strings,
// objects or arrays; an image is referenced by its asset key inside them.
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Asset is an inline image. Its key in Request.Assets must appear somewhere in
// the state, instructions or criteria.
type Asset struct {
	Type       string `json:"type,omitempty"`
	DataBase64 string `json:"data_base64"`
	Format     string `json:"format"`
}

// Request is the System One request body. Model is filled from the client.
type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
	Assets    map[string]Asset    `json:"assets,omitempty"`
}

// Box is a normalized rectangle: 0..1 fractions of the image, origin top-left.
type Box struct {
	Top    float64 `json:"top"`
	Bottom float64 `json:"bottom"`
	Left   float64 `json:"left"`
	Right  float64 `json:"right"`
}

// Answer is the answer to one question; which fields are set depends on Type.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Box           *Box               `json:"box,omitempty"`
}

// Usage reports billed tokens.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the System One response body.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Providers.
const (
	ProviderTypeSafe = "typesafe"
	ProviderOpenAI   = "openai"
)

// Client calls a decision model. HTTP is required so callers decide the proxy.
type Client struct {
	// Provider selects the API; empty means TypeSafe.
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
	HTTP     *http.Client
}

// StatusError is a non-2xx response.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("decision API returned %d: %s", e.StatusCode, e.Body)
}

// Ask sends one request. 429 and 529 are retried with backoff, as the API
// documentation asks; any other failure is returned as is.
func (c *Client) Ask(ctx context.Context, req Request) (Response, error) {
	if c == nil || c.HTTP == nil {
		return Response{}, errors.New("decision client is not configured")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return Response{}, errors.New("decision client has no API key")
	}
	if len(req.Questions) == 0 {
		return Response{}, errors.New("decision request has no questions")
	}
	req.Model = c.Model
	var (
		body     []byte
		err      error
		endpoint string
		headers  = map[string]string{}
		decode   = decodeTypeSafe
	)
	if c.Provider == ProviderOpenAI {
		body, err = encodeOpenAI(req)
		endpoint = strings.TrimRight(c.BaseURL, "/") + "/decisions"
		decode = decodeOpenAI
	} else {
		body, err = json.Marshal(req)
		endpoint = strings.TrimRight(c.BaseURL, "/") + "/systemone"
		images := len(req.Assets) > 0
		for _, question := range req.Questions {
			if question.Type == TypeBox {
				images = true
			}
		}
		if images {
			headers[imagesHeader] = "true"
		}
	}
	if err != nil {
		return Response{}, fmt.Errorf("encode decision request: %w", err)
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.post(ctx, endpoint, body, headers, decode)
		var status *StatusError
		if err == nil || attempt >= maxRetries || !errors.As(err, &status) || !retryable(status.StatusCode) {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(retryBaseDelay << attempt):
		}
	}
}

func (c *Client) post(ctx context.Context, endpoint string, body []byte, headers map[string]string, decode func([]byte) (Response, error)) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.APIKey))
	httpReq.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		httpReq.Header.Set(key, value)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := strings.TrimSpace(string(data))
		if len(snippet) > maxErrorBody {
			snippet = snippet[:maxErrorBody]
		}
		return Response{}, &StatusError{StatusCode: resp.StatusCode, Body: snippet}
	}
	out, err := decode(data)
	if err != nil {
		return Response{}, fmt.Errorf("decode decision response: %w", err)
	}
	return out, nil
}

func decodeTypeSafe(data []byte) (Response, error) {
	var out Response
	err := json.Unmarshal(data, &out)
	return out, err
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == 529
}

// Answer returns the answer for id, checking its type.
func (r Response) Answer(id, questionType string) (Answer, error) {
	answer, ok := r.Answers[id]
	if !ok {
		return Answer{}, fmt.Errorf("decision response has no answer %q", id)
	}
	if answer.Type != questionType {
		return Answer{}, fmt.Errorf("decision answer %q is %q, want %q", id, answer.Type, questionType)
	}
	switch questionType {
	case TypeNoul:
		if answer.Noul == nil {
			return Answer{}, fmt.Errorf("decision answer %q has no noul", id)
		}
	case TypeChoice:
		if answer.Choice == "" || answer.Confidence == nil {
			return Answer{}, fmt.Errorf("decision answer %q has no choice", id)
		}
	case TypeBox:
		if answer.Box == nil || answer.Confidence == nil {
			return Answer{}, fmt.Errorf("decision answer %q has no box", id)
		}
	}
	return answer, nil
}
