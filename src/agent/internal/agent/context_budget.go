package agent

import (
	"context"
	"errors"
	"fmt"
	"math"

	"aico-agent/internal/agent/compactor"
	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/tokencounter"

	"github.com/tmc/langchaingo/llms"
)

type contextPruneDecision struct {
	reason             string
	target             int
	hardBudgetExceeded bool
}

type runContextBudget struct {
	runtime            *Runtime
	compactor          *compactor.Compactor
	recorder           *EpisodeRecorder
	usableInputBudget  int
	contextWindow      int
	pruneTrigger       int
	pruneTarget        int
	pruneEnabled       bool
	stateLimit         int
	compactionTrigger  int
	compactionEnabled  bool
	providerCompaction bool
}

func newRunContextBudget(r *Runtime, cfg Config, contextCompactor *compactor.Compactor, recorder *EpisodeRecorder, usableInputBudget, contextWindow int) *runContextBudget {
	pruneTrigger, pruneTarget, pruneEnabled := historicalPruneBudgets(usableInputBudget, cfg.ContextPruneThresholdOrDefault())
	compactionTrigger, compactionEnabled := conversationCompactionTrigger(usableInputBudget, cfg.ContextCompactionThresholdOrDefault())
	return &runContextBudget{
		runtime:            r,
		compactor:          contextCompactor,
		recorder:           recorder,
		usableInputBudget:  usableInputBudget,
		contextWindow:      contextWindow,
		pruneTrigger:       pruneTrigger,
		pruneTarget:        pruneTarget,
		pruneEnabled:       pruneEnabled,
		stateLimit:         cfg.ContextPruneStateLimitOrDefault(),
		compactionTrigger:  compactionTrigger,
		compactionEnabled:  compactionEnabled,
		providerCompaction: cfg.Model.ResponsesProviderCompactionEnabled(),
	}
}

func (b *runContextBudget) historicalPruneDecision(messageTokens, stateCount int) contextPruneDecision {
	if b.pruneEnabled && messageTokens > b.pruneTrigger {
		return contextPruneDecision{reason: "threshold", target: b.pruneTarget}
	}
	// A state-count prune drops stale screenshots without compacting tool results.
	if stateCount > b.stateLimit {
		return contextPruneDecision{reason: "state_count", target: math.MaxInt}
	}
	return contextPruneDecision{}
}

func (b *runContextBudget) activePruneDecision(messageTokens, stateCount, promptTokens, toolSchemaTokens int) contextPruneDecision {
	decision := b.historicalPruneDecision(messageTokens, stateCount)
	if decision.reason != "" {
		decision.reason = "active_turn_" + decision.reason
	}
	if b.usableInputBudget > 0 && promptTokens > b.usableInputBudget {
		decision.hardBudgetExceeded = true
		// The emergency pass may break recent-exchange protection, so reduce
		// only as far as the hard request budget requires.
		decision.target = max(1, b.usableInputBudget-toolSchemaTokens)
		if decision.reason == "" {
			decision.reason = "active_turn_input_budget"
		} else {
			decision.reason = "active_turn_threshold_and_input_budget"
		}
	}
	return decision
}

func (b *runContextBudget) thresholdCompactionEnabled() bool {
	return !b.providerCompaction && b.compactionEnabled
}

func (b *runContextBudget) prepare(ctx context.Context, manager *contextmanager.ContextManager, options []llms.CallOption) error {
	var resolved llms.CallOptions
	for _, option := range options {
		if option != nil {
			option(&resolved)
		}
	}
	tokenUsage := estimateActivePromptTokens(manager, options)
	messageTokens := activeMessageTokens(manager, resolved)
	decision := b.historicalPruneDecision(messageTokens, manager.ScreenshotStateCount())
	if decision.reason != "" {
		if b.runtime.logger != nil {
			if decision.reason == "threshold" {
				b.runtime.logger.Info("Historical context prune: token usage reached threshold; tokenUsage=%d trigger=%d target=%d", messageTokens, b.pruneTrigger, b.pruneTarget)
			} else {
				b.runtime.logger.Info("Historical context prune: screenshot states reached limit; states=%d limit=%d", manager.ScreenshotStateCount(), b.stateLimit)
			}
		}
		newManager, pruned, pruneErr := b.compactor.PruneHistorical(manager, decision.target)
		if b.recorder != nil {
			b.recorder.RecordEvent(historicalPruneEvent(b.compactor.LastPruneStats(), pruned, pruneErr, decision.reason))
		}
		if pruneErr != nil {
			return pruneErr
		}
		if pruned {
			if err := manager.Activate(newManager); err != nil {
				return err
			}
			tokenUsage = estimateActivePromptTokens(manager, options)
		}
	}
	if b.thresholdCompactionEnabled() && tokenUsage > b.compactionTrigger {
		if b.runtime.logger != nil {
			b.runtime.logger.Info("Compaction: token usage reached the threshold, summarizing conversation... tokenUsage: %d, trigger: %d, contextWindow: %d", tokenUsage, b.compactionTrigger, b.contextWindow)
		}
		newManager, compacted, compactErr := b.compactor.Compact(withTelemetryRole(ctx, telemetryRoleCompaction), manager, b.runtime.sessionChunkWriter())
		if b.recorder != nil {
			b.recorder.RecordEvent(contextCompactionEvent(b.compactor.LastCompactionStats(), compacted, compactErr, "threshold"))
		}
		if compactErr != nil {
			return compactErr
		}
		if compacted {
			return manager.Activate(newManager)
		}
	}
	return nil
}

func (b *runContextBudget) guard(ctx context.Context, manager *contextmanager.ContextManager, options llms.CallOptions) (*contextmanager.ContextManager, bool, error) {
	if manager == nil {
		return nil, false, nil
	}
	messageTokens := activeMessageTokens(manager, options)
	toolSchemaTokens := tokencounter.EstimateToolSchemaTokens(options)
	decision := b.activePruneDecision(messageTokens, manager.ScreenshotStateCount(), activePromptTokens(manager, options), toolSchemaTokens)
	if decision.target == 0 {
		return manager, false, nil
	}
	if b.runtime.logger != nil {
		b.runtime.logger.Info("Context prune: model request reached budget during active run; messageTokens=%d toolSchemaTokens=%d trigger=%d target=%d usableInputBudget=%d reason=%s",
			messageTokens, toolSchemaTokens, b.pruneTrigger, decision.target, b.usableInputBudget, decision.reason)
	}
	var candidate *contextmanager.ContextManager
	var pruned bool
	var pruneErr error
	if decision.hardBudgetExceeded {
		candidate, pruned, pruneErr = b.compactor.PruneForHardBudget(manager, decision.target)
	} else {
		candidate, pruned, pruneErr = b.compactor.PruneForBudget(manager, decision.target)
	}
	activeManager := manager
	if pruned && candidate != nil {
		activeManager = candidate
	}
	var budgetErr error
	if pruneErr == nil && decision.hardBudgetExceeded && activePromptTokens(activeManager, options) > b.usableInputBudget {
		budgetErr = fmt.Errorf("context remains over usable input budget after pruning: messageTokens=%d toolSchemaTokens=%d usableInputBudget=%d",
			activeMessageTokens(activeManager, options), toolSchemaTokens, b.usableInputBudget)
	}
	if b.recorder != nil {
		b.recorder.RecordEvent(historicalPruneEvent(b.compactor.LastPruneStats(), pruned, errors.Join(pruneErr, budgetErr), decision.reason))
	}
	if pruneErr != nil {
		return nil, false, pruneErr
	}
	changed := pruned
	var pendingChunk pendingSessionChunk
	if budgetErr != nil {
		// Deterministic pruning cannot shrink historical user/assistant text.
		// Try summary recovery even when provider-managed compaction disables thresholds.
		compactedManager, compacted, compactErr := b.compactor.Compact(withTelemetryRole(ctx, telemetryRoleCompaction), activeManager, &pendingChunk)
		if b.recorder != nil {
			b.recorder.RecordEvent(contextCompactionEvent(b.compactor.LastCompactionStats(), compacted, compactErr, "active_turn_input_budget"))
		}
		if compactErr != nil {
			return nil, false, fmt.Errorf("compact context for hard input budget: %w", compactErr)
		}
		if compacted {
			activeManager = compactedManager
			changed = true
		}
		if activePromptTokens(activeManager, options) > b.usableInputBudget {
			return nil, false, fmt.Errorf("context remains over usable input budget after pruning and compaction: messageTokens=%d toolSchemaTokens=%d usableInputBudget=%d",
				activeMessageTokens(activeManager, options), toolSchemaTokens, b.usableInputBudget)
		}
	}
	// Rejected candidates must not become searchable history.
	if changed {
		if switchErr := manager.Activate(activeManager); switchErr != nil {
			return nil, false, switchErr
		}
		if chunkErr := pendingChunk.persist(ctx, b.runtime.sessionChunkWriter()); chunkErr != nil && b.runtime.logger != nil {
			b.runtime.logger.Warn("Context budget recovery: failed to persist accepted summary chunk: %v", chunkErr)
		}
	}
	return manager, changed, nil
}

func (b *runContextBudget) compact(ctx context.Context, manager *contextmanager.ContextManager, reason string) (*contextmanager.ContextManager, bool, error) {
	// Tag summary calls so their prompt becomes a `summarize-context` generation.
	newManager, compacted, compactErr := b.compactor.Compact(withTelemetryRole(ctx, telemetryRoleCompaction), manager, b.runtime.sessionChunkWriter())
	if b.recorder != nil {
		b.recorder.RecordEvent(contextCompactionEvent(b.compactor.LastCompactionStats(), compacted, compactErr, reason))
	}
	if compactErr != nil {
		return nil, false, compactErr
	}
	if !compacted {
		return manager, false, nil
	}
	if switchErr := manager.Activate(newManager); switchErr != nil {
		return nil, false, switchErr
	}
	return manager, true, nil
}

func (b *runContextBudget) compactAtThreshold(ctx context.Context, manager *contextmanager.ContextManager) (*contextmanager.ContextManager, bool, error) {
	if b.runtime.logger != nil {
		b.runtime.logger.Info("Compaction: context reached the threshold inside the agent loop; compacting before the next model call")
	}
	return b.compact(ctx, manager, "agent_loop_threshold")
}

func (b *runContextBudget) recoverOverflow(ctx context.Context, manager *contextmanager.ContextManager) (*contextmanager.ContextManager, bool, error) {
	if b.runtime.logger != nil {
		b.runtime.logger.Info("Compaction: provider rejected the request because the context window was exceeded; compacting and retrying")
	}
	return b.compact(ctx, manager, "provider_context_exceeded")
}
