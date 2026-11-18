package agent

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

func (r *Runtime) runUTGCommand(ctx context.Context, s *Server, args []string, capture *deviceCLICapture) (ToolInvokeResponse, error) {
	turn, _ := ctx.Value(utgTurnKey{}).(*utgTurn)
	if turn == nil {
		return ToolInvokeResponse{}, fmt.Errorf("UTG requires an active device turn")
	}
	graph := turn.graph
	if args[0] == "utg-tag" {
		if len(args) != 3 || len(args[2]) > 100 || strings.TrimSpace(args[2]) == "" {
			return ToolInvokeResponse{}, fmt.Errorf("usage: utg-tag STATE LABEL (max 100 bytes)")
		}
		if !slices.ContainsFunc(graph.Nodes, func(n utgNode) bool { return n.Key == args[1] }) {
			return ToolInvokeResponse{}, fmt.Errorf("unknown state: %s", args[1])
		}
		turn.tags[args[1]] = args[2]
		return ToolInvokeResponse{Output: "Label queued for the end of this turn."}, nil
	}
	if (args[0] == "utg-status" && len(args) != 1) || (args[0] != "utg-status" && len(args) != 2) {
		return ToolInvokeResponse{}, fmt.Errorf("invalid %s arguments", args[0])
	}
	var advisor *decisionAdvisor
	if tools := r.toolSnapshot(); tools != nil {
		advisor = tools.decider
	}
	run := func(candidates []utgNode, action ...string) (utgMatch, error) {
		capture.last = visualScreenshotObservation{}
		res, err := r.executeDeviceCLI(ctx, s, action, capture)
		if err != nil {
			return utgMatch{}, err
		}
		if res.IsError {
			return utgMatch{}, fmt.Errorf("%s: %s", action[0], res.Error)
		}
		if len(capture.last.ImageBytes) == 0 {
			return utgMatch{}, fmt.Errorf("no screenshot after %s", action[0])
		}
		return advisor.matchUTG(ctx, capture.last, candidates)
	}
	var target string
	var stepEdge utgEdge
	switch args[0] {
	case "utg-step":
		id, err := strconv.Atoi(strings.TrimPrefix(args[1], "#"))
		if err != nil {
			return ToolInvokeResponse{}, fmt.Errorf("invalid edge ID")
		}
		for _, e := range graph.Edges {
			if e.ID == id {
				if e.MismatchState != "" {
					return ToolInvokeResponse{}, fmt.Errorf("UTG edge #%d is disabled after a confirmed destination mismatch", id)
				}
				stepEdge = e
				break
			}
		}
		if stepEdge.From == "" {
			return ToolInvokeResponse{}, fmt.Errorf("unknown UTG edge: %d", id)
		}
	case "utg-goto":
		for _, n := range graph.Nodes {
			if n.Key == args[1] {
				target = n.Key
				break
			}
		}
		if target == "" {
			for _, n := range graph.Nodes {
				if !strings.EqualFold(n.Name, args[1]) {
					continue
				}
				if target != "" {
					return ToolInvokeResponse{}, fmt.Errorf("ambiguous target; use a state key")
				}
				target = n.Key
			}
		}
		if target == "" {
			return ToolInvokeResponse{}, fmt.Errorf("unknown state: %s", args[1])
		}
	}
	candidates := graph.discoveryCandidates(target)
	if stepEdge.From != "" {
		candidates = graph.matchCandidates(stepEdge.From)
	}
	match, err := run(candidates, "look")
	if err != nil {
		return ToolInvokeResponse{}, err
	}
	current := match.key
	if args[0] == "utg-status" {
		var out strings.Builder
		label := current
		if label == "" {
			label = "unknown (no confident match)"
		}
		fmt.Fprintf(&out, "Current state: %s\nReachable states:", label)
		for _, n := range graph.Nodes {
			if _, ok := graph.path(current, n.Key); ok {
				fmt.Fprintf(&out, "\n%s: %s", n.Key, n.Name)
			}
		}
		return ToolInvokeResponse{Output: out.String()}, nil
	}
	var plan []utgEdge
	if stepEdge.From != "" {
		if current != stepEdge.From {
			return ToolInvokeResponse{}, fmt.Errorf("current screen does not match edge #%d source", stepEdge.ID)
		}
		plan = []utgEdge{stepEdge}
	} else {
		var ok bool
		plan, ok = graph.path(current, target)
		if !ok {
			// Never substitute a visually similar but unverified source page.
			// Reset only when the graph already has a home-to-target route.
			var homeKeys []string
			for _, n := range graph.Nodes {
				if n.Home {
					if _, reachable := graph.path(n.Key, target); reachable {
						homeKeys = append(homeKeys, n.Key)
					}
				}
			}
			if homes := graph.matchCandidates(homeKeys...); len(homes) > 0 {
				match, err = run(homes, "home")
				if err != nil {
					return ToolInvokeResponse{}, err
				}
				current = match.key
				plan, ok = graph.path(current, target)
			}
		}
		if !ok {
			return ToolInvokeResponse{}, fmt.Errorf("target not reachable; navigate visually")
		}
	}
	if len(plan) > 12 {
		return ToolInvokeResponse{}, fmt.Errorf("replay exceeds 12 steps; navigate visually")
	}
	for _, e := range plan {
		destination := graph.matchCandidates(e.To)
		if len(destination) == 0 {
			return ToolInvokeResponse{}, fmt.Errorf("edge #%d has no destination reference; navigate visually", e.ID)
		}
		if current != e.From || !isUTGAction(e.Action) {
			return ToolInvokeResponse{}, fmt.Errorf("edge #%d source not confirmed", e.ID)
		}
		frame := len(turn.steps)
		match, err = run(destination, e.Action...)
		if err != nil {
			return ToolInvokeResponse{}, err
		}
		current = match.key
		if current != e.To {
			if match.rejected && len(turn.steps) == frame+1 {
				turn.mismatches = append(turn.mismatches, utgReplayMismatch{edge: e, frame: frame})
			}
			return ToolInvokeResponse{}, fmt.Errorf("TRAP: edge #%d destination not confirmed; inspect the final screenshot", e.ID)
		}
	}
	return ToolInvokeResponse{Output: "Confirmed state: " + current}, nil
}
