package agent

import (
	"encoding/json"
	"net/http"
	"slices"
)

func (s *Server) handleUTG(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodDelete:
		var sameOrigin http.CrossOriginProtection
		if err := sameOrigin.Check(r); err != nil {
			http.Error(w, "Cross-origin writes are not allowed", http.StatusForbidden)
			return
		}
		if s.runtime == nil || s.runtime.utg == nil {
			http.Error(w, "Device navigation graph is unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := s.runtime.utg.reset(); err != nil {
			if s.logger != nil {
				s.logger.Warn("UTG reset: %v", err)
			}
			http.Error(w, "Unable to reset navigation graph", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g := utgGraph{}
	if s.runtime != nil && s.runtime.utg != nil {
		g = s.runtime.utg.snapshot()
	}
	g.Edges = slices.DeleteFunc(g.Edges, func(e utgEdge) bool { return e.MismatchState != "" })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(g)
}

func (s *Server) handleUTGDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(mustReadWebUIFile("utg.html")))
}
