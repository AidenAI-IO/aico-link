package agent

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) handleServiceDiagnostics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(collectCoreServiceStatuses())
}

func (s *Server) handleServiceIncidents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := diagnosticsEventsLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 && value <= 200 {
			limit = value
		}
	}
	events, err := readServiceIncidentEvents(limit, r.URL.Query().Get("unit"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"events": events,
	})
}

func (s *Server) handleServiceIncidentLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	unit := strings.TrimSpace(r.URL.Query().Get("unit"))
	logTail, err := readServiceIncidentLogTail(unit, 32*1024)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"unit": unit,
		"log":  logTail,
	})
}
