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

func TestRetainedEpisodeMemoryCandidateKeepsEvidenceFromBothCandidateAndRewrite(t *testing.T) {
	retainReview := func(evidence []string) episodeMemoryRetentionAudit {
		return episodeMemoryRetentionAudit{Reviews: []episodeMemoryRetentionReview{{
			LessonKey: "commit_path", Decision: episodeMemoryRetentionDecisionRetain, Retention: episodeMemoryRetentionDurable,
			Reason: "verified workaround", Rewrite: &episodeMemoryRetentionRewrite{
				Situation: "Commit an edit", Guidance: "Prepare, commit, then reopen", ExpectedEffect: "The change persists",
				Scope: map[string]string{"app_name": "Editor"}, EvidenceRefs: evidence,
			},
		}}}
	}
	candidate := finalProofTestCandidate([]string{"commit_result", "verify_result"})
	candidate.EvidenceRefs = []string{"commit_result", "verify_result"}
	original := []episodeMemoryCandidate{candidate}

	// Reviewers collapse repeated evidence; the candidate's evidence is kept.
	retained := retainedEpisodeMemoryCandidates(original, retainReview([]string{"verify_result"}))
	if len(retained) != 1 || !reflect.DeepEqual(retained[0].EvidenceRefs, []string{"commit_result", "verify_result"}) {
		t.Fatalf("retained = %#v, want the candidate's full evidence", retained)
	}
	// A reviewer completing the path cites the step the candidate skipped.
	retained = retainedEpisodeMemoryCandidates(original, retainReview([]string{"prepare_result", "commit_result", "verify_result"}))
	if len(retained) != 1 || !reflect.DeepEqual(retained[0].EvidenceRefs, []string{"commit_result", "verify_result", "prepare_result"}) {
		t.Fatalf("retained = %#v, want evidence extended by the rewrite", retained)
	}
	if retained := retainedEpisodeMemoryCandidates(original, retainReview(nil)); len(retained) != 0 {
		t.Fatalf("rewrite without evidence retained %#v, want rejection", retained)
	}
}

func TestAchievedProcedureMayEndWithARepeatOfTheFinalVerification(t *testing.T) {
	episode := finalProofTestEpisode()
	episode.Events = append(episode.Events,
		TaskEpisodeEvent{EventID: "commit_again_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Commit the change"},
		TaskEpisodeEvent{EventID: "commit_again_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Committed"},
		TaskEpisodeEvent{EventID: "verify_again_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Reopen to verify"},
		TaskEpisodeEvent{EventID: "verify_again_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Change persisted again"},
	)
	assessment := episodeMemoryAssessment{GoalResult: episodeGoalAchieved, Reason: "persisted twice", EvidenceRefs: []string{"verify_result", "verify_again_result"}}
	firstRun := finalProofTestCandidate([]string{"prepare_result", "commit_result", "verify_result"})
	if _, ok := validateEpisodeMemoryCandidate(episode, assessment, firstRun, map[string]bool{}); !ok {
		t.Fatal("complete first run ending with the same verification was rejected")
	}
	stopsEarly := finalProofTestCandidate([]string{"prepare_result", "commit_result"})
	if _, ok := validateEpisodeMemoryCandidate(episode, assessment, stopsEarly, map[string]bool{}); ok {
		t.Fatal("path ending before any verification was accepted")
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

func TestCanonicalEpisodeMemoryStepRefsNameEachStepByItsResult(t *testing.T) {
	episode := finalProofTestEpisode()
	episode.Events = append(episode.Events, TaskEpisodeEvent{EventID: "unanswered_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Tap again"})
	for _, tt := range []struct {
		name string
		refs []string
		want []string
	}{
		{"results unchanged", []string{"prepare_result", "verify_result"}, []string{"prepare_result", "verify_result"}},
		{"calls resolved", []string{"prepare_call", "commit_call"}, []string{"prepare_result", "commit_result"}},
		{"call and result collapsed", []string{"prepare_call", "prepare_result", "commit_call", "commit_result"}, []string{"prepare_result", "commit_result"}},
		{"unpaired call kept for validation to reject", []string{"commit_result", "unanswered_call"}, []string{"commit_result", "unanswered_call"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalEpisodeMemoryStepRefs(episode, tt.refs); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("canonical step_refs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetainedEpisodeMemoryCandidateIgnoresRewrittenStepRefsForNonProcedures(t *testing.T) {
	original := []episodeMemoryCandidate{{
		LessonKey: "verify_after_save", Type: episodeMemoryTypeFailure, Action: episodeMemoryActionCreate,
		Retention: episodeMemoryRetentionDurable, Situation: "Saving", Guidance: "Reopen to verify", ExpectedEffect: "Persistence is confirmed",
		Scope: map[string]string{"app_name": "Editor"}, EvidenceRefs: []string{"commit_result", "verify_result"},
	}}
	audit := episodeMemoryRetentionAudit{Reviews: []episodeMemoryRetentionReview{{
		LessonKey: "verify_after_save", Decision: episodeMemoryRetentionDecisionRetain, Retention: episodeMemoryRetentionDurable,
		Reason: "app-specific guard", Rewrite: &episodeMemoryRetentionRewrite{
			Situation: "Saving", Guidance: "Reopen to verify", ExpectedEffect: "Persistence is confirmed",
			Scope: map[string]string{"app_name": "Editor"}, EvidenceRefs: []string{"commit_result", "verify_result"},
			StepRefs: []string{"commit_call", "commit_result"},
		},
	}}}
	retained := retainedEpisodeMemoryCandidates(original, audit)
	if len(retained) != 1 || len(retained[0].StepRefs) != 0 {
		t.Fatalf("retained = %#v, want the failure candidate without step_refs", retained)
	}
}

func TestOnlyAchievedProceduresRequireTheFinalProof(t *testing.T) {
	episode := finalProofTestEpisode()
	refs := []string{"commit_result", "verify_result"}
	if got := requiredEpisodeMemoryFinalProof(episode, episodeMemoryAssessment{GoalResult: episodeGoalAchieved, EvidenceRefs: refs}); got != "verify_result" {
		t.Fatalf("achieved final proof = %q, want verify_result", got)
	}
	// A failed Episode yields guards, not a path to the goal; naming its last
	// result as required verification would push the audit to discard them.
	for _, result := range []episodeGoalResult{episodeGoalNotAchieved, episodeGoalUnknown} {
		if got := requiredEpisodeMemoryFinalProof(episode, episodeMemoryAssessment{GoalResult: result, EvidenceRefs: refs}); got != "" {
			t.Fatalf("%s final proof = %q, want none", result, got)
		}
	}
}
