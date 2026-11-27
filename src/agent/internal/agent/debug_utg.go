package agent

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"time"
)

// Expose operator-facing metadata, not the persisted visual fingerprints,
// source frames, configuration paths, or raw episode data.
type debugUTGNode struct {
	Label   string `json:"label"`
	Summary string `json:"summary"`
	Samples int    `json:"samples"`
	Pitfall string `json:"pitfall,omitempty"`
}

type debugUTGSnapshot struct {
	Enabled        bool           `json:"enabled"`
	Available      bool           `json:"available"`
	Platform       string         `json:"platform"`
	CurrentLabel   string         `json:"current_label,omitempty"`
	HasRecentFrame bool           `json:"has_recent_frame"`
	Nodes          []debugUTGNode `json:"nodes"`
	Edges          []utgEdge      `json:"edges"`
	Intents        []string       `json:"intents"`
	Tips           []string       `json:"tips"`
}

func (s *Server) handleDebugUTG(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rt := s.runtime
	if rt == nil {
		http.Error(w, "Agent runtime unavailable", http.StatusServiceUnavailable)
		return
	}
	cfg := rt.ConfigSnapshot()
	view := debugUTGSnapshot{Enabled: cfg.supportsConact(), Platform: rt.devicePlatformFromState(),
		Nodes: []debugUTGNode{}, Edges: []utgEdge{}, Intents: utgIntents, Tips: []string{}}
	if path := skillUTGPath(cfg, view.Platform); path != "" {
		view.Available = true
		graph, err := loadSkillUTG(path)
		if err != nil {
			http.Error(w, "Saved UTG could not be read", http.StatusInternalServerError)
			return
		}
		for _, label := range graph.landmarks("", len(graph.Nodes)) {
			node := graph.Nodes[label]
			view.Nodes = append(view.Nodes, debugUTGNode{label, node.Summary, len(node.Samples), node.Pitfall})
		}
		view.Edges = append(view.Edges, graph.Edges...)
		slices.SortFunc(view.Edges, func(a, b utgEdge) int {
			return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To), cmp.Compare(a.Intent, b.Intent))
		})
		view.Tips = append(view.Tips, graph.Tips...)
		// Never request a new capture or change the current cached frame.
		if data, _, _, _, ok := rt.screenState.LatestScreenshot(30 * time.Second); ok {
			view.HasRecentFrame = true
			if len(view.Nodes) > 0 {
				if frame, ok := fingerprintUI(data); ok {
					view.CurrentLabel = graph.match(frame)
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}
