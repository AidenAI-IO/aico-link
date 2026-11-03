package agent

import (
	"context"
	"path/filepath"

	"aico-agent/internal/agent/agentpath"
	"aico-agent/internal/agent/contextmanager"
)

type leveledStorageCleaner struct {
	StorageCleaner
	minimumLevel StorageLevel
}

type leveledEmergencyStorageCleaner struct {
	leveledStorageCleaner
	emergencyStorageCleaner
}

func (c leveledStorageCleaner) MinimumLevel() StorageLevel { return c.minimumLevel }

func (c leveledStorageCleaner) ForceClean(ctx context.Context) (uint64, error) {
	if cleaner, ok := c.StorageCleaner.(forceStorageCleaner); ok {
		return cleaner.ForceClean(ctx)
	}
	return c.StorageCleaner.Clean(ctx)
}

func withMinimumStorageLevel(cleaner StorageCleaner, level StorageLevel) StorageCleaner {
	leveled := leveledStorageCleaner{StorageCleaner: cleaner, minimumLevel: level}
	if emergencyCleaner, ok := cleaner.(emergencyStorageCleaner); ok {
		return leveledEmergencyStorageCleaner{
			leveledStorageCleaner:   leveled,
			emergencyStorageCleaner: emergencyCleaner,
		}
	}
	return leveled
}

// runtimeStorageMonitorParts derives the monitor configuration and cleaners
// from a runtime config. Reconfiguring an existing monitor reuses these parts
// without allocating a replacement monitor object.
func runtimeStorageMonitorParts(cfg Config) (StorageMonitorConfig, []StorageCleaner) {
	storageConfig := cfg.Storage.MonitorConfig()
	storageConfig.Cleanup.NotificationContextRetentionDays = notificationContextRetentionStages(
		cfg.VoiceNotifications.RetentionDaysOrDefault(),
		storageConfig.Cleanup.NotificationContextRetentionDays,
	)
	cleaners := make([]StorageCleaner, 0)
	priority := 1
	pythonCleaner := NewPythonUserBaseCleaner(managedPythonRoot, managedPythonTmp, priority)
	priority++
	cleaners = append(cleaners, withMinimumStorageLevel(pythonCleaner, StorageLevelNormal))
	if cfg.ConfigDir != "" {
		artifactCleaner := contextmanager.NewArtifactStoreCleaner(agentpath.ContextManagerSessionFolder(cfg.ConfigDir), priority)
		priority++
		cleaners = append(cleaners, withMinimumStorageLevel(artifactCleaner, StorageLevelNormal))
	}

	// The live conversation is the current ContextManager session; log cleaners
	// use it to avoid deleting the log partition still being written.
	currentSessionID := func() (string, error) {
		if cfg.ConfigDir == "" {
			return "", nil
		}
		return contextmanager.CurrentSessionID(agentpath.ContextManagerSessionFolder(cfg.ConfigDir)), nil
	}
	logDir := ""
	// memory/session_archive/ is no longer written: conversation history now lives
	// in ContextManager transcripts and compaction chunks. Devices upgraded from a
	// build that wrote archives still have them on disk, so the cleaner stays
	// wired to reclaim that space.
	legacySessionArchiveDir := ""
	if cfg.ConfigDir != "" {
		logDir = filepath.Join(cfg.ConfigDir, "log")
		legacySessionArchiveDir = filepath.Join(cfg.ConfigDir, "memory", "session_archive")
	}

	regularLLM, aggressiveLLMDays := splitStorageRetentionDays(storageConfig.Cleanup.LLMHTTPLogRetentionDays)
	regularAudio, aggressiveAudioDays := splitStorageRetentionDays(storageConfig.Cleanup.AudioArchiveRetentionDays)
	regularSessions, aggressiveSessionDays := splitStorageRetentionDays(storageConfig.Cleanup.SessionArchiveRetentionDays)
	regularNotifications, aggressiveNotificationDays := splitStorageRetentionDays(storageConfig.Cleanup.NotificationContextRetentionDays)

	for index, retentionDays := range regularLLM {
		cleaner := NewLLMHTTPLogCleanerWithCheckedSessionProvider(logDir, retentionDays, priority, currentSessionID)
		priority++
		minimumLevel := StorageLevelCritical
		switch index {
		case 0:
			minimumLevel = StorageLevelNormal
		case 1:
			minimumLevel = StorageLevelWarning
		}
		cleaners = append(cleaners, withMinimumStorageLevel(cleaner, minimumLevel))
	}

	for index, retentionDays := range regularAudio {
		minimumLevel := StorageLevelWarning
		maxFiles := 10
		if index > 0 {
			minimumLevel = StorageLevelCritical
			maxFiles = 3
		}
		cleaner := NewAudioArchiveCleaner(cfg.AudioArchive.StoragePathOrDefault(), retentionDays, maxFiles, priority)
		priority++
		cleaners = append(cleaners, withMinimumStorageLevel(cleaner, minimumLevel))
	}

	for _, retentionDays := range regularSessions {
		cleaner := NewSessionArchiveCleaner(legacySessionArchiveDir, retentionDays, 0, priority)
		priority++
		cleaners = append(cleaners, withMinimumStorageLevel(cleaner, StorageLevelWarning))
	}

	if cfg.ConfigDir != "" {
		temporaryCleaner := NewTemporaryMemoryCleaner(filepath.Join(cfg.ConfigDir, "memory", "temporary"), priority)
		priority++
		cleaners = append(cleaners, withMinimumStorageLevel(temporaryCleaner, StorageLevelNormal))

		episodeCleaner := NewEpisodeRetentionCleaner(filepath.Join(cfg.ConfigDir, "memory"), priority)
		priority++
		cleaners = append(cleaners, withMinimumStorageLevel(episodeCleaner, StorageLevelNormal))

		for index, retentionDays := range regularNotifications {
			cleaner := NewNotificationContextCleaner(filepath.Join(cfg.ConfigDir, "memory"), retentionDays, priority)
			priority++
			minimumLevel := StorageLevelNormal
			switch index {
			case 1:
				minimumLevel = StorageLevelWarning
			case 2:
				minimumLevel = StorageLevelCritical
			}
			cleaners = append(cleaners, withMinimumStorageLevel(cleaner, minimumLevel))
		}
	}

	for range aggressiveLLMDays {
		cleaner := NewLLMHTTPLogCleanerWithCheckedSessionProvider(logDir, 0, priority, currentSessionID)
		priority++
		cleaners = append(cleaners, withMinimumStorageLevel(cleaner, StorageLevelEmergency))
	}
	for range aggressiveAudioDays {
		cleaner := NewAudioArchiveCleaner(cfg.AudioArchive.StoragePathOrDefault(), 0, 0, priority)
		priority++
		cleaners = append(cleaners, withMinimumStorageLevel(cleaner, StorageLevelEmergency))
	}
	for range aggressiveSessionDays {
		cleaner := NewSessionArchiveCleaner(legacySessionArchiveDir, 0, 0, priority)
		priority++
		cleaners = append(cleaners, withMinimumStorageLevel(cleaner, StorageLevelEmergency))
	}
	if cfg.ConfigDir != "" {
		for range aggressiveNotificationDays {
			cleaner := NewNotificationContextCleaner(filepath.Join(cfg.ConfigDir, "memory"), 0, priority)
			priority++
			cleaners = append(cleaners, withMinimumStorageLevel(cleaner, StorageLevelEmergency))
		}
	}
	return storageConfig, cleaners
}

// notificationContextRetentionStages keeps the product-facing retention value
// in Memory Settings while preserving the storage monitor's progressively more
// aggressive cleanup stages. Stages longer than the configured base retention
// are redundant and are omitted.
func notificationContextRetentionStages(base int, fallbacks []int) []int {
	if base < 1 {
		base = defaultNotificationMemoryRetentionDays
	}
	stages := []int{base}
	hasProcessedOnly := false
	for _, days := range fallbacks {
		if days == 0 {
			hasProcessedOnly = true
			continue
		}
		if days < 1 || days >= base {
			continue
		}
		duplicate := false
		for _, existing := range stages {
			if existing == days {
				duplicate = true
				break
			}
		}
		if !duplicate {
			stages = append(stages, days)
		}
	}
	if hasProcessedOnly {
		stages = append(stages, 0)
	}
	return stages
}

func newRuntimeStorageMonitor(cfg Config, logger *Logger) *StorageMonitor {
	storageConfig, cleaners := runtimeStorageMonitorParts(cfg)
	monitor := NewStorageMonitor(storageConfig, nil, logger, cleaners, nil)
	monitor.SetLevelStatePath("/run/agent/storage_level")
	if logger != nil {
		logger.SetStorageMonitor(monitor)
	}
	return monitor
}

func splitStorageRetentionDays(days []int) (regular []int, aggressive []int) {
	for _, day := range days {
		if day == 0 {
			aggressive = append(aggressive, day)
		} else {
			regular = append(regular, day)
		}
	}
	return regular, aggressive
}

// StartStorageMonitor runs the startup check and begins periodic monitoring.
func (r *Runtime) StartStorageMonitor() error {
	if r == nil || r.storageMonitor == nil {
		return nil
	}
	return r.storageMonitor.Start()
}

// StorageMonitor returns the runtime storage monitor for integration seams.
func (r *Runtime) StorageMonitor() *StorageMonitor {
	if r == nil {
		return nil
	}
	return r.storageMonitor
}

// SetVoiceNotificationSink connects the runtime monitor to the global notification manager.
func (r *Runtime) SetVoiceNotificationSink(sink VoiceNotificationSink) {
	if r != nil && r.storageMonitor != nil {
		r.storageMonitor.SetVoiceNotificationSink(sink)
	}
}
