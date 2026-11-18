package agent

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleUTG(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g := utgGraph{}
	if s.runtime != nil && s.runtime.utg != nil {
		g = s.runtime.utg.snapshot()
	}
	for i := range g.Nodes {
		g.Nodes[i].Signature = nil
		g.Nodes[i].Exact = [32]byte{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
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
