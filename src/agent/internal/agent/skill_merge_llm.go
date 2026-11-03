package agent

import (
	"aico-agent/internal/agent/model"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tmc/langchaingo/llms"
)

type LLMSkillMergeModel struct {
	model model.Model
}

func NewLLMSkillMergeModel(models model.Model) *LLMSkillMergeModel {
	return &LLMSkillMergeModel{model: models}
}

func (m *LLMSkillMergeModel) MergeSkill(ctx context.Context, input SkillMergeInput) (*SkillMergeResult, error) {
	prompt := buildMergePrompt(input)
	raw, err := llms.GenerateFromSinglePrompt(ctx, m.model, prompt, llms.WithMaxTokens(4096))
	if err != nil {
		return nil, fmt.Errorf("LLM call: %w", err)
	}

	return parseMergeResponse(raw)
}

func buildMergePrompt(input SkillMergeInput) string {
	var b strings.Builder
	b.WriteString(mergeSystemPrompt)
	b.WriteString("\n\n")

	if input.Mode == MergeThreeWay {
		b.WriteString("## Base version (common ancestor)\n```\n")
		b.WriteString(input.Base)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("## Upstream version (new bundled release)\n```\n")
	b.WriteString(input.Upstream)
	b.WriteString("\n```\n\n")

	b.WriteString("## Local version (user's current copy)\n```\n")
	b.WriteString(input.Local)
	b.WriteString("\n```\n")

	return b.String()
}

const mergeSystemPrompt = `You are a SKILL.md merge assistant. Your job is to merge two (or three) versions of a skill definition file into one coherent result.

Rules:
- Preserve the YAML frontmatter structure: name, description, metadata (preferred_model, allowed_tools, allowed_children).
- The "name" field MUST remain unchanged from the upstream version.
- Combine instructions from both versions. Keep user customizations. Incorporate upstream improvements.
- Remove duplicates. Resolve contradictions by preferring the more specific or recent instruction.
- Keep the result concise — no longer than the longest input version.
- Output valid JSON with exactly these fields: status, merged_skill_md, summary.
- status must be "merged" if you produced a valid merge, or "failed" if the versions are irreconcilable.
- merged_skill_md is the full content of the merged SKILL.md file (frontmatter + instructions).
- summary is a one-sentence explanation of what you did.

Output ONLY the JSON object. No markdown fences, no extra text.`

type mergeResponse struct {
	Status        string `json:"status"`
	MergedSkillMD string `json:"merged_skill_md"`
	Summary       string `json:"summary"`
}

func parseMergeResponse(raw string) (*SkillMergeResult, error) {
	cleaned := stripJSONFences(raw)
	var resp mergeResponse
	if err := json.Unmarshal([]byte(cleaned), &resp); err != nil {
		return nil, fmt.Errorf("parse LLM response: %w (raw: %s)", err, truncate(raw, 200))
	}
	return &SkillMergeResult{
		Status:        resp.Status,
		MergedSkillMD: resp.MergedSkillMD,
		Summary:       resp.Summary,
	}, nil
}

func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
	}
	return strings.TrimSpace(s)
}
