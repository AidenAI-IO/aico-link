package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aico-agent/internal/decision"

	"github.com/tmc/langchaingo/llms"
)

// DecisionOpInterruptedReport is asked when the user speaks over a spoken work
// result: do their words show they still want the rest?
const DecisionOpInterruptedReport = "interrupted_report"

// InterruptedReportVerdict is what the user's words over a spoken result say
// about hearing the rest of it.
type InterruptedReportVerdict string

const (
	InterruptedReportWanted   InterruptedReportVerdict = "wanted"
	InterruptedReportUnwanted InterruptedReportVerdict = "unwanted"
	// InterruptedReportUnsure covers unclear words and no answer at all.
	InterruptedReportUnsure InterruptedReportVerdict = "unsure"
)

// interruptedReportTimeout bounds the main model's answer; the realtime reply
// to the user is spoken meanwhile, so this only delays the follow-up.
const interruptedReportTimeout = 8 * time.Second

// DecideInterruptedReport judges whether words, which the user said while the
// assistant was speaking report, show that they want to hear the rest of it.
// Like other decision points it is answered by the main model, and also by
// the decision model when one is configured, whose confident answer wins.
func (r *Runtime) DecideInterruptedReport(ctx context.Context, words, report string) InterruptedReportVerdict {
	words = strings.TrimSpace(words)
	if r == nil || words == "" {
		return InterruptedReportUnsure
	}
	var decider *decisionAdvisor
	if tools := r.toolSnapshot(); tools != nil {
		decider = tools.decider
	}
	verdict, _, err := decide(ctx, decider, decisionPoint[InterruptedReportVerdict]{
		op: DecisionOpInterruptedReport,
		llm: func(ctx context.Context) (InterruptedReportVerdict, error) {
			return r.interruptedReportLLM(ctx, words, report)
		},
		ask: func(ctx context.Context, client *decision.Client, minConfidence float64) (decisionOutcome[InterruptedReportVerdict], error) {
			resp, err := client.Ask(ctx, decision.Request{
				State: map[string]any{"report": report, "utterance": words},
				Questions: map[string]decision.Question{
					"unwanted": {Type: decision.TypeNoul, Instructions: "A voice assistant was speaking `report` to the user when the user interrupted it by saying `utterance`. " +
						"Does `utterance` show that the user does not want to hear the rest of `report`? " +
						"Yes when they ask it to stop or skip it, say they have heard enough or already know, or dismiss it. " +
						"No when they ask something else, ask a follow-up, or ask it to go on or repeat."},
				},
			})
			if err != nil {
				return decisionOutcome[InterruptedReportVerdict]{}, err
			}
			unwanted, sure, p, err := noulAnswer(resp, "unwanted", minConfidence)
			if err != nil {
				return decisionOutcome[InterruptedReportVerdict]{}, err
			}
			verdict := InterruptedReportWanted
			if unwanted {
				verdict = InterruptedReportUnwanted
			}
			return decisionOutcome[InterruptedReportVerdict]{Value: verdict, Confident: sure, Confidence: p}, nil
		},
		summary: func(v InterruptedReportVerdict) string { return "verdict=" + string(v) },
	})
	if err != nil {
		return InterruptedReportUnsure
	}
	return verdict
}

// interruptedReportLLM asks the main model the same question.
func (r *Runtime) interruptedReportLLM(ctx context.Context, words, report string) (InterruptedReportVerdict, error) {
	if r.models == nil {
		return InterruptedReportUnsure, fmt.Errorf("no main model")
	}
	ctx, cancel := context.WithTimeout(ctx, interruptedReportTimeout)
	defer cancel()
	prompt := "A voice assistant was speaking the report below to the user when the user interrupted it with the words below.\n" +
		"Report: " + report + "\nUser's words: " + words + "\n\n" +
		"Do the user's words show whether they still want to hear the rest of the report? Answer \"no\" when they ask it to stop or skip it, say they have heard enough or already know, or dismiss it; " +
		"\"yes\" when they ask something else, ask a follow-up, or ask it to go on or repeat; \"unclear\" when the words could mean either.\n" +
		`Return JSON only: {"wants_rest":"yes|no|unclear"}`
	resp, err := r.models.GenerateContent(ctx, []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, "You classify a user's spoken reply. Output JSON only."),
		llms.TextParts(llms.ChatMessageTypeHuman, prompt),
	}, llms.WithJSONMode(), llms.WithMaxTokens(textInputModelMaxTokens))
	if err != nil {
		return InterruptedReportUnsure, err
	}
	if len(resp.Choices) == 0 {
		return InterruptedReportUnsure, fmt.Errorf("empty response")
	}
	var parsed struct {
		WantsRest string `json:"wants_rest"`
	}
	if err := json.Unmarshal([]byte(stripJSONCodeFence(resp.Choices[0].Content)), &parsed); err != nil {
		return InterruptedReportUnsure, fmt.Errorf("parse interrupted report verdict: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(parsed.WantsRest)) {
	case "yes":
		return InterruptedReportWanted, nil
	case "no":
		return InterruptedReportUnwanted, nil
	default:
		return InterruptedReportUnsure, nil
	}
}
