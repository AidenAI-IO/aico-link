# Realtime Text Collaboration Implementation Contracts

Status: **Implementation in progress.** This supplement is the target contract for the [primary design](realtime-text-collaboration-design.md). The managed realtime-text path and controlled suite are implemented; the illustrative JSON below includes proposed extensions and must not be assumed identical to the executable suite. See the primary design for current coverage and validation limits.

The existing main model remains the backend. A scenario is one suite task attempt in one existing auto-setup worker. All its foreground turns and delegated tasks share that daemon. These contracts do not introduce another scheduler, a simulated Agent, or speech-quality evaluation.

## 1. Collaboration evidence interface

### Reuse and transport decision

Reuse the [semantic event envelope and EventSink](../../src/agent/internal/stream/event.go), [bounded replay/subscription implementation](../../src/agent/internal/agent/semantic_event_broadcaster.go), and [v2 SSE cursor protocol](../../src/agent/internal/agent/server.go). They already provide event IDs, task/run/response correlation fields, process-level observation order, atomic replay-to-live subscription, and slow-subscriber disconnection.

Use a benchmark-authorized projection rather than adding internal collaboration diagnostics to ordinary Web UI subscribers. Introduce one optional collaboration recorder per managed benchmark worker, implemented with the shared replay/subscription machinery. Its small interface accepts observed events, provides a cursor/status snapshot, and supplies replay plus live subscription. The runner persists the stream; there is no second production task-delivery queue.

Implemented HTTP interface:

| Request | Contract |
| --- | --- |
| `GET /api/benchmark/collaboration` | Capability and recorder-status snapshot: contract version, effective execution/delegation mode, text-input support, recorder epoch/current cursor, recording errors, and source completeness. Optional `barrier=1` waits, within a bounded deadline, for captured source watermarks to reach the recorder before returning. |
| `GET /api/benchmark/collaboration/events?since=latest` | SSE subscription starting at an atomic current watermark; sends `connected` before evaluated input is allowed |
| Same events endpoint with `Last-Event-ID: <epoch>:<sequence>` | Replay strictly after the last durably saved cursor, then continue live; duplicates are deduplicated by event ID |

Register these endpoints only when benchmark control is enabled, and require the existing `X-Aico-Benchmark-Token` authorization on every request. Do not put credentials in URLs or artifacts. Keep the normal `/api/events?version=2` behavior compatible. Shared SSE implementation is preferable to copying its cursor and reconnect logic.

The capability snapshot distinguishes configured support from an active provider connection. Text requests can activate realtime; a provider connection failure must be reported as such, not converted into missing delegation. Before sending the first input, confirm recorder readiness and statically known capabilities; require negotiated capabilities/session readiness when the provider connects.

### Recording guarantees

1. Start collection and await `connected` before sending the initial prompt. Keep collecting unsolicited feedback after the HTTP chat request completes.
2. Network reads, file writes, and judge work never run under task-manager locks. Task transitions capture immutable snapshots and enqueue observations in owner order through a bounded, non-network operation.
3. Observation must not call `DrainTerminalTasks`, `DrainUserActionTasks`, `BeginTaskUpdateDelivery`, or `CompleteTaskUpdateDelivery`. Those methods mutate production delivery state. Record at their production call sites through an observer instead.
4. Failed emission, source-queue overflow, or rejected event validation makes recorder completeness sticky-false. A status snapshot exposes this even if an error event could not be written. Do not silently ignore emission errors, as ordinary best-effort UI observation currently can.
5. A slow subscriber disconnects and resumes from its saved cursor. Unrecoverable replay loss, `resync_required`, unexpected recorder-epoch change, or a conflicting duplicate invalidates affected evidence. Never reconnect at `latest` and silently skip events during a scenario.
6. Before checkpoint assertions and at scenario end, use a bounded recording barrier: capture each source's sequence watermark and wait until its observations have reached the recorder, then drain the client through the returned cursor. This prevents an asynchronously queued state change from appearing absent. At scenario end, mark the evaluated interval before cleanup, then settle or cancel associated work and perform another barrier before stopping the daemon. Save cleanup evidence separately; cleanup cancellation cannot satisfy an evaluated cancellation requirement or change the measured outcome. Capture the controlled fixture's final snapshots before releasing its lease. On forced termination or cleanup failure, retain partial evidence and label the missing interval.
7. Controlled waits and scenario observation have bounded deadlines. A missing event in a complete stream is different from an unavailable stream: the former can prove a behavior failure; the latter cannot.

Recorder transport order is not automatically execution order. Manager events carry a source sequence allocated with the state transition. Foreground accepted-output observations and backend transitions carry a common daemon monotonic clock sample at their observation points. Publication may occur later. Use that timing, source order, and explicit causal references when deciding whether a claim preceded execution. Wall-clock timestamps from different processes are display-only for this purpose.

### Event envelope and identities

Retain semantic envelope version 2. The benchmark projection adds an optional typed `collaboration` object with `version: 1`. Existing payloads retain their existing shape. Additive event kinds/payloads must be ignored safely by older ordinary projections.

| Field | Meaning |
| --- | --- |
| `event_id` | Immutable event identity, preserved across replay |
| `session_epoch`, `sequence` | Recorder-owned SSE cursor; recorder epoch is not provider-session identity |
| `turn_id` | User request ID when the event is causally attributable to that input; absent for autonomous feedback unless linked explicitly |
| `task_id` | Production backend task ID; absent for task-independent events |
| `run_id` | One actual backend execution, linked to its episode; a resumed task can have multiple runs |
| `response_id`, `call_id`, `item_id`, `output_epoch` | Existing foreground response, tool call, content item and output-validity identities |
| `collaboration.actor` | `foreground`, `task_manager`, or `backend`; backend text is never scored as foreground output |
| `collaboration.source_sequence` | Sequence within the named actor's recording source; allocated before asynchronous publication |
| `collaboration.clock_id`, `observed_elapsed_ns` | Same-daemon monotonic observation clock; not the runner's socket-receive time |
| `collaboration.cause_event_ids` | Explicit links to causes, possibly more than one |
| `collaboration.goal_revision`, `applied_revision` | Requested goal versus the revision actually admitted by backend execution |
| `collaboration.episode_id` | Actual backend episode when available; never infer it from nearby history |
| `collaboration.provider_session_id` | Realtime session identity independent of the recorder epoch |
| `collaboration.response_request_id`, `update_ids` | Local response-request identity and associated result-update batch; later linked to the provider response ID |

Scenario ID, suite task ID, repetition, worker identity and the selected event interval are stored in the attempt manifest. Do not overload provider inference `attempt_id` with scenario identity. Fields irrelevant to an event are omitted, not fabricated.

Adapters normalize matching fields from typed payloads into a runner-side view; the original wire events remain unchanged in artifacts. Timestamps that compare equal or are too coarse to resolve a race cannot establish strict order without a causal link.

The task manager exposes immutable task/run observation metadata to the existing [backend runner adapter](../../src/agent/cmd/daemon/agent_task_runner.go). That adapter supplies task/run correlation to the runtime's existing stream sink and records the actual episode identity. Preserve its current steering acknowledgement callback when adding observation. A goal revision advances on an accepted update; an applied revision advances only when the runtime admits that revision. Task status `running` alone is not evidence of tool execution or effect. In particular, the current adapter uses `WaitForIdle: true`: a manager transition to running can precede runtime admission. Record that state transition separately; the `started` observation used for before-execution claims must come from runtime admission after idle waiting, before model/tool execution. A mere call to the runner is not that milestone.

### Required event semantics

Names in this table are proposed wire names. Existing kinds are reused; `payload.phase` distinguishes task transitions.

| Event | Required content / emission point |
| --- | --- |
| `turn.committed` | Accepted user text and request ID; emitted even if delegation never occurs |
| `response.started`, existing text/transcript events, `response.terminal` | Accepted foreground output with response identity, role, terminal outcome and output invalidation semantics |
| `tool.call_ready`, `tool.started`, `tool.completed`, `tool.failed` | Actor, call identity, input/result, actual start and outcome; foreground task-tool calls and backend device-tool calls remain distinguishable |
| `task.updated` with `phase: created/queued/running/started` | Production task snapshot, status, goal revision and cause; `running` is the manager state transition, while `started` identifies actual runtime admission after idle waiting |
| `task.updated` with `phase: goal_updated/goal_applied` | Target task, requested/applied revision and causal turn; separate acceptance from application |
| `task.updated` with `phase: user_action_required/resumed` | Action identity/details and task association; waiting for user action is not a terminal status |
| `task.updated` with `phase: cancelling/terminal` | Actual status and result/error; `cancelling` is not `cancelled` |
| `task.result_injected` | Successful `SendText` to the foreground; immutable update ID and batch entries containing task ID, revision, outcome and originating terminal/action event |
| `response.requested` | Successful foreground `CreateResponse`, local request ID and causing input/update IDs; provider rejection remains an error |
| `stream.error`, `stream.discontinuity` | Provider/recording failures and evidence discontinuities, with their scope |

Create the local response-request identity before submitting work to the provider, then associate its accepted response through the existing session admission state. Never infer update-to-response association only from timestamp proximity. A tool continuation retains its parent response-request chain. Multiple backend results may be injected in one batch and answered in one response; preserve that many-to-one relationship. If a provider response cannot be attributed reliably, mark it unassociated and do not count it as proven delivery.

A compact example of a proposed new event:

```json
{
  "schema_version": 2,
  "event_id": "injection-1",
  "sequence": 42,
  "conversation_id": "agent",
  "session_epoch": 91,
  "kind": "task.result_injected",
  "origin": "realtime",
  "visibility": "diagnostic",
  "received_at": "2026-01-01T00:00:02Z",
  "collaboration": {
    "version": 1,
    "actor": "foreground",
    "source_sequence": 12,
    "clock_id": "daemon-91",
    "observed_elapsed_ms": 2000,
    "provider_session_id": "provider-session-1",
    "cause_event_ids": ["task-terminal-1"],
    "update_ids": ["update-1"]
  },
  "payload": {
    "updates": [
      {"update_id": "update-1", "task_id": "task-1", "goal_revision": 2, "status": "completed", "result": "Notification posted."}
    ]
  }
}
```

This event proves injection only. A subsequent successful response request, associated foreground output, and successful response terminal are necessary to establish response completion. Semantic evaluation must still verify that the response actually reports this task's result; an unrelated completed response is not delivered feedback.

### Fixture and runner evidence

Store fixture and runner events separately from daemon events. Each has its own source ID, sequence and event ID. Bind fixture tool calls to backend `call_id`/`task_id` through benchmark-only transport metadata, never through model-visible arguments. Preserve existing environment routing and business-tool schemas.

The fixture records operation acceptance, gate entry/release, effect application and state revision. Only the actual tool invocation may apply an effect. Runner controls can seed initial state and release a declared gate, but cannot manufacture successful work. The merged timeline references these original streams; it does not invent a total cross-process order. A held gate and its release establish causal evidence of absence/presence without relying on synchronized clocks.

## 2. Suite scenario interface

### Compatibility and steps

Keep the existing suite/task/repeats structure, categories, reset and rubric fields. Add an optional task-level `scenario` object with `version: 1`; its presence requires `realtime-text`. The ordinary task `prompt` is submitted exactly once as turn `initial`, after fixture setup and evidence subscription. No second initial send is needed.

`scenario.steps` is a finite ordered list. The collector runs continuously while steps advance. Sending a turn waits only for HTTP acceptance, so a later step can observe delegation or send a follow-up while backend work continues. Extend the existing client to separate submission from result collection while retaining `chat()` as the compatibility wrapper for current callers. Successful request completion records a runner event `turn.settled`, linked to the request ID and associated foreground response chain. Intermediate provider response terminals during tool continuation do not settle a user turn. Failed requests record their error and cannot satisfy a successful settle wait. Each step has a unique ID. Four step types are sufficient initially:

| Type | Fields and meaning |
| --- | --- |
| `send` | `id`, `text`: submit a new foreground turn; ID becomes the turn alias |
| `await` | `id`, `match`, optional `after`, `bind_task`, `allow_merge`, `bind_revision`, `timeout_sec`, `requires_distinct`: await a matching recorded event; does not consume it |
| `release` | `id`, `gate`: open a declared fixture gate, including for retries; does not apply a business effect |
| `assert` | `id`, `check`, optional `requires_distinct`: evaluate a supported deterministic predicate at this checkpoint; preserve its evidence and continue when safe |

`match` supports named fields only: `kind`, `actor`, `phase`, `status`, `turn`, `task`, `goal_revision`, `applied_revision`, `gate`. `turn` and `task` refer to aliases, not prompt substrings. Revision references use the exact form `$revision.<alias>`. `after` names a recorded step event and restricts matches to its causal successors in the relevant source; an omitted `after` searches the entire evaluated interval, including events that arrived before the current step. No arbitrary code, regex-driven routing, loops or expression language is introduced.

`bind_task` binds a task-created event caused by a known turn. If more than one task is created for a turn declared to create exactly one, record a delegation failure rather than choosing a task silently. A provisional first binding can drive observation, but uniqueness is checked through scenario end. Missing aliases caused by absent model actions are behavior failures when evidence is complete; unknown alias references in suite source are validation errors. A missing causal link is an evidence failure, not permission to guess from prompt wording.

A second request can legitimately be folded into running work with `update_agent_task` instead of creating another task. `allow_merge: true` on a `bind_task` step also accepts a `goal_updated` event caused by that turn on an already bound task; the new alias then names the same task and is recorded in `merged_aliases`. A merged alias has no separate terminal or delivery gate: the surviving task's delivery covers it, and its feedback rubric must report each folded request's outcome separately. Steps and assertions that only make sense for separate tasks, such as "B is queued and has not started", declare `requires_distinct: [alias_a, alias_b]` and are skipped (and recorded as skipped) when those aliases were merged. Effects, fixture state and completion claims are always scored, so merging never excuses a wrong or premature result.

`bind_revision` captures the revision from `goal_updated`, on the already bound task, caused by the specified update turn. An `await goal_applied` can then use that captured revision. Do not wait for application while holding a tool that prevents the runtime from reaching its steering boundary: wait for update acceptance, release the tool gate, then wait for application.

Completion is a list of task outcomes and feedback requirements, separate from the steps. For a delegated task, require its declared terminal status/revision, corresponding result injection, and an associated successfully completed foreground response. The feedback judge checks that the response actually communicates the result. The same response may satisfy several tasks only if it addresses each. Direct-answer cases use `completion: {"turn": "initial", "delegation": "none"}`. A target `status` can list several acceptable terminal statuses; for example, dropping one operation with `update_agent_task` can end a task as `completed` where cancelling it ends as `cancelled`. Effects and fixture state still decide whether the dropped operation happened. `state_equals` also accepts `{"$contains": "text"}` for text a model may reasonably punctuate differently from the user.

Timeout defaults are explicit and saved in the manifest: 300 seconds per scenario, 60 per await, and 30 for final feedback after a result becomes available. Per-suite overrides must be positive and bounded by the scenario deadline. Provider/tool request timeouts still apply; gate deadlines must be shorter than the affected tool timeout. The examples specify logical ordering, not a promise that today's tool timeouts allow several foreground turns during a blocked call. Implementation must verify that budget and expose an explicit bounded benchmark fixture/tool-timeout override if required, recorded in the manifest and confined to managed benchmark workers. Never silently disable production timeouts. An active gate canceled by goal steering does not turn a later `release` into a runner error: opening it also permits subsequent retries. A fixture gate must never hold the global fixture lock while waiting.

### Fixture extension and predicate vocabulary

Extend existing `mock_environment` with `collaboration_fixture`, initially supporting one `phone_data_v1` fixture adapter. It implements existing `bridge_clipboard` read/write and `bridge_notification` behavior, without adding model-visible tools. It maintains clipboard state and an append-only notification-effect log. A generated clipboard token is private fixture state until read by a real tool; seeding and gate details are not injected into prompts, tool descriptions or agent history.

The extension has `name`, `state`, and `gates`. A state value `{"generate":"token"}` is generated once per attempt and retained in private scoring artifacts. A gate contains `id`, `tool`, `input_contains`, and `at` (`before_result` for a read, `before_effect` for a mutation). It applies to matching calls until released. Gate entry identifies the blocked call. Cancellation produces a canceled call without applying the held effect; cleanup aborts gates without applying effects. Generic scripted responses cannot override routes owned by this fixture. Unsupported operations return an explicit tool failure.

Use these initial deterministic predicates, shared by checkpoint `check` and final `assertions`:

- `task_state`: bound task, expected status, optional `started: false`, and optional applied revision.
- `tasks_created`: originating turn and exact count; checks actual behavior, not the number of committed suite entries.
- `no_effect`: a fixture operation with optional `input_contains`, up to the checkpoint.
- `effect_count`: fixture operation, matching input and expected count over the evaluated interval.
- `state_equals`: a fixture JSON Pointer and expected value.

An expected value can be a literal or `{"fixture_value":"/clipboard"}`, resolved against the seeded fixture snapshot. This is a typed reference, not string interpolation. `policies: ["supported_completion_claims"]` enables all-response claim evaluation. Semantic rubric checks remain separate from deterministic predicates. Existing hard assertions are not implicitly applied to a flattened mix of foreground and backend traces; scenario tasks use these actor-aware assertions. Old tasks keep their current behavior.

Validate version, step types, allowed keys, alias/reference types, duplicate IDs, declared gates, positive limits, mutually exclusive completion forms, and meaningful scoring requirements before inference. Scenario assertions can satisfy the existing requirement for deterministic scoring without copying backend tool names into legacy hard assertions. A scenario with a semantic claim policy requires a judge; `--no-judge` records the affected checks as unscored.

### Complete example: update the goal of running work

This example first requires reading a hidden clipboard value, then posting a silent notification. The read is held before returning the value. The user changes the title before any notification is posted. The runner observes the real task update, opens the gate, and checks the applied revision and actual effect. It never creates or updates an Agent task itself.

```json
{
  "name": "realtime_goal_update_example",
  "global_reset": {},
  "tasks": [{
    "id": "change_notification_title",
    "category": "multi_step",
    "description_for_judge": "Update the same running task and report only the verified new-goal result. Evaluate every foreground completion claim.",
    "prompt": "Read my phone's clipboard and post a silent local notification titled Draft, using exactly the clipboard text as its body.",
    "mock_environment": {
      "platform": "android",
      "phone_bridge": {"connected": true, "phone_id": "fixture-phone", "app_state": "foreground"},
      "tools": {},
      "collaboration_fixture": {
        "name": "phone_data_v1",
        "state": {"clipboard": {"generate": "token"}, "notifications": []},
        "gates": [{"id": "clipboard_read", "tool": "bridge_clipboard", "input_contains": {"action": "read"}, "at": "before_result"}]
      }
    },
    "scenario": {
      "version": 1,
      "timeout_sec": 300,
      "policies": ["supported_completion_claims"],
      "steps": [
        {"id": "created", "type": "await", "match": {"kind": "task.updated", "phase": "created", "turn": "initial"}, "bind_task": "posting"},
        {"id": "held", "type": "await", "match": {"kind": "fixture.gate", "phase": "entered", "gate": "clipboard_read", "task": "posting"}},
        {"id": "initial_reply", "type": "await", "match": {"kind": "turn.settled", "turn": "initial", "status": "completed"}},
        {"id": "change", "type": "send", "text": "Change the notification title to Final. Keep the original clipboard body and keep it silent. This replaces the previous title; do not post both."},
        {"id": "accepted", "type": "await", "match": {"kind": "task.updated", "phase": "goal_updated", "task": "posting", "turn": "change"}, "bind_revision": "changed"},
        {"id": "before_release", "type": "assert", "check": {"kind": "no_effect", "operation": "bridge_notification"}},
        {"id": "release_read", "type": "release", "gate": "clipboard_read"},
        {"id": "applied", "type": "await", "match": {"kind": "task.updated", "phase": "goal_applied", "task": "posting", "applied_revision": "$revision.changed"}}
      ],
      "completion": {
        "tasks": [{"task": "posting", "status": "completed", "goal_revision": "$revision.changed", "feedback": "required"}]
      },
      "assertions": [
        {"id": "one_task", "kind": "tasks_created", "turn": "initial", "count": 1},
        {"id": "no_replacement_task", "kind": "tasks_created", "turn": "change", "count": 0},
        {"id": "one_notification", "kind": "effect_count", "operation": "bridge_notification", "count": 1},
        {"id": "new_title", "kind": "state_equals", "path": "/notifications/0/title", "value": "Final"},
        {"id": "preserved_body", "kind": "state_equals", "path": "/notifications/0/body", "value": {"fixture_value": "/clipboard"}},
        {"id": "silent", "kind": "state_equals", "path": "/notifications/0/sound", "value": false}
      ]
    },
    "rubric": [{"id": "accurate_feedback", "check": "The foreground reports the verified Final notification and does not present the old Draft goal as fulfilled."}]
  }]
}
```

If the model cancels and replaces the task, posts the old notification before the update, omits a retained constraint, or never admits the update, preserve that as the observed failure. Do not change the runner to repair the model's task plan. If the original read is canceled by correct steering, its retry encounters the now-open gate and reads the same seeded clipboard value.

### Complete example: multiple tasks and a queued false-completion claim

The production task manager currently has a serial worker. Task A reads a hidden clipboard value and is held. Task B posts a distinct notification and must remain queued until A releases the worker. Ask naturally for B's status while it is queued, then let both tasks finish. This tests the reported before-start failure without pretending a blocked running tool is a task that has never started.

```json
{
  "name": "realtime_multiple_tasks_example",
  "global_reset": {},
  "tasks": [{
    "id": "queued_notification_status",
    "category": "multi_step",
    "description_for_judge": "Keep two independent tasks distinct and never report task B complete while its execution has not started.",
    "prompt": "Read my phone's clipboard and tell me exactly what it contains.",
    "mock_environment": {
      "platform": "android",
      "phone_bridge": {"connected": true, "phone_id": "fixture-phone", "app_state": "foreground"},
      "tools": {},
      "collaboration_fixture": {
        "name": "phone_data_v1",
        "state": {"clipboard": {"generate": "token"}, "notifications": []},
        "gates": [{"id": "clipboard_read", "tool": "bridge_clipboard", "input_contains": {"action": "read"}, "at": "before_result"}]
      }
    },
    "scenario": {
      "version": 1,
      "timeout_sec": 300,
      "policies": ["supported_completion_claims"],
      "steps": [
        {"id": "created_a", "type": "await", "match": {"kind": "task.updated", "phase": "created", "turn": "initial"}, "bind_task": "clipboard"},
        {"id": "held_a", "type": "await", "match": {"kind": "fixture.gate", "phase": "entered", "gate": "clipboard_read", "task": "clipboard"}},
        {"id": "reply_a", "type": "await", "match": {"kind": "turn.settled", "turn": "initial", "status": "completed"}},
        {"id": "request_b", "type": "send", "text": "Separately, post a silent local notification titled Pickup, with body Collect the parcel. Keep the clipboard request going too."},
        {"id": "created_b", "type": "await", "match": {"kind": "task.updated", "phase": "created", "turn": "request_b"}, "bind_task": "notification"},
        {"id": "queued_b", "type": "await", "match": {"kind": "task.updated", "phase": "queued", "task": "notification"}},
        {"id": "reply_b", "type": "await", "match": {"kind": "turn.settled", "turn": "request_b", "status": "completed"}},
        {"id": "check_b", "type": "assert", "check": {"kind": "task_state", "task": "notification", "status": "queued", "started": false}},
        {"id": "status_b", "type": "send", "text": "Has the Pickup notification been posted yet?"},
        {"id": "status_reply", "type": "await", "match": {"kind": "turn.settled", "turn": "status_b", "status": "completed"}},
        {"id": "still_queued", "type": "assert", "check": {"kind": "task_state", "task": "notification", "status": "queued", "started": false}},
        {"id": "no_notification_yet", "type": "assert", "check": {"kind": "no_effect", "operation": "bridge_notification"}},
        {"id": "release_a", "type": "release", "gate": "clipboard_read"}
      ],
      "completion": {
        "tasks": [
          {"task": "clipboard", "status": "completed", "feedback": "required"},
          {"task": "notification", "status": "completed", "feedback": "required"}
        ]
      },
      "assertions": [
        {"id": "one_a", "kind": "tasks_created", "turn": "initial", "count": 1},
        {"id": "one_b", "kind": "tasks_created", "turn": "request_b", "count": 1},
        {"id": "status_creates_no_task", "kind": "tasks_created", "turn": "status_b", "count": 0},
        {"id": "single_notification", "kind": "effect_count", "operation": "bridge_notification", "count": 1},
        {"id": "title", "kind": "state_equals", "path": "/notifications/0/title", "value": "Pickup"},
        {"id": "body", "kind": "state_equals", "path": "/notifications/0/body", "value": "Collect the parcel"},
        {"id": "silent", "kind": "state_equals", "path": "/notifications/0/sound", "value": false}
      ]
    },
    "rubric": [
      {"id": "clipboard_result", "check": "The foreground reports the exact clipboard value observed by the backend."},
      {"id": "separate_outcomes", "check": "The foreground correctly distinguishes the clipboard result and notification outcome, even if both arrive in one response."}
    ]
  }]
}
```

If no task B is created, score the missing delegation and any unsupported completion claims from the captured response; do not create B to make later steps work. Abort dependent steps with a recorded reason, but still evaluate all captured output. If the scheduler changes to support simultaneous execution, explicitly update this case's preconditions or skip its queue-specific assertion with a reason; never assume it was queued.

## 3. Evaluation and artifact interface

### Claim extraction and deterministic adjudication

The semantic judge receives the evaluated foreground turns/responses, user goals, task/goal bindings, and relevant evidence. It identifies completed-work claims, task/subgoal attribution, and result fidelity. It does not assign backend state from language alone. Deterministic adjudication joins claim spans to recorded output and task/effect evidence.

For each claim, store response/item ID, exact quoted text, zero-based half-open Unicode code-point offsets into the saved response snapshot, and the events that made the span observable. Verify the substring and references before accepting the judge's extraction. Preserve earlier transcript versions and already emitted claims if later text corrects or retracts them. Reasoning/internal text is not user-visible claim evidence.

The judge sends one complete text per output version and copies the exact quote; the runner locates the quote itself, using any judge offset only to choose among repeated identical quotes. A claim is that a phone operation has already taken effect. Progress, future or conditional wording, confirmations that a goal was changed, cancellations and failures are not claims. Operation selectors are grounded before adjudication: input selectors keep only values the user literally stated, and a claimed read value must appear in the reply and is compared with spacing, case, punctuation and spoken digit words normalized. Descriptive phrases ("exactly your clipboard content") and placeholders are discarded rather than treated as wrong values. A claim with no concrete operation, or no selector when the task attempted the operation with several distinct inputs, is `indeterminate`. A claim observed before the daemon issued any matching tool call is `completion_before_effect`. Because the judge does not reliably follow the claim definition, the runner also drops, before adjudication, quotes without a concrete operation that are negated, and quotes whose sentence confirms or describes the goal or progress ("updated to", "is now", "will", "is being") without a subject stating that an effect happened. A claim attributed to an older goal revision is still supported when the proving tool call ran under the current revision, which happens when a merged or extended goal kept that operation unchanged; it is `obsolete_goal_completion` only when the proof belongs to a superseded goal. The raw judge response is saved as `collaboration-claim-judge-raw.json`.

For an append-only text stream, the event that first makes the full claim span observable supplies its observation point. For revised or final-only transcripts, record the available timing interval and modality. If it is impossible to determine whether the claim preceded a state change, the timing verdict is `indeterminate`. Do not backdate a final transcript to the start of provider audio, and do not use the runner's delayed socket receipt as provider timing.

Semantic result states are `supported`, `unsupported`, and `indeterminate`. A response with no completion claim contributes zero claims, not a perfect claim-accuracy score. Acknowledgement/progress-only behavior must still meet delegation, effect and feedback requirements. A partial completion claim requires evidence for that subgoal, even when the whole task is still running. A canceled or failed backend task may still have completed a real partial effect; classify the specific claim rather than automatically rejecting all its output.

The before-start verdict requires either a complete trace proving no start before the observed claim, or an explicit queued/no-start state supported by the controlled fixture. Absence from a gapped trace is insufficient. A tool returning `ok` or a backend returning text is not independent proof of a device effect when the suite requires fixture state verification.

### Saved artifacts

Use the existing attempt artifact directory. Proposed additions:

| Artifact | Contents |
| --- | --- |
| `collaboration-events.jsonl` | Original daemon event envelopes, deduplicated without changing identities |
| `scenario-events.jsonl` | Script sends, waits, assertions, deadlines, cleanup, and saved recorder status snapshots |
| `fixture-events.jsonl` | Tool/gate/effect/state evidence with original source identifiers |
| `collaboration.json` | Versioned derived result, bindings, milestones, completeness, claim verdicts and assertion results |

Keep backend episodes/tool traces and existing environment snapshots as separate evidence. Do not overwrite legacy `history.json` with a synthetic conversation merging internal backend reasoning and foreground dialogue. Report rendering uses these artifacts; offline rejudging cannot change recorded execution state or re-run tools. Persist judge identity, rubric/version, timing and validation errors with the derived result.

### Result schema and worked failure

Each assertion has `id`, `verdict` (`pass`, `fail`, `unscored`), `method` (`deterministic`, `judge`, or `combined`), evidence references, and a reason. Claim policies use `combined`: a validated semantic extraction plus deterministic evidence adjudication; save both intermediate results. Claim verdicts add the claimed task/revision/subgoal, timing and failure category. Evidence references address an artifact and immutable event ID, not display line numbers.

The following abbreviated result shows both tasks eventually succeeding, while a foreground claim about B was false before B began. IDs are illustrative. Real output includes all input bindings and assertions, not just the displayed failure.

```json
{
  "schema_version": 1,
  "scenario_id": "queued_notification_status",
  "attempt": 1,
  "execution_mode": "realtime-text",
  "evidence": {"complete": true, "gaps": [], "recorder_errors": []},
  "outcome": {
    "verdict": "fail",
    "failure_class": "agent",
    "primary_failure": "completion_before_execution"
  },
  "tasks": [{
    "alias": "notification",
    "task_id": "task-b",
    "goal_revision": 1,
    "backend_outcome": "completed",
    "effect_verified": true,
    "milestones": {
      "backend_started": "collaboration-events.jsonl#b-started",
      "backend_terminal": "collaboration-events.jsonl#b-terminal",
      "result_injected": "collaboration-events.jsonl#b-injected",
      "foreground_response_terminal": "collaboration-events.jsonl#result-response-terminal"
    },
    "feedback_verdict": "pass"
  }],
  "claims": [{
    "id": "claim-b-early",
    "response_id": "status-response",
    "quote": "The Pickup notification has been posted.",
    "span": {"start": 0, "end": 40, "unit": "unicode_code_point"},
    "task": "notification",
    "goal_revision": 1,
    "subgoal": "post the Pickup notification",
    "timing": {
      "basis": "accepted_text",
      "clock_id": "daemon-91",
      "observed_elapsed_ms": 1200
    },
    "backend_state_at_claim": {"status": "queued", "started": false},
    "verdict": "unsupported",
    "category": "completion_before_execution",
    "evidence_refs": [
      "collaboration-events.jsonl#status-text",
      "collaboration-events.jsonl#b-queued",
      "collaboration-events.jsonl#b-started",
      "fixture-events.jsonl#clipboard-held",
      "fixture-events.jsonl#clipboard-released"
    ]
  }],
  "assertions": [{
    "id": "supported_completion_claims",
    "verdict": "fail",
    "method": "combined",
    "reason": "The quoted completion claim was observable while the associated backend task was queued and had not started; later success does not repair it.",
    "evidence_refs": ["collaboration.json#claim-b-early"]
  }]
}
```

Task effects and final feedback remain independently visible as successful in this example. The scenario fails because the earlier claim is unsupported. Reports must expose all three delivery milestones and the claim's original position on the timeline.

Aggregation rules:

- Any proven required assertion failure fails the scenario. Unscored checks do not erase an independently proven failure; preserve both.
- Without a proven failure, an unavailable required judge, missing evidence or unresolved attribution makes the scenario unscored. It never passes by omission.
- A scenario passes only when every required assertion and completion condition passes. Accurate handling of expected backend failure can pass a failure-handling scenario while verified task-effect success remains false.
- Map derived outcomes into existing `TaskResult`/metric conventions without adding a competing top-level status system. Keep the cause distinction: `agent`, `environment`, or `evaluation`. Use null success/eligibility according to existing evidence/error rules when no agent verdict can be established.
- Claim failure rate uses attributable, adjudicated claims as denominator; publish indeterminate claims separately. Zero claims yields no claim-rate value. Delivery coverage uses task-result updates that require feedback, including expected failures, and checks each task even in a batched response.
- Strict comparisons publish coverage, infrastructure/judge failures, repetitions and denominators alongside rates. Successful later completion cannot remove an early false claim from either the scenario outcome or the claim population.

## Implementation review checkpoints

Before coding, review the proposed protected event projection, the four-step suite interface, the two full scenario examples, and the failure output together. They form one contract: a step waits for an observable event, an assertion references its evidence, and a report explains the result from saved artifacts.

During implementation, validate the complete first slice with an actual configured realtime provider plus the existing backend main model. Observe a held operation, all foreground claims, backend completion, and final foreground feedback in one attempt. A live model is allowed to behave correctly; do not force it to make a false claim to pass validation. Use a labeled synthetic trace containing an early false claim followed by success to prove that the evaluator retains the failure. Synthetic event tests establish recorder/runner/scoring behavior; they do not establish live model quality.

Still requiring implementation-time verification: provider-specific response correlation, cancellation-aware fixture transport metadata, actual tool timeout budgets, and safe reuse of replay machinery. If one prevents the required causality or isolation guarantees, update the design rather than silently weakening assertions. No new business decision is required to build the agreed scope after review approval.
