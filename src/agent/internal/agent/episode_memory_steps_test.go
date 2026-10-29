package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestEpisodeMemoryProcedureSeparatesSupportingEvidence(t *testing.T) {
	episode := TaskEpisode{ID: "path", DeviceScope: map[string]string{"app_name": "Editor"}, Events: []TaskEpisodeEvent{
		{EventID: "failed_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Try direct commit"},
		// A failure can be reported as an observation, without a tool-level error.
		{EventID: "failed_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "The change did not persist"},
		{EventID: "prepare_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Prepare the change"},
		{EventID: "prepare_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Ready"},
		{EventID: "commit_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Commit after preparing"},
		{EventID: "commit_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Committed"},
		{EventID: "verify_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Verify persistence"},
		{EventID: "verify_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Change persisted"},
	}}
	for _, tt := range []struct {
		name  string
		steps []string
		valid bool
	}{
		{"selected path", []string{"prepare_result", "commit_result", "verify_result"}, true},
		{"missing path", nil, false},
		{"unknown reference", []string{"prepare_result", "missing"}, false},
		// Models cite a step by its call or by its result; both name the same step.
		{"call resolves to its result", []string{"prepare_call", "commit_result", "verify_result"}, true},
		{"call and result of one step", []string{"prepare_call", "prepare_result", "commit_result", "verify_result"}, true},
		{"out of order", []string{"commit_result", "prepare_result"}, false},
		{"duplicate", []string{"prepare_result", "prepare_result"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := map[string]any{
				"lesson_key": "commit_path", "type": "procedure", "action": "create", "retention": "durable",
				"situation": "Commit an edit", "guidance": "Prepare, commit, then verify", "expected_effect": "Persisted",
				"scope":         map[string]string{"app_name": "Editor"},
				"evidence_refs": []string{"failed_result", "prepare_result", "commit_result", "verify_result"},
				"step_refs":     tt.steps,
			}
			encoded, _ := json.Marshal(raw)
			var candidate episodeMemoryCandidate
			if err := json.Unmarshal(encoded, &candidate); err != nil {
				t.Fatal(err)
			}
			candidate, ok := validateEpisodeMemoryCandidate(episode, episodeMemoryAssessment{GoalResult: episodeGoalAchieved}, candidate, map[string]bool{})
			if ok != tt.valid {
				t.Fatalf("valid=%v, want %v", ok, tt.valid)
			}
			if !ok {
				return
			}
			plane := NewFilesystemMemoryPlane(t.TempDir(), DefaultMemoryExtractionConfig(), nil)
			processor := newEpisodeMemoryProcessor(plane, nil)
			id, err := processor.createMemory(context.Background(), episode, candidate)
			if err != nil {
				t.Fatal(err)
			}
			item, found, err := plane.device.Get(context.Background(), id)
			if err != nil || !found {
				t.Fatalf("read stored memory: found=%v err=%v", found, err)
			}
			var descriptions []string
			for _, step := range item.Steps {
				descriptions = append(descriptions, step.Description)
			}
			if !reflect.DeepEqual(descriptions, []string{"Prepare the change", "Commit after preparing", "Verify persistence"}) {
				t.Fatalf("stored steps include supporting evidence or omit the path: %v", descriptions)
			}
			evidenceFound := false
			for _, ref := range item.EvidenceRefs {
				for _, eventID := range ref.EventIDs {
					if eventID == "failed_result" {
						evidenceFound = true
					}
				}
			}
			if !evidenceFound {
				t.Fatal("failed alternative lost from supporting evidence")
			}
		})
	}
}

func TestEpisodeMemoryStepRefsRequireCitedSuccessfulPairs(t *testing.T) {
	base := TaskEpisode{Events: []TaskEpisodeEvent{
		{EventID: "a", Type: runEventToolCall, ToolName: "touch_gesture"},
		{EventID: "ar", Type: "tool_result", ToolName: "touch_gesture"},
		{EventID: "b", Type: runEventToolCall, ToolName: "touch_gesture"},
		{EventID: "br", Type: "tool_result", ToolName: "touch_gesture"},
	}}
	for _, tt := range []struct {
		name   string
		change func(*TaskEpisode)
		refs   []string
	}{
		{"uncited result", func(*TaskEpisode) {}, []string{"a", "ar", "b"}},
		{"uncited call", func(*TaskEpisode) {}, []string{"a", "ar", "br"}},
		{"error result", func(e *TaskEpisode) { e.Events[3].IsError = true }, []string{"a", "ar", "b", "br"}},
		{"structured error", func(e *TaskEpisode) { e.Events[3].ToolError = &ToolError{Code: CodeCanceled} }, []string{"a", "ar", "b", "br"}},
		{"unpaired result", func(e *TaskEpisode) {
			e.Events = e.Events[:2]
			e.Events = append(e.Events, TaskEpisodeEvent{EventID: "br", Type: "tool_result", ToolName: "touch_gesture"})
		}, []string{"a", "ar", "br"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			episode := base
			episode.Events = append([]TaskEpisodeEvent(nil), base.Events...)
			tt.change(&episode)
			if validEpisodeMemoryStepRefs(episode, []string{"ar", "br"}, tt.refs) {
				t.Fatal("invalid path accepted")
			}
		})
	}
}

func TestEpisodeMemoryStepRefsPairInterleavedTools(t *testing.T) {
	episode := TaskEpisode{Events: []TaskEpisodeEvent{
		{EventID: "touch_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Tap Save"},
		{EventID: "text_call", Type: runEventToolCall, ToolName: "enter_text", Content: "Enter title"},
		{EventID: "text_result", Type: "tool_result", ToolName: "enter_text", Observation: "Title entered"},
		{EventID: "touch_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Saved"},
	}}
	stepRefs := []string{"text_result", "touch_result"}
	evidenceRefs := expandEpisodeMemoryEvidenceRefs(episode, stepRefs)
	if !validEpisodeMemoryStepRefs(episode, stepRefs, evidenceRefs) {
		t.Fatal("valid interleaved calls were rejected")
	}
	steps := episodeMemoryProcedureSteps(episode, stepRefs, nil)
	if len(steps) != 2 || steps[0].Description != "Enter title" || steps[0].OutcomeNote != "Title entered" ||
		steps[1].Description != "Tap Save" || steps[1].OutcomeNote != "Saved" {
		t.Fatalf("steps do not match selected results: %#v", steps)
	}
}

func TestCompactEpisodeMemoryCandidatesRejectsConflictingPaths(t *testing.T) {
	base := episodeMemoryCandidate{LessonKey: "path", Type: episodeMemoryTypeProcedure, Action: episodeMemoryActionCreate, StepRefs: []string{"ar", "br"}}
	other := base
	other.StepRefs = []string{"ar", "cr"}
	if got := compactEpisodeMemoryCandidates([]episodeMemoryCandidate{base, other}); len(got) != 0 {
		t.Fatalf("conflicting paths compacted: %#v", got)
	}
	proposal := episodeMemoryProposal{Candidates: []episodeMemoryCandidate{base}}
	cloned := cloneEpisodeMemoryProposal(proposal)
	cloned.Candidates[0].StepRefs[0] = "changed"
	if proposal.Candidates[0].StepRefs[0] != "ar" {
		t.Fatal("clone aliases step refs")
	}
}
