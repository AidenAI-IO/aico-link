package mnk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"unicode"
	"unicode/utf8"
)

// ValidateInsertionText runs before any focus, selection or input side effect.
func ValidateInsertionText(text string) error {
	units := 0
	if !utf8.ValidString(text) {
		return InvalidArguments("insert_text requires valid UTF-8")
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return InvalidArguments("insert_text does not accept control characters other than newline/tab")
		}
		units++
		if r > 0xffff {
			units++
		}
	}
	if units == 0 || units > 4096 {
		return InvalidArguments("insert_text requires 1–4096 UTF-16 units")
	}
	return nil
}

func (p *HTTPProvider) SupportsTextInsertion(ctx context.Context) (bool, error) {
	caps, err := p.queryCapabilities(ctx)
	return caps.InsertText, err
}

type httpCapabilities struct {
	InsertText    bool     `json:"insert_text"`
	SystemActions []string `json:"system_actions"`
}

func (p *HTTPProvider) queryCapabilities(ctx context.Context) (httpCapabilities, error) {
	var empty httpCapabilities
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/providers/mnk", nil)
	if err != nil {
		return empty, err
	}
	if p.taskID != "" {
		req.Header.Set(BenchmarkTaskIDHeader, p.taskID)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return empty, fmt.Errorf("query input capabilities: %w", err)
	}
	defer resp.Body.Close()
	// Existing bridges are POST-only and retain their original IME path.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return empty, nil
	}
	if resp.StatusCode != http.StatusOK {
		return empty, fmt.Errorf("query input capabilities: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Capabilities httpCapabilities `json:"capabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&result); err != nil {
		return empty, fmt.Errorf("decode input capabilities: %w", err)
	}
	return result.Capabilities, nil
}

func (p *HTTPProvider) InsertText(ctx context.Context, text string, replace bool) error {
	if err := ValidateInsertionText(text); err != nil {
		return err
	}
	return p.sendRequest(ctx, MNKRequest{Operation: "insert_text", InsertText: &TextInsertionParams{Text: text, Replace: replace}})
}
