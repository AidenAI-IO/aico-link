package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"aico-agent/internal/agent/model"
	"aico-agent/internal/ble"
	"aico-agent/internal/logging"
	"github.com/tmc/langchaingo/llms"
)

const (
	notificationMemoryBatchLimit  = 10
	notificationBatchMaxTokens    = 8000
	notificationJSONMaxAttempts   = 2
	notificationMemoryTTL         = 7 * 24 * time.Hour
	notificationLongTermTTL       = 90 * 24 * time.Hour
	notificationDefaultConfidence = 0.7
)

// NotificationMemoryProcessor is the notification-specific input and policy
// adapter for a MemoryWorker. It chooses the notification batch, filters
// structural no-ops, prepares model input, validates the proposal, and
// applies notification-specific memory changes. Without a model, raw context
// still advances but no semantic memory is created.
type NotificationMemoryProcessor struct {
	context   *NotificationContext
	temporary *LongTermMemoryStore
	longTerm  *LongTermMemoryStore
	merge     *MemoryMergeEngine
	logger    *Logger
	now       func() time.Time
}

var _ MemoryProcessor = (*NotificationMemoryProcessor)(nil)

func NewNotificationMemoryProcessor(notificationContext *NotificationContext, memoryDir string, longTermStore *LongTermMemoryStore, models model.Model) *NotificationMemoryProcessor {
	if longTermStore == nil {
		longTermStore = NewLongTermMemoryStore(filepath.Join(memoryDir, "long_term"))
	}
	var merge *MemoryMergeEngine
	if models != nil {
		merge = NewMemoryMergeEngine(models)
	}
	return &NotificationMemoryProcessor{
		context:   notificationContext,
		temporary: NewLongTermMemoryStore(filepath.Join(memoryDir, "temporary")),
		longTerm:  longTermStore,
		merge:     merge,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

func (p *NotificationMemoryProcessor) Initialize() error {
	return nil
}

func (p *NotificationMemoryProcessor) NextRunAt(ctx context.Context) (time.Time, error) {
	if p == nil || p.context == nil {
		return time.Time{}, nil
	}
	return p.now(), nil
}

func (p *NotificationMemoryProcessor) ProcessBatch(ctx context.Context, shouldStop func() bool) (MemoryBatchResult, error) {
	if p == nil || p.context == nil {
		return MemoryBatchResult{}, nil
	}
	limit := notificationMemoryBatchLimit
	pending, err := p.context.ReadPending(ctx, limit)
	if err != nil {
		return MemoryBatchResult{}, err
	}
	consumed, consumeErr := p.context.Consume(ctx, limit)
	if consumeErr == nil {
		pending, err = p.context.ReadPending(ctx, limit)
		if err != nil {
			return MemoryBatchResult{}, err
		}
	} else if len(pending) == 0 {
		// There is no durable work to process while BLE is unavailable.
		// Keep the worker alive, but surface the error for retry/backoff.
		return MemoryBatchResult{HasPending: true}, consumeErr
	}
	if len(pending) == 0 {
		return MemoryBatchResult{}, nil
	}
	if p.merge == nil {
		// Keep raw context available for a later run with a configured model.
		// Source ingestion can still drain in bounded pages, but semantic memory
		// is not guessed and MemoryCursor is not advanced.
		if consumeErr != nil {
			return MemoryBatchResult{HasPending: true}, consumeErr
		}
		return MemoryBatchResult{HasPending: len(consumed) >= limit}, nil
	}
	processed := append([]NotificationRecord(nil), pending...)
	stopped := false
	records := coalesceNotificationRecords(pending)
	batch := make([]NotificationRecord, 0, len(records))
	removed := make([]NotificationRecord, 0)
	for _, record := range records {
		if shouldStop != nil && shouldStop() {
			stopped = true
			break
		}
		if err := ctx.Err(); err != nil {
			return MemoryBatchResult{HasPending: true}, err
		}
		if strings.EqualFold(strings.TrimSpace(record.NotificationEvent.Event), "removed") {
			removed = append(removed, record)
		} else if !notificationMemoryIgnored(record.NotificationEvent) {
			batch = append(batch, record)
		}
	}
	if stopped {
		return MemoryBatchResult{HasPending: true}, nil
	}
	if err := p.resolveRecords(ctx, batch); err != nil {
		return MemoryBatchResult{HasPending: true}, err
	}
	for _, record := range removed {
		if err := p.removeTemporary(ctx, record); err != nil {
			return MemoryBatchResult{HasPending: true}, err
		}
	}
	if len(processed) > 0 {
		if err := p.context.CommitProcessed(ctx, processed); err != nil {
			return MemoryBatchResult{HasPending: true}, err
		}
	}
	remaining, err := p.context.ReadPending(ctx, 1)
	if err != nil {
		return MemoryBatchResult{HasPending: true}, err
	}
	if consumeErr != nil {
		return MemoryBatchResult{HasPending: true}, consumeErr
	}
	return MemoryBatchResult{HasPending: len(remaining) > 0 || len(consumed) >= limit}, nil
}

type notificationMemoryProposal struct {
	Actions []notificationMemoryAction `json:"actions"`
}

type notificationProposalError struct {
	err error
}

func (e *notificationProposalError) Error() string {
	if e == nil || e.err == nil {
		return "invalid notification memory proposal"
	}
	return e.err.Error()
}

func (e *notificationProposalError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func wrapNotificationProposalError(err error) error {
	if err == nil {
		return nil
	}
	var proposalErr *notificationProposalError
	if errors.As(err, &proposalErr) {
		return err
	}
	return &notificationProposalError{err: err}
}

type notificationMemoryAction struct {
	Action         string   `json:"action"`
	Scope          string   `json:"scope"`
	MemoryID       string   `json:"memory_id,omitempty"`
	MemoryRevision int      `json:"memory_revision,omitempty"`
	Type           string   `json:"type,omitempty"`
	Title          string   `json:"title,omitempty"`
	Content        string   `json:"content,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Entities       []string `json:"entities,omitempty"`
	Confidence     float64  `json:"confidence,omitempty"`
	ExpiresAt      string   `json:"expires_at,omitempty"`
	SourceEventIDs []string `json:"source_event_ids,omitempty"`
	confidenceSet  bool
}

func (a *notificationMemoryAction) UnmarshalJSON(data []byte) error {
	type actionAlias notificationMemoryAction
	var decoded actionAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*a = notificationMemoryAction(decoded)
	_, a.confidenceSet = fields["confidence"]
	return nil
}

type notificationMemoryBatchResult struct {
	ContextID string                     `json:"context_id"`
	Proposal  notificationMemoryProposal `json:"proposal"`
}

type notificationMemoryBatchResponse struct {
	Results []notificationMemoryBatchResult `json:"results"`
}

type validatedNotificationResult struct {
	record   NotificationRecord
	proposal notificationMemoryProposal
	refs     []MemoryMergeReference
}

func (p *NotificationMemoryProcessor) resolveRecords(ctx context.Context, records []NotificationRecord) error {
	if len(records) == 0 {
		return nil
	}
	if p.merge == nil {
		return nil
	}
	var response notificationMemoryBatchResponse
	refsByContext := make(map[string][]MemoryMergeReference, len(records))
	var correction string
	for attempt := 1; attempt <= notificationJSONMaxAttempts; attempt++ {
		_, raw, err := p.merge.Extract(ctx, MemoryMergeRequest{
			Search: func(ctx context.Context) ([]MemoryMergeReference, error) {
				all := make([]MemoryMergeReference, 0, len(records)*8)
				for _, record := range records {
					related, err := p.searchRelatedNotificationMemories(ctx, record)
					if err != nil {
						return nil, err
					}
					refsByContext[record.ContextID] = related
					all = append(all, related...)
				}
				return all, nil
			},
			BuildMessages: func(_ []MemoryMergeReference) ([]llms.MessageContent, error) {
				prompt := buildNotificationMemoryBatchPrompt(records, refsByContext)
				if correction != "" {
					prompt += "\n\nThe previous response was invalid JSON (" + correction + "). Return exactly one complete JSON object matching the requested schema, with no prose or markdown."
				}
				return []llms.MessageContent{
					llms.TextParts(llms.ChatMessageTypeSystem, "You consolidate a batch of notifications into user memory. Output JSON only."),
					llms.TextParts(llms.ChatMessageTypeHuman, prompt),
				}, nil
			},
			MaxTokens: min(1400*len(records), notificationBatchMaxTokens),
			Timeout:   45 * time.Second,
		})
		if err != nil {
			return err
		}
		response, err = parseNotificationMemoryBatchResponse(raw, records)
		if err == nil {
			break
		}
		p.logNotificationMemoryResponseFailure("batch proposal parse failed", raw, err, attempt)
		if attempt == notificationJSONMaxAttempts {
			return wrapNotificationProposalError(err)
		}
		correction = err.Error()
	}
	if len(response.Results) != len(records) {
		return wrapNotificationProposalError(fmt.Errorf("notification memory batch returned %d results for %d notifications", len(response.Results), len(records)))
	}
	recordsByContext := make(map[string]NotificationRecord, len(records))
	for _, record := range records {
		recordsByContext[record.ContextID] = record
	}
	seen := make(map[string]bool, len(response.Results))
	proposalsByContext := make(map[string]validatedNotificationResult, len(response.Results))
	for _, result := range response.Results {
		record, ok := recordsByContext[strings.TrimSpace(result.ContextID)]
		if !ok {
			return wrapNotificationProposalError(fmt.Errorf("notification memory batch returned unknown context_id %q", result.ContextID))
		}
		if seen[record.ContextID] {
			return wrapNotificationProposalError(fmt.Errorf("notification memory batch returned duplicate context_id %q", record.ContextID))
		}
		seen[record.ContextID] = true
		proposal, err := normalizeNotificationProposalShape(result.Proposal)
		if err != nil {
			p.logInvalidNotificationProposal(record, err)
			continue
		}
		proposalsByContext[record.ContextID] = validatedNotificationResult{record: record, proposal: proposal, refs: refsByContext[record.ContextID]}
	}
	ordered := make([]validatedNotificationResult, 0, len(records))
	for _, record := range records {
		if result, ok := proposalsByContext[record.ContextID]; ok {
			ordered = append(ordered, result)
		}
	}
	ordered = coalesceNotificationBatchTargets(ordered)
	validated := make([]validatedNotificationResult, 0, len(ordered))
	for _, result := range ordered {
		proposal, err := p.validateNotificationProposal(ctx, result.record, result.proposal, result.refs)
		if err != nil {
			p.logInvalidNotificationProposal(result.record, err)
			continue
		}
		result.proposal = proposal
		validated = append(validated, result)
	}
	for _, result := range validated {
		if err := p.applyNotificationProposal(ctx, result.record, result.proposal, result.refs); err != nil {
			return err
		}
	}
	return nil
}

func parseNotificationMemoryBatchResponse(raw string, records []NotificationRecord) (notificationMemoryBatchResponse, error) {
	var response notificationMemoryBatchResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return notificationMemoryBatchResponse{}, fmt.Errorf("parse notification memory batch proposal: %w", err)
	}
	if len(response.Results) == 0 && len(records) == 1 {
		var legacy notificationMemoryProposal
		if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
			return notificationMemoryBatchResponse{}, fmt.Errorf("parse notification memory proposal: %w", err)
		}
		response.Results = []notificationMemoryBatchResult{{ContextID: records[0].ContextID, Proposal: legacy}}
	}
	return response, nil
}

// Model output can repeat private notification content, so diagnostics retain
// only enough metadata to correlate malformed responses without logging them.
func (p *NotificationMemoryProcessor) logNotificationMemoryResponseFailure(reason, raw string, err error, attempt int) {
	var syntaxErr *json.SyntaxError
	var offset int64
	if errors.As(err, &syntaxErr) {
		offset = syntaxErr.Offset
	}
	digest := sha256.Sum256([]byte(raw))
	if p != nil && p.logger != nil {
		p.logger.Warn("[notification-memory] %s: attempt=%d/%d response_bytes=%d response_sha256=%x json_offset=%d",
			reason, attempt, notificationJSONMaxAttempts, len(raw), digest, offset)
		return
	}
	logging.Warnf("agent", "notification_memory", "%s: attempt=%d/%d response_bytes=%d response_sha256=%x json_offset=%d",
		reason, attempt, notificationJSONMaxAttempts, len(raw), digest, offset)
}

func coalesceNotificationBatchTargets(results []validatedNotificationResult) []validatedNotificationResult {
	type targetOwner struct {
		resultIndex int
		actionIndex int
	}
	lastTargetOwner := make(map[string]targetOwner)
	for resultIndex, result := range results {
		for actionIndex, action := range result.proposal.Actions {
			if key, ok := notificationBatchTargetKey(action); ok {
				lastTargetOwner[key] = targetOwner{resultIndex: resultIndex, actionIndex: actionIndex}
			}
		}
	}
	coalesced := make([]validatedNotificationResult, len(results))
	copy(coalesced, results)
	for resultIndex := range coalesced {
		actions := coalesced[resultIndex].proposal.Actions
		kept := make([]notificationMemoryAction, 0, len(actions))
		for actionIndex, action := range actions {
			key, targeted := notificationBatchTargetKey(action)
			if targeted && lastTargetOwner[key] != (targetOwner{resultIndex: resultIndex, actionIndex: actionIndex}) {
				continue
			}
			kept = append(kept, action)
		}
		coalesced[resultIndex].proposal.Actions = kept
	}
	return coalesced
}

func notificationBatchTargetKey(action notificationMemoryAction) (string, bool) {
	targetScope := strings.ToLower(strings.TrimSpace(action.Scope))
	switch strings.ToLower(strings.TrimSpace(action.Action)) {
	case "update", "reinforce", "remove":
	case "promote":
		targetScope = "temporary"
	default:
		return "", false
	}
	return targetScope + ":" + strings.TrimSpace(action.MemoryID), true
}

func buildNotificationMemoryBatchPrompt(records []NotificationRecord, refsByContext map[string][]MemoryMergeReference) string {
	type promptNotification struct {
		Notification     NotificationRecord `json:"notification"`
		RelatedMemoryIDs []string           `json:"related_memory_ids,omitempty"`
	}
	items := make([]promptNotification, 0, len(records))
	catalog := make([]MemoryMergeReference, 0)
	catalogIndexes := make(map[string]bool)
	for _, record := range records {
		item := promptNotification{Notification: record}
		for _, ref := range refsByContext[record.ContextID] {
			key := ref.Scope + ":" + ref.ID
			item.RelatedMemoryIDs = append(item.RelatedMemoryIDs, key)
			if !catalogIndexes[key] {
				catalogIndexes[key] = true
				catalog = append(catalog, ref)
			}
		}
		items = append(items, item)
	}
	payload, _ := json.Marshal(map[string]any{"notifications": items, "memory_catalog": catalog})
	return "Process each notification independently. Return exactly one JSON object with schema " +
		`{"results":[{"context_id":"exact context_id","proposal":{"actions":[...]}}]}. ` +
		"CRITICAL: Return one result for every notification and EXACTLY ONE action per result (the actions array must have length 0 or 1). Multiple actions per notification are not allowed. Do not combine evidence across notifications. " +
		"A memory_catalog target may be modified by at most one result; when multiple notifications relate to it, only the latest notification may target it and earlier results must ignore it. " +
		"Use only the supplied notification evidence. action must be ignore, add, update, reinforce, remove, or promote; scope must be temporary or long_term. " +
		"Choose actions by these semantics: add creates a new conclusion with no target memory_id; update replaces a changed conclusion at the exact catalog memory_id and memory_revision; reinforce keeps the same conclusion while recording new evidence at the exact catalog memory_id and memory_revision; remove withdraws the exact catalog memory_id and memory_revision; promote copies the exact temporary catalog memory_id and memory_revision into scope long_term when the notification establishes a stable rule or preference that should outlive temporary expiry. Use temporary for one-off or time-bounded user facts such as bills, repayment amounts, due dates, deliveries, appointments, and prices. Use long_term only for a stable user-specific preference, rule, or profile that should remain useful after this notification expires; public information and one-off noise are never long_term user memory. Do not use reinforce for an explicit stable rule or preference that should be promoted. " +
		"For add and update, content is required; for add also provide exactly one valid type: fact, preference, rule, or profile. For each conclusion provide confidence as a number greater than 0 and at most 1, based on how certain the notification evidence makes that specific conclusion; omit it only when certainty cannot be estimated. Priority is assigned by code and must not be returned. For reinforce, remove, and promote, preserve the exact target identity and revision even when content is omitted. " +
		"CRITICAL CLASSIFICATION: First classify each notification as user_fact, public_info, marketing, secret, or one_off_noise. Only user_fact is eligible for memory. " +
		"PUBLIC_INFO (must ignore): news apps, media apps, content aggregators, and information services deliver public_info, not user_fact. Public_info includes product or vehicle price announcements, company and industry news, product reviews and comparisons, market analysis, celebrity items, sports scores, weather forecasts, quoted market prices, and general knowledge. Even when the content is factually accurate or likely interesting to the user, if it describes the world rather than this user's own situation it is public_info and must be ignored. " +
		"USER_FACT (eligible for memory): classify as user_fact only when the notification describes this user's own state, commitment, or interaction, typically addressed to the user directly or with a possessive reference to their account, order, or device. Examples: their own order or reservation confirmation, their own bill or repayment amount and due date, their own delivery or shipment status, their own appointment or meeting time, their own account activity or balance, an alert from a device they own, or a preference or rule they have stated. " +
		"Treat general news, product facts, or public prices as public_info and ignore them even when factually true. " +
		"Test: Ask 'Is this about MY situation or about THE WORLD?' If it is general information, a product announcement, or news content that could apply to anyone, classify as public_info and ignore. Do not create memory for content the user is passively receiving unless it directly relates to their own commitments, deadlines, or personal state. " +
		"Ignore OTPs, marketing promotions, secrets, and one_off noise. Do not reject a user_fact merely because it comes from banking, health, legal, or messaging apps. For temporary conclusions use an expiry. " +
		"Every targeted action must include the exact memory_id and memory_revision from memory_catalog.\n\n" + string(payload)
}

func (p *NotificationMemoryProcessor) searchRelatedNotificationMemories(ctx context.Context, record NotificationRecord) ([]MemoryMergeReference, error) {
	event := record.NotificationEvent
	query := MemoryQuery{Tags: notificationMemoryTags(event), Entities: compactNotificationEntities(event), Limit: 4}
	temporaryRefs := make([]MemoryMergeReference, 0, 4)
	longTermRefs := make([]MemoryMergeReference, 0, 4)
	if p.temporary != nil {
		results, err := p.temporary.Search(ctx, query)
		if err != nil {
			return nil, err
		}
		for _, result := range results {
			if !result.Sensitive {
				temporaryRefs = append(temporaryRefs, memoryResultMergeReference("temporary", result))
			}
		}
	}
	if p.longTerm != nil {
		results, err := p.longTerm.Search(ctx, query)
		if err != nil {
			return nil, err
		}
		for _, result := range results {
			if !result.Sensitive {
				longTermRefs = append(longTermRefs, memoryResultMergeReference("long_term", result))
			}
		}
	}
	refs := make([]MemoryMergeReference, 0, 8)
	for index := 0; index < 4; index++ {
		if index < len(temporaryRefs) {
			refs = append(refs, temporaryRefs[index])
		}
		if index < len(longTermRefs) {
			refs = append(refs, longTermRefs[index])
		}
	}
	return refs, nil
}

func memoryResultMergeReference(scope string, result MemoryResult) MemoryMergeReference {
	return MemoryMergeReference{Scope: scope, ID: result.ID, Type: result.Type, Status: result.Status, Title: result.Title, Summary: result.Summary, Content: result.Content, Tags: result.Tags, Entities: result.Entities, Revision: result.Revision, ExpiresAt: result.ExpiresAt, Priority: result.Priority, Confidence: result.Confidence, SourceRefs: result.SourceRefs, EvidenceRefs: result.EvidenceRefs}
}

func (p *NotificationMemoryProcessor) validateNotificationProposal(ctx context.Context, record NotificationRecord, proposal notificationMemoryProposal, refs []MemoryMergeReference) (notificationMemoryProposal, error) {
	if err := validateNotificationProposalShape(proposal); err != nil {
		return notificationMemoryProposal{}, err
	}
	validated := notificationMemoryProposal{Actions: make([]notificationMemoryAction, len(proposal.Actions))}
	for index, action := range proposal.Actions {
		action.Action = strings.ToLower(strings.TrimSpace(action.Action))
		action.Scope = strings.ToLower(strings.TrimSpace(action.Scope))
		switch action.Action {
		case "ignore":
			if action.Scope != "" && action.Scope != "temporary" && action.Scope != "long_term" {
				return notificationMemoryProposal{}, fmt.Errorf("notification proposal has invalid scope %q for ignore", action.Scope)
			}
			validated.Actions[index] = action
			continue
		case "add", "update", "reinforce", "remove", "promote":
		default:
			return notificationMemoryProposal{}, fmt.Errorf("notification proposal has unsupported action %q", action.Action)
		}
		if action.Scope == "" {
			action.Scope = inferNotificationTargetScope(action, refs)
		}
		if action.Scope != "temporary" && action.Scope != "long_term" {
			return notificationMemoryProposal{}, fmt.Errorf("notification proposal has invalid scope %q", action.Scope)
		}
		if action.Action == "promote" {
			if action.Scope != "long_term" || strings.TrimSpace(action.MemoryID) == "" || action.MemoryRevision <= 0 {
				return notificationMemoryProposal{}, fmt.Errorf("notification promote proposal requires long_term scope, temporary memory_id, and memory_revision")
			}
			if err := validateNotificationProposalSourceEvents(record, action.SourceEventIDs); err != nil {
				return notificationMemoryProposal{}, err
			}
			ref, ok := findNotificationReference(action.MemoryID, "temporary", refs)
			if !ok {
				ref, ok = p.findTemporaryNotificationReference(ctx, action.MemoryID)
				if ok && ref.Status == "deleted" && !p.notificationPromotionAlreadyCreated(record, action, ref) {
					ok = false
				}
			}
			if !ok || effectiveMergeReferenceRevision(ref) != action.MemoryRevision {
				return notificationMemoryProposal{}, fmt.Errorf("notification promote proposal references unknown or stale temporary memory %q", action.MemoryID)
			}
			if p.longTerm == nil || p.temporary == nil {
				return notificationMemoryProposal{}, fmt.Errorf("notification promote stores are not configured")
			}
			if strings.TrimSpace(firstNonEmptyString([]string{action.Content, ref.Content, ref.Summary})) == "" {
				return notificationMemoryProposal{}, fmt.Errorf("notification promote proposal requires content")
			}
			if !validNotificationMemoryType(firstNonEmptyString([]string{action.Type, ref.Type, "fact"})) {
				return notificationMemoryProposal{}, fmt.Errorf("notification promote proposal has invalid memory type %q", action.Type)
			}
			confidence, err := normalizeNotificationConfidence(action.Confidence, action.confidenceSet)
			if err != nil {
				return notificationMemoryProposal{}, err
			}
			action.Confidence = confidence
			action.confidenceSet = true
			validated.Actions[index] = action
			continue
		}
		if action.Action == "remove" && strings.TrimSpace(action.MemoryID) == "" {
			return notificationMemoryProposal{}, fmt.Errorf("notification remove proposal requires memory_id")
		}
		if action.Action == "add" && strings.TrimSpace(action.MemoryID) != "" {
			return notificationMemoryProposal{}, fmt.Errorf("notification add proposal must not provide memory_id")
		}
		if action.Action == "update" || action.Action == "reinforce" || action.Action == "remove" {
			if action.MemoryRevision <= 0 {
				return notificationMemoryProposal{}, fmt.Errorf("notification %s proposal requires a positive memory_revision", action.Action)
			}
			if !notificationProposalReferencesMemory(action, refs) {
				return notificationMemoryProposal{}, fmt.Errorf("notification proposal references unknown or stale memory %q", action.MemoryID)
			}
		}
		if action.Action != "remove" && action.Action != "reinforce" && strings.TrimSpace(action.Content) == "" {
			return notificationMemoryProposal{}, fmt.Errorf("notification %s proposal requires content", action.Action)
		}
		if action.Action != "remove" && !validNotificationMemoryType(action.Type) {
			return notificationMemoryProposal{}, fmt.Errorf("notification proposal has invalid memory type %q", action.Type)
		}
		if action.Action != "remove" {
			confidence, err := normalizeNotificationConfidence(action.Confidence, action.confidenceSet)
			if err != nil {
				return notificationMemoryProposal{}, err
			}
			action.Confidence = confidence
			action.confidenceSet = true
		}
		if err := validateNotificationProposalSourceEvents(record, action.SourceEventIDs); err != nil {
			return notificationMemoryProposal{}, err
		}
		if action.Action != "remove" {
			expiresAt, err := p.notificationProposalExpiry(action)
			if err != nil {
				return notificationMemoryProposal{}, err
			}
			action.ExpiresAt = expiresAt
		}
		store := p.temporary
		if action.Scope == "long_term" {
			store = p.longTerm
		}
		if store == nil {
			return notificationMemoryProposal{}, fmt.Errorf("notification memory store is not configured for scope %s", action.Scope)
		}
		validated.Actions[index] = action
	}
	return validated, nil
}

func (p *NotificationMemoryProcessor) findTemporaryNotificationReference(ctx context.Context, id string) (MemoryMergeReference, bool) {
	if p.temporary == nil {
		return MemoryMergeReference{}, false
	}
	select {
	case <-ctx.Done():
		return MemoryMergeReference{}, false
	default:
	}
	parsed, err := readMemoryMarkdown(p.temporary.memoryPath(strings.TrimSpace(id)))
	if err != nil {
		return MemoryMergeReference{}, false
	}
	item := parsed.Item
	return MemoryMergeReference{Scope: "temporary", ID: item.ID, Type: item.Type, Status: item.Status, Title: item.Title, Summary: item.Content, Content: item.Content, Tags: item.Tags, Entities: item.Entities, Revision: effectiveMemoryRevision(item), ExpiresAt: item.ExpiresAt, Priority: item.Priority, Confidence: item.Confidence, SourceRefs: item.SourceRefs, EvidenceRefs: item.EvidenceRefs}, true
}

func (p *NotificationMemoryProcessor) notificationPromotionAlreadyCreated(record NotificationRecord, action notificationMemoryAction, ref MemoryMergeReference) bool {
	if p.longTerm == nil {
		return false
	}
	parsed, err := readMemoryMarkdown(p.longTerm.memoryPath("mem_notification_" + record.ContextID))
	if err != nil || parsed.Item.Status != "active" {
		return false
	}
	expectedContent := firstNonEmptyString([]string{action.Content, ref.Content, ref.Summary})
	if strings.TrimSpace(parsed.Item.Content) != strings.TrimSpace(expectedContent) {
		return false
	}
	expectedSourceRefs := mergeMemorySourceRefs(ref.SourceRefs, []MemorySourceRef{{Type: "notification", ID: record.ContextID, EventIDs: mergeUniqueStrings(notificationMemoryEvidenceIDs(record), action.SourceEventIDs)}})
	return memorySourceRefsContain(parsed.Item.SourceRefs, expectedSourceRefs)
}

func validateNotificationProposalShape(proposal notificationMemoryProposal) error {
	if len(proposal.Actions) > 1 {
		return fmt.Errorf("notification proposal has %d actions (%s); maximum is 1", len(proposal.Actions), describeNotificationActions(proposal.Actions))
	}
	return nil
}

// describeNotificationActions renders action/scope/memory_id triples so a shape
// violation says what the model actually proposed, not just how many.
func describeNotificationActions(actions []notificationMemoryAction) string {
	parts := make([]string, 0, len(actions))
	for _, action := range actions {
		part := strings.TrimSpace(action.Action) + "/" + strings.TrimSpace(action.Scope)
		if id := strings.TrimSpace(action.MemoryID); id != "" {
			part += "@" + id
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

func normalizeNotificationProposalShape(proposal notificationMemoryProposal) (notificationMemoryProposal, error) {
	if len(proposal.Actions) <= 1 {
		return proposal, nil
	}
	// Special case: all ignore actions → normalize to single ignore
	allIgnore := true
	for _, action := range proposal.Actions {
		if !strings.EqualFold(strings.TrimSpace(action.Action), "ignore") {
			allIgnore = false
			break
		}
	}
	if allIgnore {
		return notificationMemoryProposal{Actions: []notificationMemoryAction{{Action: "ignore"}}}, nil
	}
	return notificationMemoryProposal{}, validateNotificationProposalShape(proposal)
}

func (p *NotificationMemoryProcessor) logInvalidNotificationProposal(record NotificationRecord, err error) {
	if err == nil {
		return
	}
	if p != nil && p.logger != nil {
		p.logger.Warn("[notification-memory] invalid proposal skipped: context_id=%s error=%v", record.ContextID, err)
		return
	}
	logging.Warnf("agent", "notification_memory", "invalid proposal skipped: context_id=%s error=%v", record.ContextID, err)
}

func (p *NotificationMemoryProcessor) applyNotificationProposal(ctx context.Context, record NotificationRecord, proposal notificationMemoryProposal, refs []MemoryMergeReference) error {
	for _, action := range proposal.Actions {
		if action.Action == "ignore" {
			continue
		}
		if action.Action == "promote" {
			ref, ok := findNotificationReference(action.MemoryID, "temporary", refs)
			if !ok {
				ref, _ = p.findTemporaryNotificationReference(ctx, action.MemoryID)
			}
			sourceRefs := mergeMemorySourceRefs(ref.SourceRefs, []MemorySourceRef{{Type: "notification", ID: record.ContextID, EventIDs: mergeUniqueStrings(notificationMemoryEvidenceIDs(record), action.SourceEventIDs)}})
			memoryType := firstNonEmptyString([]string{action.Type, ref.Type, "fact"})
			priority, confidence := notificationMemoryMetadata(action, memoryType, ref)
			item := MemoryItem{ID: "mem_notification_" + record.ContextID, Type: memoryType, TimeScope: "long_term", Priority: priority, Confidence: confidence, Title: firstNonEmptyString([]string{action.Title, ref.Title}), Content: firstNonEmptyString([]string{action.Content, ref.Content, ref.Summary}), ExpiresAt: p.now().Add(notificationLongTermTTL).Format(time.RFC3339Nano), Tags: mergeUniqueStrings(ref.Tags, action.Tags), Entities: mergeUniqueStrings(ref.Entities, action.Entities), SourceRefs: sourceRefs, EvidenceRefs: ref.EvidenceRefs, Origin: memoryOriginNotification, EvidenceExcerpts: []string{firstNonEmptyString([]string{action.Content, ref.Content, ref.Summary, record.NotificationEvent.Message, record.NotificationEvent.Title})}}
			if _, err := p.longTerm.ApplyMemoryIntent(ctx, MemoryIntent{Item: item, Action: MemoryIntentActionCreate}); err != nil {
				return err
			}
			if _, err := p.temporary.ApplyMemoryIntent(ctx, MemoryIntent{Item: MemoryItem{ID: action.MemoryID}, Action: MemoryIntentActionRemove, ExpectedRevision: action.MemoryRevision}); err != nil {
				return err
			}
			continue
		}
		var existingRef MemoryMergeReference
		if action.Action == "update" || action.Action == "reinforce" {
			existingRef, _ = findNotificationReference(action.MemoryID, action.Scope, refs)
		}
		id := strings.TrimSpace(action.MemoryID)
		if id == "" {
			id = "tmp_notification_" + record.ContextID
			if action.Scope == "long_term" {
				id = "mem_notification_" + record.ContextID
			}
		}
		memoryType := firstNonEmptyString([]string{action.Type, existingRef.Type, "fact"})
		priority, confidence := notificationMemoryMetadata(action, memoryType, existingRef)
		item := MemoryItem{ID: id, Type: memoryType, TimeScope: action.Scope, Priority: priority, Confidence: confidence, Title: firstNonEmptyString([]string{action.Title, existingRef.Title}), Content: firstNonEmptyString([]string{action.Content, existingRef.Content}), Tags: mergeUniqueStrings(mergeUniqueStrings(notificationMemoryTags(record.NotificationEvent), existingRef.Tags), action.Tags), Entities: mergeUniqueStrings(mergeUniqueStrings(compactNotificationEntities(record.NotificationEvent), existingRef.Entities), action.Entities), ExpiresAt: action.ExpiresAt, SourceRefs: []MemorySourceRef{{Type: "notification", ID: record.ContextID, EventIDs: mergeUniqueStrings(notificationMemoryEvidenceIDs(record), action.SourceEventIDs)}}, EvidenceExcerpts: []string{firstNonEmptyString([]string{action.Content, existingRef.Content, record.NotificationEvent.Message, record.NotificationEvent.Title})}, Origin: memoryOriginNotification}
		if action.Action == "remove" {
			item = MemoryItem{ID: id}
		}
		store := p.temporary
		if action.Scope == "long_term" {
			store = p.longTerm
		}
		if store == nil {
			return fmt.Errorf("notification memory store is not configured for scope %s", action.Scope)
		}
		intentAction := MemoryIntentActionCreate
		switch action.Action {
		case "update":
			intentAction = MemoryIntentActionUpdate
		case "reinforce":
			intentAction = MemoryIntentActionReinforce
		case "remove":
			intentAction = MemoryIntentActionRemove
		}
		_, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: item, Action: intentAction, ExpectedRevision: action.MemoryRevision})
		if err != nil {
			return err
		}
	}
	return nil
}

func validNotificationMemoryType(memoryType string) bool {
	switch strings.ToLower(strings.TrimSpace(memoryType)) {
	case "", "fact", "preference", "rule", "profile":
		return true
	default:
		return false
	}
}

func normalizeNotificationConfidence(confidence float64, provided bool) (float64, error) {
	if !provided {
		return notificationDefaultConfidence, nil
	}
	if confidence <= 0 || confidence > 1 || math.IsNaN(confidence) || math.IsInf(confidence, 0) {
		return 0, fmt.Errorf("notification confidence must be greater than 0 and at most 1")
	}
	return confidence, nil
}

func notificationMemoryPriority(scope, memoryType string) int {
	if strings.EqualFold(strings.TrimSpace(scope), "temporary") {
		return 40
	}
	switch strings.ToLower(strings.TrimSpace(memoryType)) {
	case "rule", "preference":
		return 80
	case "profile":
		return 70
	default:
		return 60
	}
}

func notificationMemoryMetadata(action notificationMemoryAction, memoryType string, source MemoryMergeReference) (int, float64) {
	priority := notificationMemoryPriority(action.Scope, memoryType)
	confidence := action.Confidence
	if confidence <= 0 || confidence > 1 || math.IsNaN(confidence) || math.IsInf(confidence, 0) {
		confidence = notificationDefaultConfidence
	}
	if action.Action == "promote" {
		priority = max(priority, source.Priority)
		if source.Confidence > confidence && source.Confidence <= 1 {
			confidence = source.Confidence
		}
	}
	return priority, confidence
}

// inferNotificationTargetScope fills an omitted scope for update, reinforce,
// and remove from the catalog: the target memory_id already fixes its store
// when exactly one active catalog entry has that id. Add and promote choose a
// destination, so their scope is never inferred.
func inferNotificationTargetScope(action notificationMemoryAction, refs []MemoryMergeReference) string {
	switch action.Action {
	case "update", "reinforce", "remove":
	default:
		return ""
	}
	id := strings.TrimSpace(action.MemoryID)
	if id == "" {
		return ""
	}
	scope := ""
	for _, ref := range refs {
		if ref.ID != id || ref.Status != "active" {
			continue
		}
		if scope != "" && scope != ref.Scope {
			return ""
		}
		scope = ref.Scope
	}
	return scope
}

func findNotificationReference(id, scope string, refs []MemoryMergeReference) (MemoryMergeReference, bool) {
	for _, ref := range refs {
		if ref.ID == strings.TrimSpace(id) && ref.Scope == scope && ref.Status == "active" {
			return ref, true
		}
	}
	return MemoryMergeReference{}, false
}

func (p *NotificationMemoryProcessor) notificationProposalExpiry(action notificationMemoryAction) (string, error) {
	if action.Scope != "temporary" {
		expiresAt := strings.TrimSpace(action.ExpiresAt)
		if expiresAt == "" {
			expiresAt = p.now().Add(notificationLongTermTTL).Format(time.RFC3339Nano)
		}
		return expiresAt, nil
	}
	expiresAt := strings.TrimSpace(action.ExpiresAt)
	if expiresAt == "" {
		expiresAt = p.now().Add(notificationMemoryTTL).Format(time.RFC3339Nano)
	}
	parsed, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return "", fmt.Errorf("temporary notification memory expires_at must be RFC3339: %w", err)
	}
	if !parsed.After(p.now()) {
		return "", fmt.Errorf("temporary notification memory expires_at must be in the future")
	}
	return parsed.UTC().Format(time.RFC3339Nano), nil
}

func validateNotificationProposalSourceEvents(record NotificationRecord, sourceEventIDs []string) error {
	allowed := map[string]bool{}
	for _, id := range []string{record.NotificationEvent.ID, record.NotificationEvent.SourceEventID} {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = true
		}
	}
	for _, id := range sourceEventIDs {
		if id = strings.TrimSpace(id); id != "" && !allowed[id] {
			return fmt.Errorf("notification proposal references unrelated source event %q", id)
		}
	}
	return nil
}

func notificationProposalReferencesMemory(action notificationMemoryAction, refs []MemoryMergeReference) bool {
	for _, ref := range refs {
		if ref.ID != strings.TrimSpace(action.MemoryID) || ref.Scope != action.Scope || ref.Status != "active" {
			continue
		}
		return action.MemoryRevision == effectiveMergeReferenceRevision(ref)
	}
	return false
}

func effectiveMergeReferenceRevision(ref MemoryMergeReference) int {
	if ref.Revision > 0 {
		return ref.Revision
	}
	return 1
}

func coalesceNotificationRecords(records []NotificationRecord) []NotificationRecord {
	if len(records) < 2 {
		return records
	}
	last := make(map[string]int, len(records))
	for index, record := range records {
		last[notificationRecordIdentity(record)] = index
	}
	result := make([]NotificationRecord, 0, len(last))
	for index, record := range records {
		if last[notificationRecordIdentity(record)] == index {
			result = append(result, record)
		}
	}
	return result
}

func notificationRecordIdentity(record NotificationRecord) string {
	event := record.NotificationEvent
	if event.DeviceID == "" && event.Source == "" && event.SourceID == "" && event.NotificationUID == 0 {
		return "context\x00" + record.ContextID
	}
	identity := strings.Join([]string{event.DeviceID, event.Source, event.SourceID, fmt.Sprintf("%d", event.NotificationUID)}, "\x00")
	return identity
}

func (p *NotificationMemoryProcessor) removeTemporary(ctx context.Context, record NotificationRecord) error {
	if p.temporary == nil {
		return fmt.Errorf("temporary memory store is not configured")
	}
	memories, err := p.temporary.searchAll(ctx)
	if err != nil {
		return err
	}
	removed := false
	for _, memory := range memories {
		if !notificationMemoryReferencesRecord(memory.SourceRefs, record) {
			continue
		}
		if _, err := p.temporary.ApplyMemoryIntent(ctx, MemoryIntent{Item: MemoryItem{ID: memory.ID}, Action: MemoryIntentActionRemove, ExpectedRevision: effectiveMemoryResultRevision(memory)}); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return nil
	}
	fallbackID := "tmp_notification_" + record.ContextID
	for _, memory := range memories {
		if memory.ID != fallbackID {
			continue
		}
		_, err := p.temporary.ApplyMemoryIntent(ctx, MemoryIntent{Item: MemoryItem{ID: fallbackID}, Action: MemoryIntentActionRemove, ExpectedRevision: effectiveMemoryResultRevision(memory)})
		return err
	}
	return nil
}

func effectiveMemoryResultRevision(memory MemoryResult) int {
	if memory.Revision > 0 {
		return memory.Revision
	}
	return 1
}

func notificationMemoryReferencesRecord(refs []MemorySourceRef, record NotificationRecord) bool {
	identities := notificationMemoryStableIdentityIDs(record.NotificationEvent)
	identities = append(identities, strings.TrimSpace(record.NotificationEvent.ID), strings.TrimSpace(record.NotificationEvent.SourceEventID))
	identities = mergeUniqueStrings(nil, identities)
	if len(identities) == 0 {
		return false
	}
	for _, ref := range refs {
		if ref.Type != "notification" {
			continue
		}
		for _, eventID := range ref.EventIDs {
			for _, identity := range identities {
				if eventID == identity {
					return true
				}
			}
		}
	}
	return false
}

func notificationMemoryEvidenceIDs(record NotificationRecord) []string {
	ids := []string{record.NotificationEvent.ID, record.NotificationEvent.SourceEventID}
	ids = append(ids, notificationMemoryStableIdentityIDs(record.NotificationEvent)...)
	return mergeUniqueStrings(nil, ids)
}

func notificationMemoryStableIdentityIDs(event ble.NotificationEvent) []string {
	return notificationEventStableIdentityIDs(event)
}

func (p *NotificationMemoryProcessor) logBatchError(err error) {
	if err == nil {
		return
	}
	if p != nil && p.logger != nil {
		p.logger.Warn("[notification-memory] batch failed: %v", err)
		return
	}
	logging.Warnf("agent", "notification_memory", "batch failed: %v", err)
}

func notificationMemoryIgnored(event ble.NotificationEvent) bool {
	if strings.EqualFold(strings.TrimSpace(event.Event), "removed") {
		return true
	}
	return strings.TrimSpace(event.Title) == "" && strings.TrimSpace(event.Message) == ""
}

func notificationMemoryTags(event ble.NotificationEvent) []string {
	tags := []string{"notification"}
	if app := strings.TrimSpace(event.AppIdentifier); app != "" {
		tags = append(tags, app)
	}
	return tags
}

func compactNotificationEntities(event ble.NotificationEvent) []string {
	if value := strings.TrimSpace(event.AppIdentifier); value != "" {
		return []string{value}
	}
	return nil
}
