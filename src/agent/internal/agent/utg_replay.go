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
	run := func(action ...string) (string, error) {
		capture.last = visualScreenshotObservation{}
		res, err := r.executeDeviceCLI(ctx, s, action, capture)
		if err != nil {
			return "", err
		}
		if res.IsError {
			return "", fmt.Errorf("%s: %s", action[0], res.Error)
		}
		if len(capture.last.ImageBytes) == 0 {
			return "", fmt.Errorf("no screenshot after %s", action[0])
		}
		turn.position = graph.locate(capture.last, turn.position)
		return turn.position, nil
	}
	current, err := run("look")
	if err != nil {
		return ToolInvokeResponse{}, err
	}
	if args[0] == "utg-status" {
		var out strings.Builder
		fmt.Fprintf(&out, "Current state: %s\nReachable states:", current)
		for _, node := range graph.Nodes {
			if _, ok := graph.path(current, node.Key); ok {
				fmt.Fprintf(&out, "\n%s: %s", node.Key, node.Name)
			}
		}
		return ToolInvokeResponse{Output: out.String()}, nil
	}
	var plan []utgEdge
	if args[0] == "utg-step" {
		id, err := strconv.Atoi(strings.TrimPrefix(args[1], "#"))
		if err != nil {
			return ToolInvokeResponse{}, fmt.Errorf("invalid edge ID")
		}
		for _, e := range graph.Edges {
			if e.ID == id && current == e.From {
				plan = []utgEdge{e}
				break
			}
		}
		if len(plan) == 0 {
			return ToolInvokeResponse{}, fmt.Errorf("edge is unknown or current screen does not match its source")
		}
	} else {
		target := ""
		for _, n := range graph.Nodes {
			if n.Key == args[1] {
				target = n.Key
				break
			}
		}
		if target == "" {
			for _, n := range graph.Nodes {
				if strings.EqualFold(n.Name, args[1]) {
					if target != "" {
						return ToolInvokeResponse{}, fmt.Errorf("ambiguous target; use a state key")
					}
					target = n.Key
				}
			}
		}
		if target == "" {
			return ToolInvokeResponse{}, fmt.Errorf("unknown state: %s", args[1])
		}
		var ok bool
		plan, ok = graph.path(current, target)
		if !ok {
			// Never substitute a visually similar but unverified source page.
			// Reset only when the graph already has a home-to-target route.
			for _, n := range graph.Nodes {
				if n.Home {
					if _, ok = graph.path(n.Key, target); ok {
						break
					}
				}
			}
			if ok {
				current, err = run("home")
				if err != nil {
					return ToolInvokeResponse{}, err
				}
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
		if current != e.From || !isUTGAction(e.Action) {
			return ToolInvokeResponse{}, fmt.Errorf("edge #%d source not confirmed", e.ID)
		}
		current, err = run(e.Action...)
		if err != nil {
			return ToolInvokeResponse{}, err
		}
		if current != e.To && capture.last.Result.ScreenChanged != nil && !*capture.last.Result.ScreenChanged {
			for retry := 0; retry < 3; retry++ {
				current, err = run("back")
				if err != nil {
					return ToolInvokeResponse{}, err
				}
				if current != e.From {
					continue
				}
				current, err = run(e.Action...)
				if err != nil {
					return ToolInvokeResponse{}, err
				}
				if current == e.To || capture.last.Result.ScreenChanged == nil || *capture.last.Result.ScreenChanged {
					break
				}
			}
		}
		if current != e.To {
			return ToolInvokeResponse{}, fmt.Errorf("TRAP: edge #%d destination not confirmed; inspect the final screenshot", e.ID)
		}
	}
	return ToolInvokeResponse{Output: "Confirmed state: " + current}, nil
}
