package agent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aiden-agent/internal/agent/model"
	"aiden-agent/internal/ble"
)

type MemoryPlane interface {
	NewEpisodeRecorder(req MemoryRetrieveRequest, retrieved MemoryContext) *EpisodeRecorder
	CommitEpisode(ctx context.Context, episode TaskEpisode) error
}

type FilesystemMemoryPlane struct {
	memoryDir             string
	extraction            MemoryExtractionConfig
	episodes              *TaskEpisodeStore
	device                *DeviceMemoryStore
	longTerm              *LongTermMemoryStore
	logger                *Logger
	memoryWorkerMu        sync.RWMutex
	episodeProcessor      *episodeMemoryProcessor
	episodeIdleDelay      time.Duration
	notificationContext   *NotificationContext
	notificationProcessor *NotificationMemoryProcessor
	memoryWorker          *MemoryWorker
	storageWriteGate      StorageWriteGate
}

type MemoryRetrieveRequest struct {
	Input        string
	Attachments  []InputAttachment
	ToolNames    []string
	EpisodeID    string
	DeviceID     string
	CurrentHints CurrentEnvironmentHints
}

type CurrentEnvironmentHints struct {
	ScreenshotWidth  int
	ScreenshotHeight int
	Language         string
	AppName          string
}

type MemoryContext struct {
	Planner  RoleMemoryContext
	Verifier RoleMemoryContext
	Common   RoleMemoryContext
}

type RoleMemoryContext struct {
	SessionSummary   string
	Profile          string
	DeviceProfile    []MemoryHit
	AppProfiles      []MemoryHit
	Procedures       []MemoryHit
	SimilarEpisodes  []MemoryHit
	CalibrationNotes []MemoryHit
	FailureModes     []MemoryHit
	Conflicts        []MemoryHit
}

type MemoryHit struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Title         string            `json:"title,omitempty"`
	Summary       string            `json:"summary,omitempty"`
	Content       string            `json:"content,omitempty"`
	Priority      int               `json:"priority,omitempty"`
	Confidence    float64           `json:"confidence,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	Entities      []string          `json:"entities,omitempty"`
	Aliases       []string          `json:"-"`
	Source        string            `json:"source,omitempty"`
	FilePath      string            `json:"file_path,omitempty"`
	Applicability map[string]string `json:"applicability,omitempty"`
	EvidenceRefs  []MemorySourceRef `json:"evidence_refs,omitempty"`
	// 新增 procedure/导航相关结构化字段，用于 Planner 渲染
	Steps    []ProcedureStep `json:"steps,omitempty"`
	AppName  string          `json:"app_name,omitempty"`
	AppID    string          `json:"-"`
	PageName string          `json:"page_name,omitempty"`
}

const (
	episodeMemoryProcessNowMaxAttempts      = 2048
	episodeMemoryProcessNowBusyDelay        = 50 * time.Millisecond
	notificationMemoryProcessNowMaxAttempts = 2048
	notificationMemoryProcessNowBusyDelay   = 50 * time.Millisecond
)

const defaultMemoryDeviceID = "default"

type memorySearchQuery struct {
	Terms    []string
	Tags     []string
	Entities []string
	Limit    int
}

type FilesystemMemoryPlaneOption func(*FilesystemMemoryPlane)

// WithMemoryPlaneLongTermStore shares an existing long-term memory store with
// the memory plane instead of creating a second cache for the same directory.
func WithMemoryPlaneLongTermStore(store *LongTermMemoryStore) FilesystemMemoryPlaneOption {
	return func(p *FilesystemMemoryPlane) { p.longTerm = store }
}

func NewFilesystemMemoryPlane(memoryDir string, extraction MemoryExtractionConfig, logger *Logger, opts ...FilesystemMemoryPlaneOption) *FilesystemMemoryPlane {
	if strings.TrimSpace(memoryDir) == "" {
		return nil
	}
	idleDelay := extraction.EpisodeMemoryIdleDelayOrDefault()
	p := &FilesystemMemoryPlane{
		memoryDir:        memoryDir,
		extraction:       extraction,
		episodes:         NewTaskEpisodeStore(filepath.Join(memoryDir, "episodes")),
		device:           NewDeviceMemoryStore(filepath.Join(memoryDir, "device")),
		logger:           logger,
		episodeIdleDelay: idleDelay,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.longTerm == nil {
		p.longTerm = NewLongTermMemoryStore(filepath.Join(memoryDir, "long_term"), WithLifecycleDir(filepath.Join(memoryDir, "lifecycle")))
	}
	return p
}

func (p *FilesystemMemoryPlane) LongTerm() *LongTermMemoryStore {
	if p == nil {
		return nil
	}
	return p.longTerm
}

func (p *FilesystemMemoryPlane) SetStorageWriteGate(gate StorageWriteGate) {
	if p == nil {
		return
	}
	p.memoryWorkerMu.Lock()
	p.storageWriteGate = gate
	if p.notificationContext != nil {
		p.notificationContext.SetStorageWriteGate(gate)
	}
	p.memoryWorkerMu.Unlock()
}

// StartMemoryWorker registers all built-in memory scenarios before starting
// the shared idle worker. This preserves the Episode -> Notification order
// even when the configured idle delay is very short.
func (p *FilesystemMemoryPlane) StartMemoryWorker(models model.Model) error {
	if p == nil || strings.TrimSpace(p.memoryDir) == "" {
		return nil
	}
	if models == nil {
		return p.StartNotificationMemory(nil)
	}
	p.memoryWorkerMu.Lock()
	defer p.memoryWorkerMu.Unlock()
	if p.episodeProcessor != nil && p.notificationProcessor != nil {
		p.notificationProcessor.logger = p.logger
		return nil
	}
	if p.episodeProcessor != nil || p.notificationProcessor != nil {
		return fmt.Errorf("memory worker is partially initialized")
	}
	notificationContext, err := NewNotificationContext(p.memoryDir, nil)
	if err != nil {
		return err
	}
	notificationContext.SetStorageWriteGate(p.storageWriteGate)
	episodeProcessor := newEpisodeMemoryProcessor(p, models)
	notificationProcessor := NewNotificationMemoryProcessor(notificationContext, p.memoryDir, p.longTerm, models)
	notificationProcessor.logger = p.logger
	worker := newMemoryWorkerWithProcessors(nil, p.episodeIdleDelay)
	if err := worker.AddProcessor(episodeProcessor); err != nil {
		worker.Stop()
		return err
	}
	if err := worker.AddProcessor(notificationProcessor); err != nil {
		worker.Stop()
		return err
	}
	if err := worker.Start(); err != nil {
		worker.Stop()
		return err
	}
	p.memoryWorker = worker
	p.episodeProcessor = episodeProcessor
	p.notificationContext = notificationContext
	p.notificationProcessor = notificationProcessor
	return nil
}

func (p *FilesystemMemoryPlane) StartEpisodeMemory(models model.Model) error {
	if p == nil || strings.TrimSpace(p.memoryDir) == "" || models == nil {
		return nil
	}
	p.memoryWorkerMu.Lock()
	defer p.memoryWorkerMu.Unlock()
	if p.episodeProcessor != nil {
		return nil
	}
	processor := newEpisodeMemoryProcessor(p, models)
	if p.memoryWorker == nil {
		p.memoryWorker = newMemoryWorkerWithProcessors(nil, p.episodeIdleDelay)
	}
	worker := p.memoryWorker
	if err := worker.AddProcessor(processor); err != nil {
		return err
	}
	if err := worker.Start(); err != nil {
		worker.RemoveProcessor(processor)
		return err
	}
	p.episodeProcessor = processor
	return nil
}

func (p *FilesystemMemoryPlane) StopEpisodeMemory() {
	if p == nil {
		return
	}
	p.memoryWorkerMu.Lock()
	worker := p.memoryWorker
	processor := p.episodeProcessor
	p.episodeProcessor = nil
	p.memoryWorkerMu.Unlock()
	if worker == nil || processor == nil {
		return
	}
	worker.RemoveProcessor(processor)
	if worker.ProcessorCount() == 0 {
		worker.Stop()
		p.memoryWorkerMu.Lock()
		if p.memoryWorker == worker {
			p.memoryWorker = nil
		}
		p.memoryWorkerMu.Unlock()
	}
}

func (p *FilesystemMemoryPlane) StartNotificationMemory(models model.Model) error {
	if p == nil || strings.TrimSpace(p.memoryDir) == "" {
		return nil
	}
	p.memoryWorkerMu.Lock()
	defer p.memoryWorkerMu.Unlock()
	if p.notificationProcessor != nil {
		return nil
	}
	ctx, err := NewNotificationContext(p.memoryDir, nil)
	if err != nil {
		return err
	}
	ctx.SetStorageWriteGate(p.storageWriteGate)
	processor := NewNotificationMemoryProcessor(ctx, p.memoryDir, p.longTerm, models)
	processor.logger = p.logger
	if p.memoryWorker == nil {
		p.memoryWorker = newMemoryWorkerWithProcessors(nil, p.episodeIdleDelay)
	}
	worker := p.memoryWorker
	if err := worker.AddProcessor(processor); err != nil {
		return err
	}
	if err := worker.Start(); err != nil {
		worker.RemoveProcessor(processor)
		return err
	}
	p.notificationContext = ctx
	p.notificationProcessor = processor
	return nil
}

func (p *FilesystemMemoryPlane) StopNotificationMemory() {
	if p == nil {
		return
	}
	p.memoryWorkerMu.Lock()
	worker := p.memoryWorker
	processor := p.notificationProcessor
	p.notificationProcessor = nil
	p.notificationContext = nil
	p.memoryWorkerMu.Unlock()
	if worker == nil || processor == nil {
		return
	}
	worker.RemoveProcessor(processor)
	if worker.ProcessorCount() == 0 {
		worker.Stop()
		p.memoryWorkerMu.Lock()
		if p.memoryWorker == worker {
			p.memoryWorker = nil
		}
		p.memoryWorkerMu.Unlock()
	}
}

func (p *FilesystemMemoryPlane) MemoryTaskStarted() {
	if p == nil {
		return
	}
	p.memoryWorkerMu.RLock()
	worker := p.memoryWorker
	p.memoryWorkerMu.RUnlock()
	if worker != nil {
		worker.TaskStarted()
	}
}

func (p *FilesystemMemoryPlane) MemoryTaskFinished() {
	if p == nil {
		return
	}
	p.memoryWorkerMu.RLock()
	worker := p.memoryWorker
	p.memoryWorkerMu.RUnlock()
	if worker != nil {
		worker.TaskFinished()
	}
}

// NotifyMemory wakes the shared worker's idle timer without polling. It is
// used by durable producers such as notification ingestion; foreground work
// still controls when the worker may actually run.
func (p *FilesystemMemoryPlane) NotifyMemory() {
	if p == nil {
		return
	}
	p.memoryWorkerMu.RLock()
	worker := p.memoryWorker
	p.memoryWorkerMu.RUnlock()
	if worker != nil {
		worker.Notify()
	}
}

func (p *FilesystemMemoryPlane) ProcessEpisodeMemoryNow(ctx context.Context, episodeID string) (episodeMemoryEpisodeStatus, []string, error) {
	if p == nil {
		return episodeMemoryEpisodeStatus{}, nil, fmt.Errorf("episode memory is not configured")
	}
	p.memoryWorkerMu.RLock()
	worker := p.memoryWorker
	processor := p.episodeProcessor
	p.memoryWorkerMu.RUnlock()
	if worker == nil || processor == nil {
		return episodeMemoryEpisodeStatus{}, nil, fmt.Errorf("episode memory worker is not configured")
	}
	if processor == nil || processor.state == nil {
		return episodeMemoryEpisodeStatus{}, nil, fmt.Errorf("episode memory processor is not configured")
	}
	ctx = context.WithValue(ctx, episodeMemoryImmediateRetryKey{}, episodeID)
	stateKey := episodeMemoryStateKey(episodeID, episodeMemoryExtractorVersion)
	var status episodeMemoryEpisodeStatus
	found := false
	for attempt := 0; attempt < episodeMemoryProcessNowMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return episodeMemoryEpisodeStatus{}, nil, err
		}
		state, err := processor.state.Snapshot()
		if err != nil {
			return episodeMemoryEpisodeStatus{}, nil, err
		}
		if status, found = state.Episodes[stateKey]; found && episodeMemoryProcessNowTerminal(status) {
			break
		}
		var result MemoryBatchResult
		result, err = worker.ProcessProcessorNow(ctx, processor)
		if errors.Is(err, errMemoryWorkerBusy) {
			select {
			case <-ctx.Done():
				return episodeMemoryEpisodeStatus{}, nil, ctx.Err()
			case <-time.After(episodeMemoryProcessNowBusyDelay):
			}
			continue
		}
		if err != nil {
			return episodeMemoryEpisodeStatus{}, nil, err
		}
		state, err = processor.state.Snapshot()
		if err != nil {
			return episodeMemoryEpisodeStatus{}, nil, err
		}
		if status, found = state.Episodes[stateKey]; found && episodeMemoryProcessNowTerminal(status) {
			break
		}
		if !result.HasPending {
			break
		}
	}
	if !found || !episodeMemoryProcessNowTerminal(status) {
		return episodeMemoryEpisodeStatus{}, nil, fmt.Errorf("episode %q was not processed", episodeID)
	}
	items, err := p.device.readAll()
	if err != nil {
		return episodeMemoryEpisodeStatus{}, nil, err
	}
	memoryIDs := make([]string, 0)
	for _, item := range items {
		if hasEpisodeEvidence(item.EvidenceRefs, episodeID) {
			memoryIDs = append(memoryIDs, item.ID)
		}
	}
	return status, memoryIDs, nil
}

// SeedNotificationMemoryForBenchmark installs durable notification fixtures.
// The server exposes this only through its benchmark-token-protected endpoint.
func (p *FilesystemMemoryPlane) SeedNotificationMemoryForBenchmark(ctx context.Context, events []ble.NotificationEvent) ([]NotificationRecord, error) {
	if p == nil {
		return nil, fmt.Errorf("notification memory is not configured")
	}
	p.memoryWorkerMu.RLock()
	processor := p.notificationProcessor
	p.memoryWorkerMu.RUnlock()
	if processor == nil || processor.context == nil {
		return nil, fmt.Errorf("notification memory worker is not configured")
	}
	return processor.context.seedForBenchmark(ctx, events)
}

// ProcessNotificationMemoryNow drains seeded durable notification records
// through the same processor and shared worker used during normal idle work.
// It is a benchmark control path, not a foreground runtime API.
func (p *FilesystemMemoryPlane) ProcessNotificationMemoryNow(ctx context.Context) (string, []string, error) {
	return p.processNotificationMemoryNow(
		ctx,
		notificationMemoryProcessNowMaxAttempts,
		notificationMemoryProcessNowBusyDelay,
	)
}

func (p *FilesystemMemoryPlane) processNotificationMemoryNow(ctx context.Context, maxAttempts int, busyDelay time.Duration) (string, []string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	if busyDelay < 0 {
		busyDelay = 0
	}
	if p == nil {
		return "", nil, fmt.Errorf("notification memory is not configured")
	}
	p.memoryWorkerMu.RLock()
	worker := p.memoryWorker
	processor := p.notificationProcessor
	p.memoryWorkerMu.RUnlock()
	if worker == nil || processor == nil || processor.context == nil {
		return "", nil, fmt.Errorf("notification memory worker is not configured")
	}
	processor.context.setConsumeDisabledForBenchmark(true)
	defer processor.context.setConsumeDisabledForBenchmark(false)
	pendingBefore, err := processor.context.ReadPendingAll(ctx)
	if err != nil {
		return "", nil, err
	}
	seededContextIDs := make(map[string]struct{}, len(pendingBefore))
	for _, record := range pendingBefore {
		seededContextIDs[record.ContextID] = struct{}{}
	}
	var last MemoryBatchResult
	busy := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		result, err := worker.ProcessProcessorNow(ctx, processor)
		if errors.Is(err, errMemoryWorkerBusy) {
			busy = true
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-time.After(busyDelay):
			}
			continue
		}
		busy = false
		last = result
		pending, readErr := processor.context.ReadPending(ctx, notificationMemoryBatchLimit)
		if readErr != nil {
			return "", nil, readErr
		}
		if err != nil && len(pending) > 0 {
			return "", nil, err
		}
		if len(pending) == 0 && !result.HasPending {
			break
		}
		if len(pending) == 0 && err != nil {
			// A seeded run may have no BLE reader. The durable batch has still
			// committed successfully, so a consume-side error is irrelevant here.
			break
		}
	}
	if busy {
		return "", nil, errMemoryWorkerBusy
	}
	if last.HasPending {
		pending, err := processor.context.ReadPending(ctx, notificationMemoryBatchLimit)
		if err != nil {
			return "", nil, err
		}
		if len(pending) > 0 {
			return "", nil, fmt.Errorf("notification memory benchmark did not drain durable pending records")
		}
	}
	state := processor.context.State()
	ids := make([]string, 0)
	for _, store := range []*LongTermMemoryStore{processor.temporary, processor.longTerm} {
		if store == nil {
			continue
		}
		memories, err := store.searchAll(ctx)
		if err != nil {
			return "", nil, err
		}
		for _, memory := range memories {
			if memory.Status != "active" || !memoryReferencesNotificationIDs(memory.SourceRefs, seededContextIDs) {
				continue
			}
			ids = append(ids, memory.ID)
		}
	}
	sort.Strings(ids)
	return state.MemoryCursor, ids, nil
}

func memoryReferencesNotificationIDs(refs []MemorySourceRef, contextIDs map[string]struct{}) bool {
	for _, ref := range refs {
		if ref.Type != "notification" {
			continue
		}
		if _, ok := contextIDs[ref.ID]; ok {
			return true
		}
		for _, eventID := range ref.EventIDs {
			if _, ok := contextIDs[eventID]; ok {
				return true
			}
		}
	}
	return false
}

func episodeMemoryProcessNowTerminal(status episodeMemoryEpisodeStatus) bool {
	return status.Status == episodeMemoryStatusDone || status.Status == episodeMemoryStatusIgnored
}

func (p *FilesystemMemoryPlane) NewEpisodeRecorder(req MemoryRetrieveRequest, retrieved MemoryContext) *EpisodeRecorder {
	return NewPersistentEpisodeRecorder(req, retrieved, p.episodes)
}

func (p *FilesystemMemoryPlane) CommitEpisode(ctx context.Context, episode TaskEpisode) error {
	if p == nil || p.memoryDir == "" || strings.TrimSpace(episode.UserGoal) == "" {
		return nil
	}
	if err := p.commitEpisodeTrace(ctx, episode); err != nil {
		return err
	}
	p.commitEpisodeMaintenance(ctx, episode)
	return nil
}

func (p *FilesystemMemoryPlane) commitEpisodeTrace(ctx context.Context, episode TaskEpisode) error {
	if p == nil || p.memoryDir == "" || strings.TrimSpace(episode.UserGoal) == "" {
		return nil
	}
	if _, err := p.episodes.AddEpisode(ctx, episode); err != nil {
		return err
	}
	return nil
}

func (p *FilesystemMemoryPlane) commitEpisodeMaintenance(ctx context.Context, episode TaskEpisode) {
	if p == nil || p.memoryDir == "" || strings.TrimSpace(episode.UserGoal) == "" {
		return
	}
	if err := p.extractDeviceLessons(ctx, episode); err != nil && p.logger != nil {
		p.logger.Warn("[memory] deterministic device profile extraction failed: %v", err)
	}
	p.memoryWorkerMu.RLock()
	worker := p.memoryWorker
	p.memoryWorkerMu.RUnlock()
	if worker != nil {
		worker.Notify()
	}
}

func (p *FilesystemMemoryPlane) extractDeviceLessons(ctx context.Context, episode TaskEpisode) error {
	if p.device == nil || !episodeHasTaskTrace(episode) {
		return nil
	}
	deviceID := firstNonEmptyString([]string{episode.DeviceScope["device_id"], defaultMemoryDeviceID})
	now := time.Now().UTC().Format(time.RFC3339Nano)

	if err := p.recordDeviceProfile(ctx, episode, deviceID, now); err != nil {
		return err
	}
	if err := p.recordAppProfiles(ctx, episode, deviceID, now); err != nil {
		return err
	}

	return nil
}

func (p *FilesystemMemoryPlane) recordDeviceProfile(ctx context.Context, episode TaskEpisode, deviceID, now string) error {
	screen := inferEpisodeScreen(episode.Events)
	if screen == "" {
		return nil
	}
	_, err := p.device.Upsert(ctx, DeviceMemoryItem{
		ID:         "device_" + safePathName(deviceID),
		Type:       "device_profile",
		Status:     "active",
		Title:      "Observed device profile",
		Content:    "Observed screenshot size " + screen + " during task execution.",
		DeviceID:   deviceID,
		Tags:       []string{"device", "screen"},
		Confidence: 0.7,
		TTL:        "90d",
		UpdatedAt:  now,
		Applicability: map[string]string{
			"screen": screen,
		},
		EvidenceRefs: []MemorySourceRef{{Type: "episode", ID: episode.ID}},
	})
	return err
}

func (p *FilesystemMemoryPlane) recordAppProfiles(ctx context.Context, episode TaskEpisode, deviceID, now string) error {
	pagesByApp := observedPagesByApp(episode.Events)
	toolsByApp := observedToolsByApp(episode.Events)

	// 后备：如果没有从 verifier observed_state 中抽到 app，退回到老的 inferEpisodeApps
	// 逻辑（基于 entities），这样没有 observed_state 的测试仍能写入 app_profile。
	if len(pagesByApp) == 0 && len(toolsByApp) == 0 {
		for _, app := range inferEpisodeApps(episode) {
			toolsByApp[app] = episodeToolSequence(episode.Events)
		}
	}

	if len(pagesByApp) == 0 && len(toolsByApp) == 0 {
		return nil
	}
	allApps := make(map[string]bool)
	for app := range pagesByApp {
		allApps[app] = true
	}
	for app := range toolsByApp {
		allApps[app] = true
	}
	for app := range allApps {
		appID := "app_" + stableMemoryID(app)
		existing, found, err := p.device.Get(ctx, appID)
		if err != nil {
			return err
		}
		if !found {
			existing = DeviceMemoryItem{
				ID:         appID,
				Type:       "app_profile",
				Status:     "active",
				Title:      "App profile: " + app,
				DeviceID:   deviceID,
				AppID:      app,
				AppName:    app,
				Entities:   []string{app},
				Aliases:    []string{app},
				Confidence: 0.65,
				TTL:        "60d",
			}
		}
		existing.PagesSeen = mergeUniqueStrings(existing.PagesSeen, pagesByApp[app])
		existing.ToolsUsed = mergeUniqueStrings(existing.ToolsUsed, toolsByApp[app])
		existing.Confidence = clampConfidence(existing.Confidence + 0.05)
		if existing.Confidence == 0 {
			existing.Confidence = 0.7
		}
		existing.UpdatedAt = now
		existing.EvidenceRefs = appendUniqueMemoryRef(existing.EvidenceRefs, MemorySourceRef{Type: "episode", ID: episode.ID})

		var content strings.Builder
		if len(existing.PagesSeen) > 0 {
			content.WriteString("Pages observed: ")
			content.WriteString(strings.Join(existing.PagesSeen, ", "))
			content.WriteString("\n")
		}
		if len(existing.ToolsUsed) > 0 {
			content.WriteString("Tools used: ")
			content.WriteString(strings.Join(existing.ToolsUsed, ", "))
			content.WriteString("\n")
		}
		existing.Content = strings.TrimSpace(content.String())
		if existing.Content == "" {
			existing.Content = "App was referenced in task goal or extracted entities: " + app
		}

		if _, err := p.device.Upsert(ctx, existing); err != nil {
			return err
		}
	}
	return nil
}

func normalizeSearchTerms(values []string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, value := range values {
		for _, token := range splitSearchTerms(value) {
			if token == "" || seen[token] {
				continue
			}
			seen[token] = true
			terms = append(terms, token)
		}
	}
	return terms
}

func splitSearchTerms(value string) []string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return nil
	}
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return strings.ContainsRune(" \t\n\r，。,.、；;：:\"'（）()[]【】{}<>/\\|+-=*?!", r)
	})
	var out []string
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		out = append(out, field)
	}
	if len([]rune(value)) <= 32 {
		out = append(out, value)
	}
	runes := []rune(value)
	if len(runes) >= 2 && len(runes) <= 16 {
		for i := 0; i < len(runes)-1; i++ {
			if isCJKRune(runes[i]) || isCJKRune(runes[i+1]) {
				out = append(out, string(runes[i:i+2]))
			}
		}
	}
	return out
}

func isCJKRune(r rune) bool {
	return (r >= 0x4e00 && r <= 0x9fff) || (r >= 0x3400 && r <= 0x4dbf)
}

func episodeHasTaskTrace(episode TaskEpisode) bool {
	for _, event := range episode.Events {
		if event.Type == runEventToolCall || event.Type == "tool_result" {
			return true
		}
	}
	return !episode.Outcome.Success && strings.TrimSpace(episode.Outcome.FailureReason) != ""
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func ttlExpiresAt(now time.Time, ttl string) string {
	d, ok := parseRetentionDuration(ttl)
	if !ok || d <= 0 {
		return ""
	}
	return now.Add(d).UTC().Format(time.RFC3339Nano)
}

func stableMemoryID(parts ...string) string {
	hash := sha1.New()
	for _, part := range parts {
		hash.Write([]byte(strings.TrimSpace(part)))
		hash.Write([]byte{0})
	}
	sum := hash.Sum(nil)
	return hex.EncodeToString(sum[:6])
}

func inferEpisodeScreen(events []TaskEpisodeEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		raw := firstNonEmptyString([]string{events[i].RawObservation, events[i].Observation})
		if raw == "" {
			continue
		}
		var result struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			continue
		}
		if result.Width > 0 && result.Height > 0 {
			return fmt.Sprintf("%dx%d", result.Width, result.Height)
		}
	}
	return ""
}

func inferEpisodeApps(episode TaskEpisode) []string {
	seen := map[string]bool{}
	var apps []string
	for _, value := range episode.Entities {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.Contains(value, "App") || strings.Contains(value, "app") || strings.Contains(value, "APP") {
			if !seen[value] {
				seen[value] = true
				apps = append(apps, value)
			}
		}
	}
	for _, event := range episode.Events {
		if event.ObservedState == nil {
			continue
		}
		app := strings.TrimSpace(event.ObservedState.AppName)
		if app == "" || seen[app] {
			continue
		}
		seen[app] = true
		apps = append(apps, app)
	}
	return apps
}

func clampConfidence(value float64) float64 {
	if value <= 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func appendUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func mergeUniqueStrings(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, v := range b {
		out = appendUniqueString(out, v)
	}
	return out
}

func appendUniqueMemoryRef(refs []MemorySourceRef, ref MemorySourceRef) []MemorySourceRef {
	for _, existing := range refs {
		if existing.Type == ref.Type && existing.ID == ref.ID {
			return refs
		}
	}
	return append(refs, ref)
}
