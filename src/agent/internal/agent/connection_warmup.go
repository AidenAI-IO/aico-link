package agent

import (
	"aico-agent/internal/logging"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ConnectionWarmer manages keep-alive warming for HTTP endpoints
type ConnectionWarmer struct {
	client    *http.Client
	endpoints []string
	mu        sync.Mutex
	warming   bool
}

// NewConnectionWarmer creates a new connection warmer
func NewConnectionWarmer(client *http.Client, endpoints []string) *ConnectionWarmer {
	return &ConnectionWarmer{
		client:    client,
		endpoints: append([]string(nil), endpoints...),
	}
}

// SetEndpoints updates future warmups without changing an in-flight batch.
func (w *ConnectionWarmer) SetEndpoints(endpoints []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.endpoints = append([]string(nil), endpoints...)
}

// WarmupAsync starts warming connections in the background
// This sends lightweight HEAD or OPTIONS requests to establish and reuse connections
func (w *ConnectionWarmer) WarmupAsync(ctx context.Context) {
	w.mu.Lock()
	if w.warming || len(w.endpoints) == 0 {
		w.mu.Unlock()
		return
	}
	w.warming = true
	endpoints := w.endpoints
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			w.warming = false
			w.mu.Unlock()
		}()

		var wg sync.WaitGroup
		for _, endpoint := range endpoints {
			if endpoint == "" {
				continue
			}
			wg.Add(1)
			go func(url string) {
				defer wg.Done()
				if err := w.warmupEndpoint(ctx, url); err != nil {
					logging.Warnf("agent", "warmup", "Failed to warm %s: %v", url, err)
				} else {
					logging.Infof("agent", "warmup", "Warmed connection to %s", url)
				}
			}(endpoint)
		}
		wg.Wait()
	}()
}

// warmupEndpoint sends a lightweight request to establish a connection
func (w *ConnectionWarmer) warmupEndpoint(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// Try OPTIONS first (most lightweight), fallback to HEAD
	req, err := http.NewRequestWithContext(ctx, "OPTIONS", endpoint, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		// If OPTIONS fails, try HEAD
		req, err = http.NewRequestWithContext(ctx, "HEAD", endpoint, nil)
		if err != nil {
			return fmt.Errorf("create HEAD request: %w", err)
		}
		resp, err = w.client.Do(req)
		if err != nil {
			return fmt.Errorf("warmup request: %w", err)
		}
	}
	defer resp.Body.Close()
	// Drain and discard body to fully complete the request
	_, _ = io.Copy(io.Discard, resp.Body)

	return nil
}

// extractBaseURL extracts the base URL from a full endpoint URL
func extractBaseURL(fullURL string) string {
	// Simple extraction: find the protocol + host + port
	// Example: "https://api.openai.com/v1/chat/completions" -> "https://api.openai.com"
	if len(fullURL) < 8 {
		return fullURL
	}

	start := 0
	if fullURL[:7] == "http://" {
		start = 7
	} else if fullURL[:8] == "https://" {
		start = 8
	}

	remaining := fullURL[start:]
	slashIdx := len(remaining)
	for i, ch := range remaining {
		if ch == '/' {
			slashIdx = i
			break
		}
	}

	return fullURL[:start+slashIdx]
}
