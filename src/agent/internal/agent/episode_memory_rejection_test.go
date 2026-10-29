package agent

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
)

func episodeMemoryRejectionTestCandidate() episodeMemoryCandidate {
	return episodeMemoryCandidate{
		LessonKey: "qa_notes_save", Type: episodeMemoryTypeProcedure, Action: episodeMemoryActionCreate,
		Retention: episodeMemoryRetentionDurable, Situation: "Saving a title", Guidance: "Preview, Edit, then Save",
		ExpectedEffect: "The title persists", Scope: map[string]string{"app_name": "QA Notes"},
		EvidenceRefs: []string{"save_result"}, StepRefs: []string{"save_result"},
	}
}

func episodeMemoryRejectionTestEpisode(id string) TaskEpisode {
	return TaskEpisode{ID: id, DeviceScope: map[string]string{"app_name": "QA Notes"}, Events: []TaskEpisodeEvent{
		{EventID: "save_call", Type: runEventToolCall, ToolName: "touch_gesture", Content: "Run the whole save flow."},
		{EventID: "save_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Preview, Edit, Save, reopen: title persisted."},
	}}
}

func episodeMemoryRejectionTestProposal() episodeMemoryProposal {
	return episodeMemoryProposal{
		EpisodeAssessment: episodeMemoryAssessment{GoalResult: episodeGoalAchieved, Reason: "The title persisted after reopening.", EvidenceRefs: []string{"save_result"}},
		Candidates:        []episodeMemoryCandidate{episodeMemoryRejectionTestCandidate()},
	}
}

func TestRetainedEpisodeMemoryCandidatesExplainEachRejection(t *testing.T) {
	retainedReview := func() episodeMemoryRetentionReview {
		return episodeMemoryRetentionReview{
			LessonKey: "qa_notes_save", Decision: episodeMemoryRetentionDecisionRetain, Retention: episodeMemoryRetentionDurable,
			Reason: "verified workaround", Rewrite: &episodeMemoryRetentionRewrite{
				Situation: "Saving a title", Guidance: "Preview, Edit, then Save", ExpectedEffect: "The title persists",
				Scope: map[string]string{"app_name": "QA Notes"}, EvidenceRefs: []string{"save_result"},
			},
		}
	}
	for _, tt := range []struct {
		name    string
		reviews func() []episodeMemoryRetentionReview
		want    []string
	}{
		{"missing review", func() []episodeMemoryRetentionReview { return nil }, []string{"no review"}},
		{"duplicate review", func() []episodeMemoryRetentionReview {
			return []episodeMemoryRetentionReview{retainedReview(), retainedReview()}
		}, []string{"2 reviews"}},
		{"discarded", func() []episodeMemoryRetentionReview {
			review := retainedReview()
			review.Decision, review.Reason = episodeMemoryRetentionDecisionDiscard, "restates generic save advice"
			return []episodeMemoryRetentionReview{review}
		}, []string{"decision=discard", "retention=durable", "restates generic save advice"}},
		{"transient", func() []episodeMemoryRetentionReview {
			review := retainedReview()
			review.Retention = episodeMemoryRetentionTransient
			return []episodeMemoryRetentionReview{review}
		}, []string{"decision=retain", "retention=transient"}},
		{"empty reason", func() []episodeMemoryRetentionReview {
			review := retainedReview()
			review.Reason = " "
			return []episodeMemoryRetentionReview{review}
		}, []string{"no reason"}},
		{"missing rewrite", func() []episodeMemoryRetentionReview {
			review := retainedReview()
			review.Rewrite = nil
			return []episodeMemoryRetentionReview{review}
		}, []string{"no rewrite"}},
		{"changed evidence", func() []episodeMemoryRetentionReview {
			review := retainedReview()
			review.Rewrite.EvidenceRefs = []string{"other_result"}
			return []episodeMemoryRetentionReview{review}
		}, []string{"evidence_refs"}},
		{"sensitive value left", func() []episodeMemoryRetentionReview {
			review := retainedReview()
			review.SensitiveValues = []string{"Preview"}
			return []episodeMemoryRetentionReview{review}
		}, []string{"sensitive value"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			audit := episodeMemoryRetentionAudit{Reviews: tt.reviews()}
			retained, rejections := retainedEpisodeMemoryCandidates([]episodeMemoryCandidate{episodeMemoryRejectionTestCandidate()}, audit)
			if len(retained) != 0 || len(rejections) != 1 || rejections[0].LessonKey != "qa_notes_save" {
				t.Fatalf("retained=%#v rejections=%#v, want one qa_notes_save rejection", retained, rejections)
			}
			for _, want := range tt.want {
				if !strings.Contains(rejections[0].Reason, want) {
					t.Fatalf("rejection reason = %q, want %q", rejections[0].Reason, want)
				}
			}
		})
	}
}

func TestEpisodeMemoryRetentionAuditLogsDiscardedCandidate(t *testing.T) {
	var output bytes.Buffer
	logger := &Logger{logger: log.New(&output, "", 0)}
	model := &episodeMemoryScriptedModel{auditResponses: []string{`{"reviews":[{"lesson_key":"qa_notes_save","decision":"discard","retention":"durable","reason":"the Preview round trip is not shown to be necessary","sensitive_values":[]}]}`}}
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), logger)
	processor := newEpisodeMemoryProcessor(plane, model)
	reviewed, err := processor.postProcessEpisodeMemoryProposal(context.Background(), episodeMemoryRejectionTestEpisode("ep_audit_discard"), episodeMemoryRejectionTestProposal(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewed.Candidates) != 0 {
		t.Fatalf("candidates = %#v, want discarded", reviewed.Candidates)
	}
	for _, want := range []string{
		"[episode-memory] candidate rejected: episode_id=ep_audit_discard stage=retention_audit lesson_key=qa_notes_save",
		"the Preview round trip is not shown to be necessary",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("log = %q, want %q", output.String(), want)
		}
	}
}

func TestEpisodeMemoryValidationLogsRejectedCandidate(t *testing.T) {
	var output bytes.Buffer
	logger := &Logger{logger: log.New(&output, "", 0)}
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), logger)
	processor := newEpisodeMemoryProcessor(plane, nil)
	if err := processor.applyProposal(context.Background(), episodeMemoryRejectionTestEpisode("ep_single_step"), episodeMemoryRejectionTestProposal()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[episode-memory] candidate rejected: episode_id=ep_single_step stage=validation lesson_key=qa_notes_save",
		"step_refs",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("log = %q, want %q", output.String(), want)
		}
	}
}
