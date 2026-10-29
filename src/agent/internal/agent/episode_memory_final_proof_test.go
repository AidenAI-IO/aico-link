package agent

import (
	"reflect"
	"strings"
	"testing"
)

func finalProofTestEpisode() TaskEpisode {
	return TaskEpisode{ID: "ep_final_proof", DeviceScope: map[string]string{"app_name": "Editor"}, Events: []TaskEpisodeEvent{
		{EventID: "prepare_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Prepare the change"},
		{EventID: "prepare_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Ready"},
		{EventID: "commit_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Commit the change"},
		{EventID: "commit_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Committed"},
		{EventID: "verify_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Reopen to verify"},
		{EventID: "verify_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Change persisted after reopening"},
	}}
}

func finalProofTestCandidate(stepRefs []string) episodeMemoryCandidate {
	return episodeMemoryCandidate{
		LessonKey: "commit_path", Type: episodeMemoryTypeProcedure, Action: episodeMemoryActionCreate,
		Retention: episodeMemoryRetentionDurable, Situation: "Commit an edit", Guidance: "Prepare, commit, then reopen",
		ExpectedEffect: "The change persists", Scope: map[string]string{"app_name": "Editor"},
		EvidenceRefs: []string{"prepare_result", "commit_result", "verify_result"}, StepRefs: stepRefs,
	}
}

func TestAchievedProcedureMustReachTheAssessmentsFinalProof(t *testing.T) {
	assessment := episodeMemoryAssessment{GoalResult: episodeGoalAchieved, Reason: "persisted", EvidenceRefs: []string{"commit_result", "verify_result"}}
	if _, ok := validateEpisodeMemoryCandidate(finalProofTestEpisode(), assessment, finalProofTestCandidate([]string{"prepare_result", "commit_result"}), map[string]bool{}); ok {
		t.Fatal("procedure that stops before the final verification was accepted")
	}
	if _, ok := validateEpisodeMemoryCandidate(finalProofTestEpisode(), assessment, finalProofTestCandidate([]string{"prepare_result", "commit_result", "verify_result"}), map[string]bool{}); !ok {
		t.Fatal("procedure that reaches the final verification was rejected")
	}
}

func TestEpisodeMemoryFinalProofIsTheLastCitedDeviceResult(t *testing.T) {
	episode := finalProofTestEpisode()
	episode.Events = append(episode.Events, TaskEpisodeEvent{EventID: "final_answer", Type: "default_finish", Content: "Done"})
	for _, tt := range []struct {
		name string
		refs []string
		want string
	}{
		{"last cited result", []string{"verify_result", "prepare_result"}, "verify_result"},
		{"non-tool evidence ignored", []string{"commit_result", "final_answer"}, "commit_result"},
		{"no device result", []string{"final_answer"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := episodeMemoryFinalProofRef(episode, tt.refs); got != tt.want {
				t.Fatalf("final proof = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRetainedEpisodeMemoryCandidateAcceptsSubsetOfRewriteEvidence(t *testing.T) {
	retainReview := func(evidence []string) episodeMemoryRetentionAudit {
		return episodeMemoryRetentionAudit{Reviews: []episodeMemoryRetentionReview{{
			LessonKey: "commit_path", Decision: episodeMemoryRetentionDecisionRetain, Retention: episodeMemoryRetentionDurable,
			Reason: "verified workaround", Rewrite: &episodeMemoryRetentionRewrite{
				Situation: "Commit an edit", Guidance: "Prepare, commit, then reopen", ExpectedEffect: "The change persists",
				Scope: map[string]string{"app_name": "Editor"}, EvidenceRefs: evidence,
			},
		}}}
	}
	original := []episodeMemoryCandidate{finalProofTestCandidate([]string{"prepare_result", "commit_result", "verify_result"})}

	retained := retainedEpisodeMemoryCandidates(original, retainReview([]string{"verify_result"}))
	if len(retained) != 1 || !reflect.DeepEqual(retained[0].EvidenceRefs, original[0].EvidenceRefs) {
		t.Fatalf("retained = %#v, want the candidate with its full evidence", retained)
	}
	for _, evidence := range [][]string{nil, {"verify_result", "unrelated_result"}} {
		if retained := retainedEpisodeMemoryCandidates(original, retainReview(evidence)); len(retained) != 0 {
			t.Fatalf("rewrite evidence %v retained %#v, want rejection", evidence, retained)
		}
	}
}

func TestRetainedEpisodeMemoryCandidateUsesRewrittenStepRefs(t *testing.T) {
	audit := func(stepRefs []string) episodeMemoryRetentionAudit {
		return episodeMemoryRetentionAudit{Reviews: []episodeMemoryRetentionReview{{
			LessonKey: "commit_path", Decision: episodeMemoryRetentionDecisionRetain, Retention: episodeMemoryRetentionDurable,
			Reason: "verified workaround", Rewrite: &episodeMemoryRetentionRewrite{
				Situation: "Commit an edit", Guidance: "Prepare, commit, then reopen", ExpectedEffect: "The change persists",
				Scope: map[string]string{"app_name": "Editor"}, EvidenceRefs: []string{"prepare_result", "commit_result", "verify_result"},
				StepRefs: stepRefs,
			},
		}}}
	}
	original := []episodeMemoryCandidate{finalProofTestCandidate([]string{"prepare_result", "commit_result"})}

	extended := []string{"prepare_result", "commit_result", "verify_result"}
	if retained := retainedEpisodeMemoryCandidates(original, audit(extended)); len(retained) != 1 || !reflect.DeepEqual(retained[0].StepRefs, extended) {
		t.Fatalf("retained = %#v, want step_refs %v", retained, extended)
	}
	if retained := retainedEpisodeMemoryCandidates(original, audit(nil)); len(retained) != 1 || !reflect.DeepEqual(retained[0].StepRefs, original[0].StepRefs) {
		t.Fatalf("retained = %#v, want original step_refs kept when the rewrite omits them", retained)
	}
}

func TestRetentionAuditPromptNamesTheFinalProof(t *testing.T) {
	prompt := buildEpisodeMemoryRetentionAuditPrompt(`{"episode_id":"ep_final_proof"}`, "verify_result", []episodeMemoryCandidate{finalProofTestCandidate(nil)})
	for _, want := range []string{`"step_refs"`, "verify_result"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("retention audit prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(buildEpisodeMemoryRetentionAuditPrompt(`{"episode_id":"ep_unknown"}`, "", nil), "final verification result is") {
		t.Fatal("prompt names a final verification result when the assessment cites none")
	}
}
