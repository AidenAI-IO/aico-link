package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

var errMemoryRevisionChanged = errors.New("memory revision changed")
var errMemoryIDConflict = errors.New("memory id already exists")
var errMemoryTargetMissing = errors.New("memory target does not exist")

// MemoryIntentAction is the operation selected by a Processor.
type MemoryIntentAction string

const (
	MemoryIntentActionCreate    MemoryIntentAction = "create"
	MemoryIntentActionUpdate    MemoryIntentAction = "update"
	MemoryIntentActionReinforce MemoryIntentAction = "reinforce"
	MemoryIntentActionRemove    MemoryIntentAction = "remove"
)

// MemoryOperation describes the result of applying a candidate to a Memory
// store. It is an operation, not the persisted lifecycle status of a Memory.
type MemoryOperation string

const (
	MemoryOperationAdd       MemoryOperation = "add"
	MemoryOperationUpdate    MemoryOperation = "update"
	MemoryOperationReinforce MemoryOperation = "reinforce"
	MemoryOperationSupersede MemoryOperation = "supersede"
	MemoryOperationRemove    MemoryOperation = "remove"
	MemoryOperationIgnore    MemoryOperation = "ignore"
)

// MemoryIntent is the shared operation envelope used by filesystem Memory
// adapters. Item and DeviceItem retain their existing persisted schemas.
type MemoryIntent struct {
	Item             MemoryItem
	DeviceItem       *DeviceMemoryItem
	Action           MemoryIntentAction
	ExpectedRevision int
	ReplaceSteps     bool // Device procedure updates can supply a complete replacement path.
}

func (s *LongTermMemoryStore) ApplyMemoryIntent(ctx context.Context, intent MemoryIntent) (MemoryApplyResult, error) {
	if s == nil {
		return MemoryApplyResult{}, errors.New("memory store is not configured")
	}
	if intent.Action == "" {
		return MemoryApplyResult{}, errors.New("memory intent requires an action")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.applyMemoryIntentLocked(ctx, intent)
}

func (s *LongTermMemoryStore) ApplyMemoryCandidate(ctx context.Context, item MemoryItem) (MemoryApplyResult, error) {
	if s == nil {
		return MemoryApplyResult{}, errors.New("memory store is not configured")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.applyMemoryCandidateLocked(ctx, item)
}

type MemoryApplyResult struct {
	Operation MemoryOperation
	ID        string
}

func (s *LongTermMemoryStore) applyMemoryCandidateLocked(ctx context.Context, item MemoryItem) (MemoryApplyResult, error) {
	if strings.TrimSpace(item.ID) != "" {
		path := s.memoryPath(item.ID)
		parsed, err := readMemoryMarkdown(path)
		if err == nil {
			if parsed.Item.Status == "deleted" {
				// A new observation can resurrect the same identity, but it must
				// become a fresh active revision with the original ID.
				parsed.Item.Status = "active"
			}
			same := sameMemoryConclusion(parsed.Item, item)
			mergeMemoryItem(&parsed.Item, item, time.Now().UTC())
			if err := writeFileAtomic(path, []byte(formatMemoryMarkdown(parsed.Item)), 0o644); err != nil {
				return MemoryApplyResult{}, err
			}
			s.invalidateParsedMemoryCache(path)
			if err := s.rebuildIndexLocked(ctx); err != nil {
				return MemoryApplyResult{}, err
			}
			operation := MemoryOperationUpdate
			if same {
				operation = MemoryOperationReinforce
			}
			return MemoryApplyResult{Operation: operation, ID: item.ID}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return MemoryApplyResult{}, err
		}
	}

	action, existingID, err := s.decideActionLocked(ctx, item)
	if err != nil {
		return MemoryApplyResult{}, err
	}
	switch action {
	case "ignore":
		return MemoryApplyResult{Operation: MemoryOperationIgnore, ID: existingID}, nil
	case "supersede":
		id, err := s.supersedeMemoryLocked(ctx, existingID, item)
		return MemoryApplyResult{Operation: MemoryOperationSupersede, ID: id}, err
	default:
		id, err := s.addMemoryLocked(ctx, item)
		return MemoryApplyResult{Operation: MemoryOperationAdd, ID: id}, err
	}
}

func (s *LongTermMemoryStore) applyMemoryIntentLocked(ctx context.Context, intent MemoryIntent) (MemoryApplyResult, error) {
	if intent.Action == MemoryIntentActionRemove {
		return s.applyMemoryRemoveIntentLocked(ctx, intent)
	}
	return s.applyMemoryWriteIntentLocked(ctx, intent)
}

func (s *LongTermMemoryStore) applyMemoryRemoveIntentLocked(ctx context.Context, intent MemoryIntent) (MemoryApplyResult, error) {
	id := strings.TrimSpace(intent.Item.ID)
	if id == "" {
		return MemoryApplyResult{}, fmt.Errorf("memory remove requires an id")
	}
	path := s.memoryPath(id)
	parsed, err := readMemoryMarkdown(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return MemoryApplyResult{Operation: MemoryOperationIgnore, ID: id}, nil
		}
		return MemoryApplyResult{}, err
	}
	if intent.ExpectedRevision <= 0 {
		return MemoryApplyResult{}, fmt.Errorf("memory remove requires expected revision")
	}
	if effectiveMemoryRevision(parsed.Item) != intent.ExpectedRevision {
		return MemoryApplyResult{}, errMemoryRevisionChanged
	}
	if parsed.Item.Status == "deleted" {
		return MemoryApplyResult{Operation: MemoryOperationIgnore, ID: id}, nil
	}
	if err := s.forgetLocked(ctx, id, "memory intent removed"); err != nil {
		return MemoryApplyResult{}, err
	}
	return MemoryApplyResult{Operation: MemoryOperationRemove, ID: id}, nil
}

func (s *LongTermMemoryStore) applyMemoryWriteIntentLocked(ctx context.Context, intent MemoryIntent) (MemoryApplyResult, error) {
	item := intent.Item
	id := strings.TrimSpace(item.ID)
	if id == "" {
		return MemoryApplyResult{}, fmt.Errorf("memory %s requires an id", intent.Action)
	}
	path := s.memoryPath(id)
	parsed, err := readMemoryMarkdown(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return MemoryApplyResult{}, err
	}

	switch intent.Action {
	case MemoryIntentActionCreate:
		if !exists {
			newID, err := s.addMemoryLocked(ctx, item)
			return MemoryApplyResult{Operation: MemoryOperationAdd, ID: newID}, err
		}
		if memoryCreateAlreadyApplied(parsed.Item, item) {
			if parsed.Item.Status == "deleted" {
				parsed.Item.Status = "active"
				parsed.Item.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				if err := writeFileAtomic(path, []byte(formatMemoryMarkdown(parsed.Item)), 0o644); err != nil {
					return MemoryApplyResult{}, err
				}
				s.invalidateParsedMemoryCache(path)
				if err := s.rebuildIndexLocked(ctx); err != nil {
					return MemoryApplyResult{}, err
				}
			}
			return MemoryApplyResult{Operation: MemoryOperationAdd, ID: id}, nil
		}
		if memorySourceRefIdentitiesContain(parsed.Item.SourceRefs, item.SourceRefs) {
			return MemoryApplyResult{Operation: MemoryOperationAdd, ID: id}, nil
		}
		return MemoryApplyResult{}, fmt.Errorf("%w: %s", errMemoryIDConflict, id)
	case MemoryIntentActionUpdate, MemoryIntentActionReinforce:
		if !exists {
			return MemoryApplyResult{}, fmt.Errorf("%w: %s", errMemoryTargetMissing, id)
		}
		if intent.ExpectedRevision <= 0 {
			return MemoryApplyResult{}, fmt.Errorf("memory %s requires expected revision", intent.Action)
		}
		if effectiveMemoryRevision(parsed.Item) != intent.ExpectedRevision {
			alreadyApplied := memoryUpdateAlreadyApplied(parsed.Item, item)
			if intent.Action == MemoryIntentActionReinforce {
				alreadyApplied = memoryReinforceAlreadyApplied(parsed.Item, item)
			}
			if alreadyApplied {
				return MemoryApplyResult{Operation: memoryOperationForIntent(intent.Action), ID: id}, nil
			}
			return MemoryApplyResult{}, errMemoryRevisionChanged
		}
		if intent.Action == MemoryIntentActionReinforce {
			reinforceMemoryItem(&parsed.Item, item, time.Now().UTC())
		} else {
			updateMemoryItem(&parsed.Item, item, time.Now().UTC())
		}
		if err := writeFileAtomic(path, []byte(formatMemoryMarkdown(parsed.Item)), 0o644); err != nil {
			return MemoryApplyResult{}, err
		}
		s.invalidateParsedMemoryCache(path)
		if err := s.rebuildIndexLocked(ctx); err != nil {
			return MemoryApplyResult{}, err
		}
		return MemoryApplyResult{Operation: memoryOperationForIntent(intent.Action), ID: id}, nil
	default:
		return MemoryApplyResult{}, fmt.Errorf("unsupported memory intent action %q", intent.Action)
	}
}

func memoryOperationForIntent(action MemoryIntentAction) MemoryOperation {
	if action == MemoryIntentActionReinforce {
		return MemoryOperationReinforce
	}
	return MemoryOperationUpdate
}

func memoryCreateAlreadyApplied(existing, candidate MemoryItem) bool {
	if strings.TrimSpace(existing.Content) != strings.TrimSpace(candidate.Content) {
		return false
	}
	if candidate.Type != "" && existing.Type != candidate.Type {
		return false
	}
	if candidate.TimeScope != "" && existing.TimeScope != candidate.TimeScope {
		return false
	}
	if !memoryStringsContain(existing.Tags, candidate.Tags) || !memoryStringsContain(existing.Entities, candidate.Entities) {
		return false
	}
	if len(candidate.SourceRefs) == 0 {
		return false
	}
	if strings.TrimSpace(candidate.Title) != "" && strings.TrimSpace(existing.Title) != strings.TrimSpace(candidate.Title) {
		return false
	}
	return memorySourceRefsContain(existing.SourceRefs, candidate.SourceRefs) &&
		memorySourceRefsContain(existing.EvidenceRefs, candidate.EvidenceRefs) &&
		memoryStringsContain(existing.EvidenceExcerpts, candidate.EvidenceExcerpts)
}

func memoryUpdateAlreadyApplied(existing, candidate MemoryItem) bool {
	if !memoryCreateAlreadyApplied(existing, candidate) {
		return false
	}
	if candidate.ExpiresAt != "" && existing.ExpiresAt != candidate.ExpiresAt {
		return false
	}
	if candidate.TTL != "" && existing.TTL != candidate.TTL {
		return false
	}
	if candidate.Priority > 0 && existing.Priority != candidate.Priority {
		return false
	}
	if candidate.Confidence > 0 && existing.Confidence != candidate.Confidence {
		return false
	}
	return true
}

func memoryReinforceAlreadyApplied(existing, candidate MemoryItem) bool {
	if len(candidate.SourceRefs) == 0 || !memorySourceRefsContain(existing.SourceRefs, candidate.SourceRefs) {
		return false
	}
	if !memorySourceRefsContain(existing.EvidenceRefs, candidate.EvidenceRefs) ||
		!memoryStringsContain(existing.EvidenceExcerpts, candidate.EvidenceExcerpts) ||
		!memoryStringsContain(existing.Tags, candidate.Tags) ||
		!memoryStringsContain(existing.Entities, candidate.Entities) {
		return false
	}
	if candidate.ExpiresAt != "" && existing.ExpiresAt != candidate.ExpiresAt {
		return false
	}
	return existing.Priority >= candidate.Priority && existing.Confidence >= candidate.Confidence
}

func memorySourceRefsContain(existing, candidate []MemorySourceRef) bool {
	for _, candidateRef := range candidate {
		found := false
		for _, existingRef := range existing {
			if candidateRef.Type != existingRef.Type || candidateRef.ID != existingRef.ID {
				continue
			}
			if memoryStringsContain(existingRef.EventIDs, candidateRef.EventIDs) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func memorySourceRefIdentitiesContain(existing, candidate []MemorySourceRef) bool {
	if len(candidate) == 0 {
		return false
	}
	for _, candidateRef := range candidate {
		if strings.TrimSpace(candidateRef.Type) == "" || strings.TrimSpace(candidateRef.ID) == "" {
			return false
		}
		found := false
		for _, existingRef := range existing {
			if existingRef.Type == candidateRef.Type && existingRef.ID == candidateRef.ID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func memoryStringsContain(existing, candidate []string) bool {
	for _, value := range candidate {
		found := false
		for _, current := range existing {
			if current == value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func effectiveMemoryRevision(item MemoryItem) int {
	if item.Revision > 0 {
		return item.Revision
	}
	return 1
}

func mergeMemoryItem(existing *MemoryItem, candidate MemoryItem, now time.Time) {
	// Whoever writes new content owns it; an unchanged conclusion keeps its
	// author.
	if candidate.Origin != "" && candidate.Content != "" && candidate.Content != existing.Content {
		existing.Origin = candidate.Origin
	}
	if candidate.Title != "" {
		existing.Title = candidate.Title
	}
	if candidate.Content != "" {
		existing.Content = candidate.Content
	}
	if candidate.Type != "" {
		existing.Type = candidate.Type
	}
	if candidate.TimeScope != "" {
		existing.TimeScope = candidate.TimeScope
	}
	if candidate.ExpiresAt != "" {
		existing.ExpiresAt = candidate.ExpiresAt
	}
	if candidate.TTL != "" {
		existing.TTL = candidate.TTL
	}
	existing.Sensitive = existing.Sensitive || candidate.Sensitive
	if candidate.Priority > existing.Priority {
		existing.Priority = candidate.Priority
	}
	if candidate.Confidence > existing.Confidence {
		existing.Confidence = candidate.Confidence
	}
	existing.Tags = mergeUniqueStrings(existing.Tags, candidate.Tags)
	existing.Entities = mergeUniqueStrings(existing.Entities, candidate.Entities)
	existing.SourceRefs = mergeMemorySourceRefs(existing.SourceRefs, candidate.SourceRefs)
	existing.EvidenceRefs = mergeMemorySourceRefs(existing.EvidenceRefs, candidate.EvidenceRefs)
	if len(candidate.EvidenceExcerpts) > 0 {
		existing.EvidenceExcerpts = mergeUniqueStrings(existing.EvidenceExcerpts, candidate.EvidenceExcerpts)
	}
	if existing.Status == "" {
		existing.Status = "active"
	}
	if existing.CreatedAt == "" {
		existing.CreatedAt = candidate.CreatedAt
	}
	existing.UpdatedAt = now.Format(time.RFC3339Nano)
	if existing.Revision <= 0 {
		existing.Revision = 1
	} else {
		existing.Revision++
	}
}

func updateMemoryItem(existing *MemoryItem, candidate MemoryItem, now time.Time) {
	mergeMemoryItem(existing, candidate, now)
	if candidate.Priority > 0 {
		existing.Priority = candidate.Priority
	}
	if candidate.Confidence > 0 {
		existing.Confidence = candidate.Confidence
	}
}

func reinforceMemoryItem(existing *MemoryItem, candidate MemoryItem, now time.Time) {
	existing.Sensitive = existing.Sensitive || candidate.Sensitive
	existing.Tags = mergeUniqueStrings(existing.Tags, candidate.Tags)
	existing.Entities = mergeUniqueStrings(existing.Entities, candidate.Entities)
	existing.SourceRefs = mergeMemorySourceRefs(existing.SourceRefs, candidate.SourceRefs)
	existing.EvidenceRefs = mergeMemorySourceRefs(existing.EvidenceRefs, candidate.EvidenceRefs)
	if len(candidate.EvidenceExcerpts) > 0 {
		existing.EvidenceExcerpts = mergeUniqueStrings(existing.EvidenceExcerpts, candidate.EvidenceExcerpts)
	}
	if candidate.Priority > existing.Priority {
		existing.Priority = candidate.Priority
	}
	if candidate.Confidence > existing.Confidence {
		existing.Confidence = candidate.Confidence
	}
	if candidate.ExpiresAt != "" {
		existing.ExpiresAt = candidate.ExpiresAt
	}
	if existing.Status == "" {
		existing.Status = "active"
	}
	existing.UpdatedAt = now.Format(time.RFC3339Nano)
	if existing.Revision <= 0 {
		existing.Revision = 1
	} else {
		existing.Revision++
	}
}

func sameMemoryConclusion(existing, candidate MemoryItem) bool {
	return strings.TrimSpace(existing.Content) == strings.TrimSpace(candidate.Content) &&
		strings.TrimSpace(existing.Title) == strings.TrimSpace(candidate.Title)
}

func mergeMemorySourceRefs(existing, candidate []MemorySourceRef) []MemorySourceRef {
	result := append([]MemorySourceRef(nil), existing...)
	for _, ref := range candidate {
		found := false
		for i := range result {
			if result[i].Type != ref.Type || result[i].ID != ref.ID {
				continue
			}
			result[i].EventIDs = mergeUniqueStrings(result[i].EventIDs, ref.EventIDs)
			found = true
			break
		}
		if !found {
			result = append(result, ref)
		}
	}
	return result
}
