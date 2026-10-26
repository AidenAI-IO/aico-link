package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aiden-agent/internal/agent/model"

	"github.com/tmc/langchaingo/llms"
)

const (
	episodeMemoryExtractorVersion  = 2
	episodeMemoryTag               = "episode-memory:v1"
	episodeMemoryDefaultConfidence = 0.7

	deviceMemoryStatusActive     deviceMemoryStatus = "active"
	deviceMemoryStatusPending    deviceMemoryStatus = "pending"
	deviceMemoryStatusDisputed   deviceMemoryStatus = "disputed"
	deviceMemoryStatusConflicted deviceMemoryStatus = "conflicted"
)

var (
	errEpisodeMemoryRevisionChanged   = errors.New("episode memory revision changed")
	errEpisodeMemoryOmissionReview    = errors.New("episode memory omission review failed")
	errEpisodeMemoryRetentionAudit    = errors.New("episode memory retention audit failed")
	errEpisodeMemoryProcessingFailed  = errors.New("episode memory processing failed")
	errEpisodeMemoryInvalidRetryLimit = errors.New("episode memory has invalid retry batch limit")
)

type episodeGoalResult string

const (
	episodeGoalAchieved    episodeGoalResult = "achieved"
	episodeGoalNotAchieved episodeGoalResult = "not_achieved"
	episodeGoalUnknown     episodeGoalResult = "unknown"
)

type episodeMemoryType string

const (
	episodeMemoryTypeProcedure   episodeMemoryType = "procedure"
	episodeMemoryTypeNavigation  episodeMemoryType = "navigation"
	episodeMemoryTypeCalibration episodeMemoryType = "calibration"
	episodeMemoryTypeFailure     episodeMemoryType = "failure"
	episodeMemoryTypeFact        episodeMemoryType = "fact"
)

type episodeMemoryAction string

const (
	episodeMemoryActionCreate episodeMemoryAction = "create"
	episodeMemoryActionUpdate episodeMemoryAction = "update"
)

type episodeMemoryRetention string

const (
	episodeMemoryRetentionDurable   episodeMemoryRetention = "durable"
	episodeMemoryRetentionTransient episodeMemoryRetention = "transient"
	episodeMemoryRetentionSensitive episodeMemoryRetention = "sensitive"
)

type episodeMemoryAssessment struct {
	GoalResult   episodeGoalResult `json:"goal_result" yaml:"goal_result"`
	Reason       string            `json:"reason" yaml:"reason"`
	EvidenceRefs []string          `json:"evidence_refs" yaml:"evidence_refs"`
}

type episodeMemoryCandidate struct {
	LessonKey          string                 `json:"lesson_key" yaml:"lesson_key"`
	Type               episodeMemoryType      `json:"type" yaml:"type"`
	Action             episodeMemoryAction    `json:"action" yaml:"action"`
	Retention          episodeMemoryRetention `json:"retention" yaml:"retention"`
	MemoryID           string                 `json:"memory_id,omitempty" yaml:"memory_id,omitempty"`
	MemoryRevision     int                    `json:"memory_revision,omitempty" yaml:"memory_revision,omitempty"`
	UnresolvedConflict bool                   `json:"unresolved_conflict" yaml:"unresolved_conflict"`
	ConflictReason     string                 `json:"conflict_reason,omitempty" yaml:"conflict_reason,omitempty"`
	Situation          string                 `json:"situation" yaml:"situation"`
	Guidance           string                 `json:"guidance" yaml:"guidance"`
	ExpectedEffect     string                 `json:"expected_effect" yaml:"expected_effect"`
	Confidence         *float64               `json:"confidence,omitempty" yaml:"confidence,omitempty"`
	Scope              map[string]string      `json:"scope" yaml:"scope"`
	Tags               []string               `json:"tags" yaml:"tags"`
	EvidenceRefs       []string               `json:"evidence_refs" yaml:"evidence_refs"`
	SensitiveValues    []string               `json:"-" yaml:"-"`
}

func (c *episodeMemoryCandidate) UnmarshalJSON(data []byte) error {
	type candidateAlias episodeMemoryCandidate
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["confidence"]; ok && strings.TrimSpace(string(raw)) == "null" {
		return fmt.Errorf("episode memory confidence must be a number")
	}
	var decoded candidateAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*c = episodeMemoryCandidate(decoded)
	return nil
}

type episodeMemoryProposal struct {
	EpisodeAssessment episodeMemoryAssessment  `json:"episode_assessment" yaml:"episode_assessment"`
	Candidates        []episodeMemoryCandidate `json:"candidates" yaml:"candidates"`
	ExistingRevisions map[string]int           `json:"-" yaml:"existing_revisions,omitempty"`
}

type episodeMemoryRetentionDecision string

const (
	episodeMemoryRetentionDecisionRetain  episodeMemoryRetentionDecision = "retain"
	episodeMemoryRetentionDecisionDiscard episodeMemoryRetentionDecision = "discard"
)

type episodeMemoryRetentionReview struct {
	LessonKey       string                         `json:"lesson_key"`
	Decision        episodeMemoryRetentionDecision `json:"decision"`
	Retention       episodeMemoryRetention         `json:"retention"`
	Reason          string                         `json:"reason"`
	SensitiveValues []string                       `json:"sensitive_values"`
	Rewrite         *episodeMemoryRetentionRewrite `json:"rewrite,omitempty"`
}

type episodeMemoryRetentionRewrite struct {
	Situation      string            `json:"situation"`
	Guidance       string            `json:"guidance"`
	ExpectedEffect string            `json:"expected_effect"`
	Scope          map[string]string `json:"scope"`
	Tags           []string          `json:"tags"`
	EvidenceRefs   []string          `json:"evidence_refs"`
}

type episodeMemoryRetentionAudit struct {
	Reviews []episodeMemoryRetentionReview `json:"reviews"`
}

type episodeMemoryRetentionAuditStats struct {
	RetainDecisions int
	Rewrites        int
	MatchingKeys    int
}

func cloneEpisodeMemoryProposal(proposal episodeMemoryProposal) episodeMemoryProposal {
	cloned := proposal
	cloned.EpisodeAssessment.EvidenceRefs = append([]string(nil), proposal.EpisodeAssessment.EvidenceRefs...)
	cloned.Candidates = make([]episodeMemoryCandidate, len(proposal.Candidates))
	for i, candidate := range proposal.Candidates {
		cloned.Candidates[i] = candidate
		cloned.Candidates[i].Scope = cloneStringMap(candidate.Scope)
		cloned.Candidates[i].Tags = append([]string(nil), candidate.Tags...)
		cloned.Candidates[i].EvidenceRefs = append([]string(nil), candidate.EvidenceRefs...)
		cloned.Candidates[i].SensitiveValues = append([]string(nil), candidate.SensitiveValues...)
		if candidate.Confidence != nil {
			confidence := *candidate.Confidence
			cloned.Candidates[i].Confidence = &confidence
		}
	}
	cloned.ExistingRevisions = make(map[string]int, len(proposal.ExistingRevisions))
	for id, revision := range proposal.ExistingRevisions {
		cloned.ExistingRevisions[id] = revision
	}
	return cloned
}

type episodeMemoryProcessor struct {
	plane *FilesystemMemoryPlane
	model model.Model
	merge *MemoryMergeEngine
	state *episodeMemoryStateStore
	now   func() time.Time
	lock  string
}

type episodeMemoryWork struct {
	episode        TaskEpisode
	originalStatus episodeMemoryEpisodeStatus
	status         episodeMemoryEpisodeStatus
	proposal       episodeMemoryProposal
	needsModel     bool
	skip           bool
}

var _ MemoryProcessor = (*episodeMemoryProcessor)(nil)

func newEpisodeMemoryProcessor(plane *FilesystemMemoryPlane, models model.Model) *episodeMemoryProcessor {
	bootstrapAt := time.Now().UTC()
	return &episodeMemoryProcessor{
		plane: plane,
		model: models,
		merge: NewMemoryMergeEngine(models),
		state: newEpisodeMemoryStateStore(filepath.Join(plane.memoryDir, "lifecycle", "reflection.yaml"), bootstrapAt),
		now:   func() time.Time { return time.Now().UTC() },
		lock:  filepath.Join(plane.memoryDir, "lifecycle", "reflection.lock"),
	}
}

func (p *episodeMemoryProcessor) Initialize() error {
	if p == nil || p.state == nil {
		return nil
	}
	_, err := p.state.Snapshot()
	return err
}

func (p *episodeMemoryProcessor) logBatchError(err error) {
	if err != nil && p != nil && p.plane != nil && p.plane.logger != nil {
		p.plane.logger.Warn("[episode-memory] batch failed: %v", err)
	}
}

// logEpisodeMemoryResponseFailure records only response size metadata. Model
// output can repeat credentials or one-time values from an Episode, so the raw
// body must not be copied into the persistent agent log.
func (p *episodeMemoryProcessor) logEpisodeMemoryResponseFailure(reason, raw string) {
	if p == nil || p.plane == nil || p.plane.logger == nil {
		return
	}
	p.plane.logger.Warn("[episode-memory] %s: response_bytes=%d response_runes=%d", reason, len(raw), len([]rune(raw)))
}

// episodeMemoryBatchTokenBudget sizes the output budget for an episode batch.
func episodeMemoryBatchTokenBudget(episodeCount, attempt int) int {
	return memoryMergeTokenBudget(episodeMemoryBatchTokensPerEpisode, episodeCount, episodeMemoryBatchMaxTokens, attempt)
}

// episodeMemoryRetentionAuditTokenBudget grows review output headroom after a
// truncation instead of replaying the same fixed-size request.
func episodeMemoryRetentionAuditTokenBudget(attempt int) int {
	return memoryMergeTokenBudget(episodeMemoryRetentionAuditBaseTokens, 1, episodeMemoryBatchMaxTokens, attempt)
}

func (p *episodeMemoryProcessor) NextRunAt(ctx context.Context) (time.Time, error) {
	state, episodes, err := p.loadWork(ctx)
	if err != nil {
		return time.Time{}, err
	}
	now := p.now()
	var next time.Time
	for _, episode := range episodes {
		eligible, due := episodeMemoryEpisodeDue(state.Episodes[episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)], now)
		if eligible {
			return now, nil
		}
		if !due.IsZero() && (next.IsZero() || due.Before(next)) {
			next = due
		}
	}
	return next, nil
}

func (p *episodeMemoryProcessor) ProcessBatch(ctx context.Context, shouldStop func() bool) (MemoryBatchResult, error) {
	limit := episodeMemoryBatchLimit
	if p == nil {
		return MemoryBatchResult{}, nil
	}
	lock := &FileLock{path: p.lock}
	if err := lock.Lock(episodeMemoryBatchLockTimeout); err != nil {
		return MemoryBatchResult{}, fmt.Errorf("acquire episode memory batch lock: %w", err)
	}
	result, err := p.processBatchLocked(ctx, limit, shouldStop)
	if unlockErr := lock.Unlock(); err == nil && unlockErr != nil {
		err = unlockErr
	}
	return result, err
}

type episodeMemoryImmediateRetryKey struct{}

func (p *episodeMemoryProcessor) processBatchLocked(ctx context.Context, limit int, shouldStop func() bool) (MemoryBatchResult, error) {
	if limit <= 0 {
		limit = episodeMemoryBatchLimit
	}
	state, episodes, loadErr := p.loadWork(ctx)
	if loadErr != nil {
		return MemoryBatchResult{}, loadErr
	}
	// The explicit benchmark drain skips the maintenance backoff for this episode
	// only. Keep the attempt budget and persisted retry schedule unchanged; the
	// worker/file locks serialize this snapshot with normal maintenance.
	if episodeID, ok := ctx.Value(episodeMemoryImmediateRetryKey{}).(string); ok {
		key := episodeMemoryStateKey(episodeID, episodeMemoryExtractorVersion)
		status := state.Episodes[key]
		if status.Status == episodeMemoryStatusRetry {
			status.RetryAt = ""
			state.Episodes[key] = status
		}
	}
	works, result, err := p.collectEpisodeMemoryWork(&state, episodes, limit, shouldStop)
	if err != nil || len(works) == 0 {
		return result, err
	}
	stopped, err := p.extractEpisodeMemoryWork(ctx, &state, works, &result)
	if err != nil || stopped {
		return result, err
	}
	for index := range works {
		stopped, err = p.applyEpisodeMemoryWork(ctx, &state, &works[index], &result)
		if err != nil || stopped {
			return result, err
		}
	}
	return result, nil
}

func (p *episodeMemoryProcessor) collectEpisodeMemoryWork(state *episodeMemoryStateFile, episodes []TaskEpisode, limit int, shouldStop func() bool) ([]episodeMemoryWork, MemoryBatchResult, error) {
	// A truncated batch can leave every episode in the batch with the same
	// failure. Persisting a one-episode retry limit on those statuses prevents
	// the next worker pass from replaying the oversized batch; normal work keeps
	// the configured batch limit.
	for _, episode := range episodes {
		status := state.Episodes[episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)]
		if status.RetryBatchLimit < 0 {
			return nil, MemoryBatchResult{}, fmt.Errorf("%w: episode_id=%s retry_batch_limit=%d", errEpisodeMemoryInvalidRetryLimit, episode.ID, status.RetryBatchLimit)
		}
		if status.Status != episodeMemoryStatusRetry || status.RetryBatchLimit == 0 {
			continue
		}
		eligible, _ := episodeMemoryEpisodeDue(status, p.now())
		if eligible && status.RetryBatchLimit < limit {
			limit = status.RetryBatchLimit
		}
	}
	works := make([]episodeMemoryWork, 0, limit)
	result := MemoryBatchResult{}
	for _, episode := range episodes {
		if shouldStop != nil && shouldStop() {
			result.HasPending = true
			break
		}
		stateKey := episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)
		status := state.Episodes[stateKey]
		eligible, due := episodeMemoryEpisodeDue(status, p.now())
		if !eligible {
			if !due.IsZero() && (result.NextRunAt.IsZero() || due.Before(result.NextRunAt)) {
				result.NextRunAt = due
			}
			continue
		}
		if status.Status == episodeMemoryStatusProcessing {
			endedAt, endErr := episodeMemoryEpisodeEndedAt(episode)
			if endErr != nil {
				return nil, result, endErr
			}
			ignored := episodeMemoryEpisodeStatus{
				Status:           episodeMemoryStatusIgnored,
				ExtractorVersion: episodeMemoryExtractorVersion,
				LastError:        "previous extraction attempt ended before a proposal was persisted",
				AttemptCount:     max(status.AttemptCount, 1),
			}
			if completeErr := p.state.CompleteEpisode(episode.ID, endedAt, ignored); completeErr != nil {
				return nil, result, completeErr
			}
			state.Episodes[stateKey] = ignored
			continue
		}
		if reason := invalidEpisodeMemoryReason(episode); reason != "" {
			endedAt, endErr := episodeMemoryEpisodeEndedAt(episode)
			if endErr != nil {
				return nil, result, endErr
			}
			ignored := episodeMemoryEpisodeStatus{Status: episodeMemoryStatusIgnored, ExtractorVersion: episodeMemoryExtractorVersion, LastError: reason}
			if err := p.state.CompleteEpisode(episode.ID, endedAt, ignored); err != nil {
				return nil, result, err
			}
			state.Episodes[stateKey] = ignored
			continue
		}
		if len(works) >= limit {
			result.HasPending = true
			break
		}
		work := episodeMemoryWork{episode: episode, originalStatus: status, status: status}
		if status.Proposal != nil {
			work.proposal = cloneEpisodeMemoryProposal(*status.Proposal)
		} else {
			// Keep this transition in memory until the work's retry-attempt group
			// is about to call the model. Persisting it here would mark later,
			// unstarted groups as processing if an earlier group fails.
			work.needsModel = true
			work.status = episodeMemoryEpisodeStatus{
				Status:              episodeMemoryStatusProcessing,
				ExtractorVersion:    episodeMemoryExtractorVersion,
				ProcessingStartedAt: p.now().Format(time.RFC3339Nano),
				AttemptCount:        status.AttemptCount,
			}
		}
		works = append(works, work)
	}
	return works, result, nil
}

func (p *episodeMemoryProcessor) extractEpisodeMemoryWork(ctx context.Context, state *episodeMemoryStateFile, works []episodeMemoryWork, result *MemoryBatchResult) (bool, error) {
	groups := make(map[int][]int)
	groupOrder := make([]int, 0, len(works))
	for index := range works {
		if !works[index].needsModel {
			continue
		}
		attempt := max(works[index].originalStatus.AttemptCount, works[index].status.AttemptCount)
		if _, ok := groups[attempt]; !ok {
			groupOrder = append(groupOrder, attempt)
		}
		groups[attempt] = append(groups[attempt], index)
	}
	for _, attempt := range groupOrder {
		indexes := groups[attempt]
		episodes := make([]TaskEpisode, 0, len(indexes))
		for _, index := range indexes {
			episodes = append(episodes, works[index].episode)
		}
		stopped, err := p.extractEpisodeMemoryWorkGroup(ctx, state, works, indexes, episodes, attempt, result)
		if err != nil || stopped {
			return stopped, err
		}
	}
	return false, nil
}

func (p *episodeMemoryProcessor) extractEpisodeMemoryWorkGroup(ctx context.Context, state *episodeMemoryStateFile, works []episodeMemoryWork, indexes []int, episodes []TaskEpisode, attempt int, result *MemoryBatchResult) (bool, error) {
	processing := make(map[string]episodeMemoryEpisodeStatus, len(indexes))
	for _, index := range indexes {
		processing[works[index].episode.ID] = works[index].status
	}
	if err := p.state.SetEpisodes(processing); err != nil {
		return false, err
	}
	for _, index := range indexes {
		state.Episodes[episodeMemoryStateKey(works[index].episode.ID, episodeMemoryExtractorVersion)] = works[index].status
	}
	proposals, proposalErrors, err := p.proposeEpisodeBatch(ctx, episodes, attempt)
	if err != nil {
		if ctx.Err() != nil {
			for _, index := range indexes {
				work := &works[index]
				if restoreErr := p.state.SetEpisode(work.episode.ID, work.originalStatus); restoreErr != nil {
					return false, restoreErr
				}
			}
			result.HasPending = true
			return true, nil
		}
		if isEpisodeMemoryProposalRetryable(err) {
			for _, index := range indexes {
				work := &works[index]
				if retryErr := p.retryEpisodeMemoryWork(state, work, err, result); retryErr != nil {
					return false, retryErr
				}
				work.needsModel = false
				work.skip = true
			}
			return false, nil
		}
		for _, index := range indexes {
			work := &works[index]
			if finishErr := p.ignoreEpisodeMemoryWork(state, work, err); finishErr != nil {
				return false, finishErr
			}
			work.needsModel = false
			work.skip = true
		}
		return false, nil
	}
	for _, index := range indexes {
		work := &works[index]
		if proposalErr := proposalErrors[work.episode.ID]; proposalErr != nil {
			if isEpisodeMemoryProposalRetryable(proposalErr) {
				if err := p.retryEpisodeMemoryWork(state, work, proposalErr, result); err != nil {
					return false, err
				}
				work.needsModel = false
				work.skip = true
				continue
			}
			if err := p.ignoreEpisodeMemoryWork(state, work, proposalErr); err != nil {
				return false, err
			}
			work.needsModel = false
			work.skip = true
			continue
		}
		proposal, ok := proposals[work.episode.ID]
		if !ok {
			return false, fmt.Errorf("episode memory batch omitted episode %q", work.episode.ID)
		}
		persisted := cloneEpisodeMemoryProposal(proposal)
		work.proposal = proposal
		work.status = episodeMemoryEpisodeStatus{Status: episodeMemoryStatusProposed, ExtractorVersion: episodeMemoryExtractorVersion, AttemptCount: work.originalStatus.AttemptCount, Proposal: &persisted}
		work.needsModel = false
		if err := p.state.SetEpisode(work.episode.ID, work.status); err != nil {
			return false, err
		}
	}
	return false, nil
}

// isEpisodeMemoryProposalRetryable reports whether a failure is worth another
// attempt.
//
// Truncation is retryable: it means the budget ran out mid-object, so the same
// call with more headroom can succeed. Output that is malformed but complete
// stays terminal, per TestEpisodeMemoryExtractionFailureIsNotRetried -- a model
// that finished and returned garbage will likely do so again.
//
// These two were indistinguishable until the merge engine started reporting a
// stop reason, which is how budget-caused failures ended up being discarded
// under the malformed-output policy.
func isEpisodeMemoryProposalRetryable(err error) bool {
	return errors.Is(err, errEpisodeMemoryOmissionReview) ||
		errors.Is(err, errEpisodeMemoryRetentionAudit) ||
		errors.Is(err, errMemoryMergeTruncated)
}

// safeEpisodeMemoryError removes provider-generated text before an error is
// logged or persisted. Preserve the sentinels that drive retry decisions, but
// never retain a wrapped provider error or model response body.
func safeEpisodeMemoryError(err error) error {
	switch {
	case errors.Is(err, errEpisodeMemoryRetentionAudit):
		if errors.Is(err, errMemoryMergeTruncated) {
			return fmt.Errorf("%w: %w", errEpisodeMemoryRetentionAudit, errMemoryMergeTruncated)
		}
		return errEpisodeMemoryRetentionAudit
	case errors.Is(err, errEpisodeMemoryOmissionReview):
		if errors.Is(err, errMemoryMergeTruncated) {
			return fmt.Errorf("%w: %w", errEpisodeMemoryOmissionReview, errMemoryMergeTruncated)
		}
		return errEpisodeMemoryOmissionReview
	case errors.Is(err, errMemoryMergeTruncated):
		return errMemoryMergeTruncated
	case errors.Is(err, errMemoryMergeEmpty):
		return errMemoryMergeEmpty
	default:
		return errEpisodeMemoryProcessingFailed
	}
}

func (p *episodeMemoryProcessor) retryEpisodeMemoryWork(state *episodeMemoryStateFile, work *episodeMemoryWork, cause error, result *MemoryBatchResult) error {
	cause = safeEpisodeMemoryError(cause)
	attemptCount := max(work.originalStatus.AttemptCount, work.status.AttemptCount) + 1
	if attemptCount >= episodeMemoryMaxAttempts {
		return p.ignoreEpisodeMemoryWork(state, work, cause)
	}
	retryAt := p.now().Add(episodeMemoryRetryDelay)
	retry := episodeMemoryEpisodeStatus{
		Status:           episodeMemoryStatusRetry,
		ExtractorVersion: episodeMemoryExtractorVersion,
		RetryAt:          retryAt.Format(time.RFC3339Nano),
		LastError:        truncateForLog(cause.Error(), 500),
		AttemptCount:     attemptCount,
	}
	if errors.Is(cause, errMemoryMergeTruncated) {
		retry.RetryBatchLimit = 1
	}
	if err := p.state.SetEpisode(work.episode.ID, retry); err != nil {
		return err
	}
	state.Episodes[episodeMemoryStateKey(work.episode.ID, episodeMemoryExtractorVersion)] = retry
	result.HasPending = true
	result.NextRunAt = earlierTime(result.NextRunAt, retryAt)
	return nil
}

func (p *episodeMemoryProcessor) applyEpisodeMemoryWork(ctx context.Context, state *episodeMemoryStateFile, work *episodeMemoryWork, result *MemoryBatchResult) (bool, error) {
	if work.needsModel || work.skip {
		return false, nil
	}
	err := p.applyProposal(ctx, work.episode, work.proposal)
	if err == nil {
		endedAt, endErr := episodeMemoryEpisodeEndedAt(work.episode)
		if endErr != nil {
			return false, endErr
		}
		assessment := work.proposal.EpisodeAssessment
		assessment.EvidenceRefs = append([]string(nil), assessment.EvidenceRefs...)
		completed := episodeMemoryEpisodeStatus{Status: episodeMemoryStatusDone, ExtractorVersion: episodeMemoryExtractorVersion, Assessment: &assessment}
		if err := p.state.CompleteEpisode(work.episode.ID, endedAt, completed); err != nil {
			return false, err
		}
		state.Episodes[episodeMemoryStateKey(work.episode.ID, episodeMemoryExtractorVersion)] = completed
		return false, nil
	}
	if ctx.Err() != nil {
		if restoreErr := p.state.SetEpisode(work.episode.ID, work.status); restoreErr != nil {
			return false, restoreErr
		}
		result.HasPending = true
		return true, nil
	}
	stateKey := episodeMemoryStateKey(work.episode.ID, episodeMemoryExtractorVersion)
	if errors.Is(err, errEpisodeMemoryRevisionChanged) {
		retryAt := p.now()
		retry := episodeMemoryEpisodeStatus{Status: episodeMemoryStatusRetry, ExtractorVersion: episodeMemoryExtractorVersion, RetryAt: retryAt.Format(time.RFC3339Nano), LastError: truncateForLog(err.Error(), 500), AttemptCount: work.status.AttemptCount}
		if setErr := p.state.SetEpisode(work.episode.ID, retry); setErr != nil {
			return false, setErr
		}
		state.Episodes[stateKey] = retry
		result.HasPending = true
		result.NextRunAt = earlierTime(result.NextRunAt, retryAt)
		return false, nil
	}
	attemptCount := work.status.AttemptCount + 1
	if attemptCount >= episodeMemoryMaxAttempts {
		return false, p.ignoreEpisodeMemoryWork(state, work, err)
	}
	retryAt := p.now().Add(episodeMemoryRetryDelay)
	persisted := cloneEpisodeMemoryProposal(work.proposal)
	retry := episodeMemoryEpisodeStatus{Status: episodeMemoryStatusRetry, ExtractorVersion: episodeMemoryExtractorVersion, RetryAt: retryAt.Format(time.RFC3339Nano), LastError: truncateForLog(err.Error(), 500), AttemptCount: attemptCount, Proposal: &persisted}
	if setErr := p.state.SetEpisode(work.episode.ID, retry); setErr != nil {
		return false, setErr
	}
	state.Episodes[stateKey] = retry
	result.NextRunAt = earlierTime(result.NextRunAt, retryAt)
	return false, nil
}

func (p *episodeMemoryProcessor) ignoreEpisodeMemoryWork(state *episodeMemoryStateFile, work *episodeMemoryWork, cause error) error {
	cause = safeEpisodeMemoryError(cause)
	endedAt, err := episodeMemoryEpisodeEndedAt(work.episode)
	if err != nil {
		return err
	}
	if p != nil && p.plane != nil && p.plane.logger != nil {
		p.plane.logger.Info("[episode-memory] ignored: episode_id=%s error=%s", work.episode.ID, truncateForLog(cause.Error(), 500))
	}
	ignored := episodeMemoryEpisodeStatus{Status: episodeMemoryStatusIgnored, ExtractorVersion: episodeMemoryExtractorVersion, LastError: truncateForLog(cause.Error(), 500), AttemptCount: max(work.status.AttemptCount, work.originalStatus.AttemptCount) + 1}
	if err := p.state.CompleteEpisode(work.episode.ID, endedAt, ignored); err != nil {
		return err
	}
	state.Episodes[episodeMemoryStateKey(work.episode.ID, episodeMemoryExtractorVersion)] = ignored
	return nil
}

func earlierTime(current, candidate time.Time) time.Time {
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func (p *episodeMemoryProcessor) loadWork(ctx context.Context) (episodeMemoryStateFile, []TaskEpisode, error) {
	if p == nil || p.plane == nil || p.plane.episodes == nil || p.plane.device == nil || p.merge == nil {
		return episodeMemoryStateFile{}, nil, nil
	}
	state, err := p.state.Snapshot()
	if err != nil {
		return episodeMemoryStateFile{}, nil, err
	}
	enabledAt, err := time.Parse(time.RFC3339Nano, state.EnabledAt)
	if err != nil {
		return episodeMemoryStateFile{}, nil, fmt.Errorf("parse episode memory enabled_at: %w", err)
	}
	episodes, err := p.plane.episodes.listCompletedEpisodesSince(ctx, enabledAt, func(entry episodeIndexEntry, endedAt time.Time) bool {
		episodeStatus := state.Episodes[episodeMemoryStateKey(entry.ID, episodeMemoryExtractorVersion)]
		status := episodeStatus.Status
		if episodeStatus.ExtractorVersion == episodeMemoryExtractorVersion && (status == episodeMemoryStatusDone || status == episodeMemoryStatusIgnored) {
			return false
		}
		if episodeStatus.ExtractorVersion == episodeMemoryExtractorVersion && (status == episodeMemoryStatusProcessing || status == episodeMemoryStatusRetry || status == episodeMemoryStatusProposed) {
			return true
		}
		return episodeMemoryEntryAfterCursor(entry.ID, endedAt, state)
	})
	return state, episodes, err
}

type episodeMemoryBatchInput struct {
	Episode  TaskEpisode
	Payload  any
	Existing []DeviceMemoryItem
}

type episodeMemoryBatchResult struct {
	EpisodeID string                `json:"episode_id"`
	Proposal  episodeMemoryProposal `json:"proposal"`
}

type episodeMemoryBatchResponse struct {
	Results []episodeMemoryBatchResult `json:"results"`
}

// proposeEpisodeBatch asks the model for one proposal per episode. attempt is
// the highest retry count in the batch; it scales the output token budget so a
// retry after truncation gets more room instead of replaying the same failure.
func (p *episodeMemoryProcessor) proposeEpisodeBatch(ctx context.Context, episodes []TaskEpisode, attempt int) (map[string]episodeMemoryProposal, map[string]error, error) {
	if len(episodes) == 0 {
		return map[string]episodeMemoryProposal{}, map[string]error{}, nil
	}
	inputs := make([]episodeMemoryBatchInput, len(episodes))
	references := make([]MemoryMergeReference, 0, len(episodes)*8)
	_, raw, err := p.merge.Extract(ctx, MemoryMergeRequest{
		Search: func(ctx context.Context) ([]MemoryMergeReference, error) {
			for index, episode := range episodes {
				existing, err := p.plane.device.SearchEpisodeMemoryCandidates(ctx, EpisodeMemoryCandidateQuery{
					Terms:        episodeMemorySearchTerms(episode),
					PreferredIDs: episode.RetrievedMemoryRefs,
					DeviceID:     firstNonEmptyString([]string{episode.DeviceScope["device_id"], defaultMemoryDeviceID}),
					Scope:        episodeMemoryRetrievalScope(episode),
					Limit:        8,
					CharBudget:   12000,
				})
				if err != nil {
					return nil, err
				}
				inputs[index] = episodeMemoryBatchInput{Episode: episode, Payload: episodeMemoryPayload(episode), Existing: existing}
				for _, item := range existing {
					references = append(references, MemoryMergeReference{Scope: "device", ID: item.ID, Type: item.Type, Status: string(item.Status), Title: item.Title, Summary: item.Summary, Content: item.Content, Tags: item.Tags, Entities: item.Entities, Revision: effectiveDeviceMemoryRevision(item)})
				}
			}
			return references, nil
		},
		BuildMessages: func(_ []MemoryMergeReference) ([]llms.MessageContent, error) {
			parts := []llms.ContentPart{llms.TextPart(buildEpisodeMemoryBatchPrompt(inputs))}
			for _, input := range inputs {
				for _, screenshot := range loadEpisodeMemoryScreenshots(p.plane.episodes.rootDir, input.Episode) {
					parts = append(parts, llms.TextPart("Attached screenshot evidence for Episode "+input.Episode.ID+", event id: "+screenshot.EventID))
					parts = append(parts, llms.BinaryContent{MIMEType: screenshot.MIMEType, Data: screenshot.Data})
				}
			}
			return []llms.MessageContent{
				llms.TextParts(llms.ChatMessageTypeSystem, "You assess batches of completed device task episodes and extract reusable device memories. Output JSON only."),
				{Role: llms.ChatMessageTypeHuman, Parts: parts},
			}, nil
		},
		MaxTokens: episodeMemoryBatchTokenBudget(len(episodes), attempt),
		Timeout:   episodeMemoryModelCallTimeout,
	})
	if err != nil {
		// Extract returns partial content with a truncation error. Record its size
		// for diagnosis without copying potentially sensitive model output into
		// the persistent agent log.
		if errors.Is(err, errMemoryMergeTruncated) {
			p.logEpisodeMemoryResponseFailure("batch proposal truncated", raw)
		}
		return nil, nil, fmt.Errorf("extract episode memory batch: %w", err)
	}
	var response episodeMemoryBatchResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		// Terminal per isEpisodeMemoryProposalRetryable. Keep enough metadata to
		// distinguish empty and substantial malformed responses without logging
		// the response body.
		p.logEpisodeMemoryResponseFailure("batch proposal parse failed", raw)
		return nil, nil, fmt.Errorf("parse episode memory batch proposal: %w", err)
	}
	if len(response.Results) != len(episodes) {
		return nil, nil, fmt.Errorf("episode memory batch returned %d results for %d episodes", len(response.Results), len(episodes))
	}
	byID := make(map[string]TaskEpisode, len(episodes))
	existingByID := make(map[string][]DeviceMemoryItem, len(episodes))
	for _, input := range inputs {
		byID[input.Episode.ID] = input.Episode
		existingByID[input.Episode.ID] = input.Existing
	}
	proposals := make(map[string]episodeMemoryProposal, len(response.Results))
	proposalErrors := make(map[string]error)
	seen := make(map[string]bool, len(response.Results))
	for _, result := range response.Results {
		episode, ok := byID[strings.TrimSpace(result.EpisodeID)]
		if !ok {
			return nil, nil, fmt.Errorf("episode memory batch returned unknown episode %q", result.EpisodeID)
		}
		if seen[episode.ID] {
			return nil, nil, fmt.Errorf("episode memory batch returned duplicate episode %q", episode.ID)
		}
		seen[episode.ID] = true
		proposal, err := validateEpisodeMemoryProposal(episode, result.Proposal, existingByID[episode.ID])
		if err != nil {
			proposalErrors[episode.ID] = fmt.Errorf("episode %s: %w", episode.ID, err)
			continue
		}
		proposal, err = p.postProcessEpisodeMemoryProposal(ctx, episode, proposal, existingByID[episode.ID], attempt)
		if err != nil {
			proposalErrors[episode.ID] = fmt.Errorf("episode %s: %w", episode.ID, err)
			continue
		}
		proposals[episode.ID] = proposal
	}
	return proposals, proposalErrors, nil
}

func validateEpisodeMemoryProposal(episode TaskEpisode, proposal episodeMemoryProposal, existing []DeviceMemoryItem) (episodeMemoryProposal, error) {
	proposal.EpisodeAssessment.GoalResult = episodeGoalResult(strings.ToLower(strings.TrimSpace(string(proposal.EpisodeAssessment.GoalResult))))
	proposal.EpisodeAssessment.Reason = strings.TrimSpace(proposal.EpisodeAssessment.Reason)
	switch proposal.EpisodeAssessment.GoalResult {
	case episodeGoalAchieved, episodeGoalNotAchieved, episodeGoalUnknown:
	default:
		return episodeMemoryProposal{}, fmt.Errorf("invalid episode goal_result %q", proposal.EpisodeAssessment.GoalResult)
	}
	proposal.EpisodeAssessment.EvidenceRefs = validEpisodeMemoryEventIDs(episode, proposal.EpisodeAssessment.EvidenceRefs)
	if proposal.EpisodeAssessment.Reason == "" {
		return episodeMemoryProposal{}, fmt.Errorf("episode assessment requires a reason")
	}
	if len(proposal.Candidates) > 3 {
		proposal.Candidates = proposal.Candidates[:3]
	}
	proposal.ExistingRevisions = make(map[string]int, len(existing))
	for _, item := range existing {
		proposal.ExistingRevisions[item.ID] = effectiveDeviceMemoryRevision(item)
	}
	return proposal, nil
}

func (p *episodeMemoryProcessor) postProcessEpisodeMemoryProposal(ctx context.Context, episode TaskEpisode, proposal episodeMemoryProposal, existing []DeviceMemoryItem, attempt int) (episodeMemoryProposal, error) {
	proposal, err := normalizeEpisodeMemoryAssessment(episode, proposal, existing)
	if err != nil {
		return episodeMemoryProposal{}, err
	}
	if shouldReviewEpisodeMemoryProposal(episode, proposal) {
		reviewed, reviewErr := p.reviewEpisodeMemoryOmission(ctx, episode, proposal, existing, attempt)
		if reviewErr != nil {
			return episodeMemoryProposal{}, safeEpisodeMemoryError(fmt.Errorf("%w: %w", errEpisodeMemoryOmissionReview, reviewErr))
		}
		proposal, err = normalizeEpisodeMemoryAssessment(episode, reviewed, existing)
		if err != nil {
			return episodeMemoryProposal{}, err
		}
	}
	if !episodeMemoryProposalNeedsRetentionAudit(proposal) {
		return proposal, nil
	}
	proposal.Candidates = compactEpisodeMemoryCandidates(proposal.Candidates)
	if len(proposal.Candidates) == 0 {
		return proposal, nil
	}
	p.logEpisodeMemoryRetentionAudit("started", len(proposal.Candidates), 0, 0)
	audit, auditErr := p.generateEpisodeMemoryRetentionAudit(ctx, episode, proposal, existing, attempt)
	if auditErr != nil {
		p.logEpisodeMemoryRetentionAudit("failed", len(proposal.Candidates), 0, 0)
		safeErr := safeEpisodeMemoryError(fmt.Errorf("%w: %w", errEpisodeMemoryRetentionAudit, auditErr))
		if p.plane != nil && p.plane.logger != nil {
			p.plane.logger.Warn("[episode-memory] retention audit failed: episode_id=%s error_class=%s",
				episode.ID, safeErr.Error())
		}
		return episodeMemoryProposal{}, safeErr
	}
	originalCount := len(proposal.Candidates)
	stats := summarizeEpisodeMemoryRetentionAudit(proposal.Candidates, audit)
	proposal.Candidates = retainedEpisodeMemoryCandidates(proposal.Candidates, audit)
	p.logEpisodeMemoryRetentionAudit("completed", originalCount, len(audit.Reviews), len(proposal.Candidates), stats)
	return proposal, nil
}

func normalizeEpisodeMemoryAssessment(episode TaskEpisode, proposal episodeMemoryProposal, existing []DeviceMemoryItem) (episodeMemoryProposal, error) {
	proposal.EpisodeAssessment.GoalResult = episodeGoalResult(strings.ToLower(strings.TrimSpace(string(proposal.EpisodeAssessment.GoalResult))))
	proposal.EpisodeAssessment.Reason = strings.TrimSpace(proposal.EpisodeAssessment.Reason)
	proposal.EpisodeAssessment.EvidenceRefs = validEpisodeMemoryEventIDs(episode, proposal.EpisodeAssessment.EvidenceRefs)
	switch proposal.EpisodeAssessment.GoalResult {
	case episodeGoalAchieved, episodeGoalNotAchieved, episodeGoalUnknown:
	default:
		return episodeMemoryProposal{}, fmt.Errorf("invalid episode goal_result %q", proposal.EpisodeAssessment.GoalResult)
	}
	if proposal.EpisodeAssessment.GoalResult == episodeGoalNotAchieved && !hasDirectEpisodeFailureEvidence(episode, proposal.EpisodeAssessment.EvidenceRefs) {
		proposal.Candidates = nil
		if episodeExplicitlyEndedBeforeCompletion(episode) {
			proposal.EpisodeAssessment.Reason = "The Episode was explicitly ended before the requested goal completed; no actionable failure evidence was recorded."
		} else {
			proposal.EpisodeAssessment.GoalResult = episodeGoalUnknown
			proposal.EpisodeAssessment.Reason = "Final completion was not directly established, and the cited evidence does not record a structured failure or explicit termination."
		}
	}
	if proposal.EpisodeAssessment.Reason == "" {
		return episodeMemoryProposal{}, fmt.Errorf("episode assessment requires a reason")
	}
	if proposal.EpisodeAssessment.GoalResult != episodeGoalUnknown && !hasDirectEpisodeAssessmentEvidence(episode, proposal.EpisodeAssessment.EvidenceRefs) {
		if proposal.EpisodeAssessment.GoalResult != episodeGoalNotAchieved || !episodeExplicitlyEndedBeforeCompletion(episode) {
			return episodeMemoryProposal{}, fmt.Errorf("episode assessment %s requires direct evidence", proposal.EpisodeAssessment.GoalResult)
		}
	}
	if len(proposal.Candidates) > 3 {
		proposal.Candidates = proposal.Candidates[:3]
	}
	proposal.ExistingRevisions = make(map[string]int, len(existing))
	for _, item := range existing {
		proposal.ExistingRevisions[item.ID] = effectiveDeviceMemoryRevision(item)
	}
	return proposal, nil
}

func (p *episodeMemoryProcessor) reviewEpisodeMemoryOmission(ctx context.Context, episode TaskEpisode, proposal episodeMemoryProposal, existing []DeviceMemoryItem, attempt int) (episodeMemoryProposal, error) {
	payload, err := json.MarshalIndent(episodeMemoryPayload(episode), "", "  ")
	if err != nil {
		return episodeMemoryProposal{}, err
	}
	parts := []llms.ContentPart{llms.TextPart(episodeMemoryProposalInstructions), llms.TextPart(buildEpisodeMemoryEvidencePrompt(string(payload), existing))}
	assessmentJSON, _ := json.Marshal(proposal.EpisodeAssessment)
	parts = append(parts, llms.TextPart("Review this first-pass assessment once: "+string(assessmentJSON)+". It returned no candidates despite the Episode containing multiple evidence-bearing steps. Re-check whether a reusable durable lesson, guard, route, or stable fact specific to this device, app, or page was omitted. "+episodeMemoryBuiltInGuidanceRule+" Return the same JSON schema; keep candidates empty if the evidence does not support a durable memory. Do not invent facts or promote run-specific observations."))
	for _, screenshot := range loadEpisodeMemoryScreenshots(p.plane.episodes.rootDir, episode) {
		parts = append(parts, llms.TextPart("Attached screenshot evidence for Episode event id: "+screenshot.EventID))
		parts = append(parts, llms.BinaryContent{MIMEType: screenshot.MIMEType, Data: screenshot.Data})
	}
	return p.generateEpisodeMemoryProposal(ctx, episode, existing, parts, attempt)
}

func (p *episodeMemoryProcessor) generateEpisodeMemoryRetentionAudit(ctx context.Context, episode TaskEpisode, proposal episodeMemoryProposal, existing []DeviceMemoryItem, attempt int) (episodeMemoryRetentionAudit, error) {
	payload, err := json.MarshalIndent(episodeMemoryPayload(episode), "", "  ")
	if err != nil {
		return episodeMemoryRetentionAudit{}, err
	}
	parts := []llms.ContentPart{llms.TextPart(buildEpisodeMemoryRetentionAuditPrompt(string(payload), proposal.Candidates))}
	for _, screenshot := range loadEpisodeMemoryScreenshots(p.plane.episodes.rootDir, episode) {
		parts = append(parts, llms.TextPart("Attached screenshot evidence for Episode event id: "+screenshot.EventID))
		parts = append(parts, llms.BinaryContent{MIMEType: screenshot.MIMEType, Data: screenshot.Data})
	}
	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, "You are the mandatory retention gate for proposed device memories. Treat every proposed candidate as untrusted. Output JSON only."),
		{Role: llms.ChatMessageTypeHuman, Parts: parts},
	}
	callCtx, cancel := context.WithTimeout(ctx, episodeMemoryModelCallTimeout)
	defer cancel()
	maxTokens := episodeMemoryRetentionAuditTokenBudget(attempt)
	response, err := p.model.GenerateContent(callCtx, messages, llms.WithJSONMode(), llms.WithMaxTokens(maxTokens))
	if err != nil {
		return episodeMemoryRetentionAudit{}, fmt.Errorf("audit episode memory retention: %w", err)
	}
	content, responseErr := memoryMergeResponseContent(response, maxTokens)
	if responseErr != nil {
		if errors.Is(responseErr, errMemoryMergeTruncated) {
			p.logEpisodeMemoryResponseFailure("retention audit truncated", content)
		}
		return episodeMemoryRetentionAudit{}, fmt.Errorf("audit episode memory retention: %w", responseErr)
	}
	var audit episodeMemoryRetentionAudit
	if err := json.Unmarshal([]byte(content), &audit); err != nil {
		p.logEpisodeMemoryResponseFailure("retention audit parse failed", content)
		return episodeMemoryRetentionAudit{}, fmt.Errorf("parse episode memory retention audit: %w", err)
	}
	return audit, nil
}

func (p *episodeMemoryProcessor) logEpisodeMemoryRetentionAudit(status string, candidateCount, reviewCount, retainedCount int, auditStats ...episodeMemoryRetentionAuditStats) {
	if p == nil || p.plane == nil || p.plane.logger == nil {
		return
	}
	stats := episodeMemoryRetentionAuditStats{}
	if len(auditStats) > 0 {
		stats = auditStats[0]
	}
	p.plane.logger.Info("[episode-memory] retention audit %s: candidates=%d reviews=%d retain_decisions=%d rewrites=%d matching_keys=%d retained=%d", status, candidateCount, reviewCount, stats.RetainDecisions, stats.Rewrites, stats.MatchingKeys, retainedCount)
}

func summarizeEpisodeMemoryRetentionAudit(candidates []episodeMemoryCandidate, audit episodeMemoryRetentionAudit) episodeMemoryRetentionAuditStats {
	keys := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		keys[strings.TrimSpace(candidate.LessonKey)] = true
	}
	stats := episodeMemoryRetentionAuditStats{}
	for _, review := range audit.Reviews {
		if episodeMemoryRetentionDecision(strings.ToLower(strings.TrimSpace(string(review.Decision)))) == episodeMemoryRetentionDecisionRetain {
			stats.RetainDecisions++
		}
		if review.Rewrite != nil {
			stats.Rewrites++
		}
		if keys[strings.TrimSpace(review.LessonKey)] {
			stats.MatchingKeys++
		}
	}
	return stats
}

func compactEpisodeMemoryCandidates(candidates []episodeMemoryCandidate) []episodeMemoryCandidate {
	compacted := make([]episodeMemoryCandidate, 0, len(candidates))
	indexByKey := make(map[string]int, len(candidates))
	conflictingKeys := make(map[string]bool)
	for _, candidate := range candidates {
		key := strings.TrimSpace(candidate.LessonKey)
		if key == "" || conflictingKeys[key] {
			continue
		}
		index, exists := indexByKey[key]
		if !exists {
			indexByKey[key] = len(compacted)
			compacted = append(compacted, candidate)
			continue
		}
		base := &compacted[index]
		if !sameEpisodeMemoryCandidateIdentity(*base, candidate) {
			conflictingKeys[key] = true
			continue
		}
		base.Tags = uniqueNonEmpty(append(base.Tags, candidate.Tags...))
		base.EvidenceRefs = uniqueNonEmpty(append(base.EvidenceRefs, candidate.EvidenceRefs...))
	}
	if len(conflictingKeys) == 0 {
		return compacted
	}
	filtered := compacted[:0]
	for _, candidate := range compacted {
		if !conflictingKeys[strings.TrimSpace(candidate.LessonKey)] {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func episodeMemoryProposalNeedsRetentionAudit(proposal episodeMemoryProposal) bool {
	return len(proposal.Candidates) > 0
}

func retainedEpisodeMemoryCandidates(original []episodeMemoryCandidate, audit episodeMemoryRetentionAudit) []episodeMemoryCandidate {
	reviewCounts := make(map[string]int, len(audit.Reviews))
	for _, review := range audit.Reviews {
		reviewCounts[strings.TrimSpace(review.LessonKey)]++
	}
	reviewByKey := make(map[string]episodeMemoryRetentionReview, len(audit.Reviews))
	for _, review := range audit.Reviews {
		key := strings.TrimSpace(review.LessonKey)
		if reviewCounts[key] == 1 {
			reviewByKey[key] = review
		}
	}
	retained := make([]episodeMemoryCandidate, 0, len(original))
	for _, base := range original {
		key := strings.TrimSpace(base.LessonKey)
		review, found := reviewByKey[key]
		decision := episodeMemoryRetentionDecision(strings.ToLower(strings.TrimSpace(string(review.Decision))))
		retention := episodeMemoryRetention(strings.ToLower(strings.TrimSpace(string(review.Retention))))
		if !found || decision != episodeMemoryRetentionDecisionRetain || retention != episodeMemoryRetentionDurable || strings.TrimSpace(review.Reason) == "" || review.Rewrite == nil || !sameEpisodeMemoryEvidenceRefs(base.EvidenceRefs, review.Rewrite.EvidenceRefs) {
			continue
		}
		if episodeMemoryRewriteContainsSensitiveValue(*review.Rewrite, review.SensitiveValues) {
			continue
		}
		base.Retention = retention
		base.Situation = strings.TrimSpace(review.Rewrite.Situation)
		base.Guidance = strings.TrimSpace(review.Rewrite.Guidance)
		base.ExpectedEffect = strings.TrimSpace(review.Rewrite.ExpectedEffect)
		// The retention reviewer returns the complete rewritten applicability
		// scope. Preserve that semantic rewrite; validateEpisodeMemoryCandidate
		// will re-apply the Episode's non-negotiable device boundaries before
		// persistence.
		base.Scope = mergeEpisodeMemoryReviewScope(base.Scope, review.Rewrite.Scope)
		base.Tags = append([]string(nil), review.Rewrite.Tags...)
		base.EvidenceRefs = append([]string(nil), base.EvidenceRefs...)
		base.SensitiveValues = uniqueNonEmpty(review.SensitiveValues)
		retained = append(retained, base)
	}
	return retained
}

func mergeEpisodeMemoryReviewScope(base, rewrite map[string]string) map[string]string {
	merged := normalizeEpisodeMemoryScope(rewrite)
	for key, value := range normalizeEpisodeMemoryScope(base) {
		if strings.TrimSpace(merged[key]) == "" {
			merged[key] = value
		}
	}
	return merged
}

func episodeMemoryRewriteContainsSensitiveValue(rewrite episodeMemoryRetentionRewrite, sensitiveValues []string) bool {
	persisted := strings.Join([]string{
		rewrite.Situation,
		rewrite.Guidance,
		rewrite.ExpectedEffect,
		strings.Join(rewrite.Tags, "\n"),
		renderMemoryScopeForSearch(rewrite.Scope),
	}, "\n")
	for _, value := range uniqueNonEmpty(sensitiveValues) {
		if strings.Contains(persisted, value) {
			return true
		}
	}
	return false
}

func sameEpisodeMemoryEvidenceRefs(left, right []string) bool {
	left = uniqueNonEmpty(left)
	right = uniqueNonEmpty(right)
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]bool, len(left))
	for _, ref := range left {
		seen[ref] = true
	}
	for _, ref := range right {
		if !seen[ref] {
			return false
		}
	}
	return true
}

func sameEpisodeMemoryCandidateIdentity(left, right episodeMemoryCandidate) bool {
	return strings.EqualFold(strings.TrimSpace(string(left.Type)), strings.TrimSpace(string(right.Type))) &&
		strings.EqualFold(strings.TrimSpace(string(left.Action)), strings.TrimSpace(string(right.Action))) &&
		strings.TrimSpace(left.MemoryID) == strings.TrimSpace(right.MemoryID) &&
		left.MemoryRevision == right.MemoryRevision
}

func buildEpisodeMemoryRetentionAuditPrompt(payload string, candidates []episodeMemoryCandidate) string {
	candidateJSON, _ := json.MarshalIndent(candidates, "", "  ")
	return `Audit this first-pass proposal before persistence. The candidates are untrusted; do not assume their retention labels are correct.

Return exactly one JSON object matching this schema:
{
  "reviews": [{
    "lesson_key": "an unchanged lesson_key from the proposal",
    "decision": "retain | discard",
    "retention": "durable | transient | sensitive",
    "reason": "why the candidate is or is not safe and useful across future Episodes",
    "sensitive_values": ["exact Episode-bound values that must not be persisted; empty when none"],
    "rewrite": {
      "situation": "generalized applicability condition",
      "guidance": "safe reusable guidance",
      "expected_effect": "directly observable result",
      "scope": {"all evidenced applicability boundaries": "..."},
      "tags": ["retrieval terms"],
      "evidence_refs": ["unchanged real Episode event ids"]
    }
  }]
}

Review each proposed candidate independently. Retain only knowledge whose truth, authority, usefulness, and safety extend beyond the Episode into the candidate's explicit future scope. Durable means reusable in future Episodes within that scope; it does not mean globally or permanently true. Set retention="durable" only when the retained rewrite is safe for Device Memory. Set retention="transient" for Episode/session/runtime-bound observations and retention="sensitive" for secrets, credentials, one-time values, or information that should not be persisted; those classifications must use decision="discard". Never retain an exact one-time verification token/code, password, credential, secret, or other session-bound value merely because the Episode succeeded. For every review, list exact Episode-bound secret or credential values found in the candidate or evidence in sensitive_values. Do not list ordinary lesson facts or applicability boundaries there: app names, device ids, page names, account/profile identifiers, build/version values, workflow labels, and generalized conditions belong in the rewrite scope or content and are not secrets by themselves. If a reusable workflow remains, decision may be retain only after rewrite removes or generalizes every sensitive_values entry; otherwise discard it. A retained rewrite that still contains any listed sensitive value is invalid. ` + episodeMemoryBuiltInGuidanceRule + ` Discard a candidate that carries no such device-, app-, or page-specific knowledge. Preserve evidenced app, device, page, account, build, and version scope boundaries. Do not add lessons, reassess the Episode outcome, or invent evidence. When uncertain, discard.

A procedure rewrite must preserve the observed step sequence; do not reduce it to the final action or infer that the final action alone succeeds.

Episode:
` + payload + `

Untrusted candidates:
` + string(candidateJSON)
}

func buildEpisodeMemoryBatchPrompt(inputs []episodeMemoryBatchInput) string {
	var builder strings.Builder
	builder.WriteString("Process each independent Episode below. Return exactly one JSON object with this schema:\n")
	builder.WriteString(`{"results":[{"episode_id":"the exact Episode id","proposal":<the proposal object described below>}]}`)
	builder.WriteString("\nReturn one result for every Episode, using the exact Episode id. Do not combine evidence across Episodes.\n\n")
	builder.WriteString(episodeMemoryProposalInstructions)
	builder.WriteString("\n\n")
	for _, input := range inputs {
		payload, _ := json.MarshalIndent(input.Payload, "", "  ")
		builder.WriteString("===== Episode ")
		builder.WriteString(input.Episode.ID)
		builder.WriteString(" =====\n")
		builder.WriteString(buildEpisodeMemoryEvidencePrompt(string(payload), input.Existing))
		builder.WriteString("\n\n")
	}
	return builder.String()
}

func hasDirectEpisodeAssessmentEvidence(episode TaskEpisode, refs []string) bool {
	allowed := make(map[string]bool, len(refs))
	for _, ref := range refs {
		allowed[ref] = true
	}
	for _, event := range episode.Events {
		if !allowed[event.EventID] {
			continue
		}
		if event.Type == "tool_result" || event.Type == "steer" || event.ToolError != nil || event.IsError || strings.TrimSpace(event.ScreenshotRef) != "" {
			return true
		}
	}
	return false
}

func hasDirectEpisodeFailureEvidence(episode TaskEpisode, refs []string) bool {
	switch strings.ToLower(strings.TrimSpace(episode.Status)) {
	case "interrupted", "cancelled", "canceled":
		return true
	}
	if strings.TrimSpace(episode.Outcome.FailureReason) != "" {
		return true
	}
	allowed := make(map[string]bool, len(refs))
	for _, ref := range refs {
		allowed[ref] = true
	}
	for _, event := range episode.Events {
		if allowed[event.EventID] && (event.Type == "steer" || event.IsError || (event.ToolError != nil && event.ToolError.Code != CodeCanceled)) {
			return true
		}
	}
	return false
}

func episodeExplicitlyEndedBeforeCompletion(episode TaskEpisode) bool {
	switch strings.ToLower(strings.TrimSpace(episode.Status)) {
	case "abandoned", "interrupted", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func shouldReviewEpisodeMemoryProposal(episode TaskEpisode, proposal episodeMemoryProposal) bool {
	if len(proposal.Candidates) != 0 || proposal.EpisodeAssessment.GoalResult == episodeGoalUnknown || !hasDirectEpisodeAssessmentEvidence(episode, proposal.EpisodeAssessment.EvidenceRefs) {
		return false
	}
	deviceCalls, deviceResults, hasProblem := 0, 0, false
	for _, event := range episode.Events {
		if event.IsError || (event.ToolError != nil && event.ToolError.Code != CodeCanceled) || event.Type == "steer" {
			hasProblem = true
		}
		if !isEpisodeMemoryDeviceTool(event.ToolName) {
			continue
		}
		switch event.Type {
		case runEventToolCall:
			deviceCalls++
		case "tool_result":
			deviceResults++
		}
	}
	return (deviceCalls >= 2 && deviceResults >= 2) || (hasProblem && deviceCalls >= 1 && deviceResults >= 1)
}

func (p *episodeMemoryProcessor) generateEpisodeMemoryProposal(ctx context.Context, episode TaskEpisode, existing []DeviceMemoryItem, parts []llms.ContentPart, attempt int) (episodeMemoryProposal, error) {
	if p == nil || p.model == nil {
		return episodeMemoryProposal{}, fmt.Errorf("episode memory model is not configured")
	}
	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, "You assess completed device task episodes and extract reusable device memories. Output JSON only."),
		{Role: llms.ChatMessageTypeHuman, Parts: parts},
	}
	callCtx, cancel := context.WithTimeout(ctx, episodeMemoryModelCallTimeout)
	defer cancel()
	maxTokens := episodeMemoryBatchTokenBudget(1, attempt)
	response, err := p.model.GenerateContent(callCtx, messages, llms.WithJSONMode(), llms.WithMaxTokens(maxTokens))
	if err != nil {
		return episodeMemoryProposal{}, fmt.Errorf("extract episode memory: %w", err)
	}
	content, responseErr := memoryMergeResponseContent(response, maxTokens)
	if responseErr != nil {
		if errors.Is(responseErr, errMemoryMergeTruncated) {
			p.logEpisodeMemoryResponseFailure("omission review truncated", content)
		}
		return episodeMemoryProposal{}, fmt.Errorf("extract episode memory: %w", responseErr)
	}
	var proposal episodeMemoryProposal
	if err := json.Unmarshal([]byte(content), &proposal); err != nil {
		p.logEpisodeMemoryResponseFailure("omission review parse failed", content)
		return episodeMemoryProposal{}, fmt.Errorf("parse episode memory proposal: %w", err)
	}
	proposal.ExistingRevisions = make(map[string]int, len(existing))
	for _, item := range existing {
		proposal.ExistingRevisions[item.ID] = effectiveDeviceMemoryRevision(item)
	}
	return proposal, nil
}

type episodeMemoryEventPayload struct {
	EventID       string     `json:"event_id,omitempty"`
	Type          string     `json:"type"`
	Role          string     `json:"role,omitempty"`
	Objective     string     `json:"objective,omitempty"`
	ToolName      string     `json:"tool_name,omitempty"`
	ToolInput     string     `json:"tool_input,omitempty"`
	ToolError     *ToolError `json:"tool_error,omitempty"`
	IsError       bool       `json:"is_error,omitempty"`
	Content       string     `json:"content,omitempty"`
	Observation   string     `json:"observation,omitempty"`
	ScreenshotRef string     `json:"screenshot_ref,omitempty"`
	Reason        string     `json:"reason,omitempty"`
}

func episodeMemoryPayload(episode TaskEpisode) map[string]interface{} {
	screenshotEvents := make(map[string]string)
	for _, event := range episode.Events {
		if ref := strings.TrimSpace(event.ScreenshotRef); ref != "" {
			screenshotEvents[ref] = event.EventID
		}
	}
	events := episode.Events
	if len(events) > 60 {
		events = events[len(events)-60:]
	}
	views := make([]episodeMemoryEventPayload, 0, len(events))
	for _, event := range events {
		view := episodeMemoryEventPayload{
			EventID:     event.EventID,
			Type:        event.Type,
			Role:        event.Role,
			Objective:   truncateForLog(event.Objective, 500),
			ToolName:    event.ToolName,
			ToolInput:   truncateForLog(event.ToolInput, 800),
			ToolError:   event.ToolError,
			IsError:     event.IsError,
			Content:     truncateForLog(sanitizeEpisodeMemoryScreenshotPaths(event.Content, screenshotEvents), 1200),
			Observation: truncateForLog(sanitizeEpisodeMemoryScreenshotPaths(event.Observation, screenshotEvents), 1600),
			Reason:      truncateForLog(event.Reason, 800),
		}
		if strings.TrimSpace(event.ScreenshotRef) != "" {
			view.ScreenshotRef = "attached screenshot for event " + event.EventID
		}
		views = append(views, view)
	}
	return map[string]interface{}{
		"episode_id":   episode.ID,
		"user_goal":    episode.UserGoal,
		"device_scope": episode.DeviceScope,
		"recorded_outcome": map[string]interface{}{
			"success":        episode.Outcome.Success,
			"final_state":    sanitizeEpisodeMemoryScreenshotPaths(episode.Outcome.FinalState, screenshotEvents),
			"final_answer":   sanitizeEpisodeMemoryScreenshotPaths(episode.Outcome.FinalAnswer, screenshotEvents),
			"failure_reason": sanitizeEpisodeMemoryScreenshotPaths(episode.Outcome.FailureReason, screenshotEvents),
		},
		"tags":              episode.Tags,
		"entities":          episode.Entities,
		"recalled_memories": episode.RetrievedMemoryRefs,
		"events":            views,
	}
}

func (p *episodeMemoryProcessor) applyProposal(ctx context.Context, episode TaskEpisode, proposal episodeMemoryProposal) error {
	seenLessonKeys := map[string]bool{}
	for _, raw := range proposal.Candidates {
		candidate, ok := validateEpisodeMemoryCandidate(episode, proposal.EpisodeAssessment, raw, seenLessonKeys)
		if !ok {
			continue
		}
		seenLessonKeys[candidate.LessonKey] = true
		switch candidate.Action {
		case episodeMemoryActionCreate:
			if _, err := p.createMemory(ctx, episode, candidate); err != nil {
				return err
			}
		case episodeMemoryActionUpdate:
			if revision, ok := proposal.ExistingRevisions[candidate.MemoryID]; !ok || candidate.MemoryRevision != revision {
				return fmt.Errorf("%w: proposal for %s expected revision %d", errEpisodeMemoryRevisionChanged, candidate.MemoryID, candidate.MemoryRevision)
			}
			current, found, err := p.plane.device.Get(ctx, candidate.MemoryID)
			if err != nil {
				return err
			}
			if found && hasEpisodeEvidence(current.EvidenceRefs, episode.ID) {
				continue
			}
			if found && !strings.EqualFold(strings.TrimSpace(current.Type), strings.TrimSpace(string(candidate.Type))) {
				if candidate.UnresolvedConflict {
					continue
				}
				candidate.Action = episodeMemoryActionCreate
				candidate.MemoryID = ""
				candidate.MemoryRevision = 0
				if _, err := p.createMemory(ctx, episode, candidate); err != nil {
					return err
				}
				continue
			}
			if err := p.updateMemory(ctx, episode, candidate); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateEpisodeMemoryCandidate(episode TaskEpisode, assessment episodeMemoryAssessment, candidate episodeMemoryCandidate, seen map[string]bool) (episodeMemoryCandidate, bool) {
	hadExplicitScope := len(candidate.Scope) > 0
	candidate.LessonKey = strings.TrimSpace(candidate.LessonKey)
	candidate.Type = episodeMemoryType(strings.ToLower(strings.TrimSpace(string(candidate.Type))))
	candidate.Action = episodeMemoryAction(strings.ToLower(strings.TrimSpace(string(candidate.Action))))
	candidate.Retention = episodeMemoryRetention(strings.ToLower(strings.TrimSpace(string(candidate.Retention))))
	candidate.MemoryID = strings.TrimSpace(candidate.MemoryID)
	candidate.Situation = strings.TrimSpace(candidate.Situation)
	candidate.Guidance = strings.TrimSpace(candidate.Guidance)
	candidate.ExpectedEffect = strings.TrimSpace(candidate.ExpectedEffect)
	candidate.ConflictReason = strings.TrimSpace(candidate.ConflictReason)
	candidate.SensitiveValues = uniqueNonEmpty(candidate.SensitiveValues)
	var scopeOK bool
	candidate.Scope, scopeOK = mergeEpisodeMemoryHardScope(episode, candidate.Scope)
	if !scopeOK || !hadExplicitScope {
		return episodeMemoryCandidate{}, false
	}
	if candidate.LessonKey == "" || seen[candidate.LessonKey] {
		return episodeMemoryCandidate{}, false
	}
	confidence, err := normalizeEpisodeMemoryConfidence(candidate.Confidence)
	if err != nil {
		return episodeMemoryCandidate{}, false
	}
	candidate.Confidence = episodeMemoryConfidencePointer(confidence)
	switch candidate.Type {
	case episodeMemoryTypeProcedure, episodeMemoryTypeNavigation, episodeMemoryTypeCalibration, episodeMemoryTypeFailure, episodeMemoryTypeFact:
	default:
		return episodeMemoryCandidate{}, false
	}
	if candidate.Action != episodeMemoryActionCreate && candidate.Action != episodeMemoryActionUpdate {
		return episodeMemoryCandidate{}, false
	}
	if candidate.Retention != episodeMemoryRetentionDurable {
		return episodeMemoryCandidate{}, false
	}
	if candidate.Action == episodeMemoryActionUpdate && (candidate.MemoryID == "" || candidate.MemoryRevision <= 0) {
		return episodeMemoryCandidate{}, false
	}
	if candidate.Action == episodeMemoryActionCreate && (candidate.MemoryID != "" || candidate.UnresolvedConflict) {
		return episodeMemoryCandidate{}, false
	}
	if candidate.Action == episodeMemoryActionCreate {
		candidate.MemoryRevision = 0
	}
	if candidate.UnresolvedConflict && candidate.ConflictReason == "" {
		return episodeMemoryCandidate{}, false
	}
	if !candidate.UnresolvedConflict {
		candidate.ConflictReason = ""
	}
	if candidate.Situation == "" || candidate.Guidance == "" || candidate.ExpectedEffect == "" || len(candidate.Scope) == 0 {
		return episodeMemoryCandidate{}, false
	}
	originalRefs := uniqueNonEmpty(candidate.EvidenceRefs)
	candidate.EvidenceRefs = validEpisodeMemoryEventIDs(episode, originalRefs)
	if len(candidate.EvidenceRefs) == 0 || len(candidate.EvidenceRefs) != len(originalRefs) {
		return episodeMemoryCandidate{}, false
	}
	candidate.EvidenceRefs = expandEpisodeMemoryEvidenceRefs(episode, candidate.EvidenceRefs)
	if !episodeMemoryTypeEvidenceValid(candidate.Type, episode, candidate.EvidenceRefs, assessment) {
		return episodeMemoryCandidate{}, false
	}
	if candidate.Type == episodeMemoryTypeProcedure && assessment.GoalResult == episodeGoalNotAchieved && !isPartialProcedureScope(candidate.Scope) {
		return episodeMemoryCandidate{}, false
	}
	candidate.Tags = normalizeEpisodeMemoryTags(candidate.Tags)
	return candidate, true
}

func expandEpisodeMemoryEvidenceRefs(episode TaskEpisode, refs []string) []string {
	expanded := append([]string(nil), refs...)
	pendingCalls := make(map[string][]string)
	pairedCalls := make(map[string]string)
	for _, event := range episode.Events {
		name := strings.ToLower(strings.TrimSpace(event.ToolName))
		if name == "" || !isEpisodeMemoryDeviceTool(name) {
			continue
		}
		switch event.Type {
		case runEventToolCall:
			pendingCalls[name] = append(pendingCalls[name], event.EventID)
		case "tool_result":
			calls := pendingCalls[name]
			if len(calls) == 0 {
				continue
			}
			pairedCalls[event.EventID] = calls[len(calls)-1]
			pendingCalls[name] = calls[:len(calls)-1]
		}
	}
	for _, ref := range refs {
		if callID := pairedCalls[ref]; callID != "" {
			expanded = appendUniqueString(expanded, callID)
		}
	}
	return expanded
}

func episodeMemoryTypeEvidenceValid(memoryType episodeMemoryType, episode TaskEpisode, refs []string, assessment episodeMemoryAssessment) bool {
	events := make(map[string]TaskEpisodeEvent, len(episode.Events))
	for _, event := range episode.Events {
		events[event.EventID] = event
	}
	callCount, resultCount, observationCount := 0, 0, 0
	hasCoordinateInput, hasProblem := false, false
	for _, ref := range refs {
		event := events[ref]
		switch event.Type {
		case runEventToolCall:
			if isEpisodeMemoryDeviceTool(event.ToolName) {
				callCount++
				input := strings.ToLower(event.ToolInput)
				hasCoordinateInput = hasCoordinateInput || strings.Contains(input, `"x"`) || strings.Contains(input, `"y"`) || strings.Contains(input, "coord") || strings.Contains(input, "point")
			}
		case "tool_result":
			resultCount++
			if strings.TrimSpace(event.Content) != "" || strings.TrimSpace(event.Observation) != "" || strings.TrimSpace(event.ScreenshotRef) != "" {
				observationCount++
			}
		case "steer":
			hasProblem = true
		}
		if event.Type != "tool_result" && (strings.TrimSpace(event.Observation) != "" || strings.TrimSpace(event.ScreenshotRef) != "") {
			observationCount++
		}
		if event.IsError || (event.ToolError != nil && event.ToolError.Code != CodeCanceled) {
			hasProblem = true
		}
	}
	switch memoryType {
	case episodeMemoryTypeProcedure:
		return callCount >= 2 && resultCount >= 2 && observationCount >= 1
	case episodeMemoryTypeNavigation:
		return callCount >= 1 && resultCount >= 1 && observationCount >= 2
	case episodeMemoryTypeCalibration:
		return callCount >= 1 && resultCount >= 1 && hasCoordinateInput && observationCount >= 1
	case episodeMemoryTypeFailure:
		return hasProblem || (assessment.GoalResult == episodeGoalNotAchieved && episodeMemoryRefsOverlap(refs, assessment.EvidenceRefs))
	case episodeMemoryTypeFact:
		return resultCount >= 1 && observationCount >= 1
	default:
		return false
	}
}

func episodeMemoryRefsOverlap(left, right []string) bool {
	seen := make(map[string]bool, len(left))
	for _, ref := range left {
		seen[strings.TrimSpace(ref)] = true
	}
	for _, ref := range right {
		if seen[strings.TrimSpace(ref)] {
			return true
		}
	}
	return false
}

func isPartialProcedureScope(scope map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(scope["partial"]), "true") || strings.EqualFold(strings.TrimSpace(scope["goal_scope"]), "partial")
}

func normalizeEpisodeMemoryTags(tags []string) []string {
	result := []string{episodeMemoryTag}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || strings.EqualFold(tag, episodeMemoryTag) {
			continue
		}
		result = appendUniqueString(result, truncateForLog(tag, 48))
		if len(result) == 9 {
			break
		}
	}
	return result
}

func normalizeEpisodeMemoryScope(scope map[string]string) map[string]string {
	result := make(map[string]string, len(scope))
	for key, value := range scope {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			result[key] = value
		}
	}
	return result
}

// Episode device scope contains runtime facts that are not all appropriate as
// memory applicability (for example, the current screen resolution). These
// keys, however, are hard identity/version/page boundaries: a retained lesson
// must not silently become applicable outside the Episode in which it was
// evidenced. The LLM still owns the semantic scope and may add conditions;
// code only fills these explicit boundaries and rejects contradictions.
func mergeEpisodeMemoryHardScope(episode TaskEpisode, candidate map[string]string) (map[string]string, bool) {
	result := normalizeEpisodeMemoryScope(candidate)
	if len(episode.DeviceScope) == 0 {
		return result, true
	}
	for key, value := range normalizeEpisodeMemoryScope(episode.DeviceScope) {
		if !isEpisodeMemoryHardScopeKey(key) {
			continue
		}
		if current := strings.TrimSpace(result[key]); current != "" && !strings.EqualFold(current, value) {
			return nil, false
		}
		if value != "" {
			result[key] = value
		}
	}
	return result, true
}

func isEpisodeMemoryHardScopeKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "device", "device_id", "app", "app_id", "app_name", "app_version", "page_name", "account_id", "profile_id", "workspace_id", "tenant_id":
		return true
	default:
		return false
	}
}

func (p *episodeMemoryProcessor) createMemory(ctx context.Context, episode TaskEpisode, candidate episodeMemoryCandidate) (string, error) {
	if existing, found, err := p.plane.device.FindEpisodeMemoryByLesson(ctx, episode.ID, candidate.LessonKey); err != nil {
		return "", err
	} else if found {
		return existing.ID, nil
	}
	deviceID := firstNonEmptyString([]string{candidate.Scope["device_id"], episode.DeviceScope["device_id"], defaultMemoryDeviceID})
	priority, ttl := episodeMemoryDefaults(candidate.Type)
	confidence, err := normalizeEpisodeMemoryConfidence(candidate.Confidence)
	if err != nil {
		return "", err
	}
	item := DeviceMemoryItem{
		ID:               "devmem_" + stableMemoryID(episode.ID, candidate.LessonKey),
		Type:             string(candidate.Type),
		Status:           deviceMemoryStatusActive,
		Revision:         1,
		ExtractorVersion: episodeMemoryExtractorVersion,
		LessonKey:        candidate.LessonKey,
		Title:            truncateForLog(candidate.Situation, 120),
		Summary:          truncateForLog(candidate.Guidance, 240),
		Content:          renderEpisodeMemoryContent(candidate),
		DeviceID:         deviceID,
		AppName:          candidate.Scope["app_name"],
		PageName:         candidate.Scope["page_name"],
		Tags:             candidate.Tags,
		Entities:         redactEpisodeMemorySensitiveStrings(episode.Entities, candidate.SensitiveValues),
		Confidence:       confidence,
		Priority:         priority,
		TTL:              ttl,
		Applicability:    cloneStringMap(candidate.Scope),
		EvidenceRefs:     []MemorySourceRef{episodeMemoryEvidenceRef(episode, candidate.EvidenceRefs)},
	}
	if candidate.Type == episodeMemoryTypeProcedure {
		item.Steps = episodeMemoryProcedureSteps(episode, candidate.EvidenceRefs, candidate.SensitiveValues)
	}
	result, err := p.plane.device.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &item, Action: MemoryIntentActionCreate})
	return result.ID, err
}

func equalEpisodeMemoryScope(left, right map[string]string) bool {
	left = normalizeEpisodeMemoryScope(left)
	right = normalizeEpisodeMemoryScope(right)
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if !strings.EqualFold(value, right[key]) {
			return false
		}
	}
	return true
}

func (p *episodeMemoryProcessor) updateMemory(ctx context.Context, episode TaskEpisode, candidate episodeMemoryCandidate) error {
	existing, found, err := p.plane.device.Get(ctx, candidate.MemoryID)
	if err != nil {
		return err
	}
	if !found || existing.Type != string(candidate.Type) {
		return nil
	}
	if effectiveDeviceMemoryRevision(existing) != candidate.MemoryRevision {
		return fmt.Errorf("%w: device memory %s changed from revision %d", errEpisodeMemoryRevisionChanged, candidate.MemoryID, candidate.MemoryRevision)
	}
	deviceID := firstNonEmptyString([]string{candidate.Scope["device_id"], episode.DeviceScope["device_id"], defaultMemoryDeviceID})
	if existing.DeviceID != "" && deviceID != "" && !strings.EqualFold(existing.DeviceID, deviceID) {
		return nil
	}
	priority, _ := episodeMemoryDefaults(candidate.Type)
	confidence, err := normalizeEpisodeMemoryConfidence(candidate.Confidence)
	if err != nil {
		return err
	}
	newStatus := deviceMemoryStatusActive
	if candidate.UnresolvedConflict {
		newStatus = deviceMemoryStatusDisputed
	}
	newTitle := truncateForLog(candidate.Situation, 120)
	newSummary := truncateForLog(candidate.Guidance, 240)
	newContent := renderEpisodeMemoryContent(candidate)
	newScope := cloneStringMap(candidate.Scope)
	var newSteps []ProcedureStep
	if candidate.Type == episodeMemoryTypeProcedure {
		newSteps = mergeEpisodeMemorySteps(existing.Steps, episodeMemoryProcedureSteps(episode, candidate.EvidenceRefs, candidate.SensitiveValues))
	}
	item := DeviceMemoryItem{
		ID: candidate.MemoryID, Type: string(candidate.Type), Status: newStatus,
		Revision: candidate.MemoryRevision, ExtractorVersion: episodeMemoryExtractorVersion,
		Title: newTitle, Summary: newSummary, Content: newContent, DeviceID: deviceID,
		AppName: candidate.Scope["app_name"], PageName: candidate.Scope["page_name"],
		Tags: normalizeEpisodeMemoryTags(candidate.Tags), Applicability: newScope,
		Priority: priority, Confidence: confidence,
		EvidenceRefs: []MemorySourceRef{episodeMemoryEvidenceRef(episode, candidate.EvidenceRefs)},
		Steps:        newSteps,
	}
	if candidate.UnresolvedConflict {
		item.ConflictsWith = []string{episode.ID}
	}
	_, err = p.plane.device.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &item, Action: MemoryIntentActionUpdate, ExpectedRevision: candidate.MemoryRevision})
	return err
}

func equalEpisodeMemorySteps(left, right []ProcedureStep) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func mergeEpisodeMemorySteps(existing, observed []ProcedureStep) []ProcedureStep {
	merged := append([]ProcedureStep(nil), existing...)
	for _, step := range observed {
		duplicate := false
		for _, current := range merged {
			if current == step {
				duplicate = true
				break
			}
		}
		if !duplicate {
			merged = append(merged, step)
		}
	}
	return merged
}

func effectiveDeviceMemoryRevision(item DeviceMemoryItem) int {
	if item.Revision > 0 {
		return item.Revision
	}
	return 1
}

func episodeMemorySearchTerms(episode TaskEpisode) []string {
	terms := []string{episode.UserGoal}
	terms = append(terms, episode.Tags...)
	terms = append(terms, episode.Entities...)
	for _, event := range episode.Events {
		if event.Type == runEventToolCall {
			terms = append(terms, event.ToolName)
		}
		if event.Type == "tool_result" || event.Type == "steer" {
			terms = append(terms, truncateForLog(firstNonEmptyString([]string{event.Observation, event.Content, event.Reason}), 240))
		}
	}
	return uniqueNonEmpty(terms)
}

func episodeMemoryRetrievalScope(episode TaskEpisode) map[string]string {
	scope := normalizeEpisodeMemoryScope(episode.DeviceScope)
	if scope == nil {
		scope = map[string]string{}
	}
	if _, ok := scope["device_id"]; !ok {
		scope["device_id"] = defaultMemoryDeviceID
	}
	for key, value := range episode.NormalizedGoal {
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "app_name" && key != "page_name" && key != "goal_pattern" {
			continue
		}
		if value = strings.TrimSpace(value); value != "" {
			scope[key] = value
		}
	}
	return scope
}

func episodeMemoryDefaults(memoryType episodeMemoryType) (priority int, ttl string) {
	switch memoryType {
	case episodeMemoryTypeFailure:
		return 80, "60d"
	case episodeMemoryTypeProcedure:
		return 70, "45d"
	case episodeMemoryTypeCalibration:
		return 75, "30d"
	case episodeMemoryTypeNavigation:
		return 65, "30d"
	default:
		return 60, "45d"
	}
}

func normalizeEpisodeMemoryConfidence(confidence *float64) (float64, error) {
	if confidence == nil {
		return episodeMemoryDefaultConfidence, nil
	}
	value := *confidence
	if value <= 0 || value > 1 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("episode memory confidence must be greater than 0 and at most 1")
	}
	return value, nil
}

func episodeMemoryConfidencePointer(confidence float64) *float64 {
	return &confidence
}

func renderEpisodeMemoryContent(candidate episodeMemoryCandidate) string {
	parts := []string{
		"Situation: " + candidate.Situation,
		"Guidance: " + candidate.Guidance,
		"Expected effect: " + candidate.ExpectedEffect,
	}
	if candidate.ConflictReason != "" {
		parts = append(parts, "Conflict: "+candidate.ConflictReason)
	}
	return strings.Join(parts, "\n")
}

func episodeMemoryProcedureSteps(episode TaskEpisode, refs, sensitiveValues []string) []ProcedureStep {
	allowed := make(map[string]bool, len(refs))
	for _, ref := range refs {
		allowed[ref] = true
	}
	var steps []ProcedureStep
	for index, event := range episode.Events {
		if event.Type != runEventToolCall || !allowed[event.EventID] || !isEpisodeMemoryDeviceTool(event.ToolName) {
			continue
		}
		step := ProcedureStep{
			Tool:        event.ToolName,
			Description: redactEpisodeMemorySensitiveValues(truncateForLog(event.Content, 160), sensitiveValues),
			Coords:      extractToolCallCoords(event.ToolInput),
			Text:        redactEpisodeMemorySensitiveValues(extractToolCallText(event.ToolInput), sensitiveValues),
		}
		for nextIndex := index + 1; nextIndex < len(episode.Events); nextIndex++ {
			next := episode.Events[nextIndex]
			if next.Type == runEventToolCall {
				break
			}
			if next.Type == "tool_result" && next.ToolName == event.ToolName && allowed[next.EventID] {
				step.OutcomeNote = redactEpisodeMemorySensitiveValues(truncateForLog(firstNonEmptyString([]string{next.Observation, next.Content}), 240), sensitiveValues)
				break
			}
		}
		steps = append(steps, step)
	}
	return steps
}

func redactEpisodeMemorySensitiveStrings(values, sensitiveValues []string) []string {
	redacted := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(redactEpisodeMemorySensitiveValues(value, sensitiveValues))
		if value != "" {
			redacted = appendUniqueString(redacted, value)
		}
	}
	return redacted
}

func redactEpisodeMemorySensitiveValues(value string, sensitiveValues []string) string {
	for _, sensitive := range uniqueNonEmpty(sensitiveValues) {
		value = strings.ReplaceAll(value, sensitive, "[session-bound value omitted]")
	}
	return value
}

func invalidEpisodeMemoryReason(episode TaskEpisode) string {
	if strings.TrimSpace(episode.EndedAt) == "" || episode.Status == "running" {
		return "episode is not completed"
	}
	if strings.TrimSpace(episode.UserGoal) == "" {
		return "missing user goal"
	}
	if hasDeviceToolCallAndResult(episode.Events) || hasNonCanceledStructuredError(episode.Events) {
		return ""
	}
	return "missing device action or non-canceled structured error"
}

func hasDeviceToolCallAndResult(events []TaskEpisodeEvent) bool {
	pendingCalls := make(map[string]int)
	for _, event := range events {
		name := strings.ToLower(strings.TrimSpace(event.ToolName))
		if !isEpisodeMemoryDeviceTool(name) {
			continue
		}
		switch event.Type {
		case runEventToolCall:
			pendingCalls[name]++
		case "tool_result":
			if pendingCalls[name] > 0 {
				return true
			}
		}
	}
	return false
}

func hasNonCanceledStructuredError(events []TaskEpisodeEvent) bool {
	for _, event := range events {
		if event.ToolError != nil && event.ToolError.Code != CodeCanceled {
			return true
		}
		if event.IsError && (event.ToolError == nil || event.ToolError.Code != CodeCanceled) {
			return true
		}
	}
	return false
}

func isEpisodeMemoryDeviceTool(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	if metadata, ok := builtInToolSpecMetadata[name]; ok {
		switch metadata.Category {
		case "memory", "skills", "web", "handoff":
			return false
		case "system":
			return name == "shell"
		}
	}
	return name != toolWaitForWakeup
}

func (s *TaskEpisodeStore) listCompletedEpisodesSince(ctx context.Context, since time.Time, include func(episodeIndexEntry, time.Time) bool) ([]TaskEpisode, error) {
	if s == nil || s.rootDir == "" {
		return nil, nil
	}
	index, err := s.loadIndex()
	if err != nil {
		return nil, err
	}
	type datedEntry struct {
		entry episodeIndexEntry
		ended time.Time
	}
	var matches []datedEntry
	for _, entry := range index.Episodes {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if entry.Status == "running" || strings.TrimSpace(entry.EndedAt) == "" {
			continue
		}
		endedAt, err := time.Parse(time.RFC3339Nano, entry.EndedAt)
		if err != nil || endedAt.Before(since) {
			continue
		}
		if include != nil && !include(entry, endedAt) {
			continue
		}
		matches = append(matches, datedEntry{entry: entry, ended: endedAt})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].ended.Equal(matches[j].ended) {
			return matches[i].entry.ID < matches[j].entry.ID
		}
		return matches[i].ended.Before(matches[j].ended)
	})
	episodes := make([]TaskEpisode, 0, len(matches))
	for _, match := range matches {
		episode, found, err := s.getFromIndex(index, match.entry.ID)
		if err != nil {
			return nil, err
		}
		if found {
			episodes = append(episodes, episode)
		}
	}
	return episodes, nil
}

func buildEpisodeMemoryEvidencePrompt(payload string, existing []DeviceMemoryItem) string {
	type memoryView struct {
		ID            string            `json:"id"`
		Type          string            `json:"type"`
		Status        string            `json:"status"`
		Revision      int               `json:"revision"`
		Title         string            `json:"title,omitempty"`
		Summary       string            `json:"summary,omitempty"`
		Content       string            `json:"content,omitempty"`
		Applicability map[string]string `json:"scope,omitempty"`
		Tags          []string          `json:"tags,omitempty"`
	}
	views := make([]memoryView, 0, len(existing))
	for _, item := range existing {
		views = append(views, memoryView{
			ID: item.ID, Type: item.Type, Status: string(item.Status), Revision: effectiveDeviceMemoryRevision(item),
			Title: item.Title, Summary: item.Summary, Content: item.Content,
			Applicability: cloneStringMap(item.Applicability), Tags: append([]string(nil), item.Tags...),
		})
	}
	memoryJSON, _ := json.MarshalIndent(views, "", "  ")
	return `Existing related Device Memories (maximum 8, including disputed records):
` + string(memoryJSON) + `

Episode:
` + payload
}

// episodeMemoryBuiltInGuidanceRule keeps guidance the Agent already receives
// out of Device Memory. The extraction prompt, the omission review, and the
// retention audit all apply it. The extractor sees tool results but not tool
// definitions, so the rule is judged against the results in the Episode.
// It separates the remediation advice a tool returns, which the Agent gets
// again on the next failure, from the device, app, and page facts that tool
// results observe, which are the evidence Device Memory is built from.
const episodeMemoryBuiltInGuidanceRule = `The Agent already receives every tool result, including the suggestion a failed tool returns, so a memory that only repeats that suggestion adds nothing. None of the following is device-, app-, or page-specific knowledge: guidance that only restates the remediation advice a tool returned in this Episode; generic tool use such as taking a screenshot after a failure, refocusing a field, or retrying; and either of those with an app, page, or control name attached, for example "in Safari, tap the address bar and retry enter_text" after enter_text already said to refocus the field and retry. Facts that tool results observed about this device, app, or page are evidence, not built-in guidance: a verified sequence of steps, a route, a required field or precondition, a page layout, a coordinate, or a stop condition is specific knowledge even though a tool result reported it. An observed application precondition is reusable even if it seems obvious: when Send fails because the recipient is empty, retaining a guard to fill the recipient before sending records an app constraint, not generic tool retry advice. Do not discard that guard solely because the tool reported the error. Evidence alone does not make a fact durable: a current IP address, token, timestamp, or other changing value seen once is transient, even on a named app or page. Merely completing a read-only lookup of such a value does not create a reusable lesson. A workaround for a tool that misbehaved is a defect to fix in the tool; it counts only when it also records such a device, app, or page fact beyond the tool's own advice.`

const episodeMemoryProposalInstructions = `For each Episode, return a proposal object matching this schema:
{
  "episode_assessment": {
    "goal_result": "achieved | not_achieved | unknown",
    "reason": "evidence-based explanation",
    "evidence_refs": ["real Episode event ids"]
  },
  "candidates": [{
    "lesson_key": "unique stable key within this Episode",
    "type": "procedure | navigation | calibration | failure | fact",
    "action": "create | update",
    "retention": "durable | transient | sensitive",
    "memory_id": "required only for update",
    "unresolved_conflict": false,
	    "conflict_reason": "required only when unresolved_conflict is true",
	    "situation": "when this lesson applies",
	    "guidance": "what the future Agent should do or consider",
	    "expected_effect": "directly observable expected result",
	    "confidence": 0.85,
	    "scope": {"device_id":"...", "app_name":"...", "app_version":"...", "page_name":"...", "goal_pattern":"...", "precondition":"..."},
    "tags": ["short retrieval terms"],
    "evidence_refs": ["real Episode event ids"]
  }]
}

Return at most 3 independent candidates; an empty candidates array is correct when nothing is worth retaining. Every candidate must declare a retention class and a confidence greater than 0 and at most 1 that reflects the semantic certainty of that specific conclusion from its evidence. Confidence is conclusion-specific; do not infer it from the memory type. Use durable only for reusable knowledge that remains safe and useful within the evidenced future scope. Use transient for Episode/session/runtime-bound observations and sensitive for secrets or values that must not be persisted. One-time verification codes, passwords, credentials, tokens, and other session-only values must never be durable; generalize a reusable workflow without the value. Only durable candidates are eligible for Device Memory and every non-empty proposal is independently audited before persistence. Every durable candidate must be reusable in future similar tasks, change future behavior or decisions, have explicit scope, add new knowledge or evidence, and be safer to recall than to omit. Do not retain greetings, task-specific prose, temporary values, OTPs, transient page contents, or information already explicitly saved through a Memory-management tool.
Device Memory holds what the Agent cannot know without this device: routes, coordinates, page layouts, workarounds, stop conditions, and stable facts specific to this device, app, or page. ` + episodeMemoryBuiltInGuidanceRule + ` Do not emit a candidate for anything that is not such knowledge. When direct evidence verifies such device-, app-, or page-specific knowledge and it satisfies the type rules, you must emit at least one candidate. Do not return an empty candidates array merely because the Episode achieved its goal.

Assess goal_result independently from the recorded success flag. achieved and not_achieved require direct result, final-state, screenshot, or user-correction evidence. Use unknown when final proof is missing and say what is missing. User steer is correction evidence, not an admission gate. Do not rely on verifier_decision or ObservedState; they are not part of this pipeline.

Type rules:
- procedure: reusable multi-step goal path supported by tool calls and results. Preserve the observed sequence, including intermediate transitions and verification, when that sequence is the evidence of success. Do not replace it with a fact claiming the final action alone works unless that shorter path was independently verified. If goal_result is not_achieved, only emit an independently proven partial procedure and set scope.partial="true".
- navigation: independently reusable page entry or transition, supported by an action and comparable before/after observations. Keep a navigation step inside a procedure when it has no independent recall value.
- calibration: coordinate, screen, control, or coordinate-space relationship supported by coordinate input and post-action evidence.
- failure: reusable guard, check, stop condition, or recovery action. A failed Episode is not automatically a failure memory; omit one-off failures with no actionable future guard.
- fact: stable device, app, or page fact directly observed by a tool. Do not turn guesses, one-time values, or a multi-step procedure into facts.

Deduplication and conflict rules:
- Use action=create only when no existing memory has the same scoped lesson.
- For action=create, omit memory_id and memory_revision.
- Use action=update with an existing memory_id and its exact memory_revision when the lesson overlaps. Output the complete merged situation, guidance, expected_effect, scope, and tags; preserve valid older conditions rather than merely copying the new Episode.
- device_profile and app_profile entries are deterministic context only. Do not update them or duplicate facts already represented by them.
- Resolve apparent conflicts by conditioning the merged memory on version, page, account state, or another evidenced precondition when possible.
- Set unresolved_conflict=true only when the same scope still has incompatible conclusions and no safe condition can distinguish them. The memory will be quarantined as disputed.
- An achieved Episode is not automatically a procedure, and an Episode-level failure is not automatically a failure memory.

Scope rules: treat explicit fields in the Episode's device_scope as hard applicability boundaries. Preserve device_id, app_name, app_version, page_name, and any other identity/version/account boundary in scope when supplied. If device_scope contains app_version, put it in scope.app_version; do not encode it only inside precondition. Do not invent a boundary that is not evidenced. Evidence rules: cite only real event ids. Prefer tool results, structured errors, attached screenshots, final visible state, and user correction over Agent commentary. A cited tool result is deterministically linked to its paired tool call before type validation. Screenshots support only what is visibly shown. Preserve uncertainty and never invent causal ownership, UI state, app/page names, or unsupported recovery tools.`
