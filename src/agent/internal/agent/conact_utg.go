package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const (
	utgMaxNodes = 192
	utgMaxEdges = 768
	utgMaxFile  = 2 << 20
)

var utgIntents = []string{"nav", "create", "edit", "commit", "cancel", "delete"}

func utgIntent(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "nav", "navigate", "open", "launch", "select", "search", "swipe", "scroll":
		return "nav"
	case "create", "add", "new", "compose":
		return "create"
	case "edit", "modify", "update", "move", "archive":
		return "edit"
	case "commit", "save", "confirm", "submit", "send":
		return "commit"
	case "cancel", "discard", "back", "close", "exit":
		return "cancel"
	case "delete", "remove", "clear", "trash":
		return "delete"
	default:
		return ""
	}
}

type utgFingerprint struct {
	Hash      uint64     `json:"hash"`
	Gray      [288]uint8 `json:"gray"`
	Landscape bool       `json:"landscape,omitempty"`
}

func fingerprintUI(data []byte) (utgFingerprint, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 2 || cfg.Height < 2 || cfg.Width > 8192 || cfg.Height > 8192 || cfg.Width*cfg.Height > 16_000_000 {
		return utgFingerprint{}, false
	}
	im, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return utgFingerprint{}, false
	}
	b := im.Bounds()
	sample := func(x, y, w, h int) uint8 {
		// Average four sub-cell samples so a narrow border doesn't dominate a cell.
		var sum int
		for _, dy := range []int{1, 3} {
			for _, dx := range []int{1, 3} {
				sum += int(color.GrayModel.Convert(im.At(b.Min.X+(4*x+dx)*b.Dx()/(4*w), b.Min.Y+(4*y+dy)*b.Dy()/(4*h))).(color.Gray).Y)
			}
		}
		return uint8(sum / 4)
	}
	f := utgFingerprint{Landscape: b.Dx() > b.Dy()}
	for y := 0; y < 8; y++ {
		left := sample(0, y, 9, 8)
		for x := 1; x < 9; x++ {
			right := sample(x, y, 9, 8)
			f.Hash <<= 1
			if left > right {
				f.Hash |= 1
			}
			left = right
		}
	}
	for i := range f.Gray {
		f.Gray[i] = sample(i%12, i/12, 12, 24)
	}
	return f, true
}

func (f utgFingerprint) distance(g utgFingerprint) (int, float64) {
	if f.Landscape != g.Landscape {
		return 64, 255
	}
	var sum float64
	for i, v := range f.Gray {
		sum += math.Abs(float64(v) - float64(g.Gray[i]))
	}
	return bits.OnesCount64(f.Hash ^ g.Hash), sum / float64(len(f.Gray))
}

type utgNode struct {
	Label   string           `json:"label"`
	Summary string           `json:"summary"`
	Samples []utgFingerprint `json:"samples"`
	Pitfall string           `json:"pitfall,omitempty"`
}
type utgEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Intent    string `json:"intent"`
	Guide     string `json:"guide"`
	Successes int    `json:"successes"`
}
type skillUTG struct {
	Nodes map[string]*utgNode `json:"nodes"`
	Edges []utgEdge           `json:"edges"`
	Tips  []string            `json:"tips,omitempty"`
}

func (g skillUTG) match(frame utgFingerprint) string {
	var best string
	low, next := math.Inf(1), math.Inf(1)
	for label, n := range g.Nodes {
		if n == nil {
			continue
		}
		score := math.Inf(1)
		for _, s := range n.Samples {
			h, d := frame.distance(s)
			if h <= 7 && d <= 16 {
				score = min(score, 2.5*float64(h)+d)
			}
		}
		if score < low {
			best, low, next = label, score, low
		} else if score < next {
			next = score
		}
	}
	// A visually ambiguous match must not silently choose a semantic screen.
	if next-low < 2 {
		return ""
	}
	return best
}

// route is shortest-hop search. Only ordinary navigation is eligible for
// intermediate hops. A non-nav final hop requires the caller's exact intent.
// At a given depth, keep only the strongest route to each intermediate state.
func (g skillUTG) route(from, to, intent string) []utgEdge {
	if from == to || g.Nodes[from] == nil || g.Nodes[to] == nil {
		return nil
	}
	if intent == "" {
		intent = "nav"
	}
	type path struct {
		edges []utgEdge
		score int
	}
	front := map[string]path{from: {}}
	seen := map[string]bool{from: true}
	for depth := 0; depth < 8 && len(front) > 0; depth++ {
		next := map[string]path{}
		for _, e := range g.Edges {
			p, ok := front[e.From]
			if !ok || seen[e.To] || (e.To == to && e.Intent != intent) || (e.To != to && e.Intent != "nav") {
				continue
			}
			if old, exists := next[e.To]; exists && old.score >= p.score+e.Successes {
				continue
			}
			next[e.To] = path{append(append([]utgEdge(nil), p.edges...), e), p.score + e.Successes}
		}
		if p, ok := next[to]; ok {
			return p.edges
		}
		for label := range next {
			seen[label] = true
		}
		front = next
	}
	return nil
}

var utgLabel = regexp.MustCompile(`^[a-z][a-z0-9]{1,23}(?:_[a-z][a-z0-9]{0,23}){1,4}$`)
var utgOverfit = regexp.MustCompile(`(?i)(https?://|[\w.+-]+@[\w.-]+|\d|[<>` + "`" + `]|cannot be completed|无法完成)`)

func utgGeneric(s string) bool {
	return len(s) >= 6 && len(s) <= 360 && !strings.ContainsAny(s, "\r\n") && !utgOverfit.MatchString(s)
}

// One step represents one dispatched action bracketed by actual observations;
// shell chains are not attributed to individual transitions.
type utgStep struct {
	Before, After utgFingerprint
	Kind          string
	Pointer, Back bool
	OK            bool
}

func (s utgStep) progressed() bool { h, d := s.Before.distance(s.After); return h >= 3 || d >= 5 }

type utgProposal struct {
	States []struct {
		Label, Summary, Side string
		Step                 int
	} `json:"states"`
	Edges []struct {
		From, To, Intent, Guide string
		Step                    int
	} `json:"edges"`
	Pitfalls []struct {
		Label, Guide      string
		Failed, Recovered int
	} `json:"pitfalls"`
	Tips []string `json:"tips"`
}

func (g *skillUTG) merge(p utgProposal, steps []utgStep) bool {
	if g.Nodes == nil {
		g.Nodes = make(map[string]*utgNode)
	}
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	changed := false
	bindings := make(map[int][2]string)
	for _, s := range p.States {
		l := norm(s.Label)
		if len(l) > 64 || !utgLabel.MatchString(l) || utgOverfit.MatchString(l) || !utgGeneric(s.Summary) || s.Step < 1 || s.Step > len(steps) || (s.Side != "before" && s.Side != "after") {
			continue
		}
		pair := bindings[s.Step]
		side := 0
		f := steps[s.Step-1].Before
		if s.Side == "after" {
			side = 1
			f = steps[s.Step-1].After
		}
		if pair[side] != "" {
			continue
		}
		n := g.Nodes[l]
		if n == nil {
			if len(g.Nodes) >= utgMaxNodes {
				continue
			}
			n = &utgNode{Label: l, Summary: s.Summary}
			g.Nodes[l] = n
			changed = true
		}
		pair[side] = l
		bindings[s.Step] = pair
		duplicate := false
		for _, sample := range n.Samples {
			h, d := sample.distance(f)
			if h <= 2 && d <= 5 {
				duplicate = true
				break
			}
		}
		if !duplicate {
			if len(n.Samples) == 5 {
				copy(n.Samples[1:], n.Samples[2:])
				n.Samples = n.Samples[:4]
			}
			n.Samples = append(n.Samples, f)
			changed = true
		}
	}
	verified := make(map[int]bool)
	for _, e := range p.Edges {
		from, to, intent := norm(e.From), norm(e.To), utgIntent(e.Intent)
		if verified[e.Step] || e.Step < 1 || e.Step > len(steps) || from == to || intent == "" || !utgGeneric(e.Guide) || bindings[e.Step] != [2]string{from, to} || !steps[e.Step-1].OK || !steps[e.Step-1].progressed() {
			continue
		}
		verified[e.Step] = true
		parallel := 0
		var exists bool
		for i, old := range g.Edges {
			if old.From == from && old.To == to {
				parallel++
				if old.Intent == intent {
					g.Edges[i].Successes = min(old.Successes, 9999) + 1
					g.Edges[i].Guide = e.Guide
					exists = true
					changed = true
					break
				}
			}
		}
		if !exists && parallel < 3 && len(g.Edges) < utgMaxEdges {
			g.Edges = append(g.Edges, utgEdge{from, to, intent, e.Guide, 1})
			changed = true
		}
	}
	for _, p := range p.Pitfalls {
		l := norm(p.Label)
		n := g.Nodes[l]
		if n == nil || n.Pitfall != "" || !utgGeneric(p.Guide) || p.Failed < 1 || p.Recovered > len(steps) || p.Recovered <= p.Failed || p.Recovered-p.Failed > 8 || bindings[p.Failed][0] != l {
			continue
		}
		end := steps[p.Recovered-1]
		continuous := true
		for i := p.Failed; i < p.Recovered; i++ {
			h, d := steps[i-1].After.distance(steps[i].Before)
			if h > 2 || d > 5 {
				continuous = false
				break
			}
		}
		if !continuous || !end.OK || !end.progressed() {
			continue
		}
		friction := false
		repeats := map[string]int{}
		for _, s := range steps[p.Failed-1 : p.Recovered-1] {
			repeats[s.Kind]++
			if (s.Pointer && !s.progressed()) || s.Back || (s.Pointer && repeats[s.Kind] >= 2) {
				friction = true
				break
			}
		}
		if friction {
			n.Pitfall = p.Guide
			changed = true
		}
	}
	if len(verified) == 0 {
		return changed
	}
	for _, t := range p.Tips {
		if len(g.Tips) >= 12 {
			break
		}
		if !utgGeneric(t) {
			continue
		}
		duplicate := false
		for _, old := range g.Tips {
			if strings.EqualFold(old, t) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			g.Tips = append(g.Tips, t)
			changed = true
		}
	}
	return changed
}

func (g skillUTG) landmarks(query string, limit int) []string {
	query = strings.ToLower(strings.TrimSpace(query))
	var labels []string
	for label, n := range g.Nodes {
		if n != nil && (query == "" || strings.Contains(label, query) || strings.Contains(strings.ToLower(n.Summary), query)) {
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	return labels[:min(len(labels), limit)]
}

var utgWriteMu sync.Mutex

func loadSkillUTG(path string) (skillUTG, error) {
	var g skillUTG
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return g, nil
	}
	if err != nil {
		return g, err
	}
	if !st.Mode().IsRegular() || st.Size() > utgMaxFile {
		return g, fmt.Errorf("invalid UTG file size or type")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return g, err
	}
	err = json.Unmarshal(b, &g)
	return g, err
}

func saveSkillUTG(path string, g skillUTG) error {
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	if len(b) > utgMaxFile {
		return fmt.Errorf("UTG exceeds storage limit")
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".utg-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(b)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
