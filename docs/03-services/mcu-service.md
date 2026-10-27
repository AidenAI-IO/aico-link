---
sidebar_position: 7
---

# MCU Service and Post-Flash Validation

`mcu_service` replaces the diagnostic Python logger under the existing
`aico-mcu-event.service` unit. It exclusively owns `/dev/ttyS5` at 115200 8N1,
answers MCU state probes, forwards hardware events, and coordinates Linux
poweroff requested by the MCU or by the local root-only CLI. Local applications use
`/run/mcu_service/mcu_service.sock` (`0660 root:aico`); the Agent exposes the
same operations over USB ECM or loopback HTTP.

The MCU implementation is maintained in `aiden-mcu`. Install both the new RV
business package and an MCU image containing the LED extension to exercise
all features. Older MCU images continue to support battery/timer/wake handling;
GET_LED reports `UNSUPPORTED`. This page describes source implementation and
offline validation, not a claim that a particular board has been flashed or
its shutdown timing measured.

## Application operations

UDS uses the common [12-byte envelope](../06-protocols/uds-protocol.md): JSON
length LE32, binary length LE64 (must be zero), then UTF-8 JSON. Requests are
limited to 16 KiB and responses to 256 KiB. Use one request per connection.
The Go client is in `src/agent/internal/mcu/uds.go`.

| UDS `op` | HTTP | Result field / request fields |
| --- | --- | --- |
| `get_state` | `GET /api/mcu/state` | `state`: RV lifecycle/version, generation, UART-open status, last RX, LED support, plug state |
| `get_battery` | `GET /api/mcu/battery` | `battery`: percent/millivolts (null if unknown), charge state, raw sentinels, receipt time |
| `get_wakeup_timer` | `GET /api/mcu/wakeup-timer` | `timer`: timer_id, active, remaining_s |
| `set_wakeup_timer` | `PUT /api/mcu/wakeup-timer` | `delay_s`: 1–604800; returns `timer` |
| `cancel_wakeup_timer` | `DELETE /api/mcu/wakeup-timer` | Returns confirmed inactive `timer` |
| `events_since` | `GET /api/mcu/events` | `since`, `generation`, `limit`, `wait_ms`; event page |
| `get_led` | `GET /api/mcu/leds/red` or `/white` | `led`: requested/effective mode, timings, remaining lease, output and priority reason |
| `set_led` | `PUT /api/mcu/leds/red` or `/white` | `mode`, `on_ms`, `off_ms`, `lease_s`; returns `led` |
| `poweroff` | Root-only local UDS/CLI | `generation`, `request_id`; returns accepted `shutting_down` state |
| `suspend_prepare` | Root-only local UDS | Requests MCU permission before Linux suspend; returns `sleeping` state |
| `suspend_resume` | Root-only local UDS | Clears the MCU grant after wake or failed suspend; returns `ready` state |

For UDS LED requests also supply `led: "red"` or `"white"`. HTTP derives the
LED and operation from the path. All successful responses have `status: "OK"`.
Other statuses retain the original numeric `mcu_status` when available. HTTP
maps invalid arguments to 400, denied access to 403, mutation conflicts to
409, unsupported optional commands to 501, unavailable/busy/not-ready to 503,
timeouts to 504, malformed peer data to 502 and internal errors to 500.

`get_state` is a **local RV query**. UART `GET_STATE` is MCU-to-RV and cannot
read MCU firmware or supervisor state. UART-open status is not proof of a
healthy peer; inspect `last_rx_at` and a successful battery/timer query.
Battery receipt time is not hardware sample time. Unknown percentage/voltage
remain null; the MCU's six-byte battery response uses `FF`/`FFFF` sentinels.

Application mutations require `generation` from `get_state` and a unique
`request_id`. The two root-only suspend operations are reserved for
`aico-power` and serialized by the service; they do not use mutation IDs.
Before `suspend_prepare`, the worker persists recovery intent. A timeout or
lost/invalid PREPARE reply leaves the MCU grant outcome unknown, so the worker
must abort suspend and send `suspend_resume`. Cleanup intent remains until
RESUME succeeds. Worker and systemd post-stop cleanup each make one bounded
attempt; `aico-suspend-recover.timer` retries pending actions every ten seconds
after the preceding activation. It also runs at boot. Before the first PREPARE
per RV boot, the worker confirms RESUME even with no `/run` recovery file,
because MCU/UART power and the grant can survive an RV reboot. An unavailable
link leaves recovery pending and prevents a new suspend. A shared lock excludes
recovery while a suspend worker owns the grant.
This obligation also applies to any direct caller of the root-only operations.
Repeated RV_READY events do not revoke an MCU grant, and the grant has no timer
expiry that could interrupt a valid long sleep.
For example, a timer PUT body is:

```json
{"delay_s":120,"generation":"<current-generation>","request_id":"<unique-id>"}
```

The daemon retains up to 512 mutation results for ten minutes. Identical
retries return the original result; conflicting arguments or an old
generation are rejected. DELETE also takes a JSON body with these fields.
After a transmission timeout or a lost/unreadable UDS mutation response,
`unknown_outcome=true` means the command might have executed. Connection failures
before sending the request do not set this flag. Read current state before
deliberately sending a new operation.
Do not retry expired IDs automatically. There is one shared MCU timer: a new
SET replaces the previous timer. It is relative and survives RV power-off
while MCU remains powered, but not MCU reset or power loss.

## Wake long polling

```text
GET /api/mcu/events?since=0&limit=50&wait_ms=25000
```

Start from zero without a generation, then retain the returned generation and
advance to **`next_id`**, the last returned event. `last_id` is the whole ring's
high-water mark and can be ahead of a limited page. Read all page entries even
if your application only acts on wakeup. `reset_required=true` means restart
from zero without generation; `truncated=true` means history was evicted.
An empty page after a normal timeout is successful and preserves the cursor.

The ring retains 512 events, with independent consumer cursors. `limit` is
1–100 (default 50); `wait_ms` is 0–25000 (UDS default 0, HTTP default 25000).
Clients should allow five additional seconds for transport and cancel by
closing their dedicated request connection. Active waits are bounded to 32.
Waiting does not occupy the UART request slot. Daemon shutdown cancels waits.

Wake events carry `type: "wakeup"`, decimal-string `id`, `received_at`,
`uart_seq`, `source_code` and `source`. Sources 1–6 are `external_power`,
`power_switch`, `touch_key`, `rtc_timer`, `other`, `asr`. Unknown codes retain
their numeric value with `source: "unknown"`; applications must not select
a known action for them. Invalid payload lengths/reserved bytes are rejected.
The separate `rk628_plugin_det_changed` event includes inserted/raw-level data.

Normal maintained-switch startup does not synthesize wakeup. UART events have
no ACK or retry, MCU can coalesce unsent triggers, and daemon restart clears
the ring. Deduplicate application actions using `(generation,id)`, not UART
SEQ. This API is replayable within retained history, not durable/exactly-once.

### Agent voice activation

In STT (single-turn and follow-up) and realtime modes, the Agent consumes this
stream directly over the local UDS. Only `wakeup` events with `source: "asr"`
and `source_code: 6` replace the former GPIO32/GPIO33 voice trigger. They enter
the existing recording, output interruption and conversation logic, with the
existing 500 ms debounce and pending-event coalescing. Touch, timer, plug and
unknown events advance the cursor without activating voice. Quick Capture's
configured GPIO3 input is independent. Text mode does not start this consumer.

The consumer uses `AICO_MCU_SERVICE_SOCKET`, or the default service socket.
It long-polls with a 25-second wait, retries service failures every second,
and handles pagination, generation resets and truncated history. Stopping or
reloading the voice loop cancels the pending UDS request and joins the watcher.
Applications retain independent event cursors.

The Agent atomically checkpoints `(generation,id)` before each action in
`/run/aico/agent-mcu-wakeup.json` (mode `0600`), preserving its cursor across
configuration reloads and Agent process restarts within the same boot. A fresh
boot or MCU-service generation starts from retained history, so an ASR wake
that started RV can reach voice after Agent initialization. Events received
more than 30 seconds ago, or with no valid receipt timestamp, are skipped to
avoid starting a delayed conversation from old history. This age check uses
the RV wall clock; a forward clock correction can also cause an event to be
skipped. A crash between checkpoint and callback can lose an action. This
consumer does not provide durable delivery or recover evicted history.

On the device, validate the full voice path in STT or realtime mode:

```bash
tail -F /userdata/agent/log/agent.log
```

Say the ASR wake word and correlate `MCU ASR wakeup event generation=... id=...`
with the service event and the ensuing voice recording/session. Repeat during
an active answer to verify the configured interruption behavior. Touch and
timer events must remain visible through `/api/mcu/events` without activating
voice. Restart the Agent and reload its voice configuration to verify that an
already handled event is not repeated. The post-flash runner's
`--expect-source asr` checks event forwarding only; it does not prove that
recording, cloud recognition or speech output succeeded. Audible ASR-module
feedback is not an acceptance criterion while RV is powered off.

## LED extension

MCU command `0x0B SET_LED` takes eight bytes: ID, mode, on_ms LE16, off_ms
LE16, lease_s LE16. `0x0C GET_LED` takes one ID byte. Both return 12 bytes:
STATUS, ID, requested/effective mode, on_ms LE16, off_ms LE16, remaining_s
LE16, output_level, override_reason. IDs are red 0 and white 1. Modes are
auto 0, off 1, on 2, blink 3.

Auto releases control with all timing/lease values zero. Off/on require zero
durations and a 1–3600 second lease. Blink requires each duration in 20–60000
ms and a 1–3600 second lease. Startup indication and red low-battery/charging
policy take priority. Leases continue while masked, expire to auto, and clear
on MCU reset or RV shutdown/off. Duplicate UART SETs never renew the lease.

## Shutdown and operations

The MCU maintained-switch OFF state globally inhibits every wake source, including
touch, ASR, timers and future additions. It cancels the timer, queued restarts and
unsent WAKEUP notifications; inhibited inputs are not replayed on ON. After MCU
reset, wake stays inhibited until ON qualifies. This policy is enforced by MCU,
before RV service/event forwarding can run.

Positive wake-from-OFF tests must keep the switch ON and use coordinated software
shutdown. The corrected MCU waits in OFF after observing RV SHUTTING_DOWN and
receiving safe SHUTDOWN_READY, until a fresh wake arrives. Use the local CLI:

```sh
sudo /usr/lib/aico/mcu_service poweroff
```

The CLI queries the current service generation and submits an idempotent UDS
`poweroff` operation. The daemon checks Linux `SO_PEERCRED` and accepts this
operation only from UID 0; ordinary `aico` group clients receive `DENIED`.
Credential lookup failures also deny access. Poweroff has no HTTP endpoint.
The daemon persists current-boot intent, enters
SHUTTING_DOWN, closes event waits and stops application UART requests. It waits
for the MCU's next GET_STATE probe (normally within five seconds), then schedules
Linux poweroff after writing the SHUTTING_DOWN response. The final shutdown hook
retains its storage-safety checks. Failed intent persistence leaves the running
service unchanged. An accepted response means shutdown is scheduled, not that
the MCU has already cut power.

If the state response cannot be transmitted, a ten-second fallback still stops
Linux safely. As with recovery of a persisted intent after a daemon restart,
MCU power removal is then unconfirmed and must be checked before a wake test.
Bare `systemctl poweroff` does not provide the coordinated handshake.
Existing board reports for OFF-switch touch/timer wake describe
older firmware behavior and are not acceptance evidence for the corrected policy.

The daemon persists current-boot intent on tmpfs before ACK, then schedules
systemd poweroff after writing the response (with a bounded fallback if UART
cannot deliver it). `/usr/lib/systemd/system-shutdown/aico-mcu-shutdown`
invokes the statically linked service binary at the final shutdown stage.
It sends SHUTDOWN_READY only for `poweroff` with a valid root-owned intent,
inactive swap and no writable persistent filesystems. Service restarts,
package upgrades and Linux reboots do not authorize MCU power removal.

If intent persistence fails for an MCU-requested shutdown, RV replies
INTERNAL_ERROR but still schedules Linux cleanup: the MCU's forced cutoff has
already started and cannot be canceled by that error. The final hook withholds
SHUTDOWN_READY without a valid intent. A local software-poweroff request has no
preexisting MCU cutoff, so persistence failure instead leaves it unstarted.

The MCU's 30-second deadline starts at its shutdown state transition, even if
the RV rejects/misses the request. A received SHUTDOWN_READY permits a cut
after 75 ms. Agent and swap stop limits are shortened, but the **entire** OS
shutdown path must still be measured below 25 seconds under write/swap/OTA
load. The final helper withholds READY on an unsafe filesystem state; it
cannot cancel the MCU's forced cutoff. Audit other final shutdown hooks and
measure the last write and rail cutoff. Do not claim this physical timing
from passing host tests or a network reconnect alone.

```sh
systemctl status aico-mcu-event.service
journalctl -u aico-mcu-event.service -f
```

Stop the service before using the MCU repository's direct serial peer.
`RV_HOLD` remains unused. The retained service name lets existing package
lifecycle handling stop/start the replacement without a competing logger.

## After flashing: executable checks

The runner [scripts/test_mcu_after_flash.py](../../scripts/test_mcu_after_flash.py)
does not flash devices or issue raw power commands. Its default is read-only.
Run it from a checkout on the RV, or use `--url` from a USB-connected host.
The Agent HTTP route must be available for host-side tests. Stop other timer
and LED writers before opt-in mutation tests: these resources are shared.

```sh
# On RV: ready/link state, battery, timer and required new LED support.
python3 scripts/test_mcu_after_flash.py --require-led --report /tmp/mcu-query.json

# On RV: short white blink, lease expiry, two-second timer and its WAKEUP.
python3 scripts/test_mcu_after_flash.py --require-led --exercise-led \
  --exercise-timer --report /tmp/mcu-functional.json

# Keep the maintained switch ON; press/release after the runner starts waiting.
python3 scripts/test_mcu_after_flash.py --expect-source touch_key \
  --timeout 60 --report /tmp/mcu-touch.json

# Keep the maintained switch ON; speak after the runner starts waiting.
python3 scripts/test_mcu_after_flash.py --expect-source asr \
  --timeout 60 --report /tmp/mcu-asr.json

# Select this scenario on an external host. Keep the switch ON; after timer
# arming, run sudo /usr/lib/aico/mcu_service poweroff on the board.
python3 scripts/test_mcu_after_flash.py --url http://192.168.42.1:8080 \
  --power-cycle --allow-power-cycle --wake-delay 120 \
  --report /tmp/mcu-power-cycle.json
```

The runner refuses to replace an already active timer or white LED override,
cleans up its changes, and records failure/cleanup errors in its JSON report.
The power-cycle scenario requires an observed disconnect, a new daemon
generation, and timer-source WAKEUP. It still requires instrumented rail and
filesystem timing to establish safe physical shutdown. `--image <MCU.bin>`
records the expected artifact SHA256; it is not proof that image was flashed.

Offline checks run in `make check`, including Go race tests, simulated link,
UDS and HTTP tests, Linux/ARM cross-compilation, and runner tests with no device
access. The MCU repository separately runs build, host/firmware tool tests and
dependency verification. Keep board reports outside Git and record exact
image identities in physical acceptance results.


### Persistent deployment for suspend

The installed `aico-mcu-event.service` must own UART5 and run the matching
`/usr/lib/aico/mcu_service` binary. A drop-in under `/run/systemd/system` is
only a temporary diagnostic override and disappears on reboot. When adapting
an older development rootfs, save the original unit and binary, install the
reviewed override under `/etc/systemd/system/<existing-mcu-unit>.d/`, reload
systemd and restart that existing unit. Do not start a second UART owner.
Verify the effective ExecStart, socket permissions and MCU READY state after
an actual reboot before attempting suspend. New images already install the
production unit and do not need this compatibility override.
