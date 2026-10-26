package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sequenceStorageSampler struct {
	samples []StorageSample
	calls   int
}

func (s *sequenceStorageSampler) Sample(context.Context, string) (StorageSample, error) {
	index := s.calls
	if index >= len(s.samples) {
		index = len(s.samples) - 1
	}
	s.calls++
	return s.samples[index], nil
}

type recordingStorageCleaner struct {
	name     string
	priority int
	freed    uint64
	calls    int
}

type recordingEmergencyStorageCleaner struct {
	name                 string
	priority             int
	regularEstimateCalls int
	regularCleanCalls    int
	forceCleanCalls      int
	emergencyCleanCalls  int
}

type delayedStorageCleaner struct {
	name          string
	priority      int
	freed         uint64
	estimateCalls int
	cleanCalls    int
}

func (c *delayedStorageCleaner) Name() string  { return c.name }
func (c *delayedStorageCleaner) Priority() int { return c.priority }
func (c *delayedStorageCleaner) EstimateReclaimable(context.Context) (uint64, error) {
	c.estimateCalls++
	if c.estimateCalls == 1 {
		return 0, nil
	}
	return c.freed, nil
}
func (c *delayedStorageCleaner) Clean(context.Context) (uint64, error) {
	c.cleanCalls++
	return c.freed, nil
}

type recordingVoiceNotificationSink struct {
	events   []VoiceNotificationEvent
	err      error
	failures int
}

func (s *recordingVoiceNotificationSink) Publish(_ context.Context, event VoiceNotificationEvent) error {
	s.events = append(s.events, event)
	if s.failures > 0 {
		s.failures--
		return s.err
	}
	return s.err
}

func (c *recordingStorageCleaner) Name() string  { return c.name }
func (c *recordingStorageCleaner) Priority() int { return c.priority }
func (c *recordingStorageCleaner) EstimateReclaimable(context.Context) (uint64, error) {
	return c.freed, nil
}
func (c *recordingStorageCleaner) Clean(context.Context) (uint64, error) {
	c.calls++
	return c.freed, nil
}

func (c *recordingEmergencyStorageCleaner) Name() string  { return c.name }
func (c *recordingEmergencyStorageCleaner) Priority() int { return c.priority }
func (c *recordingEmergencyStorageCleaner) EstimateReclaimable(context.Context) (uint64, error) {
	c.regularEstimateCalls++
	return storageMegabyte, nil
}
func (c *recordingEmergencyStorageCleaner) Clean(context.Context) (uint64, error) {
	c.regularCleanCalls++
	return storageMegabyte, nil
}
func (c *recordingEmergencyStorageCleaner) ForceClean(context.Context) (uint64, error) {
	c.forceCleanCalls++
	return storageMegabyte, nil
}
func (c *recordingEmergencyStorageCleaner) EmergencyClean(context.Context) (uint64, error) {
	c.emergencyCleanCalls++
	return storageMegabyte, nil
}

func storageSampleWithAvailableMB(availableMB uint64) StorageSample {
	const totalMB = uint64(100)
	return StorageSample{
		TotalBytes:     totalMB * 1024 * 1024,
		AvailableBytes: availableMB * 1024 * 1024,
	}
}

func TestStorageMonitorThresholdsUseExactBytes(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{{
		TotalBytes:     100 * storageMegabyte,
		AvailableBytes: 50*storageMegabyte + 1,
	}}}
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, sampler, nil, nil, nil)
	status, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("CheckAndRemediate() error = %v", err)
	}
	if status.Level != StorageLevelNormal {
		t.Fatalf("level at 50MB+1 byte = %q, want normal", status.Level)
	}
}

func TestStorageMonitorPublishesFinalLevelToRuntimeStateFile(t *testing.T) {
	levelPath := filepath.Join(t.TempDir(), "storage_level")
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, &sequenceStorageSampler{
		samples: []StorageSample{storageSampleWithAvailableMB(8)},
	}, nil, nil, nil)
	monitor.SetLevelStatePath(levelPath)

	if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic}); err != nil {
		t.Fatalf("CheckAndRemediate() error = %v", err)
	}
	data, err := os.ReadFile(levelPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "critical\n" {
		t.Fatalf("level state = %q, want critical", data)
	}
}

func TestStorageMonitorDisabledStartClearsStaleRuntimeStateFile(t *testing.T) {
	levelPath := filepath.Join(t.TempDir(), "storage_level")
	if err := os.WriteFile(levelPath, []byte("critical\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	config := DefaultStorageConfig()
	config.Enabled = false
	monitor := NewStorageMonitor(config, nil, nil, nil, nil)
	monitor.SetLevelStatePath(levelPath)

	if err := monitor.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := os.Stat(levelPath); !os.IsNotExist(err) {
		t.Fatalf("disabled Start() left stale level state, stat error = %v", err)
	}
}

func TestStorageMonitorDisabledPeriodicCheckClearsStateWithoutDeadlock(t *testing.T) {
	levelPath := filepath.Join(t.TempDir(), "storage_level")
	if err := os.WriteFile(levelPath, []byte("critical\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	config := DefaultStorageConfig()
	config.Enabled = false
	monitor := NewStorageMonitor(config, nil, nil, nil, nil)
	monitor.SetLevelStatePath(levelPath)

	done := make(chan error, 1)
	go func() {
		_, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CheckAndRemediate() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("disabled periodic check deadlocked")
	}
	if _, err := os.Stat(levelPath); !os.IsNotExist(err) {
		t.Fatalf("disabled periodic check left stale level state, stat error = %v", err)
	}
}

func TestStorageMonitorStopClearsRuntimeStateFile(t *testing.T) {
	levelPath := filepath.Join(t.TempDir(), "storage_level")
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, &sequenceStorageSampler{
		samples: []StorageSample{storageSampleWithAvailableMB(8)},
	}, nil, nil, nil)
	monitor.SetLevelStatePath(levelPath)

	if err := monitor.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	monitor.Stop()
	if _, err := os.Stat(levelPath); !os.IsNotExist(err) {
		t.Fatalf("Stop() left stale level state, stat error = %v", err)
	}
}

func TestStorageMonitorCheckAndRemediatePublishesOnlyCleanupFinalState(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(60),
	}}
	cleaner := &recordingStorageCleaner{name: "logs", priority: 1, freed: 7 * 1024 * 1024}
	monitor := NewStorageMonitor(StorageMonitorConfig{
		Enabled:              true,
		RootPath:             "/userdata",
		WarningThresholdMB:   50,
		CriticalThresholdMB:  10,
		EmergencyThresholdMB: 5,
		Cleanup:              StorageCleanupConfig{Enabled: true},
	}, sampler, nil, []StorageCleaner{cleaner}, nil)

	status, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonStartup})
	if err != nil {
		t.Fatalf("CheckAndRemediate() error = %v", err)
	}
	if status.Level != StorageLevelNormal {
		t.Fatalf("final level = %q, want %q", status.Level, StorageLevelNormal)
	}
	if status.AvailableBytes != 60*1024*1024 {
		t.Fatalf("final available bytes = %d, want %d", status.AvailableBytes, 60*1024*1024)
	}
	if status.LastCleanupFreedBytes != cleaner.freed {
		t.Fatalf("last cleanup freed = %d, want %d", status.LastCleanupFreedBytes, cleaner.freed)
	}
	if cleaner.calls != 1 {
		t.Fatalf("cleaner calls = %d, want 1", cleaner.calls)
	}
	if sampler.calls != 2 {
		t.Fatalf("sampler calls = %d, want 2", sampler.calls)
	}
	if got := monitor.Status(); got.Level != StorageLevelNormal || got.AvailableBytes != status.AvailableBytes {
		t.Fatalf("Status() = %+v, want final status %+v", got, status)
	}
}

func TestStorageMonitorCheckAndRemediateAppliesRecoveryHysteresis(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(50),
		storageSampleWithAvailableMB(53),
		storageSampleWithAvailableMB(56),
	}}
	monitor := NewStorageMonitor(StorageMonitorConfig{
		Enabled:              true,
		RootPath:             "/userdata",
		WarningThresholdMB:   50,
		CriticalThresholdMB:  10,
		EmergencyThresholdMB: 5,
		RecoveryHysteresisMB: 5,
	}, sampler, nil, nil, nil)

	first, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("first CheckAndRemediate() error = %v", err)
	}
	if first.Level != StorageLevelWarning {
		t.Fatalf("first level = %q, want warning", first.Level)
	}

	withinHysteresis, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("second CheckAndRemediate() error = %v", err)
	}
	if withinHysteresis.Level != StorageLevelWarning {
		t.Fatalf("level at 53MB = %q, want warning until available space exceeds 55MB", withinHysteresis.Level)
	}

	recovered, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("third CheckAndRemediate() error = %v", err)
	}
	if recovered.Level != StorageLevelNormal {
		t.Fatalf("level at 56MB = %q, want normal", recovered.Level)
	}
}

func TestStorageMonitorContinuesCleanupUntilRecoveryHysteresisClears(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(53),
		storageSampleWithAvailableMB(56),
	}}
	firstCleaner := &delayedStorageCleaner{name: "first", priority: 1, freed: 5 * 1024 * 1024}
	secondCleaner := &delayedStorageCleaner{name: "second", priority: 2, freed: 5 * 1024 * 1024}
	config := DefaultStorageConfig()
	config.Cleanup.CleanupRetryIntervalSeconds = 0
	monitor := NewStorageMonitor(config, sampler, nil, []StorageCleaner{firstCleaner, secondCleaner}, nil)

	initial, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("initial CheckAndRemediate() error = %v", err)
	}
	if initial.Level != StorageLevelWarning {
		t.Fatalf("initial level = %q, want warning", initial.Level)
	}

	recovered, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("cleanup CheckAndRemediate() error = %v", err)
	}
	if recovered.Level != StorageLevelNormal || recovered.AvailableBytes != 56*1024*1024 {
		t.Fatalf("recovered status = %+v, want normal at 56MB", recovered)
	}
	if firstCleaner.cleanCalls != 1 || secondCleaner.cleanCalls != 1 {
		t.Fatalf("cleaner calls = first:%d second:%d, want both once", firstCleaner.cleanCalls, secondCleaner.cleanCalls)
	}
}

func TestStorageMonitorHonorsCleanupRetryInterval(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(40),
	}}
	cleaner := &recordingStorageCleaner{name: "logs", priority: 1, freed: storageMegabyte}
	config := DefaultStorageConfig()
	config.Cleanup.CleanupRetryIntervalSeconds = 3600
	monitor := NewStorageMonitor(config, sampler, nil, []StorageCleaner{cleaner}, nil)

	first, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("first CheckAndRemediate() error = %v", err)
	}
	second, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("second CheckAndRemediate() error = %v", err)
	}
	if cleaner.calls != 1 {
		t.Fatalf("cleaner calls = %d, want 1 within retry interval", cleaner.calls)
	}
	if first.LastCleanupFreedBytes != storageMegabyte || second.LastCleanupFreedBytes != storageMegabyte {
		t.Fatalf("last cleanup freed changed across throttled check: first=%d second=%d", first.LastCleanupFreedBytes, second.LastCleanupFreedBytes)
	}
}

func TestStorageMonitorManualCleanupRunsAtNormalLevelWithoutForce(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(60),
		storageSampleWithAvailableMB(65),
	}}
	cleaner := &recordingStorageCleaner{name: "logs", priority: 1, freed: storageMegabyte}
	config := DefaultStorageConfig()
	monitor := NewStorageMonitor(config, sampler, nil, []StorageCleaner{cleaner}, nil)

	status, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonManual})
	if err != nil {
		t.Fatalf("CheckAndRemediate() error = %v", err)
	}
	if cleaner.calls != 1 {
		t.Fatalf("cleaner calls = %d, want 1 for manual cleanup", cleaner.calls)
	}
	if status.AvailableBytes != 65*storageMegabyte || status.Level != StorageLevelNormal {
		t.Fatalf("manual cleanup status = %+v", status)
	}
}

func TestStorageMonitorManualCleanupWithoutForceSkipsEmergencyStages(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(60),
		storageSampleWithAvailableMB(61),
	}}
	regular := &recordingStorageCleaner{name: "llm_http_log_7d", priority: 1, freed: 0}
	emergency := &recordingStorageCleaner{name: "llm_http_log_0d", priority: 2, freed: storageMegabyte}
	config := DefaultStorageConfig()
	monitor := NewStorageMonitor(config, sampler, nil, []StorageCleaner{
		withMinimumStorageLevel(regular, StorageLevelNormal),
		withMinimumStorageLevel(emergency, StorageLevelEmergency),
	}, nil)

	status, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonManual})
	if err != nil {
		t.Fatalf("CheckAndRemediate() error = %v", err)
	}
	if emergency.calls != 0 {
		t.Fatalf("emergency cleaner calls = %d for non-force manual cleanup, want 0", emergency.calls)
	}
	if status.Level != StorageLevelNormal || status.CurrentCleanupFreedBytes != 0 {
		t.Fatalf("manual cleanup status = %+v", status)
	}
}

func TestStorageMonitorUsesEmergencyCleanupOnlyAtHighestAlertLevel(t *testing.T) {
	newMonitor := func(availableMB uint64, cleaner StorageCleaner) *StorageMonitor {
		config := DefaultStorageConfig()
		config.Cleanup.CleanupRetryIntervalSeconds = 0
		return NewStorageMonitor(config, &sequenceStorageSampler{samples: []StorageSample{
			storageSampleWithAvailableMB(availableMB),
			storageSampleWithAvailableMB(60),
		}}, nil, []StorageCleaner{withMinimumStorageLevel(cleaner, StorageLevelNormal)}, nil)
	}

	t.Run("automatic emergency cleanup", func(t *testing.T) {
		cleaner := &recordingEmergencyStorageCleaner{name: "python_userbase", priority: 1}
		monitor := newMonitor(4, cleaner)

		if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic}); err != nil {
			t.Fatalf("CheckAndRemediate() error = %v", err)
		}
		if cleaner.emergencyCleanCalls != 1 {
			t.Fatalf("emergency clean calls = %d, want 1", cleaner.emergencyCleanCalls)
		}
		if cleaner.regularEstimateCalls != 0 || cleaner.regularCleanCalls != 0 || cleaner.forceCleanCalls != 0 {
			t.Fatalf("non-emergency calls = estimate:%d clean:%d force:%d, want all 0", cleaner.regularEstimateCalls, cleaner.regularCleanCalls, cleaner.forceCleanCalls)
		}
	})

	t.Run("manual force at normal level", func(t *testing.T) {
		cleaner := &recordingEmergencyStorageCleaner{name: "python_userbase", priority: 1}
		monitor := newMonitor(60, cleaner)

		if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonManual, Force: true}); err != nil {
			t.Fatalf("CheckAndRemediate() error = %v", err)
		}
		if cleaner.forceCleanCalls != 1 {
			t.Fatalf("force clean calls = %d, want 1", cleaner.forceCleanCalls)
		}
		if cleaner.emergencyCleanCalls != 0 {
			t.Fatalf("emergency clean calls at normal level = %d, want 0", cleaner.emergencyCleanCalls)
		}
	})
}

func TestStorageMonitorAllowWriteTracksDegradedCapabilities(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(8),
		storageSampleWithAvailableMB(4),
		storageSampleWithAvailableMB(60),
	}}
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, sampler, nil, nil, nil)

	critical, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("critical CheckAndRemediate() error = %v", err)
	}
	if critical.Level != StorageLevelCritical {
		t.Fatalf("critical level = %q, want critical", critical.Level)
	}
	for _, capability := range []StorageCapability{StorageCapabilityLLMHTTPLog, StorageCapabilityAudioArchive, StorageCapabilitySessionArchive, StorageCapabilitySessionPersistence, StorageCapabilityNotificationContext} {
		if monitor.AllowWrite(capability) {
			t.Errorf("AllowWrite(%q) = true at critical, want false", capability)
		}
	}
	emergency, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonWrite})
	if err != nil {
		t.Fatalf("emergency CheckAndRemediate() error = %v", err)
	}
	if emergency.Level != StorageLevelEmergency || monitor.AllowWrite(StorageCapabilitySessionPersistence) {
		t.Fatalf("emergency status = %+v, session persistence should be unavailable", emergency)
	}

	recovered, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("recovery CheckAndRemediate() error = %v", err)
	}
	if recovered.Level != StorageLevelNormal {
		t.Fatalf("recovered level = %q, want normal", recovered.Level)
	}
	for _, capability := range []StorageCapability{StorageCapabilityLLMHTTPLog, StorageCapabilityAudioArchive, StorageCapabilitySessionArchive, StorageCapabilitySessionPersistence, StorageCapabilityNotificationContext} {
		if !monitor.AllowWrite(capability) {
			t.Errorf("AllowWrite(%q) = false after recovery, want true", capability)
		}
	}
}

func TestStorageMonitorPublishesActiveUpgradeAndResolvedEvents(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(35),
		storageSampleWithAvailableMB(8),
		storageSampleWithAvailableMB(60),
		storageSampleWithAvailableMB(65),
	}}
	notifier := &recordingVoiceNotificationSink{}
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, sampler, nil, nil, notifier)

	for i := 0; i < len(sampler.samples); i++ {
		if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic}); err != nil {
			t.Fatalf("CheckAndRemediate() call %d error = %v", i+1, err)
		}
	}

	if len(notifier.events) != 4 {
		t.Fatalf("published %d events, want 4: %+v", len(notifier.events), notifier.events)
	}
	warning := notifier.events[0]
	if warning.Code != "storage" || warning.DedupeKey != "storage:device" || warning.State != VoiceNotificationActive || warning.Severity != SeverityWarning {
		t.Fatalf("warning event = %+v", warning)
	}
	updatedWarning := notifier.events[1]
	if updatedWarning.State != VoiceNotificationActive || updatedWarning.Severity != SeverityWarning || updatedWarning.Params["available_mb"] != "35" {
		t.Fatalf("updated warning event = %+v", updatedWarning)
	}
	critical := notifier.events[2]
	if critical.State != VoiceNotificationActive || critical.Severity != SeverityCritical {
		t.Fatalf("critical event = %+v", critical)
	}
	resolved := notifier.events[3]
	if resolved.State != VoiceNotificationResolved || resolved.Severity != SeverityCritical {
		t.Fatalf("resolved event = %+v, want resolved with previous critical severity", resolved)
	}
	if got := resolved.Params["available_mb"]; got != "60" {
		t.Fatalf("resolved available_mb = %#v, want 60", got)
	}
}

func TestStorageMonitorPublishesToVoiceNotificationManager(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(60),
	}}
	manager := NewVoiceNotificationManager(VoiceNotificationsConfig{
		ResponseTail: VoiceNotificationResponseTailConfig{MaxItems: 1},
	}, WithVoiceNotificationLocale("zh-CN"))
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, sampler, nil, nil, manager)

	if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonStartup}); err != nil {
		t.Fatalf("warning CheckAndRemediate() error = %v", err)
	}
	spoken := manager.PrepareSpokenText(context.Background(), SpokenTextInput{
		ResponseText:   "处理完成。",
		TailAppendable: true,
	})
	if spoken.Mode != SpokenTextModeTail || !strings.Contains(spoken.Text, "存储空间不足") {
		t.Fatalf("warning spoken result = %+v", spoken)
	}

	if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonStartup}); err != nil {
		t.Fatalf("resolved CheckAndRemediate() error = %v", err)
	}
	resolved := manager.PrepareSpokenText(context.Background(), SpokenTextInput{
		ResponseText:   "再次完成。",
		TailAppendable: true,
	})
	if resolved.Mode != SpokenTextModeNormal || resolved.Text != "再次完成。" {
		t.Fatalf("resolved spoken result = %+v", resolved)
	}
}

func TestStorageMonitorSuppressesEquivalentActiveEvent(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(40),
	}}
	notifier := &recordingVoiceNotificationSink{}
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, sampler, nil, nil, notifier)

	for range sampler.samples {
		if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonStartup}); err != nil {
			t.Fatalf("CheckAndRemediate() error = %v", err)
		}
	}
	if len(notifier.events) != 1 {
		t.Fatalf("published %d equivalent active events, want 1: %+v", len(notifier.events), notifier.events)
	}
}

func TestStorageMonitorRefreshesEquivalentActiveEventOnPeriodicCheck(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(40),
	}}
	notifier := &recordingVoiceNotificationSink{}
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, sampler, nil, nil, notifier)

	if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonStartup}); err != nil {
		t.Fatalf("startup CheckAndRemediate() error = %v", err)
	}
	if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic}); err != nil {
		t.Fatalf("periodic CheckAndRemediate() error = %v", err)
	}
	if len(notifier.events) != 2 {
		t.Fatalf("published %d active events, want initial plus periodic refresh: %+v", len(notifier.events), notifier.events)
	}
}

func TestStorageMonitorRetriesFailedNotificationWithoutRollingBackState(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(40),
		storageSampleWithAvailableMB(40),
	}}
	notifier := &recordingVoiceNotificationSink{err: fmt.Errorf("temporarily unavailable"), failures: 1}
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, sampler, nil, nil, notifier)

	first, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic})
	if err != nil {
		t.Fatalf("first CheckAndRemediate() error = %v", err)
	}
	if first.Level != StorageLevelWarning || monitor.Status().Level != StorageLevelWarning {
		t.Fatalf("status rolled back after notification failure: returned=%+v stored=%+v", first, monitor.Status())
	}

	notifier.err = nil
	if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic}); err != nil {
		t.Fatalf("retry CheckAndRemediate() error = %v", err)
	}
	if _, err := monitor.CheckAndRemediate(context.Background(), StorageCheckRequest{Reason: CheckReasonPeriodic}); err != nil {
		t.Fatalf("steady-state CheckAndRemediate() error = %v", err)
	}
	if len(notifier.events) != 3 {
		t.Fatalf("notification attempts = %d, want failed attempt, retry, and periodic refresh", len(notifier.events))
	}
}

func TestRuntimeStartStorageMonitorRunsStartupCheck(t *testing.T) {
	sampler := &sequenceStorageSampler{samples: []StorageSample{storageSampleWithAvailableMB(40)}}
	config := DefaultStorageConfig()
	config.Cleanup.Enabled = false
	monitor := NewStorageMonitor(config, sampler, nil, nil, nil)
	runtime := &Runtime{storageMonitor: monitor}

	if err := runtime.StartStorageMonitor(); err != nil {
		t.Fatalf("StartStorageMonitor() error = %v", err)
	}
	defer monitor.Stop()
	if status := monitor.Status(); status.Level != StorageLevelWarning || status.Revision != 1 {
		t.Fatalf("startup status = %+v, want warning revision 1", status)
	}
}

func TestRuntimeStorageCleanerOrderAndLevels(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	cfg.AudioArchive.StoragePath = t.TempDir()
	monitor := newRuntimeStorageMonitor(cfg, nil)

	wantNames := []string{
		"python_userbase",
		"tool_result_artifacts",
		"llm_http_log_7d",
		"llm_http_log_3d",
		"llm_http_log_1d",
		"audio_archive_30d",
		"audio_archive_7d",
		"session_archive_30d",
		"temporary_memory_expired",
		"episode_retention",
		"notification_context_14d",
		"notification_context_7d",
		"notification_context_1d",
		"llm_http_log_0d",
		"audio_archive_keep_0",
		"notification_context_processed",
	}
	wantLevels := []StorageLevel{
		StorageLevelNormal,
		StorageLevelNormal,
		StorageLevelNormal,
		StorageLevelWarning,
		StorageLevelCritical,
		StorageLevelWarning,
		StorageLevelCritical,
		StorageLevelWarning,
		StorageLevelNormal,
		StorageLevelNormal,
		StorageLevelNormal,
		StorageLevelWarning,
		StorageLevelCritical,
		StorageLevelEmergency,
		StorageLevelEmergency,
		StorageLevelEmergency,
	}
	if len(monitor.cleaners) != len(wantNames) {
		t.Fatalf("cleaner count = %d, want %d", len(monitor.cleaners), len(wantNames))
	}
	for index, cleaner := range monitor.cleaners {
		if cleaner.Name() != wantNames[index] {
			t.Errorf("cleaner %d name = %q, want %q", index, cleaner.Name(), wantNames[index])
		}
		leveled, ok := cleaner.(storageCleanerLevel)
		if !ok {
			t.Errorf("cleaner %q has no minimum level", cleaner.Name())
			continue
		}
		if leveled.MinimumLevel() != wantLevels[index] {
			t.Errorf("cleaner %q minimum level = %v, want %v", cleaner.Name(), leveled.MinimumLevel(), wantLevels[index])
		}
	}
	firstAudio := monitor.cleaners[5].(leveledStorageCleaner).StorageCleaner.(*AudioArchiveCleaner)
	secondAudio := monitor.cleaners[6].(leveledStorageCleaner).StorageCleaner.(*AudioArchiveCleaner)
	if firstAudio.maxFiles != 10 || secondAudio.maxFiles != 3 {
		t.Fatalf("audio max files = %d/%d, want 10/3", firstAudio.maxFiles, secondAudio.maxFiles)
	}
	if err := monitor.ValidateCleanupTargets([]string{"python_userbase"}); err != nil {
		t.Fatalf("ValidateCleanupTargets(python_userbase) error = %v", err)
	}
}

func TestNotificationContextRetentionStagesUseMemorySetting(t *testing.T) {
	tests := []struct {
		name      string
		base      int
		fallbacks []int
		want      []int
	}{
		{name: "default", base: 14, fallbacks: []int{14, 7, 1, 0}, want: []int{14, 7, 1, 0}},
		{name: "longer", base: 30, fallbacks: []int{14, 7, 1, 0}, want: []int{30, 14, 7, 1, 0}},
		{name: "shorter", base: 3, fallbacks: []int{14, 7, 1, 0}, want: []int{3, 1, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := notificationContextRetentionStages(tt.base, tt.fallbacks)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("stages = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRuntimeStorageMonitorSkipsArtifactCleanerWithoutConfigDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ConfigDir = ""
	cfg.AudioArchive.StoragePath = t.TempDir()
	monitor := newRuntimeStorageMonitor(cfg, nil)

	for _, cleaner := range monitor.cleaners {
		if cleaner.Name() == "tool_result_artifacts" {
			t.Fatal("artifact cleaner registered without an absolute config directory")
		}
	}
}

func TestLLMHTTPLogCleaner(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()

	// Create some test log files
	now := time.Now()
	files := []struct {
		name string
		age  time.Duration
		size int64
	}{
		{"llm-http-20260101.log", 30 * 24 * time.Hour, 1024 * 1024}, // 30 days old, 1MB
		{"llm-http-20260701.log", 8 * 24 * time.Hour, 512 * 1024},   // 8 days old, 512KB
		{"llm-http-20260714.log", 1 * 24 * time.Hour, 256 * 1024},   // 1 day old, 256KB
		{"llm-http-session-abc.log", 0, 128 * 1024},                 // Current, 128KB
		{"other.log", 10 * 24 * time.Hour, 64 * 1024},               // Not llm-http, should be ignored
	}

	for _, f := range files {
		path := filepath.Join(tmpDir, f.name)
		if err := os.WriteFile(path, make([]byte, f.size), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}

		// Set modification time
		modTime := now.Add(-f.age)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("failed to set mtime: %v", err)
		}
	}

	// Test cleaner with 7-day retention
	cleaner := NewLLMHTTPLogCleaner(tmpDir, 7, 1)

	// Estimate reclaimable
	estimate, err := cleaner.EstimateReclaimable(context.Background())
	if err != nil {
		t.Fatalf("EstimateReclaimable failed: %v", err)
	}

	// Should estimate ~1.5MB (30-day old + 8-day old files)
	expectedMin := uint64((1024*1024 + 512*1024) * 9 / 10) // Allow 10% tolerance
	if estimate < expectedMin {
		t.Errorf("expected estimate >= %d, got %d", expectedMin, estimate)
	}

	// Clean
	freed, err := cleaner.Clean(context.Background())
	if err != nil {
		t.Fatalf("Clean failed: %v", err)
	}

	if freed < expectedMin {
		t.Errorf("expected freed >= %d, got %d", expectedMin, freed)
	}

	// Verify files
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}

	expectedRemaining := []string{
		"llm-http-20260714.log",
		"llm-http-session-abc.log",
		"other.log",
	}

	if len(entries) != len(expectedRemaining) {
		t.Errorf("expected %d files, got %d", len(expectedRemaining), len(entries))
	}

	for _, entry := range entries {
		found := false
		for _, expected := range expectedRemaining {
			if entry.Name() == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected file remaining: %s", entry.Name())
		}
	}
}

func TestLLMHTTPLogCleanerProtectsCurrentSession(t *testing.T) {
	tmpDir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"llm-http-current-session.log", "llm-http-old-session.log"} {
		path := filepath.Join(tmpDir, name)
		if err := os.WriteFile(path, []byte("log"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
	cleaner := NewLLMHTTPLogCleanerWithSessionProvider(tmpDir, 0, 1, func() string { return "current-session" })

	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "llm-http-current-session.log")); err != nil {
		t.Fatalf("current session log was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "llm-http-old-session.log")); !os.IsNotExist(err) {
		t.Fatalf("old session log still exists, stat error = %v", err)
	}
}

func TestLLMHTTPLogCleanerFailsClosedWhenCurrentSessionCannotBeResolved(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "llm-http-old-session.log")
	if err := os.WriteFile(path, []byte("log"), 0o644); err != nil {
		t.Fatalf("write old session log: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes old session log: %v", err)
	}
	wantErr := fmt.Errorf("session index unavailable")
	cleaner := NewLLMHTTPLogCleanerWithCheckedSessionProvider(tmpDir, 0, 1, func() (string, error) {
		return "", wantErr
	})

	if _, err := cleaner.EstimateReclaimable(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("EstimateReclaimable() error = %v, want %v", err, wantErr)
	}
	if _, err := cleaner.ForceClean(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("ForceClean() error = %v, want %v", err, wantErr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log was removed after session resolution failure: %v", err)
	}
}

func TestLLMHTTPLogCleanerForceCleanDeletesSameDayNonCurrentLog(t *testing.T) {
	tmpDir := t.TempDir()
	name := "llm-http-" + time.Now().Add(-time.Minute).Format("200601021504") + ".log"
	path := filepath.Join(tmpDir, name)
	if err := os.WriteFile(path, []byte("log"), 0o644); err != nil {
		t.Fatalf("write same-day log: %v", err)
	}
	cleaner := NewLLMHTTPLogCleanerWithSessionProvider(tmpDir, 7, 1, func() string { return "current-session" })

	if _, err := cleaner.ForceClean(context.Background()); err != nil {
		t.Fatalf("ForceClean() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("same-day non-current log still exists, stat error = %v", err)
	}
}

func TestLLMHTTPLogCleanerZeroDayDeletesSameDayNonCurrentLog(t *testing.T) {
	tmpDir := t.TempDir()
	name := "llm-http-" + time.Now().Add(-time.Minute).Format("200601021504") + ".log"
	path := filepath.Join(tmpDir, name)
	if err := os.WriteFile(path, []byte("log"), 0o644); err != nil {
		t.Fatalf("write same-day log: %v", err)
	}
	cleaner := NewLLMHTTPLogCleanerWithSessionProvider(tmpDir, 0, 1, func() string { return "current-session" })

	estimate, err := cleaner.EstimateReclaimable(context.Background())
	if err != nil {
		t.Fatalf("EstimateReclaimable() error = %v", err)
	}
	if estimate != uint64(len("log")) {
		t.Fatalf("EstimateReclaimable() = %d, want %d", estimate, len("log"))
	}
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("same-day non-current log still exists, stat error = %v", err)
	}
}

func TestAudioArchiveCleaner(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test audio files
	now := time.Now()
	for i := 0; i < 15; i++ {
		name := filepath.Join(tmpDir, fmt.Sprintf("msg_%d.wav", i))
		if err := os.WriteFile(name, make([]byte, 500*1024), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}

		// Set different modification times
		modTime := now.Add(-time.Duration(i) * 24 * time.Hour)
		if err := os.Chtimes(name, modTime, modTime); err != nil {
			t.Fatalf("failed to set mtime: %v", err)
		}
	}

	// Test cleaner with max 10 files
	cleaner := NewAudioArchiveCleaner(tmpDir, 0, 10, 2)

	// Estimate
	estimate, err := cleaner.EstimateReclaimable(context.Background())
	if err != nil {
		t.Fatalf("EstimateReclaimable failed: %v", err)
	}

	// Should estimate exactly 2.5MB (5 oldest files)
	expected := uint64(5 * 500 * 1024)
	if estimate != expected {
		t.Errorf("expected estimate %d, got %d", expected, estimate)
	}

	// Clean
	freed, err := cleaner.Clean(context.Background())
	if err != nil {
		t.Fatalf("Clean failed: %v", err)
	}

	if freed != expected {
		t.Errorf("expected freed %d, got %d", expected, freed)
	}

	// Verify only 10 files remain
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}

	if len(entries) != 10 {
		t.Errorf("expected 10 files remaining, got %d", len(entries))
	}
}

func TestAudioArchiveCleanerZeroRetentionDeletesAllArchives(t *testing.T) {
	tmpDir := t.TempDir()
	for _, name := range []string{"one.wav", "two.wav", "ignore.txt"} {
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte("data"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	cleaner := NewAudioArchiveCleaner(tmpDir, 0, 0, 1)

	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "one.wav")); !os.IsNotExist(err) {
		t.Fatalf("one.wav still exists, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "two.wav")); !os.IsNotExist(err) {
		t.Fatalf("two.wav still exists, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "ignore.txt")); err != nil {
		t.Fatalf("non-audio file removed: %v", err)
	}
}

func TestSessionArchiveCleaner(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test session archive directories
	now := time.Now()
	for i := 0; i < 5; i++ {
		sessionDir := filepath.Join(tmpDir, fmt.Sprintf("session_%d", i))
		if err := os.MkdirAll(sessionDir, 0755); err != nil {
			t.Fatalf("failed to create session dir: %v", err)
		}

		// Create some files in each session
		for j := 0; j < 3; j++ {
			filePath := filepath.Join(sessionDir, fmt.Sprintf("file_%d.json", j))
			if err := os.WriteFile(filePath, make([]byte, 1024*1024), 0644); err != nil {
				t.Fatalf("failed to create session file: %v", err)
			}
		}

		// Set modification time
		modTime := now.Add(-time.Duration(i*10) * 24 * time.Hour)
		if err := os.Chtimes(sessionDir, modTime, modTime); err != nil {
			t.Fatalf("failed to set mtime: %v", err)
		}
	}

	// Test cleaner with max 3 sessions
	cleaner := NewSessionArchiveCleaner(tmpDir, 0, 3, 3)

	// Estimate
	estimate, err := cleaner.EstimateReclaimable(context.Background())
	if err != nil {
		t.Fatalf("EstimateReclaimable failed: %v", err)
	}

	// Should estimate ~6MB (2 oldest sessions, 3MB each)
	expectedMin := uint64(2 * 3 * 1024 * 1024 * 9 / 10)
	if estimate < expectedMin {
		t.Errorf("expected estimate >= %d, got %d", expectedMin, estimate)
	}

	// Clean
	freed, err := cleaner.Clean(context.Background())
	if err != nil {
		t.Fatalf("Clean failed: %v", err)
	}

	if freed < expectedMin {
		t.Errorf("expected freed >= %d, got %d", expectedMin, freed)
	}

	// Verify only 3 sessions remain
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("expected 3 sessions remaining, got %d", len(entries))
	}
}

func TestSessionArchiveCleanerZeroRetentionDeletesAllArchives(t *testing.T) {
	tmpDir := t.TempDir()
	for _, name := range []string{"session-one", "session-two"} {
		archiveDir := filepath.Join(tmpDir, name)
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(archiveDir, "events.jsonl"), []byte("event"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	cleaner := NewSessionArchiveCleaner(tmpDir, 0, 0, 1)

	estimate, err := cleaner.EstimateReclaimable(context.Background())
	if err != nil {
		t.Fatalf("EstimateReclaimable() error = %v", err)
	}
	if estimate != uint64(2*len("event")) {
		t.Fatalf("EstimateReclaimable() = %d, want %d", estimate, 2*len("event"))
	}
	freed, err := cleaner.Clean(context.Background())
	if err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	if freed != estimate {
		t.Fatalf("Clean() freed = %d, want estimate %d", freed, estimate)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("zero-retention cleanup left %d archives, want 0", len(entries))
	}
}

func TestSessionArchiveCleanerForceCleanIgnoresRetention(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "recent-session")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(archiveDir, "events.jsonl"), []byte("event"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cleaner := NewSessionArchiveCleaner(tmpDir, 30, 0, 1)
	if _, err := cleaner.ForceClean(context.Background()); err != nil {
		t.Fatalf("ForceClean() error = %v", err)
	}
	if _, err := os.Stat(archiveDir); !os.IsNotExist(err) {
		t.Fatalf("recent archive still exists after force cleanup, stat error = %v", err)
	}
}
