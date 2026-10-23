package executor

import (
	"strings"

	"aiden-agent/internal/agent/constants"
	"aiden-agent/internal/agent/messages"
)

var _ OutboundMessageTransform = ScreenshotPruner{}

type ScreenshotPruningConfig struct {
	KeepN    int
	Interval int
}

func (c ScreenshotPruningConfig) WithDefaults() ScreenshotPruningConfig {
	if c.KeepN <= 0 {
		c.KeepN = constants.DefaultScreenshotKeepN
	}
	if c.Interval <= 0 {
		c.Interval = constants.DefaultScreenshotPruneInterval
	}
	return c
}

func (c ScreenshotPruningConfig) PrunedCount(total int) int {
	c = c.WithDefaults()
	if total <= c.KeepN+c.Interval {
		return 0
	}
	pruned := ((total - c.KeepN - 1) / c.Interval) * c.Interval
	if maxPruned := total - c.KeepN; pruned > maxPruned {
		return maxPruned
	}
	return pruned
}

const (
	screenshotAttachedText    = "The image is attached in the next message."
	screenshotPlaceholderText = "The image is replaced with a placeholder in the next message."
)

// ScreenshotPruner replaces all but the newest screenshot observations in the
// outbound message list with placeholders. It applies to every provider: the
// persisted history keeps every screenshot, and without pruning each request
// resends all of them (one per turn from the <state> hook), which grows the
// request, its token cost, and the agent's memory with the conversation.
type ScreenshotPruner struct {
	Config ScreenshotPruningConfig
}

func (p ScreenshotPruner) Transform(messageList []messages.Message) []messages.Message {
	if len(messageList) == 0 {
		return messageList
	}
	cfg := p.Config.WithDefaults()

	total := 0
	for _, msg := range messageList {
		for _, a := range msg.Attachments {
			if a.Source == messages.AttachmentSourceScreenshotObservation {
				total++
			}
		}
	}
	pruned := cfg.PrunedCount(total)
	if pruned <= 0 {
		return messageList
	}

	out := make([]messages.Message, len(messageList))
	seen := 0
	for i, msg := range messageList {
		cloned := msg
		if len(msg.Attachments) > 0 {
			cloned.Attachments = append([]messages.Attachment(nil), msg.Attachments...)
		}
		if len(msg.ToolCalls) > 0 {
			cloned.ToolCalls = append([]messages.ToolCall(nil), msg.ToolCalls...)
		}
		if len(msg.ToolResults) > 0 {
			cloned.ToolResults = append([]messages.ToolResult(nil), msg.ToolResults...)
		}

		kept := cloned.Attachments[:0]
		for _, a := range cloned.Attachments {
			if a.Source != messages.AttachmentSourceScreenshotObservation {
				kept = append(kept, a)
				continue
			}
			seen++
			if seen <= pruned {
				if i > 0 {
					out[i-1] = rewritePrunedScreenshotToolResult(out[i-1])
				}
				if !strings.Contains(cloned.Content, "[Image omitted]") {
					if strings.TrimSpace(cloned.Content) == "" {
						cloned.Content = "[Image omitted]"
					} else {
						cloned.Content = cloned.Content + "\n[Image omitted]"
					}
				}
				continue
			}
			kept = append(kept, a)
		}
		cloned.Attachments = kept
		out[i] = cloned
	}
	return out
}

func rewritePrunedScreenshotToolResult(msg messages.Message) messages.Message {
	if msg.Role != messages.MessageRoleToolResult || len(msg.ToolResults) == 0 {
		return msg
	}

	cloned := msg
	cloned.ToolResults = append([]messages.ToolResult(nil), msg.ToolResults...)
	for i, result := range cloned.ToolResults {
		if !strings.Contains(result.Content, screenshotAttachedText) {
			continue
		}
		result.Content = strings.ReplaceAll(result.Content, screenshotAttachedText, screenshotPlaceholderText)
		cloned.ToolResults[i] = result
	}
	return cloned
}
