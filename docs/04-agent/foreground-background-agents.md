# Foreground and Background Agents

Realtime voice mode uses two cooperating agents:

- The realtime voice model is the foreground agent. It owns the live
  conversation and must not wait for device operations or long-running work.
- The legacy agent loop is the background agent. It executes queued tasks one
  at a time with the existing runtime tools, memory, and episode recording.

The orchestration layer lives in `internal/agenttask`. It owns task data,
state transitions, queueing, cancellation, and terminal notifications without
depending on `internal/agent`. The daemon supplies a narrow runner adapter that
maps `Run(ctx, prompt)` to the legacy `agent.Runtime`.

The two conversation contexts are persisted separately under the configured
session root: `sessions/user` contains the realtime foreground conversation,
while `sessions/backend` contains the legacy device-operation context.
The user context also persists the realtime foreground's own tool calls and
tool results, so they can be restored when a new realtime websocket session is
opened. Backend tool traces remain isolated in `sessions/backend`; only their
aggregated task updates are injected into the foreground as user messages.
To keep realtime context bounded, a new websocket session replays only the
latest 10 user turns, including the assistant/tool messages belonging to those
turns. The full user history remains on disk.

The backend also exposes [schedules and routines](./schedules-and-routines.md)
through the `schedule` tool. MCU timer expiry resumes scheduled work in its
originating backend conversation, following compaction revisions. Scheduled
runs and ordinary queued backend tasks wait for the runtime to become idle.

The foreground realtime session can be activated by either the physical GPIO
wakeup signal or a text request to `/api/chat`. A text request submitted while
no realtime session is connected stays queued while the daemon connects, then
becomes the first user message in that session. GPIO initialization failure does
not disable `/api/chat` activation, which keeps the same foreground path usable
on PC and other hosts without board GPIO.

An outstanding foreground response has a 60-second no-progress timeout.
Accepted assistant text/audio and foreground tool calls/results renew that
deadline; microphone traffic, input transcript refinements, usage reports, and
stale response events do not. Foreground tools retain their separate 30-second
execution timeout. An idle conversation with no pending turn is not timed out.
Timeouts fail the pending text request and close the realtime session, including
its playback and foreground tool context. Explicit cancellation uses the
provider's ResponseInterrupter when available, stops playback and foreground
tools, and retains admission ownership until the terminal acknowledgement.
Late output cannot renew the cancellation deadline. Unsupported interruption,
a failed write, or a missing acknowledgement closes the session; the next
request then reconnects and restores history. Successful cancellation keeps
supported provider sessions connected. Background tasks have their own
lifecycle and are not canceled by canceling a foreground response.

Gemini interrupts through clientContent with turnComplete=false, leaving the
server waiting for input rather than requesting another answer. This follows
Google's ClientContent interruption semantics without switching off automatic
VAD (manual activityStart requires that switch). Gemini's interrupted followed
by turnComplete is normalized to a canceled terminal response; the daemon
retains the interrupted response's terminal ownership even when it has no ID,
without consuming the new user's pending input. See the [official protocol](https://github.com/googleapis/googleapis/blob/64aa30b277168edd20efee0c9ceb4ca01248931d/google/ai/generativelanguage/v1beta/generative_service.proto#L1589).
The empty-content cancel payload is covered by a local WebSocket protocol test;
its behavior against the deployed Gemini service still requires live validation.
Gemini tool-call cancellation is scoped to the listed call IDs: it cancels those
foreground invocations and drops their late results while preserving other
parallel calls and the response lifecycle.

Chat admission logs include the incoming and active request IDs, response ID,
occupancy duration, and each admission guard. Response completion, cancellation,
timeout, and session release are logged separately for diagnosing busy reports.

## Foreground tools

The realtime model receives this focused catalog:

| Tool                    | Purpose                                                                          |
| ----------------------- | -------------------------------------------------------------------------------- |
| `get_current_time`      | Return controller-local date, time, timezone, and UTC offset.                    |
| `recall_memory`         | Recall long-term user preferences, facts, rules, and procedures.                 |
| `save_memory`           | Save a long-term memory without waiting for a background task.                   |
| `forget_memory`         | Delete a saved memory by the ID returned from `recall_memory`.                   |
| `recall_session_chunks` | Recall compressed history older than the replayed window.                        |
| `audio_volume`          | Read or set the realtime playback volume.                                        |
| `create_agent_task`     | Queue background work and return immediately with a task ID.                     |
| `cancel_agent_task`     | Cancel queued work or request cancellation of running work.                      |
| `update_agent_task`     | Replace the complete goal of queued or running work.                              |
| `query_agent_task`      | Read one task's state and result, or every outstanding task when no ID is given. |
| `response_user_action`  | Resume a background task after the user completes the requested device action.   |
| `end_conversation`      | Return to standby after the farewell finishes playing.                           |

`create_agent_task` only enqueues work. The foreground response never waits for
the background agent to start or finish.

When the user changes work that is already outstanding, the foreground calls
`update_agent_task` with the task ID and the complete replacement goal. It does
not cancel the old task and create a new one. Queued work reads the replacement
goal when it starts. Running work receives the replacement as an in-context
steer: the current model or tool call is interrupted, the updated goal is added
to the backend conversation as a user message, and the agent loop resumes with
the same task ID and backend context.

Before creating work, the foreground calls `query_agent_task` without a task ID
and continues the task that already covers the request instead of starting a
duplicate. That check answers with the tasks that are still in flight plus the
finished tasks whose result has not been delivered to the foreground yet;
delivered results are left out because the foreground holds them in its own
context, so asking for the same work later is a genuine new request. A repeated
or reworded request therefore does not start the same device work twice.

The memory tools reuse the same registrations and stores as the background
agent, so both agents read and write one memory plane. Their foreground
descriptions are trimmed to the foreground catalog: the background
`recall_memory` description points at `shell` for raw notification records,
which the realtime model cannot call.

`recall_session_chunks` exists because a new websocket session replays only the
latest 10 user turns. Older turns stay on disk but are invisible to the
foreground until recalled.

## Ending a conversation

`end_conversation` puts the session back into standby. It does not introduce a
new state: `runRealtimeSession` returns, and the outer loop waits for the next
GPIO wakeup or `/api/chat` activation, exactly as it does after any other
session ends.

Teardown is deferred rather than immediate, because cancelling the session as
soon as the tool returns would cut off the farewell mid-sentence:

1. The tool result only records the request; the session loop owns teardown.
2. On `response.done` for the farewell, playback is finalized and a drain
   starts, bounded by a 30 s timeout.
3. Standby begins once the drain reports the speaker is empty.

The request is abandoned if the user re-engages first, via either
`input_audio_buffer.speech_started` or a text request through the chat bridge.
Both keep the session open and answer the new input instead. A text request that
arrives while the farewell response is still active is queued until its
`response.done`; during playback drain it starts immediately. While a request
is pending, task updates and voice notifications are not injected, so a queued
result cannot start a new response during the goodbye.

Background work is unaffected by standby. A background task that reaches a
terminal state while the foreground is in standby activates the foreground by
itself, so the result is reported without the user having to speak first. The
existing session teardown returns undelivered task updates and pending user
actions to the manager; they are delivered by the next session, whether it was
started by the task update or by the user. Activation is attempted once per
terminal batch: if that session cannot deliver the update, it is returned to the
queue and waits for the next activation instead of re-activating in a loop. A
pending user action does not activate the foreground on its own; it is announced
by the next session.

## Task lifecycle

Tasks use the following states:

```text
created -> queued -> running -> completed
                            -> failed
                            -> cancelling -> cancelled
                            -> running (waiting for user action)
                 -> cancelled
```

Queued cancellation is immediate. Running cancellation first publishes
`cancelling`; it becomes `cancelled` after the legacy runtime returns from
context cancellation.

Task updates follow these state rules:

- `created` or `queued`: replace the stored goal before execution.
- `running`: replace the stored goal and publish a latest-wins steer to the
  active backend run.
- `running` while waiting for user action: invalidate the old action request
  and resume the task with the replacement goal.
- `cancelling` or terminal: reject the update.

Every goal has a revision. Persisting a steer in the backend context advances
the applied revision. If the backend run returns after an update but before
persisting it, the manager discards that stale result and immediately runs the
latest complete goal again. This closes the race between a direct final answer
and an update arriving after the agent loop's last steer check.

## Backend interruption context

Backend execution writes persisted `notice` messages when a run stops early,
so the next model request can distinguish interrupted work from completed work.
These notices belong to `sessions/backend`; frontend playback and task-result
delivery retain their own lifecycle.

| Situation | Notice and behavior |
| --- | --- |
| Cancel a running task, cancel a legacy chat request, STT wakeup cancellation, or service shutdown | `Interrupt [canceled]`; completion is not confirmed. |
| Another `Runtime.Run` takes over | `Interrupt [preempted]`. Creating a queued backend task does not preempt its predecessor. |
| Execution deadline expires | `Interrupt [deadline_exceeded]`. |
| Update a running task or submit chat steer | `Interrupt [steer]` immediately before the replacement user instruction; the same run continues. |
| A model/tool call was interrupted but its steer was withdrawn or yielded no text | `Interrupt [steer_resumed]`; continue the original task from its last confirmed state. |
| Unrecovered model request failure | `Interrupt [model_error]`. |
| Context budget, compaction, or other execution failure | `Interrupt [execution_error]`. |
| Iteration/time budget, repeated actions, no progress, or repeated parsing failures | `Interrupt` with the loop guard's stop reason. Existing guard warnings and stop results remain available. |
| Incompatible device touch mode | `Interrupt [device_mode_mismatch]`. |
| Runtime panic | `Interrupt [panic]`, when persistence remains possible; the panic is rethrown. |
| Abrupt process exit with no recorded end | `Interrupt [agent_restart]`, recovered before the next backend input; completion is unknown. |
| Successful `request_user_action` or `wait_for_wakeup` | `Pause [...]`; intentional suspension is not task completion. |

The runtime atomically records `.pending-run.json` in `sessions/backend` before
execution. It clears the record after a normal end or after persisting an
interruption. A failed notice write leaves a recovery record; if recovery also
fails, the next run reports the error instead of executing without that context.
Recovery follows active compaction lineage, deduplicates notices, and preserves
history rotation/clearing.

Notices are appended after tool results, preserving tool-call/result pairing.
They tell the model to verify the current device state before repeating an action,
because cancellation cannot undo side effects. Tools that ignore cancellation
must return before the loop can record their result and finish.

Normal answers, recovered model errors, individual recoverable tool errors,
HTTP client disconnection, foreground realtime interruption, and stopping TTS
after the loop completed do not produce backend interruption notices. Canceling
queued work or an already-paused task does not interrupt an active AgentLoop;
the task manager handles those state changes separately.

## Result delivery

Completed, failed, and cancelled tasks are delivered to the foreground model as
user messages. Delivery follows three rules:

1. A terminal update starts a 500 ms sliding debounce window. Every additional
   update resets that window, so results finishing close together are included
   in one message and one foreground response.
2. An update is injected only while the foreground session is idle. It never
   interrupts live user speech, an active response, or a text request forwarded
   through the realtime chat bridge.
3. A terminal update produced while the foreground is in standby activates the
   foreground session, as long as the update is still pending. The manager
   publishes a wake signal per terminal batch, separate from the drain signal
   the session owns, and the daemon keeps a watermark of the manager's terminal
   sequence so one batch activates at most one session.

Terminal result delivery has three manager-owned phases: queued, claimed by a
foreground session, and delivering. Draining claims an update but does not mark
it delivered. Immediately before text injection, the foreground resolves every
claimed snapshot against current manager state; after the realtime provider
accepts the response request, it acknowledges the update. If the session ends
first, the claim is released to the pending queue for a later session.

Calling `cancel_agent_task` for a completed, failed, or cancelled task preserves
that historical task status but suppresses its result while the delivery is
queued or claimed. A claimed result already held in the foreground's pending
list is therefore removed during the final resolve and is never announced. Once
delivery has begun, the provider response is the point of no return and is not
retroactively interrupted by task cancellation.

A terminal task never carries a pending user action: the action is cleared when
the task reaches a terminal state, so the foreground is never asked to complete
an action for finished work, and teardown can tell a restored result update from
a restored action request.

## User action handoff

`request_user_action` is mode-aware:

- In an ordinary legacy run, it preserves the existing human-handoff response
  (`HUMAN_HANDOFF_REQUESTED`).
- In a background task run, it publishes a pending action while leaving the
  task in `running`. The background agent loop returns at that point, but the
  task is not terminal and its pending action remains queryable.

The foreground agent receives the request when it is idle and tells the user
what to do on the device, including that they should say when it is complete.
After the user confirms completion, the foreground calls
`response_user_action` with the task ID and a concise `user_message`. The
manager clears the pending action and starts another loop on the same backend
runtime/session. The `user_message` is appended as the next user message on
the existing context, so the background agent can verify the new state and
continue its original task. If the realtime session ends before delivery, the
pending action is retained for the next foreground session.

Updating a task while it waits for user action is different from responding to
that action: the old request is no longer authoritative, so the manager clears
it, invalidates any claimed foreground snapshot, and resumes the backend with
the complete replacement goal.
