---
sidebar_position: 12
---

# Notification Persistence and Automatic Memory

This document describes the notification consumption, persistence, and automatic memory extraction pipeline. Notification events from the BLE service are consumed by the Agent, persisted to disk, and consolidated into Temporary or Long-Term Memory during idle windows.

## Architecture

The notification memory system uses a shared background worker that processes both Episode and Notification scenarios serially:

```text
MemoryWorker (idle-scheduled, serial execution)
├── EpisodeProcessor
└── NotificationProcessor
```

The Worker is responsible for foreground preemption, a default 5-minute idle delay, serial invocation of registered Processors in order, pending state, and lifecycle management. The Worker does not understand Episode, Notification, Memory types, or Store semantics, nor does it establish cross-scenario priority queues.

Each Processor owns its read strategy, batch size, extraction rules, proposal validation, and persistence. Both processors are registered at startup before the Worker starts, ensuring the first idle batch also follows the fixed Episode → Notification order.

`MemoryMergeEngine` extracts only the mechanical steps both scenarios need:

```text
Scenario input + related Memory → build messages → call LLM → return raw proposal
```

Episode and Notification proposal schemas, evidence thresholds, and conflict resolution remain in their respective Processors. After validation, both Processors submit explicit `MemoryIntent` operations to their Store adapter. Intent execution has shared create/update/reinforce/remove semantics while retaining the existing Device Memory and Temporary/Long-Term record schemas.

## Component Responsibilities

| Module                        | Responsibility                                                                                            |
| ----------------------------- | --------------------------------------------------------------------------------------------------------- |
| `ble_service`                 | Receives iOS/Android notifications, normalizes, assigns event IDs, maintains short-term event ring        |
| `NotificationContext`         | Consumes events, deduplicates, persists to JSONL, manages source/memory cursors, cleanup                  |
| `MemoryWorker`                | Unified idle window, foreground cancellation, serial Processor execution, retry, and stop                 |
| `episodeMemoryProcessor`      | Reads Episodes, extracts per-Episode rules, writes to Device Memory                                       |
| `NotificationMemoryProcessor` | Reads notifications, filters, calls extraction, validates proposals, writes to Temporary/Long-Term Memory |
| `MemoryMergeEngine`           | Recalls related Memory, builds messages, calls LLM, returns raw proposal                                  |

## Lifecycle

Notifications enter the `ble_service` event ring first. When the Agent enters an idle state, the NotificationProcessor consumes the ring, persists raw records to NotificationContext, and continues with extraction:

```text
Notification source → ble_service event ring
Agent foreground task ends → wait 5 minutes idle
                           → EpisodeProcessor (up to 3 Episodes)
                           → NotificationProcessor
                                → Consume / NotificationContext.append
                                → Extract up to 10 notifications
                           → Continue next batch in same idle window if still pending
```

When a notification is published, it wakes the shared Worker's idle timer. When the Worker is scheduled, the Processor calls `Consume` on the BLE ring, then reads its pending data. When no pending work remains, it returns an empty result.

## Notification Context

`NotificationContext` provides the runtime with:

```text
Consume(limit)               Pull from events_since and persist reliably
ReadPending(limit)           Read records after memory cursor
CommitProcessed(batch)       Advance memory cursor after Memory write succeeds
CleanupProcessedBefore(...)  Only clean processed date shards
```

Persisted records retain BLE original fields and add an Agent-local monotonically increasing `context_id`. The memory cursor uses `context_id` and does not depend on BLE generation. The BLE `source_cursor` is generation-local, while `stored_cursor` and `memory_cursor` remain monotonic Agent cursors; after a BLE generation reset, `source_cursor=0` alongside a larger stored cursor is expected. Reset diagnostics retain both the new generation and `previous_generation`.

Persistence uses UTC date-sharded JSONL. The shard date prefers the phone-provided notification `date` (including ANCS `YYYYMMDDTHHMMSS`) and falls back to board `received_at` only when the phone date is unavailable or invalid. Cleanup and reclaim estimation inspect each record before deleting an old-named shard. A recent or unparseable record protects the whole shard, which also protects historical records written under an incorrect board-clock filename. The source cursor advances only after write succeeds. On power recovery, completely written records are retained and the last incomplete JSON line is repaired.

`CommitProcessed` requires passing in a contiguous batch starting from the current cursor. Cleaners must call `NotificationContext` cleanup methods and cannot bypass Context to delete JSONL directly, to avoid racing with append/commit.

The Agent's `shell` tool can access `/userdata/agent/memory/notifications/events/` read-only. Files are saved as `YYYY-MM-DD.jsonl` by UTC date, one raw notification record per line. Date, app, body, and count filtering use the system's existing read-only shell tools. The Agent must not modify these files through the shell.

## Notification Processor

The Processor owns the complete business flow for the notification scenario:

1. `Consume` the current BLE ring, then `ReadPending` for this batch;
2. Coalesce added/modified/removed changes for the same notification;
3. Structurally filter removed and empty notifications;
4. For each notification in the batch, retrieve related Memory and merge into one `MemoryMergeEngine.Extract` call;
5. Classify each notification as user-specific fact, public information, marketing, secret, or one-off noise, then split by `context_id` and validate each proposal; each notification produces at most one action;
6. Apply actions (add, update, remove, reinforce, or promote) in Temporary or Long-Term Store;
7. Only `CommitProcessed` after all related Memory writes succeed; if backlog remains, continue next batch in the same idle window without waiting another 5 minutes.

Only user-specific state, commitments, deadlines, reservations, deliveries, bills, preferences, and rules are eligible for memory. General news, product facts, and public prices are ignored even when factually true. Source domains such as banking, health, legal, and messaging are not rejection criteria by themselves. When no model is available, source ingestion and raw storage continue, but `memory_cursor` stays behind so a later run with a configured model can classify the pending context. The Processor does not place notification business rules into the Worker, nor does it require Episode and Notification to use the same action set or state machine.

Notification proposal actions include `ignore`, `add`, `update`, `reinforce`, `remove`, and `promote`. Updating existing records requires accurate `memory_id` and `memory_revision`. Batch protocol violations such as missing, duplicate, or unknown `context_id` remain fatal and do not advance the memory cursor. A malformed proposal for one known notification is logged and treated as a no-op, valid siblings are still applied, and the completed source batch advances. Multiple redundant `ignore` actions normalize to one `ignore`.

Explicit Processor actions are not reinterpreted by the Store. `add` creates the Processor-assigned ID without similarity search; `update`, `reinforce`, and `remove` operate only on the specified ID with revision validation. The separate `save_memory` candidate path continues to use Store-owned duplicate detection because it does not carry a prevalidated target action.

## Episode Processor

EpisodeProcessor retains existing Episode semantics:

- Only accepts completed Episodes with device evidence;
- Processes at most 5 Episodes per batch and merges the entire batch of Episodes and related Device Memory into one LLM call;
- The LLM returns results with `episode_id`, then the Processor validates and persists item-by-item per Episode;
- Validates `goal_result`, candidate types, evidence references, and revisions itself;
- Executes Device Memory add, update, and conflict resolution itself;
- Uses the Episode processing ledger to save `processing → proposed → done/retry/ignored`, supporting crash recovery.

For both Processors, priority is code-owned so cross-scenario recall has stable weights. Confidence is supplied per model conclusion, validated to `(0, 1]`, and defaults to `0.7` only when omitted; explicit zero is invalid. An `update` replaces both values with metadata for the current conclusion, while `reinforce` and Notification promotion preserve stronger accumulated metadata.

## Memory and Recall

```text
memory/
├── notifications/   # Raw notifications and cursors
├── temporary/       # Short-term conclusions with expires_at
├── long_term/       # Stable preferences, rules, and facts
├── device/          # Device knowledge produced by Episodes
└── episodes/        # Immutable Episode traces
```

Temporary and Long-Term use the same retrievable record format. `recall_memory` searches both Stores simultaneously and filters expired Temporary entries; Device Memory is maintained separately by EpisodeProcessor.

Default values: single notification body up to 4 KiB; single BLE consume up to 100 entries; Notification single batch up to 10 entries; notification replay deduplication window retains up to 4096 fingerprints; Temporary default 7 days; auto-generated Long-Term default 90 days. Episode idle delay is configured by `episode_memory_idle_delay_seconds`, default 300 seconds; this value is also the shared Worker's idle window.

## Storage Levels and Privacy

`StorageMonitor` uniformly manages storage levels and Cleaners. Normal/Warning/Critical enable Notification Context cleanup at 14/7/1 days respectively; Emergency cleans all processed shards; no level deletes unprocessed records. Critical and Emergency disable `notification_context` write capability, so the Processor no longer consumes new notifications from BLE and resumes from the original source cursor after the level recovers. The Temporary Memory Cleaner can clean expired or deleted records starting at Normal level.

Notification body content is sent to the currently configured LLM provider as extraction input; only when a local model is configured does this content remain entirely on-device. Logs record event ID, App ID, processing results, and gaps only; they do not record body content.

## Failure and Recovery

- BLE consume failure: does not advance source cursor, retains pending, retries at next idle;
- LLM batch protocol failure or Memory write failure: does not advance memory cursor; a single record-level proposal validation failure is isolated as a no-op;
- Single Processor failure: logs the scenario error and retains its pending, backs off by idle delay to avoid hot loops from persistent BLE/LLM/disk errors; other registered scenarios can still execute serially in the same round and do not share business transactions;
- Foreground task starts: shared Worker cancels in-flight model call; Processor retains its own persistent state and continues at next idle;
- Worker restart: Episode recovers from processing ledger, Notification recovers from source/memory cursors;
- Notification add/update logic must remain idempotent; if the process crashes after Memory write succeeds but before cursor commit, the next run will re-extract that record;
- Store actions within one Notification batch execute sequentially; on mid-I/O failure, the cursor is not advanced and convergence relies on fixed IDs, revision checks, and retry;
- Any gap must be written to `NotificationContextState.Gaps` and must not be silently swallowed.

## Acceptance Criteria

- Episode and Notification are serially scheduled by the same `MemoryWorker` in registration order; each Processor calls the LLM once per batch, then validates and persists results item-by-item locally;
- Agent foreground tasks are not blocked by background Memory extraction and can cancel in-flight model calls;
- Both Processors can independently decide batch size, proposal structure, and Apply logic;
- `MemoryMergeEngine` does not own cross-scenario Apply semantics; Store adapters execute the shared explicit `MemoryIntent` contract;
- Cursors advance only after corresponding Memory writes succeed.

## Verification

Run the core regression suite from `src/agent`:

```bash
go test ./internal/agent -count=1
go vet ./...
```

Real-device validation is still required to measure notification extraction quality, cursor correctness, and foreground p50/p95 latency under a real model provider and BLE event stream.

## Related Documentation

- [Memory Plane](memory-plane.md)
- [Agent Context Lifecycle](context-lifecycle.md)
- [Storage Manager](storage-manager.md)
- [BLE Service](../03-services/ble-service.md)
