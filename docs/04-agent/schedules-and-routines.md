# Schedules and Routines

The Agent `schedule` tool creates one-off tasks and repeating routines. It uses
the MCU's single wakeup countdown and resumes work through the existing backend
runtime. It does not keep an Agent turn open or use a shell sleep to wait.

The daemon installs the tool in every input mode. In realtime voice mode with a
backend agent, the foreground delegates scheduling requests through
`create_agent_task`, just like other device work. Scheduled execution requires
the backend agent; creation is rejected in realtime mode with that agent
disabled.

## Tool interface

Use `action: "list"` before creating a task to check for existing work. The
tool accepts these actions:

| Action | Fields | Behavior |
| --- | --- | --- |
| `create` | `task`, either `delay_s` or `run_at`, optional `interval_s` | Save a task in the current backend conversation. |
| `list` | None | Return schedules, statuses, latest results/errors, and the MCU timer error, if any. |
| `update` | `id`, complete replacement `task`, either `delay_s` or `run_at`, optional `interval_s` | Replace a stopped or pending schedule, keeping its originating session. |
| `cancel` | `id` | Cancel future execution and request cancellation if this task is running. |

`delay_s` is an integer from 1 to 604800 (seven days). `run_at` is an absolute
RFC3339 timestamp with an explicit timezone. Longer deadlines use successive
MCU countdowns, each no longer than seven days. `interval_s` is 0 or omitted
for a one-off task, or 1–604800 for a repeating routine. Intervals are elapsed
seconds; they are not local calendar rules and do not adjust for daylight
saving time.

For example, continue a shipment check in ten minutes:

```json
{
  "action": "create",
  "task": "Check the shipment we discussed and report any delivery update.",
  "delay_s": 600
}
```

Run a task every 24 hours, starting at a specified time:

```json
{
  "action": "create",
  "task": "Check the order status and report any changes.",
  "run_at": "2026-10-23T09:00:00+02:00",
  "interval_s": 86400
}
```

The existing tool HTTP endpoint also accepts these inputs at
`POST /api/tools/schedule`, using the same request envelope as other tools. A
backend session must already exist. Successful mutations return the routine
under `result`; listing returns an array under `result`. Every successful tool
response includes `timer_error`. A nonempty error means the schedule is saved
but MCU arming or reconciliation has not been confirmed; do not tell the user
that its wakeup is ready. The daemon retries while running. Initial creation
fails if the MCU is unavailable or an independently programmed timer is active.

## Execution and session continuity

The schedule records its originating backend `session_id`. When due, its task
is appended to that conversation as a new input, retaining the existing tool
results and conversation context. The runtime waits for the active run to
finish. Ordinary queued background tasks also wait, so they do not preempt a
scheduled run. An interactive user request can still interrupt scheduled work.

Session identity survives Agent restarts, including restarts on a later date.
The generated prompt date is refreshed as appended state without replacing the
conversation. Context compaction creates revisions;
scheduled execution follows that lineage rather than reopening an obsolete
transcript. Clearing history or a configuration change that replaces the
conversation with a new root makes the old schedule fail explicitly. It never
recreates cleared history or silently executes in an unrelated conversation.
Create a new schedule in the new conversation to continue there.

Results remain in the backend transcript and in `schedule(action="list")`.
This scheduler does not itself start a foreground voice response or synthesize
speech. The scheduled task can use the existing execution tools as usual.

## Persistence and recovery

State is stored atomically in `<config_dir>/routines/state.json`, including the
event cursor, pending work, schedule/session mapping, and latest result. The
normal configuration directory is `/userdata/agent`. State files have mode
`0600`. No model credentials are stored in them.

If loading or recovering the routine state fails, the daemon logs the error and
continues serving HTTP and voice requests without the `schedule` tool. It keeps
the state file intact for inspection and recovery. Fix the reported state or
storage problem and restart the Agent to re-enable scheduling. Loading unchanged
state does not rewrite it; persisting recovered interrupted work still requires
writable storage.

The normal lifecycle is:

```text
scheduled -> queued -> running -> completed
                              -> scheduled (next repetition)
                              -> failed
scheduled / queued / running -> cancelled
running at shutdown/restart  -> interrupted
```

The MCU wake event (`rtc_timer`, source code 4) makes due work eligible for
execution. ASR and other wake sources do not run schedules. Cursor advancement
and queued work are persisted together before execution. Startup also confirms
the hardware timer: a previously armed, now-expired countdown can recover a
lost event. The host clock alone never dispatches a task while MCU state is
unavailable.

Multiple schedules share the earliest countdown. Reconciliation reads the
live timer before changing it, so normal polling, Agent restart, and a lost SET
response do not restart a matching countdown. Cancelling or postponing the
earliest task may leave an earlier harmless wake; only due tasks execute.
The HTTP MCU timer mutation endpoints return `409 CONFLICT` while routines own
the timer. Direct UDS clients must respect this ownership too; the MCU protocol
has no per-client timer lease.

Repeated wake delivery does not rerun a completed occurrence. A delayed routine
runs once and skips missed intervals, preserving its original cadence. A failed
run stops the routine until it is explicitly updated. A graceful shutdown during
execution records `interrupted` and retains any partial result. Recovery after a
crash also marks unfinished execution as `interrupted`. Neither automatically
replays potentially completed device actions. Inspect the session before
updating that routine. This also applies to a running occurrence waiting for the
backend runtime to become idle; work still in the queue survives restart.
These rules do not promise exactly-once external side effects.

The MCU countdown is not a battery-backed calendar alarm and does not survive
loss of MCU power. Persisted schedules are reconciled when the Agent and MCU
become available again. The Agent does not power off or flash the device to
schedule work. See [MCU service](../03-services/mcu-service.md) for the hardware
wakeup and event-delivery limits.
