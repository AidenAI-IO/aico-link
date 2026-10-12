package langfuse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
)

type Config struct {
	BaseURL       string
	PublicKey     string
	SecretKey     string
	UploadTimeout time.Duration
}

type Client struct {
	baseURL    string
	publicKey  string
	secretKey  string
	httpClient *http.Client
}

func NewClient(cfg Config) *Client {
	var transport http.RoundTripper
	if proxy := strings.TrimSpace(os.Getenv("AIDEN_LANGFUSE_HTTPS_PROXY")); proxy != "" {
		// CI's model and device endpoints use direct routing. Only telemetry
		// inherits this proxy; retain NO_PROXY for local Langfuse deployments.
		proxyConfig := httpproxy.FromEnvironment()
		proxyConfig.HTTPSProxy = proxy
		proxyForURL := proxyConfig.ProxyFunc()
		cloned := http.DefaultTransport.(*http.Transport).Clone()
		cloned.Proxy = func(req *http.Request) (*url.URL, error) {
			return proxyForURL(req.URL)
		}
		transport = cloned
	}
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		publicKey: strings.TrimSpace(cfg.PublicKey),
		secretKey: strings.TrimSpace(cfg.SecretKey),
		httpClient: &http.Client{
			Timeout:   cfg.UploadTimeout,
			Transport: transport,
		},
	}
}

func (c *Client) Configured() bool {
	return c != nil && c.baseURL != "" && c.publicKey != "" && c.secretKey != ""
}

// Score is a Langfuse score. Scores are written through the dedicated scores
// API rather than the trace ingestion path.
type Score struct {
	ID          string
	TraceID     string
	Name        string
	Value       float64
	DataType    string
	Comment     string
	Environment string
	Metadata    map[string]interface{}
}

func (c *Client) CreateScore(ctx context.Context, score Score) error {
	if !c.Configured() {
		return fmt.Errorf("langfuse client is not configured")
	}
	payload, err := json.Marshal(map[string]interface{}{
		"id":          strings.TrimSpace(score.ID),
		"traceId":     strings.TrimSpace(score.TraceID),
		"name":        score.Name,
		"value":       score.Value,
		"dataType":    score.DataType,
		"comment":     score.Comment,
		"environment": score.Environment,
		"metadata":    score.Metadata,
	})
	if err != nil {
		return fmt.Errorf("marshal score: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/public/scores", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create score request: %w", err)
	}
	req.SetBasicAuth(c.publicKey, c.secretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("score request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("score HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

type MediaCreateRequest struct {
	TraceID       string `json:"traceId"`
	ObservationID string `json:"observationId,omitempty"`
	ContentType   string `json:"contentType"`
	ContentLength int    `json:"contentLength"`
	SHA256Hash    string `json:"sha256Hash"`
	Field         string `json:"field"`
}

type MediaCreateResponse struct {
	MediaID   string `json:"mediaId"`
	UploadURL string `json:"uploadUrl"`
}

type MediaPatchRequest struct {
	UploadedAt       string  `json:"uploadedAt"`
	UploadHTTPStatus int     `json:"uploadHttpStatus"`
	UploadHTTPError  *string `json:"uploadHttpError,omitempty"`
	UploadTimeMs     *int64  `json:"uploadTimeMs,omitempty"`
}

func (c *Client) UploadMedia(ctx context.Context, traceID, observationID, contentType string, data []byte, field string) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if !c.Configured() {
		return "", fmt.Errorf("langfuse client is not configured")
	}
	if field == "" {
		field = "output"
	}
	hash := sha256.Sum256(data)
	hashB64 := base64.StdEncoding.EncodeToString(hash[:])

	createBody, err := json.Marshal(MediaCreateRequest{
		TraceID:       traceID,
		ObservationID: strings.TrimSpace(observationID),
		ContentType:   contentType,
		ContentLength: len(data),
		SHA256Hash:    hashB64,
		Field:         field,
	})
	if err != nil {
		return "", fmt.Errorf("marshal media create request: %w", err)
	}
	createReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/public/media", bytes.NewReader(createBody))
	if err != nil {
		return "", fmt.Errorf("create media request: %w", err)
	}
	createReq.SetBasicAuth(c.publicKey, c.secretKey)
	createReq.Header.Set("Content-Type", "application/json")

	createResp, err := c.httpClient.Do(createReq)
	if err != nil {
		return "", fmt.Errorf("media create request failed: %w", err)
	}
	defer createResp.Body.Close()
	createPayload, _ := io.ReadAll(io.LimitReader(createResp.Body, 1<<20))
	if createResp.StatusCode < 200 || createResp.StatusCode >= 300 {
		return "", fmt.Errorf("media create HTTP %d: %s", createResp.StatusCode, strings.TrimSpace(string(createPayload)))
	}
	var created MediaCreateResponse
	if err := json.Unmarshal(createPayload, &created); err != nil {
		return "", fmt.Errorf("decode media create response: %w", err)
	}
	if strings.TrimSpace(created.MediaID) == "" {
		return "", fmt.Errorf("media create returned empty mediaId")
	}
	if strings.TrimSpace(created.UploadURL) != "" {
		uploadStarted := time.Now()
		uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPut, created.UploadURL, bytes.NewReader(data))
		if err != nil {
			return "", fmt.Errorf("create media upload request: %w", err)
		}
		uploadReq.Header.Set("Content-Type", contentType)
		uploadReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))
		uploadReq.Header.Set("x-amz-checksum-sha256", hashB64)
		uploadResp, err := c.httpClient.Do(uploadReq)
		if err != nil {
			return "", fmt.Errorf("media upload failed: %w", err)
		}
		uploadPayload, _ := io.ReadAll(io.LimitReader(uploadResp.Body, 1<<20))
		uploadResp.Body.Close()
		uploadTimeMs := time.Since(uploadStarted).Milliseconds()
		if err := c.patchMediaUploadStatus(ctx, created.MediaID, uploadResp.StatusCode, string(uploadPayload), uploadTimeMs); err != nil {
			return "", err
		}
		if uploadResp.StatusCode < 200 || uploadResp.StatusCode >= 300 {
			return "", fmt.Errorf("media upload HTTP %d: %s", uploadResp.StatusCode, strings.TrimSpace(string(uploadPayload)))
		}
	}
	return created.MediaID, nil
}

func (c *Client) patchMediaUploadStatus(ctx context.Context, mediaID string, statusCode int, uploadBody string, uploadTimeMs int64) error {
	var uploadErr *string
	if statusCode < 200 || statusCode >= 300 {
		msg := strings.TrimSpace(uploadBody)
		if msg == "" {
			msg = fmt.Sprintf("upload failed with HTTP %d", statusCode)
		}
		uploadErr = &msg
	}
	payload, err := json.Marshal(MediaPatchRequest{
		UploadedAt:       RFC3339(time.Now().UTC()),
		UploadHTTPStatus: statusCode,
		UploadHTTPError:  uploadErr,
		UploadTimeMs:     &uploadTimeMs,
	})
	if err != nil {
		return fmt.Errorf("marshal media patch request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+"/api/public/media/"+mediaID, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create media patch request: %w", err)
	}
	req.SetBasicAuth(c.publicKey, c.secretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("media patch request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("media patch HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func MediaToken(contentType, mediaID string) string {
	return fmt.Sprintf("@@@langfuseMedia:type=%s|id=%s|source=bytes@@@", contentType, mediaID)
}

func RFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
