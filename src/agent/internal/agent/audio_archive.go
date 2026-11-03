package agent

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"aico-agent/internal/logging"

	"github.com/google/uuid"
)

// AudioArchiveManager handles saving and cleanup of audio recordings.
type AudioArchiveManager struct {
	config         AudioArchiveConfig
	storageMu      sync.RWMutex
	storage        *StorageManager
	storageMonitor *StorageMonitor
}

// NewAudioArchiveManager creates a new audio archive manager.
func NewAudioArchiveManager(config AudioArchiveConfig) *AudioArchiveManager {
	return &AudioArchiveManager{
		config: config,
	}
}

// SetStorageManager routes recordings through the SD/eMMC storage modes. A
// non-default storage_path in the config wins over the mode machinery (the
// built-in default names the managed eMMC tier, see ExplicitStoragePath).
func (m *AudioArchiveManager) SetStorageManager(sm *StorageManager) {
	m.storageMu.Lock()
	defer m.storageMu.Unlock()
	m.storage = sm
}

func (m *AudioArchiveManager) SetStorageMonitor(monitor *StorageMonitor) {
	m.storageMu.Lock()
	defer m.storageMu.Unlock()
	m.storageMonitor = monitor
}

func (m *AudioArchiveManager) storageComponents() (*StorageManager, *StorageMonitor) {
	m.storageMu.RLock()
	defer m.storageMu.RUnlock()
	return m.storage, m.storageMonitor
}

// writeDir returns the directory new recordings should be written to.
func (m *AudioArchiveManager) writeDir() string {
	if path := m.config.ExplicitStoragePath(); path != "" {
		return path
	}
	storage, _ := m.storageComponents()
	if storage != nil {
		if dir, err := storage.ResolveDir(StorageClassAudio); err == nil {
			return dir
		}
	}
	return m.config.StoragePathOrDefault()
}

// cleanupRoots returns the directories the retention limits apply to. With
// the storage manager present, eMMC space is governed by watermark migration
// and the limits act as the final eviction tier (SD when a card is mounted,
// eMMC otherwise); a non-default storage_path bypasses all of that.
func (m *AudioArchiveManager) cleanupRoots() []string {
	if path := m.config.ExplicitStoragePath(); path != "" {
		return []string{path}
	}
	storage, _ := m.storageComponents()
	if storage != nil {
		return storage.CleanupRoots(StorageClassAudio)
	}
	return []string{m.config.StoragePathOrDefault()}
}

// SaveAudio saves audio samples to a WAV file and returns the path, duration in ms, and error.
// If archival is disabled, only calculates duration without saving.
func (m *AudioArchiveManager) SaveAudio(samples []int16, sampleRate int) (string, int, error) {
	if sampleRate <= 0 {
		return "", 0, fmt.Errorf("invalid sample rate: %d", sampleRate)
	}

	// Calculate duration in milliseconds
	durationMs := (len(samples) * 1000) / sampleRate

	// If disabled, return only duration
	if !m.config.Enabled {
		return "", durationMs, nil
	}
	_, storageMonitor := m.storageComponents()
	if storageMonitor != nil && !storageMonitor.AllowWrite(StorageCapabilityAudioArchive) {
		return "", durationMs, nil
	}

	// Ensure storage directory exists
	storagePath := m.writeDir()
	if err := os.MkdirAll(storagePath, 0755); err != nil {
		m.handleStorageWriteError(err)
		return "", 0, fmt.Errorf("create storage dir: %w", err)
	}

	// Generate filename with timestamp and UUID
	timestamp := time.Now().Unix()
	id := uuid.New().String()[:8]
	filename := fmt.Sprintf("msg_%d_%s.wav", timestamp, id)
	filePath := filepath.Join(storagePath, filename)

	// Write WAV file
	if err := writeWAVFile(filePath, samples, sampleRate); err != nil {
		m.handleStorageWriteError(err)
		return "", 0, fmt.Errorf("write WAV: %w", err)
	}

	// Cleanup old files if needed
	if err := m.cleanup(); err != nil {
		_ = logging.LogEvent(logging.Warn, "agent", "audio_archive", "cleanup_failed",
			logging.Field{Key: "error", Value: err},
		)
	}

	return filePath, durationMs, nil
}

func (m *AudioArchiveManager) handleStorageWriteError(err error) {
	if m.storageMonitor != nil {
		m.storageMonitor.HandleWriteError(err)
	}
}

// cleanup applies the max_files and max_size_mb limits to the eviction-tier
// roots (see cleanupRoots).
func (m *AudioArchiveManager) cleanup() error {
	var firstErr error
	for _, root := range m.cleanupRoots() {
		if err := m.cleanupDir(root); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// cleanupDir removes old audio files in one directory based on the limits.
func (m *AudioArchiveManager) cleanupDir(storagePath string) error {
	entries, err := os.ReadDir(storagePath)
	if err != nil {
		return err
	}

	// Collect audio files with metadata
	type fileInfo struct {
		path    string
		modTime time.Time
		size    int64
	}
	var files []fileInfo

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".wav" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		files = append(files, fileInfo{
			path:    filepath.Join(storagePath, entry.Name()),
			modTime: info.ModTime(),
			size:    info.Size(),
		})
	}

	// Sort by modification time (oldest first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	maxFiles := m.config.MaxFilesOrDefault()
	maxSizeBytes := int64(m.config.MaxSizeMBOrDefault()) * 1024 * 1024

	// Calculate total size
	var totalSize int64
	for _, f := range files {
		totalSize += f.size
	}

	// Delete oldest files until under limits
	removedFiles := 0
	var removedBytes int64
	for len(files) > maxFiles || totalSize > maxSizeBytes {
		if len(files) == 0 {
			break
		}

		oldest := files[0]
		if err := os.Remove(oldest.path); err != nil {
			return fmt.Errorf("remove %s: %w", oldest.path, err)
		}
		removedFiles++
		removedBytes += oldest.size
		files = files[1:]
		totalSize -= oldest.size
	}
	if removedFiles > 0 {
		_ = logging.LogEvent(logging.Info, "agent", "audio_archive", "recordings_evicted",
			logging.Field{Key: "files", Value: removedFiles},
			logging.Field{Key: "size_bytes", Value: removedBytes},
			logging.Field{Key: "path", Value: storagePath},
			logging.Field{Key: "max_files", Value: maxFiles},
			logging.Field{Key: "max_size_mb", Value: m.config.MaxSizeMBOrDefault()},
		)
	}

	return nil
}

// writeWAVFile writes PCM samples to a WAV file.
func writeWAVFile(path string, samples []int16, sampleRate int) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	// WAV header (44 bytes)
	dataSize := len(samples) * 2
	fileSize := 36 + dataSize
	writeString := func(s string) error {
		_, err := file.WriteString(s)
		return err
	}
	writeLE := func(v interface{}) error {
		return binary.Write(file, binary.LittleEndian, v)
	}

	// RIFF header
	if err := writeString("RIFF"); err != nil {
		return err
	}
	if err := writeLE(uint32(fileSize)); err != nil {
		return err
	}
	if err := writeString("WAVE"); err != nil {
		return err
	}

	// fmt chunk
	if err := writeString("fmt "); err != nil {
		return err
	}
	if err := writeLE(uint32(16)); err != nil { // fmt chunk size
		return err
	}
	if err := writeLE(uint16(1)); err != nil { // audio format (PCM)
		return err
	}
	if err := writeLE(uint16(1)); err != nil { // num channels
		return err
	}
	if err := writeLE(uint32(sampleRate)); err != nil { // sample rate
		return err
	}
	if err := writeLE(uint32(sampleRate * 2)); err != nil { // byte rate
		return err
	}
	if err := writeLE(uint16(2)); err != nil { // block align
		return err
	}
	if err := writeLE(uint16(16)); err != nil { // bits per sample
		return err
	}

	// data chunk
	if err := writeString("data"); err != nil {
		return err
	}
	if err := writeLE(uint32(dataSize)); err != nil {
		return err
	}

	// Write samples
	return binary.Write(file, binary.LittleEndian, samples)
}
