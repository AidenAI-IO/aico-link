package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aico-agent/internal/ble"
)

const (
	defaultNotificationConsumeLimit = 100
	defaultNotificationPendingLimit = 20
	maxNotificationFingerprints     = 4096
	trimNotificationFingerprintsTo  = 3072
	maxNotificationStoredText       = 4096
	maxNotificationStoredApp        = 255
)

// NotificationEventReader is the narrow dependency used by NotificationContext.
// Production passes ble.RequestEvents; tests can provide a deterministic reader.
type NotificationEventReader func(context.Context, string, string, int) (ble.EventPage, error)

// NotificationRecord is the durable envelope around the original BLE event.
// ID remains the producer-local cursor; ContextID is monotonic across BLE
// service generations and is therefore safe for Memory processing cursors.
type NotificationRecord struct {
	ble.NotificationEvent
	ContextID  string `json:"context_id"`
	Generation string `json:"generation,omitempty"`
}

// NotificationContextState is the durable consumer state. Cursors remain
// strings on disk so the file is safe for clients that cannot represent uint64
// precisely in JSON numbers.
type NotificationContextState struct {
	Version       int               `json:"version"`
	Generation    string            `json:"generation,omitempty"`
	SourceCursor  string            `json:"source_cursor,omitempty"`
	MemoryCursor  string            `json:"memory_cursor,omitempty"`
	StoredCursor  string            `json:"stored_cursor,omitempty"`
	OldestEventID string            `json:"oldest_event_id,omitempty"`
	GapCount      int               `json:"gap_count,omitempty"`
	Gaps          []NotificationGap `json:"gaps,omitempty"`
	UpdatedAt     string            `json:"updated_at"`
}

type NotificationGap struct {
	At                 string `json:"at"`
	Reason             string `json:"reason"`
	Generation         string `json:"generation,omitempty"`
	PreviousGeneration string `json:"previous_generation,omitempty"`
	FromID             string `json:"from_id,omitempty"`
	ToID               string `json:"to_id,omitempty"`
}

type notificationFingerprintFile struct {
	Version      int               `json:"version"`
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
}

type NotificationContext struct {
	mu           *sync.Mutex
	rootDir      string
	eventsDir    string
	statePath    string
	fingerPath   string
	reader       NotificationEventReader
	storageGate  StorageWriteGate
	state        NotificationContextState
	fingerprints map[string]string
	// consumeDisabled is used only by the benchmark fixture path. It lets a
	// seeded durable log be processed without requiring a live BLE socket.
	// Production ingestion always leaves this false.
	consumeDisabled bool
}

var notificationContextMutexes sync.Map

func notificationContextMutex(rootDir string) *sync.Mutex {
	key := filepath.Clean(rootDir)
	mutex, _ := notificationContextMutexes.LoadOrStore(key, &sync.Mutex{})
	return mutex.(*sync.Mutex)
}

func NewNotificationContext(memoryDir string, reader NotificationEventReader) (*NotificationContext, error) {
	if strings.TrimSpace(memoryDir) == "" {
		return nil, fmt.Errorf("notification memory directory is required")
	}
	if reader == nil {
		reader = func(ctx context.Context, since, generation string, limit int) (ble.EventPage, error) {
			return ble.RequestEvents(ctx, configuredBLEServiceSocketPath(), since, generation, limit)
		}
	}
	root := filepath.Join(memoryDir, "notifications")
	c := &NotificationContext{
		mu:           notificationContextMutex(root),
		rootDir:      root,
		eventsDir:    filepath.Join(root, "events"),
		statePath:    filepath.Join(root, "state.json"),
		fingerPath:   filepath.Join(root, "fingerprints.json"),
		reader:       reader,
		state:        NotificationContextState{Version: 1},
		fingerprints: map[string]string{},
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *NotificationContext) State() NotificationContextState {
	if c == nil {
		return NotificationContextState{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneNotificationContextState(c.state)
}

func (c *NotificationContext) SetStorageWriteGate(gate StorageWriteGate) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.storageGate = gate
	c.mu.Unlock()
}

// seedForBenchmark appends deterministic notification fixtures directly to the
// durable context log. It is intentionally package-private; the only caller
// is the benchmark-token-protected HTTP endpoint.
func (c *NotificationContext) seedForBenchmark(ctx context.Context, events []ble.NotificationEvent) ([]NotificationRecord, error) {
	if c == nil || len(events) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoadedLocked(); err != nil {
		return nil, err
	}
	accepted := make([]NotificationRecord, 0, len(events))
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return accepted, err
		}
		event = sanitizeNotificationEvent(event)
		fingerprint := notificationEventFingerprint(event)
		if contextID, exists := c.fingerprints[fingerprint]; exists {
			// Setup may be retried after the processor or HTTP response fails.
			// Return the original identity so benchmark fixture injection is
			// idempotent instead of reporting that zero events were persisted.
			accepted = append(accepted, NotificationRecord{
				NotificationEvent: event,
				ContextID:         contextID,
				Generation:        firstNonEmptyString([]string{c.state.Generation, "benchmark"}),
			})
			continue
		}
		contextID := parseCursorOrZero(c.state.StoredCursor) + 1
		record := NotificationRecord{
			NotificationEvent: event,
			ContextID:         strconv.FormatUint(contextID, 10),
			Generation:        firstNonEmptyString([]string{c.state.Generation, "benchmark"}),
		}
		if err := c.appendEventLocked(record); err != nil {
			return accepted, err
		}
		c.fingerprints[fingerprint] = record.ContextID
		pruneNotificationFingerprints(c.fingerprints)
		c.state.StoredCursor = record.ContextID
		c.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		accepted = append(accepted, record)
	}
	if err := c.persistLocked(); err != nil {
		return accepted, err
	}
	return accepted, nil
}

func (c *NotificationContext) setConsumeDisabledForBenchmark(disabled bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.consumeDisabled = disabled
	c.mu.Unlock()
}

// Consume fetches a page from ble_service, appends accepted events durably,
// and advances SourceCursor only after each event is synced to disk.
func (c *NotificationContext) Consume(ctx context.Context, limit int) ([]NotificationRecord, error) {
	if c == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		limit = defaultNotificationConsumeLimit
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.consumeDisabled {
		return nil, nil
	}
	if c.storageGate != nil && !c.storageGate.AllowWrite(StorageCapabilityNotificationContext) {
		return nil, nil
	}
	if err := c.ensureLoadedLocked(); err != nil {
		return nil, err
	}
	since := normalizeCursor(c.state.SourceCursor)
	generation := strings.TrimSpace(c.state.Generation)
	page, err := c.reader(ctx, since, generation, limit)
	if err != nil {
		return nil, err
	}
	if page.ResetRequired {
		previousGeneration := generation
		generation = page.Generation
		c.recordGapLocked("ble_generation_reset", generation, previousGeneration, since, page.OldestID)
		since = "0"
		c.state.Generation = generation
		c.state.SourceCursor = since
		if err := c.persistLocked(); err != nil {
			return nil, err
		}
		page, err = c.reader(ctx, since, generation, limit)
		if err != nil {
			return nil, err
		}
	}
	if page.Truncated {
		c.recordGapLocked("ble_ring_truncated", generation, "", since, page.OldestID)
	}
	if page.Generation != "" {
		c.state.Generation = page.Generation
	}
	if page.OldestID != "" {
		c.state.OldestEventID = page.OldestID
	}

	accepted := make([]NotificationRecord, 0, len(page.Events))
	for _, event := range page.Events {
		if err := ctx.Err(); err != nil {
			return accepted, err
		}
		event = sanitizeNotificationEvent(event)
		fingerprint := notificationEventFingerprint(event)
		if _, exists := c.fingerprints[fingerprint]; exists {
			if cursor, ok := parseNotificationCursor(event.ID); ok && cursor > parseCursorOrZero(c.state.SourceCursor) {
				c.state.SourceCursor = strconv.FormatUint(cursor, 10)
			}
			continue
		}
		contextCursor := parseCursorOrZero(c.state.StoredCursor) + 1
		record := NotificationRecord{
			NotificationEvent: event,
			ContextID:         strconv.FormatUint(contextCursor, 10),
			Generation:        c.state.Generation,
		}
		if err := c.appendEventLocked(record); err != nil {
			c.handleWriteErrorLocked(err)
			return accepted, err
		}
		c.fingerprints[fingerprint] = record.ContextID
		pruneNotificationFingerprints(c.fingerprints)
		accepted = append(accepted, record)
		c.state.StoredCursor = record.ContextID
		if cursor, ok := parseNotificationCursor(event.ID); ok {
			c.state.SourceCursor = strconv.FormatUint(cursor, 10)
		}
		c.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if len(page.Events) == 0 && page.LastID != "" {
		if cursor, ok := parseNotificationCursor(page.LastID); ok && cursor > parseCursorOrZero(c.state.SourceCursor) {
			c.state.SourceCursor = strconv.FormatUint(cursor, 10)
		}
	}
	if err := c.persistLocked(); err != nil {
		return accepted, err
	}
	return accepted, nil
}

func (c *NotificationContext) handleWriteErrorLocked(err error) {
	if c.storageGate != nil {
		c.storageGate.HandleWriteError(err)
	}
}

// ReadPending reads persisted events after MemoryCursor. It never contacts
// ble_service and is safe to call while the producer is disconnected.
func (c *NotificationContext) ReadPending(ctx context.Context, limit int) ([]NotificationRecord, error) {
	if c == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		limit = defaultNotificationPendingLimit
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoadedLocked(); err != nil {
		return nil, err
	}
	return c.readPendingLocked(ctx, parseCursorOrZero(c.state.MemoryCursor), limit)
}

// ReadPendingAll reads every persisted event after MemoryCursor. It is used by
// control paths that need a complete provenance set rather than a processing
// batch and therefore must not silently truncate at the normal batch limit.
func (c *NotificationContext) ReadPendingAll(ctx context.Context) ([]NotificationRecord, error) {
	if c == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoadedLocked(); err != nil {
		return nil, err
	}
	return c.readPendingLocked(ctx, parseCursorOrZero(c.state.MemoryCursor), 0)
}

// CommitProcessed advances MemoryCursor after the corresponding Memory write
// succeeds. The caller must pass the contiguous batch returned by ReadPending.
func (c *NotificationContext) CommitProcessed(ctx context.Context, events []NotificationRecord) error {
	if c == nil || len(events) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ensureLoadedLocked(); err != nil {
		return err
	}
	current := parseCursorOrZero(c.state.MemoryCursor)
	for _, event := range events {
		cursor, ok := parseNotificationCursor(event.ContextID)
		if !ok || cursor <= current {
			continue
		}
		if cursor != current+1 {
			return fmt.Errorf("notification memory cursor gap: current=%d next=%d", current, cursor)
		}
		current = cursor
	}
	if current == parseCursorOrZero(c.state.MemoryCursor) {
		return nil
	}
	c.state.MemoryCursor = strconv.FormatUint(current, 10)
	c.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return c.persistLocked()
}

// CleanupProcessedBefore removes only complete date shards whose records are
// all at or before MemoryCursor. It owns the context lock so StorageMonitor
// cannot race an append or cursor commit. A zero retention age means all
// processed shards are eligible (used by emergency cleanup).
func (c *NotificationContext) CleanupProcessedBefore(ctx context.Context, retentionAge time.Duration, now time.Time) (uint64, error) {
	if c == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoadedLocked(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(c.eventsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := now.UTC().Add(-retentionAge)
	memoryCursor := parseCursorOrZero(c.state.MemoryCursor)
	var freed uint64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(c.eventsDir, entry.Name())
		records, err := readNotificationRecordFile(path)
		if err != nil {
			return freed, err
		}
		if retentionAge > 0 && !notificationShardBeforeCutoff(entry.Name(), info.ModTime(), cutoff, records) {
			continue
		}
		processed := true
		for _, record := range records {
			cursor, ok := parseNotificationCursor(record.ContextID)
			if !ok || cursor > memoryCursor {
				processed = false
				break
			}
		}
		if !processed {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return freed, err
		}
		freed += uint64(info.Size())
	}
	return freed, nil
}

func (c *NotificationContext) EstimateCleanupProcessedBefore(ctx context.Context, retentionAge time.Duration, now time.Time) (uint64, error) {
	if c == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoadedLocked(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(c.eventsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := now.UTC().Add(-retentionAge)
	memoryCursor := parseCursorOrZero(c.state.MemoryCursor)
	var total uint64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		records, err := readNotificationRecordFileReadOnly(filepath.Join(c.eventsDir, entry.Name()))
		if err != nil {
			return total, err
		}
		if retentionAge > 0 && !notificationShardBeforeCutoff(entry.Name(), info.ModTime(), cutoff, records) {
			continue
		}
		processed := true
		for _, record := range records {
			cursor, ok := parseNotificationCursor(record.ContextID)
			if !ok || cursor > memoryCursor {
				processed = false
				break
			}
		}
		if processed {
			total += uint64(info.Size())
		}
	}
	return total, nil
}

func notificationShardBeforeCutoff(name string, modTime, cutoff time.Time, records []NotificationRecord) bool {
	cutoffDate := time.Date(cutoff.UTC().Year(), cutoff.UTC().Month(), cutoff.UTC().Day(), 0, 0, 0, 0, time.UTC)
	if len(records) > 0 {
		// A shard name is only a routing hint. Validate every record so a bad
		// controller clock cannot make recent phone notifications look expired.
		for _, record := range records {
			ts, ok := notificationRecordTimestamp(record.NotificationEvent)
			if !ok || !notificationDateBefore(ts, cutoffDate) {
				return false
			}
		}
		return true
	}
	date := strings.TrimSuffix(name, ".jsonl")
	if parsed, err := time.Parse("2006-01-02", date); err == nil {
		return parsed.Before(cutoffDate)
	}
	return modTime.Before(cutoff)
}

func notificationDateBefore(value, cutoff time.Time) bool {
	date := value.UTC()
	day := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	return day.Before(cutoff)
}

func notificationRecordTimestamp(event ble.NotificationEvent) (time.Time, bool) {
	for _, value := range []string{event.Date, event.ReceivedAt} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		for _, layout := range []string{
			time.RFC3339Nano,
			"20060102T150405",
			"2006-01-02",
			"20060102",
		} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func (c *NotificationContext) readPendingLocked(ctx context.Context, cursor uint64, limit int) ([]NotificationRecord, error) {
	entries, err := os.ReadDir(c.eventsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	capacity := limit
	if capacity < 0 {
		capacity = 0
	}
	result := make([]NotificationRecord, 0, capacity)
	for _, name := range files {
		records, err := readNotificationRecordFile(filepath.Join(c.eventsDir, name))
		if err != nil {
			return nil, err
		}
		for _, event := range records {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			eventCursor, ok := parseNotificationCursor(event.ContextID)
			if !ok || eventCursor <= cursor {
				continue
			}
			result = append(result, event)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, lok := parseNotificationCursor(result[i].ContextID)
		right, rok := parseNotificationCursor(result[j].ContextID)
		if lok && rok && left != right {
			return left < right
		}
		return result[i].ReceivedAt < result[j].ReceivedAt
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (c *NotificationContext) appendEventLocked(event NotificationRecord) error {
	if err := os.MkdirAll(c.eventsDir, 0o755); err != nil {
		return err
	}
	name := notificationEventFileName(event.NotificationEvent)
	path := filepath.Join(c.eventsDir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func readNotificationRecordFile(path string) ([]NotificationRecord, error) {
	return readNotificationRecordFileMode(path, true)
}

func readNotificationRecordFileReadOnly(path string) ([]NotificationRecord, error) {
	return readNotificationRecordFileMode(path, false)
}

func readNotificationRecordFileMode(path string, repair bool) ([]NotificationRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if !bytes.HasSuffix(data, []byte{'\n'}) {
		lastNewline := bytes.LastIndexByte(data, '\n')
		tail := data[lastNewline+1:]
		var record NotificationRecord
		if err := json.Unmarshal(tail, &record); err != nil {
			// A crash can leave only the final JSONL record incomplete. Keep
			// complete records and repair the file before retrying reads.
			if !repair {
				return nil, fmt.Errorf("incomplete notification record %s", path)
			}
			if err := writeFileAtomic(path, data[:lastNewline+1], 0o644); err != nil {
				return nil, fmt.Errorf("repair incomplete notification record %s: %w", path, err)
			}
			data = data[:lastNewline+1]
		} else {
			// The final record is complete even without a newline. Keep the
			// preceding records and normalize the in-memory input so the whole
			// file is decoded below.
			data = append(data, '\n')
			if repair {
				if err := appendNotificationRecordNewline(path); err != nil {
					return nil, fmt.Errorf("repair notification record newline %s: %w", path, err)
				}
			}
		}
	}
	lines := bytes.Split(data, []byte{'\n'})
	result := make([]NotificationRecord, 0)
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record NotificationRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decode notification record %s: %w", path, err)
		}
		result = append(result, record)
	}
	return result, nil
}

func appendNotificationRecordNewline(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, writeErr := file.Write([]byte{'\n'})
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func (c *NotificationContext) load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoadedLocked(); err != nil {
		return err
	}
	changed, err := c.recoverFromEventLogLocked()
	if err != nil || !changed {
		return err
	}
	return c.persistLocked()
}

func (c *NotificationContext) ensureLoadedLocked() error {
	if c.state.Version == 0 {
		c.state.Version = 1
	}
	if data, err := os.ReadFile(c.statePath); err == nil {
		var state NotificationContextState
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("decode notification context state: %w", err)
		}
		if state.Version == 0 {
			state.Version = 1
		}
		c.state = state
	} else if !os.IsNotExist(err) {
		return err
	}
	if data, err := os.ReadFile(c.fingerPath); err == nil {
		var file notificationFingerprintFile
		if err := json.Unmarshal(data, &file); err != nil {
			return fmt.Errorf("decode notification fingerprints: %w", err)
		}
		if file.Fingerprints != nil {
			c.fingerprints = file.Fingerprints
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if c.fingerprints == nil {
		c.fingerprints = map[string]string{}
	}
	pruneNotificationFingerprints(c.fingerprints)
	return nil
}

func (c *NotificationContext) recoverFromEventLogLocked() (bool, error) {
	entries, err := os.ReadDir(c.eventsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	fingerprintCount := len(c.fingerprints)
	storedCursor := parseCursorOrZero(c.state.StoredCursor)
	latestCursor := storedCursor
	var latest NotificationRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		records, err := readNotificationRecordFile(filepath.Join(c.eventsDir, entry.Name()))
		if err != nil {
			return false, err
		}
		for _, record := range records {
			cursor, ok := parseNotificationCursor(record.ContextID)
			if !ok {
				continue
			}
			c.fingerprints[notificationEventFingerprint(record.NotificationEvent)] = record.ContextID
			pruneNotificationFingerprints(c.fingerprints)
			if cursor > latestCursor {
				latestCursor = cursor
				latest = record
			}
		}
	}
	pruneNotificationFingerprints(c.fingerprints)
	if latestCursor == storedCursor {
		return len(c.fingerprints) != fingerprintCount, nil
	}
	c.state.StoredCursor = strconv.FormatUint(latestCursor, 10)
	c.state.SourceCursor = normalizeCursor(latest.ID)
	if latest.Generation != "" {
		c.state.Generation = latest.Generation
	}
	c.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return true, nil
}

func (c *NotificationContext) persistLocked() (err error) {
	defer func() {
		if err != nil {
			c.handleWriteErrorLocked(err)
		}
	}()
	if err := os.MkdirAll(c.rootDir, 0o755); err != nil {
		return err
	}
	if c.state.Version == 0 {
		c.state.Version = 1
	}
	state, err := json.MarshalIndent(c.state, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(c.statePath, append(state, '\n'), 0o644); err != nil {
		return err
	}
	fingerprints, err := json.MarshalIndent(notificationFingerprintFile{Version: 1, Fingerprints: c.fingerprints}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.fingerPath, append(fingerprints, '\n'), 0o644)
}

func (c *NotificationContext) recordGapLocked(reason, generation, previousGeneration, fromID, toID string) {
	c.state.GapCount++
	c.state.Gaps = append(c.state.Gaps, NotificationGap{
		At:                 time.Now().UTC().Format(time.RFC3339Nano),
		Reason:             reason,
		Generation:         generation,
		PreviousGeneration: previousGeneration,
		FromID:             fromID,
		ToID:               toID,
	})
	if len(c.state.Gaps) > 32 {
		c.state.Gaps = append([]NotificationGap(nil), c.state.Gaps[len(c.state.Gaps)-32:]...)
	}
}

func sanitizeNotificationEvent(event ble.NotificationEvent) ble.NotificationEvent {
	event.ID = strings.TrimSpace(event.ID)
	event.Source = truncateNotificationText(event.Source, maxNotificationStoredText)
	event.SourceID = truncateNotificationText(event.SourceID, maxNotificationStoredText)
	event.SourceEventID = truncateNotificationText(event.SourceEventID, maxNotificationStoredText)
	event.DeviceID = truncateNotificationText(event.DeviceID, maxNotificationStoredText)
	event.AppIdentifier = truncateNotificationText(event.AppIdentifier, maxNotificationStoredApp)
	event.Title = truncateNotificationText(event.Title, maxNotificationStoredText)
	event.Subtitle = truncateNotificationText(event.Subtitle, maxNotificationStoredText)
	event.Message = truncateNotificationText(event.Message, maxNotificationStoredText)
	event.Category = truncateNotificationText(event.Category, maxNotificationStoredText)
	event.Date = truncateNotificationText(event.Date, maxNotificationStoredText)
	event.MetadataError = truncateNotificationText(event.MetadataError, maxNotificationStoredText)
	for i := range event.Flags {
		event.Flags[i] = truncateNotificationText(event.Flags[i], 128)
	}
	return event
}

func truncateNotificationText(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}

func pruneNotificationFingerprints(fingerprints map[string]string) {
	if len(fingerprints) <= maxNotificationFingerprints {
		return
	}
	type fingerprintCursor struct {
		fingerprint string
		cursor      uint64
	}
	ordered := make([]fingerprintCursor, 0, len(fingerprints))
	for fingerprint, contextID := range fingerprints {
		ordered = append(ordered, fingerprintCursor{fingerprint: fingerprint, cursor: parseCursorOrZero(contextID)})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].cursor < ordered[j].cursor })
	removeCount := len(ordered) - trimNotificationFingerprintsTo
	for _, item := range ordered[:removeCount] {
		delete(fingerprints, item.fingerprint)
	}
}

func notificationEventFingerprint(event ble.NotificationEvent) string {
	key := strings.Join([]string{event.DeviceID, event.Source, event.SourceEventID}, "\x00")
	if event.SourceEventID == "" {
		key = strings.Join([]string{event.DeviceID, event.Source, event.SourceID, strconv.FormatUint(uint64(event.NotificationUID), 10), event.Event, event.AppIdentifier, event.Title, event.Message, event.ReceivedAt}, "\x00")
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func notificationEventStableIdentityIDs(event ble.NotificationEvent) []string {
	var ids []string
	if value := strings.TrimSpace(event.SourceID); value != "" {
		ids = append(ids, strings.Join([]string{"source_id", event.DeviceID, event.Source, value}, ":"))
	}
	if event.NotificationUID != 0 {
		ids = append(ids, strings.Join([]string{"uid", event.DeviceID, event.Source, strconv.FormatUint(uint64(event.NotificationUID), 10)}, ":"))
	}
	return ids
}

func notificationEventFileName(event ble.NotificationEvent) string {
	if ts, ok := notificationRecordTimestamp(event); ok {
		return ts.UTC().Format("2006-01-02") + ".jsonl"
	}
	return "unknown.jsonl"
}

func parseNotificationCursor(value string) (uint64, bool) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	return parsed, err == nil
}

func parseCursorOrZero(value string) uint64 {
	parsed, _ := parseNotificationCursor(value)
	return parsed
}

func normalizeCursor(value string) string {
	if parsed, ok := parseNotificationCursor(value); ok {
		return strconv.FormatUint(parsed, 10)
	}
	return "0"
}

func cloneNotificationContextState(state NotificationContextState) NotificationContextState {
	state.Gaps = append([]NotificationGap(nil), state.Gaps...)
	return state
}
