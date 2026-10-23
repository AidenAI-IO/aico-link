package session

import (
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/tokencounter"
	"errors"
	"sync"
)

// Session is the durable conversation state owned by a context manager.
//
// A Session has immutable identity and lineage. Its message list is protected
// by its own lock so callers can keep a session value while the context
// manager changes which session is active.
type Session struct {
	sessionID       string
	parentSessionID string
	messageList     []messages.Message
	attachmentStore AttachmentStore
	artifactStore   ArtifactStore
	persist         func([]messages.Message) error
	version         uint64
	tokenCount      int
	hasTokenUsage   bool
	persistTokens   func(TokenState)
	mu              sync.RWMutex
}

// TokenState is a reconstructable snapshot of context occupancy, not cumulative
// billed usage. MessageCount identifies the transcript prefix it describes.
type TokenState struct {
	TokenCount    int  `json:"token_count"`
	HasTokenUsage bool `json:"has_token_usage"`
	MessageCount  int  `json:"message_count"`
}

// New creates an in-memory session. The message list is deep-cloned so the
// caller can safely reuse or mutate its input after construction.
func New(sessionID, parentSessionID string, messageList []messages.Message) *Session {
	return NewWithStores(sessionID, parentSessionID, messageList, nil, nil)
}

// NewWithStores creates a session and associates its session-scoped resource
// stores with it. Stores are optional so the value remains useful for pure
// in-memory callers and tests.
func NewWithStores(sessionID, parentSessionID string, messageList []messages.Message, attachmentStore AttachmentStore, artifactStore ArtifactStore) *Session {
	return NewWithStoresAndPersistence(sessionID, parentSessionID, messageList, attachmentStore, artifactStore, nil)
}

// NewWithStoresAndPersistence creates a session with optional resource stores
// and an append callback. The callback is invoked before the in-memory message
// list changes, preserving the durable-session write ordering.
func NewWithStoresAndPersistence(sessionID, parentSessionID string, messageList []messages.Message, attachmentStore AttachmentStore, artifactStore ArtifactStore, persist func([]messages.Message) error) *Session {
	return NewWithTokenPersistence(sessionID, parentSessionID, messageList, attachmentStore, artifactStore, persist, 0, nil)
}

// NewWithTokenPersistence ignores usage on the first usageStart messages, which
// were retained from a rewritten context. persistTokens runs under the session
// lock after transcript commit and must not call back into the session.
func NewWithTokenPersistence(sessionID, parentSessionID string, messageList []messages.Message, attachmentStore AttachmentStore, artifactStore ArtifactStore, persist func([]messages.Message) error, usageStart int, persistTokens func(TokenState)) *Session {
	s := &Session{
		sessionID:       sessionID,
		parentSessionID: parentSessionID,
		messageList:     cloneMessages(messageList),
		attachmentStore: attachmentStore,
		artifactStore:   artifactStore,
		persist:         persist,
		version:         uint64(len(messageList)),
		persistTokens:   persistTokens,
	}
	for i, message := range s.messageList {
		s.addMessageTokens(message, i >= usageStart)
	}
	return s
}

// TokenCount reports current context occupancy. Provider usage is the baseline
// when available; later messages are estimated.
func (s *Session) TokenCount() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tokenCount
}

// HasTokenUsage reports whether the occupancy baseline came from provider usage.
func (s *Session) HasTokenUsage() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasTokenUsage
}

// TokenSnapshot returns occupancy and its transcript generation atomically.
func (s *Session) TokenSnapshot() TokenState {
	if s == nil {
		return TokenState{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tokenSnapshot()
}

func (s *Session) tokenSnapshot() TokenState {
	return TokenState{TokenCount: s.tokenCount, HasTokenUsage: s.hasTokenUsage, MessageCount: len(s.messageList)}
}

func (s *Session) addMessageTokens(message messages.Message, allowUsage bool) {
	isResponse := message.Role == messages.MessageRoleAssistant || message.Role == messages.MessageRoleToolCall
	if allowUsage && isResponse && message.Usage != nil {
		usage := message.Usage
		count := usage.TotalTokens
		if count == 0 {
			count = usage.InputTokens + usage.OutputTokens
		}
		if count > 0 && usage.TotalTokens >= 0 && usage.InputTokens >= 0 && usage.OutputTokens >= 0 {
			s.tokenCount, s.hasTokenUsage = count, true
			return
		}
	}
	s.tokenCount += tokencounter.EstimateMessageTokens(message)
}

// ID returns the immutable identifier of the session.
func (s *Session) ID() string {
	if s == nil {
		return ""
	}
	return s.sessionID
}

func (s *Session) SessionID() string {
	return s.ID()
}

// ParentID returns the identifier of the session from which this session was
// derived. Root sessions return an empty string.
func (s *Session) ParentID() string {
	if s == nil {
		return ""
	}
	return s.parentSessionID
}

// ParentSessionID is kept as a descriptive alias for callers that use the
// context-manager terminology.
func (s *Session) ParentSessionID() string {
	return s.ParentID()
}

// CloneMessageList returns a deep copy of the messages currently in the
// session.
func (s *Session) CloneMessageList() []messages.Message {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMessages(s.messageList)
}

// Snapshot returns a deep copy of the messages and the generation from the
// same lock acquisition.
func (s *Session) Snapshot() ([]messages.Message, uint64) {
	if s == nil {
		return nil, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMessages(s.messageList), s.version
}

// ViewMessages calls fn with a read-only view of the current message slice
// while the session read lock is held. fn must not mutate or retain the slice.
func (s *Session) ViewMessages(fn func([]messages.Message)) {
	if fn == nil {
		return
	}
	if s == nil {
		fn(nil)
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.messageList)
}

// IsEmpty reports whether the session contains no messages.
func (s *Session) IsEmpty() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.messageList) == 0
}

// Version is a monotonically increasing message generation. It lets a
// context manager reject activation of a revision prepared from a stale
// snapshot.
func (s *Session) Version() uint64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// AppendMessages commits messages to the session. When the session was created
// with a persistence callback, that callback completes before the in-memory
// list changes. This keeps direct session users subject to the same
// write-before-memory-update invariant as context-manager appends.
func (s *Session) AppendMessages(messageList []messages.Message) error {
	if s == nil || len(messageList) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	messageList = cloneMessages(messageList)
	if s.persist != nil {
		if err := s.persist(cloneMessages(messageList)); err != nil {
			return err
		}
	}
	s.messageList = append(s.messageList, messageList...)
	s.version += uint64(len(messageList))
	for _, message := range messageList {
		s.addMessageTokens(message, true)
	}
	if s.persistTokens != nil {
		s.persistTokens(s.tokenSnapshot())
	}
	return nil
}

// StoreAttachment writes bytes to this session's attachment store.
func (s *Session) StoreAttachment(mimeType string, data []byte) (messages.Attachment, error) {
	if s == nil || s.attachmentStore == nil {
		return messages.Attachment{}, errors.New("attachment store is unavailable")
	}
	return s.attachmentStore.StoreAttachment(mimeType, data)
}

// StoreArtifact writes a tool-result artifact to this session's artifact store.
func (s *Session) StoreArtifact(mimeType string, data []byte, metadata ArtifactMetadata) (ArtifactFile, error) {
	if s == nil || s.artifactStore == nil {
		return ArtifactFile{}, errors.New("artifact store is unavailable")
	}
	return s.artifactStore.StoreArtifact(mimeType, data, metadata)
}

// ArtifactStoreRoot exposes the store root for maintenance code and diagnostics
// without exposing the concrete storage implementation.
func (s *Session) ArtifactStoreRoot() string {
	if s == nil || s.artifactStore == nil {
		return ""
	}
	if rooted, ok := s.artifactStore.(interface{ Root() string }); ok {
		return rooted.Root()
	}
	return ""
}

func cloneMessages(messageList []messages.Message) []messages.Message {
	if len(messageList) == 0 {
		return nil
	}
	cloned := make([]messages.Message, len(messageList))
	for i, message := range messageList {
		cloned[i] = message.Clone()
	}
	return cloned
}
