---
sidebar_position: 9
---

# Memory Plane

This document describes the current filesystem-backed memory architecture used by the Aiden Agent. Session memory is documented separately in [Session Memory Compaction](session-memory.md).

## Responsibilities

The memory plane separates four kinds of persisted state:

- session memory keeps the active conversation and compressed history;
- long-term memory keeps information explicitly saved for the user;
- device memory keeps reusable device, application, navigation, procedure, calibration, failure, and fact knowledge;
- task episodes keep immutable execution evidence for audit and background consolidation.

Automatic Episode learning writes only Device Memory. It does not create user profile, preference, or rule entries in Long-Term Memory.

## Runtime Flow

Device memory is not injected into every prompt. The model decides whether to call `recall_device_memory` during execution when the task materially depends on saved device or UI knowledge. Runtime does not perform a pre-run relevance route and does not force a first tool call.

```text
Run starts
  |
  v
build the normal model-controlled run
  |
  v
Agent calls recall_device_memory only when needed
  |
  v
Agent executes tools and records an Episode
  |
  v
persist the completed Episode
  |
  +--> update deterministic device/app profiles
  |
  `--> notify the shared background MemoryWorker
```

Recall tools record the IDs actually shown to the Agent. The background worker checks those records first when deciding whether a new lesson should update an existing Memory.

## Storage Layout

Memory is stored under `<config_dir>/memory/`.

```text
memory/
├── session/                     # active conversation memory
├── session_archive/             # closed session logs
├── long_term/                   # explicit user memory
├── episodes/                    # immutable task Episodes
├── device/
│   ├── profile.yaml             # deterministic device profile
│   ├── apps/                    # deterministic app profiles
│   ├── procedures/
│   ├── navigation/
│   ├── calibration/
│   ├── failures/
│   └── memories/                # facts and uncategorized Device Memory
└── lifecycle/
    ├── reflection.yaml          # Episode Memory processing ledger
    └── reflection.lock          # cross-runtime batch lock
```

The `reflection.yaml` and `reflection.lock` names are retained for upgrade compatibility. Their owner is now the Episode Memory pipeline, not the removed failure-only Reflection implementation.

## User Memory Recall

`recall_memory` searches both temporary and long-term records. Topic terms in
`tags` and `entities` can match either metadata field, so classifying a term
differently at save and recall time does not make the record inaccessible.
Matches in the requested field receive more weight than matches in the other
field; title, summary, and content matches also contribute to relevance.
The same relevance scoring is used when merging results from both stores.
Active-state and expiry checks still apply, and a matching type alone cannot
make an unrelated record satisfy a topical query.

Explicit requests to remember information or a rule for future conversations
require a successful `save_memory` call before the Agent confirms persistence.

`recall_session_chunks` searches the backend ContextManager's shared chunk
index. Topic queries match tags, entities, and summary text, including topics
outside the configured extraction vocabulary. Explicit chunk IDs remain exact
selectors. Results retain newest-first ordering and the requested limit.
An empty topical result permits one broader query before reporting that the
conversation history is unavailable.

## Device Memory Recall

Normal `recall_device_memory` results have the following limits:

- only `active` records are returned;
- at most five records are returned;
- output is limited to approximately 4,800 characters;
- expired or device-inapplicable records are excluded;
- `pending`, `disputed`, and legacy `conflicted` records are consolidation-only context.

The recall tool searches active, applicable Device Memory using the query supplied by the model. It records the IDs actually returned on the Episode; no runtime-side candidate list is injected or used to rewrite the model's query.

## Episode Recording

Every completed run with a user goal persists a task Episode before background learning starts. Success, failure, and interruption are recording states, not learning admission rules.

An Episode can contain:

- the original user goal and normalized scope;
- tool calls, inputs, results, and structured errors;
- user steer or correction events;
- screenshot and post-action screenshot references;
- recalled Memory IDs;
- the recorded runtime outcome and final answer.

Screenshot binary data is stored as an Episode artifact. Event records contain relative references rather than embedding large base64 payloads.

The recorded `outcome.success` value is not treated as verified truth. It is input evidence for later assessment.

### Episode Storage Retention

The current Episode implementation does not register a retention cleaner for
`memory/episodes/`. Episode metadata, events, and artifacts remain on disk until
the whole memory plane is explicitly cleared.

The consolidation ledger is bounded separately. It retains the latest 64
`done` or `ignored` terminal statuses while preserving processing and retry
state. Device Memory TTL also affects recall only: expired records are excluded
from search, but StorageMonitor does not routinely delete their YAML files.

## Episode Memory Consolidation

The background pipeline is:

```text
completed Episode batch (up to three)
  |
  v
deterministic prefilter
  |
  v
one background LLM assessment and extraction for the batch
  |
  v
code-level evidence and type validation
  |
  v
create or update Device Memory
```

### Admission

An Episode is eligible for the model call when it contains either:

- a paired device tool call and result; or
- a non-canceled structured error.

Greetings, ordinary conversation, Memory-only traces, successful Web-only traces, and cancellation before a meaningful device action are ignored without calling the model.

An interrupted Episode remains eligible when it contains useful device evidence before the interruption.

### Model Input

The model receives a bounded view of the Episode:

- up to the latest 60 events;
- tool inputs, results, errors, and user corrections;
- up to three persisted screenshots;
- up to eight related Device Memories within an approximate 12,000-character budget.

Related Memory selection prioritizes records recalled during the Episode, active records with the same scope, disputed records with the same scope, and then other text matches.

PEV fields such as `verifier_decision`, `ObservedState`, and `VerifierReason` are excluded from this pipeline.

### Episode Assessment

The model independently assigns:

```text
goal_result = achieved | not_achieved | unknown
```

`achieved` and `not_achieved` must cite a direct Episode event such as a tool result, screenshot, structured error, or user correction. When final proof is missing, the result must be `unknown`.

A recorded successful outcome can therefore become `not_achieved`. This corrects the learned Memory, but it does not retroactively change the foreground Run result.

### Candidate Types

One Episode can produce zero to three independent candidates.

| Type | Intended use | Minimum evidence shape |
| --- | --- | --- |
| `procedure` | reusable multi-step path | multiple device calls and results plus an observation |
| `navigation` | reusable page entry or transition | action, result, and before/after observations |
| `calibration` | coordinate or screen relationship | coordinate input and post-action evidence |
| `failure` | guard, stop condition, or recovery | direct problem or not-achieved evidence |
| `fact` | stable device, app, or page observation | observed tool result |

An Episode-level failure does not automatically create Failure Memory. A Failure candidate must contain reusable guidance for a future task.

When `goal_result=not_achieved`, a candidate cannot claim that the full goal path succeeded. A locally proven Procedure is allowed only when its scope explicitly marks it as partial.

## Create, Update, And Conflict

The model proposes `create` or `update`, but code owns admission, storage paths, and final status. The Processor supplies the bounded related-memory view used for that decision and assigns a deterministic create ID. The Store executes an explicit create intent without running a second similarity or scope decision.

If a create proposal missed an existing record outside the bounded view, duplicate resolution is handled as a later explicit consolidation/update operation rather than silently rewriting the create intent at persistence time.

Update requires the exact Memory revision observed during extraction. It preserves prior evidence and existing Procedure steps. Material body changes append the previous value to bounded revision history.

If evidence can be reconciled by adding a version, page, account state, or other precondition, the Memory remains active with a conditioned rule.

If the same scope still has incompatible conclusions, the Memory becomes `disputed`. Disputed records are excluded from normal recall but remain available to later consolidation.

## Idempotency And Recovery

The processing ledger uses `episode_id@extractor_version` keys and the following states:

```text
processing -> proposed -> done
                    `--> retry
processing/proposed -> ignored
```

The proposal is persisted before Device Memory writes. Recovery can therefore apply an existing proposal without repeating the model call.

Empty responses and invalid JSON from a normally finished model response are terminal `ignored` results. A response identified as output-token truncation is scheduled for retry; its Episodes are retried individually so the worker does not replay the same oversized batch, and extraction, omission-review, and retention-audit calls receive a larger output budget on each retry. Failure logs record response sizes but never the response body, which may repeat sensitive Episode values. A process crash after entering `processing` but before persisting a proposal is ignored after its lease expires.

If a target Memory revision changes before update, the stale proposal is discarded and the Episode is re-extracted against the latest revision. Output-token truncation and retention/omission review failures may also schedule another model call for the same extractor version.

## Latency Isolation

Episode consolidation does not add a synchronous LLM call, screenshot, OCR pass, or verifier to the foreground Agent loop.

The worker:

- starts after an idle delay;
- processes at most three Episodes per batch; a truncation retry lowers the batch to one Episode per model call; each Processor batch uses one model call and applies returned results locally;
- has only one in-flight background model call;
- cancels that call when a foreground task starts;
- resumes scheduling after the foreground task finishes.

Episode trace persistence happens before background maintenance is scheduled. A maintenance failure is logged and does not replace the user-facing task result.

## Notification Memory

Notification Memory extends the memory plane while keeping `ble_service` as the
event producer. The Agent consumes notifications through the `ble_service` API;
Memory processing remains in the Agent. During the shared idle window, the
Agent-side `NotificationProcessor` consumes the BLE event ring and
`NotificationContext` deduplicates and persists sanitized source records before
Memory extraction.

The storage layout includes two additional roots:

```text
memory/
├── notifications/              # sanitized source evidence and cursors
├── temporary/                  # time-bounded recallable conclusions
│   ├── index.yaml
│   └── memories/
└── long_term/                  # durable user memory
```

Temporary Memory uses the Long-Term Memory record and index schema, requires an
`expires_at`, and is searched by the existing `recall_memory` tool. Relevant
unexpired Temporary Memory ranks ahead of a Long-Term Memory baseline for the
same subject without replacing it.

The plane owns one shared `MemoryWorker`, registers both scenario processors,
and starts the Worker only after registration completes. The Worker is the only
idle scheduler: after the Agent has been idle for five minutes it invokes the
processors in Episode -> Notification order, serially, and cancels in-flight
work when a foreground task starts. While the Agent stays idle, bounded pending
batches continue without another five-minute wait. Each Processor owns its own
input, batch size, proposal validation, retry state, and Store Apply rules.
Processor failures are isolated: the error is logged and retried without
preventing a later registered scenario from running its own batch. Normal
pending work continues in the current idle window; failed batches wait for the
idle retry delay to avoid a hot loop.

| Processor | Trigger | Batch | Output |
| --- | --- | --- | --- |
| EpisodeProcessor | Shared Agent idle window | 5 Episodes / 1 LLM call | Device Memory |
| NotificationProcessor | Shared Agent idle window | 10 events / 1 LLM call | Temporary or Long-Term Memory |

Notification persistence does not start a 30-second timer. A Notification
Processor run first consumes the BLE ring and then checks its durable pending
cursor; when there is no pending work it returns without polling again.

## Compatibility

Legacy `reflection:v1` Failure Memory remains readable. Older Device Memory files with an omitted status retain their previous meaning and are treated as active.

The old failure-only Reflection processor, synchronous Procedure/Navigation/Calibration extraction, automatic verified-task-path Long-Term Memory, and outcome-based confidence updates are no longer active.

## Verification

The core regression suite covers prefiltering, false success, screenshot attachment, type-specific evidence validation, interrupted Episodes, deduplication, conflict quarantine, revision recovery, recall budgets, and foreground preemption.

Run the focused checks from `src/agent`:

```bash
go test ./internal/agent -count=1
go vet ./...
```

Real-device validation is still required to measure multimodal assessment quality and foreground p50/p95 latency under a real model provider.

## Related Documentation

- [Context Lifecycle](context-lifecycle.md)
- [Session Memory Compaction](session-memory.md)
- [Agent Configuration](configuration.md)
- [Tools HTTP API](tools-http-api.md)
