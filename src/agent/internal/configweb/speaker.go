package configweb

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Keep in sync with agent.speakerEnrollMaxBodyBytes (16s PCM16 mono @ 16 kHz).
const speakerProxyMaxUploadBytes = 16 * 16000 * 2

// handleSpeakerProxy forwards Classic speaker-gate management to the Agent.
// Browser clients talk to Config Web only; the Agent owns capture, embeddings,
// and profile storage.
func (s *Server) handleSpeakerProxy(w http.ResponseWriter, r *http.Request) {
	if !isSameOriginBrowserRequest(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if s.maintenance != nil && s.maintenance.active() &&
		(r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		s.writeMaintenanceLocked(w)
		return
	}
	base, err := s.agentBaseURL()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	targetPath := r.URL.Path
	if !strings.HasPrefix(targetPath, apiPrefix+"/speaker") {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	target, err := url.Parse(strings.TrimRight(base.String(), "/") + targetPath)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent speaker endpoint is invalid")
		return
	}
	target.RawQuery = r.URL.RawQuery

	var body io.Reader
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		upload, readErr := io.ReadAll(io.LimitReader(r.Body, speakerProxyMaxUploadBytes+1))
		if readErr != nil {
			writeJSONError(w, http.StatusBadRequest, "read speaker upload: "+readErr.Error())
			return
		}
		if len(upload) > speakerProxyMaxUploadBytes {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "speaker upload exceeds 16s PCM16 limit")
			return
		}
		body = bytes.NewReader(upload)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "create agent speaker request: "+err.Error())
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	timeout := 20 * time.Second
	if strings.HasSuffix(strings.TrimSuffix(targetPath, "/"), "/enroll/capture") {
		timeout = 45 * time.Second
	}
	response, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("agent speaker request failed: %v", err))
		return
	}
	defer response.Body.Close()

	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxRequestBodySize+1))
	if readErr != nil || len(payload) > maxRequestBodySize {
		writeJSONError(w, http.StatusBadGateway, "agent speaker returned an oversized or unreadable body")
		return
	}
	if response.StatusCode == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if ct := response.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
}
