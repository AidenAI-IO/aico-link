---
sidebar_position: 10
---

# Storage Subsystem: StorageManager and StorageMonitor

## Overview

The device has two separate storage components. They share the `[storage_settings.storage]`
configuration section, but run in different service owners and solve different
problems.

| Component | Responsibility | Primary HTTP API |
| --- | --- | --- |
| `StorageManager` (Config Web) | Detects and mounts the SD card, derives eMMC-only or dual-storage mode, routes governed data, migrates older data, safely ejects the card, and formats the card | Config Web `/api/storage/status`, `/api/storage/eject`, `/api/storage/format` |
| `StorageMonitor` | Samples persistent-storage capacity, classifies pressure levels, runs cleanup, and degrades non-essential writes when space is low | `/api/storage/monitor/status`, `/api/storage/cleanup` |

`StorageManager` does not replace `StorageMonitor`. Adding an SD card provides a
migration tier for governed data, while `StorageMonitor` continues to protect
the configured persistent root, which defaults to `/userdata`.

Related documentation:

- [Paths, Artifacts & Config Cheat Sheet](../02-architecture/paths-and-artifacts.md)
- [Agent Configuration Reference](configuration.md)
- [Persistent Python Package Environment Design](python-packages.md)

## StorageManager: SD Card and Dual-Storage Routing

The production Config Web process creates and starts `StorageManager`
unconditionally. Missing, removed, rejected, or unusable card hardware falls
back to eMMC-only operation; there is no user preference that forces a storage
mode. The Agent creates only a read-only view of the state mirror and never
touches SD-card hardware.

### Card Lifecycle and Effective Mode

The manager polls for the configured SD block device and debounces insertion and
removal. When a card appears, it:

1. Prefers the first partition, falling back to the whole device when there is
   no partition.
2. Runs `fsck` with a 60-second deadline; a missing checker falls back to the
   mount/probe gate, while I/O errors or uncorrected errors reject the card.
3. Mounts the card read-write at the configured mount point.
4. Verifies the mount with a unique probe file, `fsync`, and readback. Existing
   mounts must belong to the requested device. Read-only mounts are unhealthy.
5. Attempts to reject a card with less than the configured minimum free space
   by unmounting it. If a busy filesystem cannot be unmounted, the manager
   keeps the card mounted and reports it as usable rather than recording a false
   unmounted state.

The default mount point is `/mnt/sdcard`, the default device setting is `auto`,
and the default minimum free space is 64 MiB. Discovery selects exactly one
`/sys/block/mmcblk*/device/type` with value `SD`; it excludes eMMC (`MMC`) and
Wi-Fi (`SDIO`). It does not rely on the kernel's `removable` flag. Aiden SCH v1
uses `mmcblk1`; older Pico Zero configurations can use `mmcblk2`. An explicit
device override is preserved; ambiguous discovery requires an override.
A card normally becomes usable only after it passes the mount and write checks;
the busy-unmount case above is the exception to the minimum-free-space rejection.

| Effective mode | JSON value | Condition |
| --- | --- | --- |
| eMMC only | `1` | No usable card is mounted |
| Dual storage | `2` | A usable card is mounted |

A positively identified blank card is automatically formatted as FAT32 once per
insertion. Cards with an existing but unsupported, damaged, or otherwise
unmountable filesystem are never automatically erased. Blank detection also
requires a successful low-level signature check and zeroed initial sectors;
failed reads and I/O failures never trigger automatic formatting.

The manager mirrors card, format, migration, and effective-mode state to
`/run/aiden/storage.state` for the Agent and other device services. This state
file is separate from the StorageMonitor level file at
`/run/agent/storage_level`.

### Data Routing and Migration

New governed data is always written to eMMC. The current production integration
uses this routing for audio archives:

| Operation | No mounted SD card | Mounted SD card |
| --- | --- | --- |
| New audio archive | `/userdata/audio` | `/userdata/audio` |
| Read audio archives | `/userdata/audio` | eMMC first, then `/mnt/sdcard/aiden/audio` |
| Retention cleanup roots | `/userdata/audio` | `/mnt/sdcard/aiden/audio` |

Keeping new writes on eMMC avoids making active recording depend on removable
media. When the SD tier is available and eMMC free space falls below the start
watermark, the manager migrates older audio archives to the SD card, oldest
first. It stops after eMMC reaches the stop watermark, no eligible files remain,
the card becomes unavailable, or an eject or format operation cancels the run.

The default migration watermarks are:

- Start when eMMC free space is below 10%.
- Stop when eMMC free space reaches 50%.

Only regular files at least 60 seconds old are eligible. Each file is copied to
an `.aiden-partial` path, synced, size-checked, renamed into place, and only then
removed from eMMC. Readers prefer the eMMC copy during the brief window where
both copies may exist. Failed or exhausted runs use a ten-minute retry cooldown.

### Safe Eject and Formatting

Safe eject cancels any active migration, syncs and unmounts the card, and latches
the card as ejected. It is not mounted again until it is physically removed and
reinserted.

Manual formatting is destructive: it rewrites the whole card with a fresh MBR,
one partition, and a new filesystem labeled `AIDEN`. Supported filesystems are
FAT32, ext4, and exFAT. Formatting runs asynchronously. After `FormatDisk`
succeeds, the manager attempts to mount the new filesystem; check `card.mounted`
and `effective_mode` to confirm that dual-storage mode was restored, because the
post-format mount can still fail.

The formatter resolves the required `mkfs` executable before changing the MBR.
FAT32 uses `mkfs.vfat -F 32`. Debian includes `dosfstools`, `e2fsprogs`, and
`exfatprogs`; the image audit checks their formatting and checking executables.
Mount/unmount/sync commands have 15-second deadlines, signature probes have
3-second deadlines per invocation, and formatting has a 5-minute deadline.
Kernel I/O in uninterruptible sleep can still exceed a userspace deadline.
Storage events and rejection reasons are logged to the Config Web journal.
Mounted cards also receive a synced write/readback probe every 30 seconds.
Health I/O runs outside the status lock; a failed probe detaches the SD tier and
prevents repeated mount attempts until reinsertion.

### StorageManager HTTP API

These endpoints report and control the SD/eMMC manager. They are distinct from
the StorageMonitor endpoints documented later.

All three endpoints return HTTP 405 for the wrong method and HTTP 503 if the
runtime has no StorageManager. A normal production runtime always creates the
manager, even when no card is present.

#### Get Manager Status

~~~http
GET /api/storage/status
~~~

Example response:

~~~json
{
  "effective_mode": 2,
  "card": {
    "present": true,
    "mounted": true,
    "device": "/dev/mmcblk1p1",
    "total_bytes": 68719476736,
    "free_bytes": 64424509440
  },
  "mount_point": "/mnt/sdcard",
  "format_job": {
    "status": "idle"
  },
  "migration": {
    "status": "idle"
  }
}
~~~

`format_job.status` and `migration.status` are `idle`, `running`, `success`, or
`failed`. Active and completed jobs also report the applicable filesystem,
timestamps, detail, error, moved-file count, and moved-byte count.

#### Safely Eject the Card

~~~http
POST /api/storage/eject
~~~

The successful response is the updated manager status. The endpoint returns
HTTP 409 when the card cannot be ejected, including when no card is mounted, a
mount attempt or format is active, migration cannot stop promptly, or the
filesystem is busy and cannot be unmounted.

#### Format the Card

~~~http
POST /api/storage/format
Content-Type: application/json
~~~

~~~json
{
  "fs": "fat32",
  "confirm": "format-sd-card"
}
~~~

`fs` may be `fat32`, `ext4`, or `exfat`; an empty value defaults to `fat32`.
The exact confirmation token is required because formatting erases the entire
card. HTTP 202 means that the asynchronous job was accepted, not that formatting
has finished. Poll `GET /api/storage/status` and inspect `format_job` for the
result. Invalid requests return HTTP 400.

### StorageManager Configuration

~~~toml
[storage_settings.storage]
mount_point = "/mnt/sdcard"
device = "auto" # Or an explicit mmcblk1/mmcblk2 override
min_card_free_mb = 64
migrate_start_free_pct = 10
migrate_stop_free_pct = 50
~~~

## StorageMonitor: Capacity Protection

`StorageMonitor` protects the Agent from persistent-storage exhaustion. It is
responsible for:

- Sampling the configured storage root.
- Classifying the final storage level.
- Running ordered cleanup stages.
- Resampling after every cleaner.
- Applying recovery hysteresis.
- Disabling non-essential writes under pressure.
- Triggering remediation after ENOSPC or EROFS errors.
- Exposing status and manual-cleanup HTTP endpoints.
- Publishing the final level to deployment-side guards.

Notification consumers, Companion App behavior, LED presentation, and other
user-facing delivery policies are outside StorageMonitor's scope.

### Capabilities

| Capability | Status | Notes |
| --- | --- | --- |
| Startup check | Available | Runs once when the Agent runtime starts |
| Periodic monitoring | Available | Defaults to a 300-second interval |
| Tiered classification | Available | Normal, Warning, Critical, Emergency |
| Recovery hysteresis | Available | Prevents level flapping near thresholds |
| Automatic cleanup | Available | LLM HTTP logs, audio archives, session archives |
| Post-cleanup resampling | Available | Final state always comes from statfs |
| Degraded write controls | Available | Suspends non-essential persistence |
| Write-error remediation | Available | Handles ENOSPC and EROFS |
| Status endpoint | Available | `GET /api/storage/monitor/status` |
| Cleanup endpoint | Available | `POST /api/storage/cleanup` |
| Deployment state file | Available | /run/agent/storage_level |
| Agent log integration | Available | Default path is /userdata/agent/log/agent.log |
| Notification event output | Available | Publishes through the global VoiceNotificationManager |
| LED and App presentation | Out of scope | Consumers can poll the status endpoint |

### Storage Scope

#### Managed Root

StorageMonitor manages one configured persistent-storage root. The default is:

~~~text
/userdata
~~~

The default Agent configuration directory is /userdata/agent. Important persistent paths include:

| Data | Default path |
| --- | --- |
| Agent main log | /userdata/agent/log/agent.log |
| LLM HTTP logs | /userdata/agent/log/llm-http-*.log |
| Session data | /userdata/agent/memory |
| Session archives | /userdata/agent/memory/session_archive |
| Long-term memory | /userdata/agent/memory/long_term |
| Audio archives | /userdata/audio |
| Agent configuration | /userdata/agent/agent.toml |

The Agent main log has moved from /var/log/agent/agent.log to `<CONFIG_DIR>/log/agent.log`. With the default configuration directory, the resolved path is /userdata/agent/log/agent.log. The legacy file is not migrated and is not used as a fallback.

Other services may still write logs under /var/log, including audio_service, frame_service, and adb. Those logs are not managed by the Agent StorageMonitor cleaners.

#### Protected Data

StorageMonitor does not delete:

- The active session.
- Long-term memory.
- Agent or device configuration.
- OTA files currently in use.
- Arbitrary files outside configured cleaner directories.

It also does not manage RAM, CPU, temperature, battery pressure, or volatile /tmp usage.

#### OTA Partition Interaction

OTA uses a dedicated 300 MiB ext4 partition mounted at `/userdata/ota`.
StorageMonitor still samples the `/userdata` filesystem, so OTA downloads do
not reduce the available-byte value used by the 50/10/5 MiB thresholds.

StorageMonitor cleaners do not target OTA paths. The SD-card StorageManager
does not route or migrate OTA downloads and has no OTA lock or capacity
coupling. Existing cleanup stages and user-facing storage notifications remain
necessary for the non-OTA data stored on `/userdata`. See
[OTA Dedicated Storage Partition](../08-ota/no-space-plan.md).

### Runtime Flow

~~~text
Startup check ───────┐
Periodic check ──────┤
ENOSPC / EROFS ──────┼→ StorageMonitor.CheckAndRemediate
Manual cleanup ──────┘              │
                                    ├→ statfs(root_path)
                                    ├→ classify initial level
                                    ├→ run eligible cleaners by priority
                                    ├→ statfs after every cleaner
                                    ├→ apply recovery hysteresis
                                    └→ publish final StorageMonitorStatus
                                           ├→ update write capabilities
                                           ├→ update storage_level file
                                           ├→ serve HTTP status
                                           └→ optionally publish an event
~~~

All entry points share the same remediation path. A monitor-wide mutex serializes checks so startup, periodic, write-failure, and manual cleanup requests cannot delete the same files concurrently.

### Check Triggers

#### Startup

The daemon calls StartStorageMonitor after creating the Agent runtime.

StartStorageMonitor:

1. Runs CheckAndRemediate with the startup reason.
2. Starts the periodic monitor loop even if the initial check reports an error.
3. Stops the loop and removes the deployment state file when the runtime closes.

#### Periodic Monitoring

The default interval is 300 seconds. Periodic checks can run maintenance cleaners while the storage level is Normal. For example, the normal LLM HTTP log stage removes files beyond the default retention window.

#### Write Errors

Writers pass storage-related errors to StorageMonitor. An immediate asynchronous check is scheduled only for:

- ENOSPC: no space left on device.
- EROFS: read-only filesystem.

Repeated failures are coalesced. At most one write-failure remediation request is pending at a time.

#### Manual Cleanup

POST /api/storage/cleanup uses the same CheckAndRemediate flow with the manual reason.

- With force=false, Normal, Warning, and Critical cleanup stages are eligible even when the current level is Normal. Emergency-only stages remain gated unless the effective level is Emergency. The flow still stops after a cleaner resamples at Normal.
- With force=true, selected cleaners ignore ordinary retention thresholds and run their force-clean behavior.
- Force cleanup still respects cleaner-directory boundaries and protected-data rules.

### Storage Levels

Levels are based on available bytes, not percent used.

| Level | Default available-space range | Behavior |
| --- | --- | --- |
| Normal | Greater than 50 MB | Normal operation and low-risk maintenance |
| Warning | Greater than 10 MB and at most 50 MB | Warning-stage cleanup |
| Critical | Greater than 5 MB and at most 10 MB | Aggressive cleanup and non-essential write suspension |
| Emergency | At most 5 MB | Enables configured emergency cleanup stages while the effective level remains Emergency |

Threshold comparisons use exact bytes. Exactly 10 MB is Critical, and exactly 5 MB is Emergency.

#### Recovery Hysteresis

Escalation takes effect immediately. De-escalation requires available space to exceed the previous level threshold plus recovery_hysteresis_mb.

With the default 5 MB hysteresis:

| Previous level | Space required to de-escalate |
| --- | --- |
| Warning | Greater than 55 MB |
| Critical | Greater than 15 MB |
| Emergency | Greater than 10 MB |

This prevents repeated transitions when available space oscillates near a threshold.

#### Final-State Rule

The initial sample only decides whether remediation should begin. StorageMonitor resamples the filesystem after every cleaner.

The final level, unavailable capabilities, deployment state file, and HTTP response are based on the final statfs sample after hysteresis. A cleaner's reported freed-byte count is retained for audit history but never replaces resampling.

### Cleanup Pipeline

#### Cleaner Interface

~~~go
type StorageCleaner interface {
    Name() string
    Priority() int
    EstimateReclaimable(ctx context.Context) (uint64, error)
    Clean(ctx context.Context) (freed uint64, err error)
}
~~~

Cleaners run in ascending Priority order. Non-force checks normally call EstimateReclaimable first and skip a cleaner when its estimate is zero. A cleaner may additionally implement EmergencyClean for behavior that is safe only when the effective level is Emergency.

#### Default Stages

The runtime builds cleanup stages from the configured retention arrays.

| Data | Default stage | Minimum level | Protection |
| --- | --- | --- | --- |
| LLM HTTP logs | 7 days | Normal | Protects the active session log |
| LLM HTTP logs | 3 days | Warning | Protects the active session log |
| LLM HTTP logs | 1 day | Critical | Protects the active session log |
| LLM HTTP logs | 0 days | Emergency | Removes matching logs except the active session log |
| Audio archives | 30 days, keep up to 10 | Warning | Only touches the audio archive directory |
| Audio archives | 7 days, keep up to 3 | Critical | Only touches the audio archive directory |
| Audio archives | 0 days, keep none | Emergency | Only touches the audio archive directory |
| Session archives | 30 days | Warning | Does not touch the active session or long-term memory |

A retention value of 0 creates an emergency cleanup stage. The default session_archive_retention_days value is [30], so an all-session-archives emergency stage is not enabled unless 0 is explicitly added.

#### Notification Memory Stages

The runtime registers Temporary Memory and Notification Context cleaners when
the Agent config directory is available.

| Data | Stage | Minimum level | Protection |
| --- | --- | --- | --- |
| Temporary Memory | Per-record `expires_at` | Normal | Deletes only expired records and rebuilds its index |
| Notification Context | 14 days | Normal | Deletes only Memory-processed date shards |
| Notification Context | 7 days | Warning | Deletes only Memory-processed events |
| Notification Context | 1 day | Critical | Deletes only Memory-processed events |
| Notification Context | 0 days | Emergency | Deletes all fully processed shards; unprocessed records remain protected |

Notification cleanup is cursor-aware. StorageMonitor invokes the registered
cleaner, while `NotificationContext.CleanupProcessedBefore(...)` owns file
selection; StorageMonitor never deletes notification JSONL directly.
At Critical and Emergency levels the monitor also marks
`notification_context` unavailable, so Agent pauses new BLE consumption until
the storage level recovers. Existing unprocessed JSONL remains protected.

This is not a reuse of an existing Episode cleaner. The current Episode pipeline
has no retention cleaner for `memory/episodes/`; its bounded processing ledger
only removes old terminal statuses. Device Memory TTL is likewise a recall
filter rather than physical storage reclamation.

#### Python Package Cleanup

The persistent Python environment registers one `python_userbase` cleaner.

| Cleaner | Normal and manual force behavior | Emergency behavior | Protection |
| --- | --- | --- | --- |
| `python_userbase` | Removes direct children of `/userdata/tmp` older than 24 hours; retains the whole Python user base | Clears `/userdata/agent/python`, recreates it empty, and preserves recent temp entries | Never follows symlink targets; never clears the user base below Emergency |

Manual cleanup accepts `python_userbase` as a target. `force=true` below
Emergency still preserves installed packages. See
[Persistent Python Packages](python-packages.md) for environment and cleanup
details.

#### Stage Eligibility and Stop Conditions

After each cleaner, StorageMonitor resamples and recalculates the effective level.

- Non-force remediation stops after recovery to Normal.
- Automatic startup, periodic, and write-error remediation skip a cleaner when its minimum level is above the recalculated effective level.
- Non-force manual cleanup bypasses the Normal, Warning, and Critical minimum-level gates, but retains the Emergency gate.
- A cleanup that raises the level from Emergency to Critical prevents later Emergency-only stages from running.
- Force cleanup continues through all selected cleaners.
- A cleaner failure is recorded and does not prevent later cleaners from running.

#### Cleanup Retry Interval

Automatic cleanup has a default minimum retry interval of 60 seconds to avoid repeatedly scanning directories under sustained pressure.

The retry interval is bypassed for:

- force=true.
- Manual cleanup.
- ENOSPC or EROFS remediation.

### Degraded Writes

Warning triggers cleanup but does not disable capabilities. Critical and Emergency compute unavailable capabilities from the degraded-mode configuration.

| Capability | Default behavior |
| --- | --- |
| llm_http_log | Stops new raw LLM HTTP logs |
| audio_archive | Stops new audio archives |
| session_archive | Stops new session archives |
| session_persistence | Keeps the in-memory session and marks persistence as pending |

Capabilities are recalculated after every successful check and recover automatically when storage returns to a safe level.

#### Agent Main Log

The Agent main log is not completely disabled through AllowWrite. The
`aiden-agent-log-retention.timer` checks it once per minute. During normal
operation, a log larger than 10 MiB is trimmed to its newest 5 MiB. At Critical
or Emergency, the timer instead trims it to
`storage_settings.storage.degraded_mode.max_agent_log_mb`, which defaults to 1
MiB. Trimming atomically collapses an aligned range at the front of the file,
preserving the inode and newest content so systemd can continue appending
without restarting the Agent. Filesystem alignment may leave the retained size
up to one block below the target. One filesystem block is the minimum effective
limit for byte-level deployment overrides; product configuration is expressed
in MiB and is therefore unaffected.

### Status Model

StorageMonitorStatus is the immutable snapshot produced by a successful check.

| Field | Meaning |
| --- | --- |
| Path | Managed root |
| TotalBytes, UsedBytes, AvailableBytes | Final statfs values |
| PercentUsed | Used-space percentage |
| Level | Final storage level |
| UnavailableCapabilities | Writes currently suspended |
| Revision | Increments after every successful check |
| LastCleanupAt | Time of the most recent cleaner execution |
| LastCleanupFreedBytes | Bytes reported by the latest cleanup flow |
| CleanupHistory | Most recent cleaner results, capped at 50 entries |
| CheckedAt | Final sample time |

Status returns copies of slice fields so callers cannot mutate the monitor's internal state.

#### Deployment State File

Every successful check atomically updates:

~~~text
/run/agent/storage_level
~~~

The file contains normal, warning, critical, or emergency. Deployment-side guards use it to apply the Agent main-log limit.

Disabling or stopping StorageMonitor removes both the state file and its temporary file so deployment scripts do not consume stale state.

### StorageMonitor HTTP API

#### Get Status

~~~http
GET /api/storage/monitor/status
~~~

Example response:

~~~json
{
  "path": "/userdata",
  "total_mb": 3072,
  "used_mb": 3027,
  "available_mb": 45,
  "percent_used": 98.5,
  "alert_level": "warning",
  "degraded_mode": false,
  "unavailable_capabilities": [],
  "status_revision": 12,
  "last_cleanup": "2026-07-22T08:30:00Z",
  "last_cleanup_freed_mb": 32,
  "cleanup_history": [
    {
      "timestamp": "2026-07-22T08:30:00Z",
      "cleaner": "llm_http_log_7d",
      "freed_mb": 32,
      "status": "success"
    }
  ]
}
~~~

The endpoint provides a consistent snapshot suitable for polling. Real-time push and user-interface presentation are outside this module.

#### Run Cleanup

~~~http
POST /api/storage/cleanup
Content-Type: application/json
~~~

Example request:

~~~json
{
  "force": false,
  "targets": ["llm_http_log", "audio_archive"]
}
~~~

targets can be empty or contain these cleanup categories:

- llm_http_log
- audio_archive
- session_archive

Full cleaner names are also accepted. Unknown categories, abbreviations, unknown JSON fields, trailing JSON documents, and request bodies larger than 64 KB return HTTP 400.

Example response:

~~~json
{
  "success": true,
  "freed_mb": 45,
  "final_alert_level": "normal",
  "available_mb": 90,
  "cleaners_run": [
    {
      "name": "llm_http_log_7d",
      "freed_mb": 45,
      "status": "success"
    }
  ]
}
~~~

freed_mb reports only cleaners executed by the current POST request. It does not repeat the previous cleanup's freed-space value.

### StorageMonitor Configuration

Default configuration:

~~~toml
[storage_settings.storage]
monitor_enabled = true
root_path = "/userdata"
check_interval_seconds = 300
warning_threshold_mb = 50
critical_threshold_mb = 10
emergency_threshold_mb = 5
recovery_hysteresis_mb = 5

[storage_settings.storage.degraded_mode]
disable_llm_http_log = true
disable_audio_archive = true
disable_session_archive = true
max_agent_log_mb = 1

[storage_settings.storage.cleanup]
enabled = true
llm_http_log_retention_days = [7, 3, 1, 0]
audio_archive_retention_days = [30, 7, 0]
session_archive_retention_days = [30]
notification_context_retention_days = [14, 7, 1, 0]
cleanup_retry_interval_seconds = 60
~~~

Validation rules:

- root_path is required when storage.monitor_enabled is true.
- check_interval_seconds must be greater than zero.
- Thresholds must satisfy emergency < critical < warning.
- max_agent_log_mb must be greater than zero.
- cleanup_retry_interval_seconds cannot be negative.
- Retention values cannot be negative. Notification Context cleanup deletes only fully processed date shards; records newer than `memory_cursor` remain protected even for forced cleanup.

### Optional Event Output

StorageMonitor exposes a VoiceNotificationSink interface for final storage-state events. The Agent runtime injects the global VoiceNotificationManager as the production consumer.

- Non-Normal states publish active.
- Severity changes update the stable storage:device condition.
- Recovery to Normal publishes resolved.
- Periodic checks can refresh an equivalent active event.
- Publish failures do not roll back cleanup or StorageMonitorStatus.

Queueing, presentation, playback, deduplication, and delivery policy remain owned by VoiceNotificationManager.

### Testing

The current test suite covers:

- Exact-byte threshold boundaries.
- Recovery hysteresis.
- Post-cleanup resampling and final-state publication.
- Cleanup retry intervals.
- Manual and force cleanup behavior.
- Runtime cleaner order and minimum levels.
- Capability degradation and recovery.
- ENOSPC and EROFS remediation.
- Deployment state-file lifecycle.
- Event activation, severity updates, resolution, and retry.
- GET status consistency and cleanup history.
- POST cleanup target and JSON validation.
- Agent main-log capping under Critical and Emergency.

Common checks:

~~~bash
cd src/agent
go test ./internal/agent
~~~

### Current Limitations

- Only one root_path is managed.
- Logs for other services under /var/log are not cleaned.
- The status API supports polling but not real-time push.
- LED, Companion App, and complete Web UI presentation are outside this module.
