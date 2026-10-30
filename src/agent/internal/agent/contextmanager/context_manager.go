package contextmanager

import (
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/session"
	"aico-agent/internal/logging"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type AppendMessageHook func(messages.Message) AppendMessageHookResult

type AppendMessageHookResult struct {
	Before  []messages.Message
	Message *messages.Message
	After   []messages.Message
}

// ContextManager owns the long-lived context orchestration for an agent. It
// keeps append hooks and the active session while allowing the session object
// to be replaced without replacing the manager itself.
//
// It is thread safe and can be used concurrently by multiple goroutines.
// Session identity and lineage belong to session.Session; the manager only
// exposes them through the active-session facade methods below.
type ContextManager struct {
	currentSession *session.Session
	appendHooks    []AppendMessageHook
	sessionFolder  string
	hooksMu        sync.RWMutex
	operationMu    sync.Mutex
	// Revision candidates retain the base active-session generation so
	// activation can reject a candidate prepared from stale context.
	activationParentID string
	parentVersion      uint64
	parentVersionSet   bool
}

// LoadContextManagerFromSessionID loads a context manager from the session folder
func LoadContextManagerFromSessionID(sessionFolder string, sessionID string) (*ContextManager, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session ID is empty")
	}

	messageList, err := loadSession(sessionFolder, sessionID)
	if err != nil {
		return nil, err
	}

	attachmentStore, err := newAttachmentStore(sessionFolder, sessionID)
	if err != nil {
		return nil, err
	}
	artifactStore, err := newArtifactStore(sessionFolder, sessionID)
	if err != nil {
		return nil, err
	}

	// A missing or unreadable sidecar leaves lineage unknown rather than failing
	// the load: sessions written before the sidecar existed remain usable.
	parentSessionID := ""
	metadata, found, err := loadSessionMetadata(sessionFolder, sessionID)
	switch {
	case err != nil:
		logging.Warnf("agent", "cm", "Failed to load session metadata for %s: %v", sessionID, err)
		// The lost metadata may have marked a revision's inherited usage as
		// invalid. Do not reinstate those measurements while recovering.
		metadata = sessionMetadata{UsageStartIndex: len(messageList)}
	case found:
		parentSessionID = strings.TrimSpace(metadata.ParentSessionID)
	}
	// Older revisions have no usage boundary. Conservatively ignore inherited
	// usage until a new response arrives rather than using the parent's count.
	if metadata.Tokens == nil && parentSessionID != "" {
		metadata.UsageStartIndex = len(messageList)
	}
	metadata.UsageStartIndex = min(max(metadata.UsageStartIndex, 0), len(messageList))
	persistTokens := tokenMetadataWriter(sessionFolder, sessionID, metadata)

	manager := &ContextManager{
		currentSession: session.NewWithTokenPersistence(
			sessionID,
			parentSessionID,
			messageList,
			attachmentStore,
			artifactStore,
			func(messageList []messages.Message) error {
				return appendSession(sessionFolder, sessionID, messageList)
			},
			metadata.UsageStartIndex,
			persistTokens,
		),
		sessionFolder: sessionFolder,
	}
	// Rebuild from the transcript rather than trusting a snapshot that may lag
	// after a crash between the transcript append and sidecar rename.
	persistTokens(manager.currentSession.TokenSnapshot())
	return manager, nil
}

func LoadContextManagerFromCurrentSession(sessionFolder string) (*ContextManager, error) {
	sessionID := fetchCurrentSession(sessionFolder)
	if sessionID == "" {
		return nil, fmt.Errorf("current session ID is empty")
	}
	return LoadContextManagerFromSessionID(sessionFolder, sessionID)
}

// NewContextManager creates a new context manager and saves the session ID to the session folder as current session.
func NewContextManager(sessionFolder string, systemPrompt string) (*ContextManager, error) {
	manager, err := NewContextManagerCandidate(sessionFolder, systemPrompt)
	if err != nil {
		return nil, err
	}

	// Do not publish a partially-created session as current. The transcript,
	// metadata and stores are ready by the time the pointer is installed.
	if err := saveCurrentSession(sessionFolder, manager.GetSessionID()); err != nil {
		return nil, err
	}
	return manager, nil
}

// NewContextManagerCandidate creates a complete root session without changing
// the folder's current-session pointer. Call Activate on the long-lived
// manager once the candidate is ready to publish it.
func NewContextManagerCandidate(sessionFolder string, systemPrompt string) (*ContextManager, error) {
	manager, err := newContextManagerFromMessageList(sessionFolder, newSessionID(), "", nil)
	if err != nil {
		return nil, err
	}
	if err := manager.AppendMessage(messages.Message{
		Role:    messages.MessageRoleSystem,
		Content: systemPrompt,
	}); err != nil {
		return nil, err
	}
	return manager, nil
}

func NewContextManagerFromMessageList(sessionFolder string, messageList []messages.Message) (*ContextManager, error) {
	return newContextManagerFromMessageList(sessionFolder, newSessionID(), "", messageList)
}

// NewContextManagerRevisionFromMessageList creates a new session that continues
// parent's conversation, for example after compaction. The revision records
// parent's session ID in its metadata sidecar so the lineage stays traceable.
func NewContextManagerRevisionFromMessageList(parent *ContextManager, messageList []messages.Message) (*ContextManager, error) {
	if parent == nil {
		return nil, fmt.Errorf("parent context manager is nil")
	}
	parent.operationMu.Lock()
	if parent.currentSession == nil {
		parent.operationMu.Unlock()
		return nil, fmt.Errorf("parent current session is unavailable")
	}
	sessionFolder := parent.sessionFolder
	parentSessionID := parent.currentSession.SessionID()
	parentVersion := parent.currentSession.Version()
	activationParentID := parentSessionID
	if parent.parentVersionSet {
		activationParentID = parent.activationParentID
		parentVersion = parent.parentVersion
	}
	parent.operationMu.Unlock()

	return newContextManagerRevisionFromMessageList(sessionFolder, parentSessionID, activationParentID, parentVersion, messageList)
}

// NewContextManagerRevisionFromMessageListAtVersion is the snapshot-aware
// revision constructor used by compaction. Activation will fail if the parent
// session has been appended to since parentVersion was captured.
func NewContextManagerRevisionFromMessageListAtVersion(parent *ContextManager, messageList []messages.Message, parentVersion uint64) (*ContextManager, error) {
	if parent == nil {
		return nil, fmt.Errorf("parent context manager is nil")
	}
	parent.operationMu.Lock()
	if parent.currentSession == nil {
		parent.operationMu.Unlock()
		return nil, fmt.Errorf("parent current session is unavailable")
	}
	sessionFolder := parent.sessionFolder
	parentSessionID := parent.currentSession.SessionID()
	activationParentID := parentSessionID
	if parent.parentVersionSet {
		activationParentID = parent.activationParentID
		parentVersion = parent.parentVersion
	}
	parent.operationMu.Unlock()
	return newContextManagerRevisionFromMessageList(sessionFolder, parentSessionID, activationParentID, parentVersion, messageList)
}

func newContextManagerRevisionFromMessageList(sessionFolder, parentSessionID, activationParentID string, parentVersion uint64, messageList []messages.Message) (*ContextManager, error) {
	revision, err := newContextManagerFromMessageList(sessionFolder, newSessionID(), parentSessionID, messageList)
	if err != nil {
		return nil, err
	}
	revision.activationParentID = activationParentID
	revision.parentVersion = parentVersion
	revision.parentVersionSet = true
	return revision, nil
}

func newContextManagerFromMessageList(sessionFolder, sessionID, parentSessionID string, messageList []messages.Message) (*ContextManager, error) {
	attachmentStore, err := newAttachmentStore(sessionFolder, sessionID)
	if err != nil {
		return nil, err
	}
	artifactStore, err := newArtifactStore(sessionFolder, sessionID)
	if err != nil {
		return nil, err
	}
	parentSessionID = strings.TrimSpace(parentSessionID)
	now := time.Now().UTC()
	metadata := sessionMetadata{
		ParentSessionID: parentSessionID,
		CreatedAt:       now,
	}
	if parentSessionID != "" {
		metadata.UsageStartIndex = len(messageList)
	}
	initialMessages := messages.CloneList(messageList)
	for i := range initialMessages {
		if initialMessages[i].Timestamp.IsZero() {
			initialMessages[i].Timestamp = now
		}
	}
	manager := &ContextManager{
		currentSession: session.NewWithTokenPersistence(
			sessionID,
			parentSessionID,
			initialMessages,
			attachmentStore,
			artifactStore,
			func(messageList []messages.Message) error {
				return appendSession(sessionFolder, sessionID, messageList)
			},
			metadata.UsageStartIndex,
			tokenMetadataWriter(sessionFolder, sessionID, metadata),
		),
		sessionFolder: sessionFolder,
	}
	state := manager.currentSession.TokenSnapshot()
	metadata.Tokens = &state
	if err := saveSessionMetadata(sessionFolder, sessionID, metadata); err != nil {
		return nil, err
	}
	if err := manager.flushFull(); err != nil {
		return nil, err
	}
	return manager, nil
}

// TokenCount returns the active session's context occupancy.
func (c *ContextManager) TokenCount() int {
	if c == nil {
		return 0
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return 0
	}
	return c.currentSession.TokenCount()
}

// HasTokenUsage reports whether occupancy has a provider-usage baseline.
func (c *ContextManager) HasTokenUsage() bool {
	if c == nil {
		return false
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	return c.currentSession != nil && c.currentSession.HasTokenUsage()
}

// TokenSnapshot reads count and baseline validity from the same active session.
func (c *ContextManager) TokenSnapshot() session.TokenState {
	if c == nil {
		return session.TokenState{}
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	return c.currentSession.TokenSnapshot()
}

func newSessionID() string {
	return "s_" + uuid.New().String()
}

// SwitchSession updates the on-disk pointer for callers that do not hold a
// manager. Long-lived agents should use ContextManager.SwitchSession or
// ContextManager.Activate so the in-memory current session changes atomically
// with the pointer.
func SwitchSession(sessionFolder string, sessionID string) error {
	if err := saveCurrentSession(sessionFolder, sessionID); err != nil {
		return err
	}
	return nil
}

func sessionFilesExist(sessionFolder, sessionID string) bool {
	if info, err := os.Stat(filepath.Join(sessionFolder, sessionID+".jsonl")); err == nil && info.Mode().IsRegular() {
		return true
	}
	if info, err := os.Stat(filepath.Join(sessionFolder, sessionID+".meta.json")); err == nil && info.Mode().IsRegular() {
		return true
	}
	return false
}

// SwitchSession loads sessionID and makes it the current session managed by c.
// Hooks stay attached to c, while session-scoped stores are replaced together
// with the session. The on-disk current pointer is written before the in-memory
// pointer changes, so a failed activation leaves c unchanged.
func (c *ContextManager) SwitchSession(sessionID string) error {
	if c == nil {
		return fmt.Errorf("context manager is nil")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("session ID is empty")
	}
	if _, err := validateArtifactSessionID(sessionID); err != nil {
		return err
	}

	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if !sessionFilesExist(c.sessionFolder, sessionID) {
		return fmt.Errorf("session %q does not exist", sessionID)
	}

	candidate, err := LoadContextManagerFromSessionID(c.sessionFolder, sessionID)
	if err != nil {
		return err
	}
	if err := saveCurrentSession(c.sessionFolder, sessionID); err != nil {
		return err
	}
	c.currentSession = candidate.currentSession
	return nil
}

// Activate adopts a fully-created candidate manager's session without
// replacing the long-lived manager itself. It is useful for compaction and
// pruning, which create an unactivated revision first and only publish it once
// all preparation checks have succeeded.
func (c *ContextManager) Activate(candidate *ContextManager) error {
	if c == nil {
		return fmt.Errorf("context manager is nil")
	}
	if candidate == nil {
		return fmt.Errorf("candidate context manager is nil")
	}
	if filepath.Clean(candidate.GetSessionFolder()) != filepath.Clean(c.GetSessionFolder()) {
		return fmt.Errorf("candidate session folder differs from context manager")
	}
	candidate.operationMu.Lock()
	candidateSession := candidate.currentSession
	candidateActivationParentID := ""
	candidateParentVersion := uint64(0)
	candidateParentVersionSet := candidate.parentVersionSet
	if candidateSession != nil {
		candidateActivationParentID = candidate.activationParentID
	}
	if candidateParentVersionSet {
		candidateParentVersion = candidate.parentVersion
	}
	candidate.operationMu.Unlock()
	if candidateSession == nil || candidateSession.SessionID() == "" {
		return fmt.Errorf("candidate session is unavailable")
	}
	if !sessionFilesExist(c.sessionFolder, candidateSession.SessionID()) {
		return fmt.Errorf("candidate session %q does not exist", candidateSession.SessionID())
	}

	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if candidateParentVersionSet {
		if c.currentSession == nil ||
			c.currentSession.SessionID() != candidateActivationParentID ||
			c.currentSession.Version() != candidateParentVersion {
			return fmt.Errorf("candidate session is based on a stale parent session")
		}
	}
	if err := saveCurrentSession(c.sessionFolder, candidateSession.SessionID()); err != nil {
		return err
	}
	c.currentSession = candidateSession
	return nil
}

func (c *ContextManager) CloneMessageList() []messages.Message {
	if c == nil {
		return nil
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return nil
	}
	return c.currentSession.CloneMessageList()
}

// MessageSnapshot returns the active session's messages and generation from a
// single manager operation.
func (c *ContextManager) MessageSnapshot() ([]messages.Message, uint64) {
	if c == nil {
		return nil, 0
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return nil, 0
	}
	return c.currentSession.Snapshot()
}

func (c *ContextManager) GetSessionFolder() string {
	return c.sessionFolder
}

func (c *ContextManager) GetSessionID() string {
	if c == nil {
		return ""
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return ""
	}
	return c.currentSession.SessionID()
}

// CurrentSessionID returns the ID of the session currently managed by c.
func (c *ContextManager) CurrentSessionID() string {
	return c.GetSessionID()
}

// ArtifactStoreRoot returns the current session's artifact directory for
// maintenance and diagnostics.
func (c *ContextManager) ArtifactStoreRoot() string {
	if c == nil {
		return ""
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return ""
	}
	return c.currentSession.ArtifactStoreRoot()
}

// GetParentSessionID returns the session this context was derived from, or an
// empty string for a root session or a session with no recorded lineage.
func (c *ContextManager) GetParentSessionID() string {
	if c == nil {
		return ""
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return ""
	}
	return c.currentSession.ParentSessionID()
}

func (c *ContextManager) StoreArtifact(mimeType string, data []byte, metadata ArtifactMetadata) (ArtifactFile, error) {
	if c == nil {
		return ArtifactFile{}, fmt.Errorf("artifact store is unavailable")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return ArtifactFile{}, fmt.Errorf("artifact store is unavailable")
	}
	return c.currentSession.StoreArtifact(mimeType, data, metadata)
}

// ScreenshotStateCount returns how many <state> messages in the active session
// carry a screenshot, without copying the message list.
func (c *ContextManager) ScreenshotStateCount() int {
	if c == nil {
		return 0
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return 0
	}
	count := 0
	c.currentSession.ViewMessages(func(currentMessages []messages.Message) {
		for _, message := range currentMessages {
			if message.Role != messages.MessageRoleState {
				continue
			}
			for _, attachment := range message.Attachments {
				if attachment.Source == messages.AttachmentSourceScreenshotObservation {
					count++
					break
				}
			}
		}
	})
	return count
}

// AppendResult identifies the messages committed by one append after hooks and
// protocol repair have run. Start is their index in the active session.
type AppendResult struct {
	SessionID string
	Start     int
	Messages  []messages.Message
}

func (c *ContextManager) appendToList(messagesToAppend []messages.Message) (AppendResult, error) {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return AppendResult{}, fmt.Errorf("current session is unavailable")
	}
	result := AppendResult{SessionID: c.currentSession.SessionID()}
	c.currentSession.ViewMessages(func(currentMessages []messages.Message) {
		result.Start = len(currentMessages)
		messagesToAppend = repairToolCallTailBeforeAppend(currentMessages, messagesToAppend)
	})
	if len(messagesToAppend) == 0 {
		return result, nil
	}
	now := time.Now().UTC()
	for i := range messagesToAppend {
		if messagesToAppend[i].Timestamp.IsZero() {
			messagesToAppend[i].Timestamp = now
		}
	}

	if err := c.currentSession.AppendMessages(messagesToAppend); err != nil {
		logging.Errorf("agent", "cm", "Failed to append messages to session %v", c.currentSession.SessionID())
		return AppendResult{}, err
	}

	result.Messages = messages.CloneList(messagesToAppend)
	return result, nil
}

func (c *ContextManager) flushFull() error {
	if c == nil || c.currentSession == nil {
		return fmt.Errorf("current session is unavailable")
	}
	return appendSession(c.sessionFolder, c.currentSession.SessionID(), c.currentSession.CloneMessageList())
}

func (c *ContextManager) AppendMessage(message messages.Message) error {
	return c.AppendMessages([]messages.Message{message})
}

// AppendMessages applies append hooks to a batch and persists the resulting
// messages in one context-manager append operation. This keeps related
// protocol messages, such as a tool call and its result, together.
func (c *ContextManager) AppendMessages(messagesToAppend []messages.Message) error {
	_, err := c.AppendMessagesWithResult(messagesToAppend)
	return err
}

// AppendMessagesWithResult returns only the messages durably added by this
// operation, including hook-inserted messages and their assigned timestamps.
func (c *ContextManager) AppendMessagesWithResult(messagesToAppend []messages.Message) (AppendResult, error) {
	if c == nil {
		return AppendResult{}, fmt.Errorf("context manager is nil")
	}
	c.hooksMu.RLock()
	hooks := append([]AppendMessageHook(nil), c.appendHooks...)
	c.hooksMu.RUnlock()

	messageList := messages.CloneList(messagesToAppend)
	for _, entry := range hooks {
		var next []messages.Message
		for _, current := range messageList {
			result := entry(current.Clone())
			next = append(next, messages.CloneList(result.Before)...)
			if result.Message != nil {
				next = append(next, result.Message.Clone())
			}
			next = append(next, messages.CloneList(result.After)...)
		}
		messageList = next
	}

	if len(messageList) == 0 {
		return AppendResult{}, nil
	}
	return c.appendToList(messageList)
}

func (c *ContextManager) AddAppendMessageHook(hook AppendMessageHook) {
	if c == nil || hook == nil {
		return
	}
	c.hooksMu.Lock()
	c.appendHooks = append(c.appendHooks, hook)
	c.hooksMu.Unlock()
}

func (c *ContextManager) AddAppendMessageHooks(hooks []AppendMessageHook) {
	if c == nil || len(hooks) == 0 {
		return
	}
	c.hooksMu.Lock()
	defer c.hooksMu.Unlock()
	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		c.appendHooks = append(c.appendHooks, hook)
	}
}

func (c *ContextManager) IsEmpty() bool {
	if c == nil {
		return true
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	return c.currentSession == nil || c.currentSession.IsEmpty()
}

type MessageListDump struct {
	SessionID       string             `json:"session_id"`
	ParentSessionID string             `json:"parent_session_id,omitempty"`
	Messages        []messages.Message `json:"messages"`
}

func (c *ContextManager) MessageListDump() MessageListDump {
	if c == nil {
		return MessageListDump{}
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return MessageListDump{}
	}
	return MessageListDump{
		SessionID:       c.currentSession.SessionID(),
		ParentSessionID: c.currentSession.ParentSessionID(),
		Messages:        c.currentSession.CloneMessageList(),
	}
}

// MessageListWindow is a deep copy of one contiguous range of the active
// session. Start is the index of Messages[0] in the full list, and Total is
// the full list length when the range was taken.
type MessageListWindow struct {
	SessionID string
	Total     int
	Start     int
	Messages  []messages.Message
}

// MessageListWindow copies only the range chosen by selectRange, so callers
// that page through a long session do not clone the whole message list.
// selectRange receives a read-only view and must not retain it; the returned
// range is clamped to the list.
func (c *ContextManager) MessageListWindow(selectRange func(sessionID string, view []messages.Message) (start, end int)) MessageListWindow {
	if c == nil || selectRange == nil {
		return MessageListWindow{}
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return MessageListWindow{}
	}
	window := MessageListWindow{SessionID: c.currentSession.SessionID()}
	c.currentSession.ViewMessages(func(view []messages.Message) {
		window.Total = len(view)
		start, end := selectRange(window.SessionID, view)
		if end > len(view) {
			end = len(view)
		}
		if start < 0 {
			start = 0
		}
		if start > end {
			start = end
		}
		window.Start = start
		window.Messages = messages.CloneList(view[start:end])
	})
	return window
}

// StoreAttachment persists attachment bytes on disk and returns metadata only.
func (c *ContextManager) StoreAttachment(mimeType string, data []byte) (messages.Attachment, error) {
	if c == nil {
		return messages.Attachment{}, fmt.Errorf("attachment store is unavailable")
	}
	if len(data) == 0 {
		return messages.Attachment{}, fmt.Errorf("attachment data is empty")
	}
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return messages.Attachment{}, fmt.Errorf("attachment store is unavailable")
	}
	return c.currentSession.StoreAttachment(mimeType, data)
}

// ReadAttachment returns a registered attachment from the active context. The
// caller supplies only the opaque attachment filename; arbitrary paths are
// rejected.
func (c *ContextManager) ReadAttachment(attachmentID string) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("context manager is nil")
	}
	attachmentID = strings.TrimSpace(attachmentID)
	if attachmentID == "" || filepath.Base(attachmentID) != attachmentID || strings.ContainsAny(attachmentID, `/\\`) {
		return nil, fmt.Errorf("invalid attachment ID")
	}

	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return nil, fmt.Errorf("current session is unavailable")
	}
	filePath := ""
	c.currentSession.ViewMessages(func(currentMessages []messages.Message) {
		for _, message := range currentMessages {
			for _, attachment := range message.Attachments {
				candidate := strings.TrimSpace(attachment.FilePath)
				if candidate != "" && filepath.Base(candidate) == attachmentID {
					filePath = candidate
					return
				}
			}
		}
	})
	sessionFolder := c.sessionFolder

	if filePath == "" {
		return nil, fmt.Errorf("attachment is not present in the active context")
	}
	root, err := filepath.Abs(sessionFolder)
	if err != nil {
		return nil, fmt.Errorf("resolve session folder: %w", err)
	}
	candidate, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("resolve attachment path: %w", err)
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("attachment path is outside the session folder")
	}

	data, err := os.ReadFile(candidate)
	if err != nil {
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("attachment is empty")
	}
	return data, nil
}
