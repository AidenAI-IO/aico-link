---
sidebar_position: 1
---

# User Data Backup and Restore

This document describes the supported backup and restore contract for Aico Link
firmware. It is the operational reference for Config Web, the Bridge App
WebView, and the computer CLI. Detailed implementation notes and historical
design alternatives are intentionally kept out of this document.

## Scope

The feature protects user data before a full firmware flash and restores it
after the first boot of the new image. It covers:

- `/userdata`, including Agent configuration, memory, sessions, skills,
  identity, audio, and the persistent user home;
- Aico Link-managed SD-card data under `/mnt/sdcard`;
- configuration and credentials that are explicitly listed in the backup
  manifest; and
- integrity verification, transactional restore, rollback, and interrupted
  restore recovery.

Routine A/B OTA updates preserve userdata and do not require a backup. The
backup must be transferred off the device before a full `update.img` flash.
Backup archives must not be stored on the SD card being backed up.

The feature does not back up the Debian root filesystem, OTA download state,
locks, swap, temporary files, or arbitrary system files modified through SSH.
Loader and Maskrom modes do not provide the backup API.

## Existing configuration backup

The existing routes remain configuration-only:

| Operation | Endpoint | Format |
| --- | --- | --- |
| Export Agent configuration | `GET /api/config/backup` | Grouped TOML |
| Import Agent configuration | `PUT /api/config/backup` | Grouped TOML |

These routes cover `/userdata/agent/agent.toml` and must not be changed to
accept the binary user-data archive. Full backup and restore use separate
`/api/backup/*` and `/api/restore/*` routes so old clients cannot confuse TOML
with an archive.

## Device API

The full-archive workflow is exposed through these routes:

| Method and endpoint | Purpose |
| --- | --- |
| `GET /api/backup/capabilities` | Report supported format, storage, components, and transfer limits. |
| `POST /api/backup/jobs` | Create an automatic same-device backup job. |
| `GET /api/backup/jobs/{id}` | Read job state and progress. |
| `GET /api/backup/jobs/{id}/archive` | Stream the completed archive. |
| `POST /api/restore/jobs` | Create a restore job for an uploaded archive. |
| `GET /api/restore/jobs/{id}` | Read restore state and progress. |
| `PUT /api/restore/jobs/{id}/chunks/{index}` | Upload an ordered archive chunk. Upload pauses with `restore_plan_required` when the parsed manifest requires a plan. |
| `POST /api/restore/jobs/{id}/plan` | Parse the manifest and create the restore plan before remaining chunks are uploaded. A successful response reports `uploading_and_staging`, and the next chunk is accepted immediately; it reports `validating` when the whole archive was already uploaded. |
| `POST /api/restore/jobs/{id}/validate` | Validate the complete archive, including compatibility and integrity. |
| `POST /api/restore/jobs/{id}/apply` | Apply the validated restore transaction. |

The full workflow requires a local maintenance session and USB same-origin
authorization. Restore uploads are bounded chunks (up to 4 MiB), and the
server reports the negotiated chunk size when the restore job is created.
Clients must use the returned job state and error code rather than assuming
that an accepted job has completed.

## Backup components

The server selects all available components automatically; the UI does not
expose a component checklist. SD components are skipped when no SD card is
available.

| Component | Data |
| --- | --- |
| `agent_config` | Agent, model, device, and storage configuration |
| `system_environment` | API keys, tokens, secrets, and proxy variables |
| `network` | Wi-Fi and proxy configuration |
| `ota_settings` | User OTA source, proxy, credentials, and approved public key |
| `agent_skills` | User skills and skill state |
| `agent_memory` | Long-term, event, notification, and temporary memory |
| `agent_sessions` | Conversations, attachments, metadata, and tool artifacts |
| `user_home` | Persistent `/root` content |
| `preferences` | Local playback preferences |
| `device_identity` | Machine, SSH, and Bluetooth identity in same-device mode |
| `audio_archive` | eMMC audio archive |
| `sd_managed_audio` | Aico Link-managed SD audio archive |
| `sd_user_files` | User files on the SD card outside Aico Link-managed directories |
| `python_environment` | Dynamically installed user Python environment |
| `diagnostics` | Agent and system diagnostic logs |

`agent_memory` and `agent_sessions` are atomic components: restore must not
replace only an index, metadata directory, or attachment subdirectory.

## Identity and compatibility

New archives use same-device mode by default. The restore process compares the
hardware identity recorded in the archive with the target device and rejects
an identity mismatch unless an explicitly supported compatibility mode is
requested. The target firmware controls immutable fields such as partition
layout, device nodes, lock paths, and temporary paths; those fields are never
restored from the archive.

The archive manifest records the format version, firmware contract, hardware
identity, component list, sizes, and checksums. A restore must reject an
unsupported schema, incompatible platform contract, missing component, or
failed checksum before committing any component.

Archives use the `aico-backup` identity and `AICOBKP1` header. Managed SD audio
is stored in `aico/audio`, and restore transactions use `.aico-restore`.
Backup excludes these managed roots from `sd_user_files`; restore rejects
user files that overlap them, including FAT case, trailing-dot, and short-name
aliases. Top-level DOS 8.3 names containing a tilde and numeric suffix are
reserved because an archive cannot identify their long-name targets. Move
such user files to another directory before creating an archive.

## Transaction model

1. Acquire the global backup/restore lock and report the maintenance state.
2. Stop or quiesce writers, including the Agent, before taking the snapshot.
3. Create the archive or stage the uploaded archive outside the destination
   paths.
4. Verify archive header, manifest, component checksums, path constraints, and
   available space.
5. Apply components using temporary files and directories, `fsync`, atomic
   rename, and parent-directory `fsync`.
6. Preserve old files until the complete component passes validation.
7. Commit the transaction and restart the affected services.
8. If commit fails, attempt rollback from the transaction log. If the operation
   is interrupted, resume the recorded commit or rollback before services read
   the affected data; do not assume every interruption restores the last
   known-good state automatically.

The archive is unencrypted and its contents remain recoverable even when
compression is used. Per-file, per-chunk, and footer checksums provide
integrity, not confidentiality. Treat downloaded archives as sensitive
plaintext and store them only in access-controlled locations.

## Client behavior

Config Web presents full backup and restore as a guided operation. Export and
Import remain the primary actions; Agent TOML import/export is available only
as an explicitly named advanced operation. The browser and Bridge App transfer
the archive in bounded chunks rather than buffering the entire archive in
memory.

The CLI uses the same device API and must display progress, verification
failures, maintenance state, and the final transaction result. A cancelled
operation must either complete its current atomic step or leave the device
recoverable through the transaction log.

## Implementation status

The device-side and CLI portions of the first implementation are available.
Native Bridge App WebView bridging remains a separate integration task. When
the archive format, component set, or compatibility rules change, update this
document together with the manifest version and compatibility tests.
