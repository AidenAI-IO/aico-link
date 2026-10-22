---
sidebar_position: 7
---

# Reference operation validation

This record accompanies the [coverage inventory](./phone-operation-coverage.md).
It records environment qualification, not agent task success. No model was
asked to choose actions during the reference keyboard/navigation replay.

## Environment examined on 2026-10-21

The checkout is based on firmware commit
`2f686e4a6f05954a5f5fd8307a620d46c44609f1`, with the suite-contraction changes
still uncommitted. The daemon was rebuilt from this checkout using the
`golang:1.26` container, rather than using a cached daemon image. A deliberately
unreachable model endpoint was configured for this primitive-only replay;
there was no configured working vision model. The daemon SHA-256 was
`c20ee90a0120bd4b57a010b1cbb3a1908126ec712b94095703acdd60c8124eaf`.

The simulator used `aiden-mobilegym-simulator:benchmark-ci-test`, whose image ID is
`sha256:e188550498ca1a6cef08c7b7314777095e0f2f7b51d86206ac63578b31a2827e`,
and its label matches the pinned MobileGym submodule
`d0549195beccbe08f963bf7758bef3a65fe3ac08`. The bridge scripts were mounted
read-only from this checkout. The browser ran headless at a screenshot size
of 1080 by 2400. The daemon and bridge ran in isolated temporary containers;
no physical device or personal phone data was used.

Docker was initially stopped and was started for this investigation. An
authorized `adb devices -l` check returned no connected devices. Native
Android, iOS and physical HID reference replay remain unavailable here.

## Replay fixture and evidence

The source fixture is `mobilegym_workflow_v1.json` task
`note_replace_one_item`: Shopping list with `Milk\nEggs\nRice`, plus an
unchanged Travel checklist guard. Setup loads Notes and foregrounds its list.
Normalized coordinates below were chosen after inspecting the fixture screen;
they are reference steps for this pinned layout, not a general targeting policy.

All actions go to Go daemon `POST /api/tools/<name>` with
`{"input": ...}`. Its bridge providers use task ID `reference-operation`.
After each action, the harness reads bridge `/state` and `/route` and captures
`/api/providers/screenshot`; it never uses state access to perform the edit.

| Step | Daemon tool input | Required checkpoint |
| --- | --- | --- |
| Open note | `touch_gesture`: `{"type":"tap","point":{"x":250,"y":420}}` | Route `/note/note-shopping` |
| Focus body | `touch_gesture`: `{"type":"tap","point":{"x":250,"y":340}}` | Note editor remains open |
| Insert one character | `keyboard_tap`: `{"keys":["a"]}` | Exact body `Milk\nEggs\naRice`; guard unchanged |
| Delete that character | `keyboard_tap`: `{"keys":["backspace"]}` | Exact original body restored |
| Return to list | `touch_gesture`: `{"type":"tap","point":{"x":80,"y":80}}` | Notes route `/` |
| Reopen | Same Open note input | Same note identity and original saved body; guard unchanged |

Each attempt resets the fixture first. The character is inserted before Rice
at the known cursor position; this does not qualify append-to-end or general
cursor positioning. Final-state equality alone is not sufficient: the
intermediate insertion is checked before deletion, preventing a sequence of
no-ops from passing.

Raw investigation artifacts are local to `/tmp/aiden-operation-reference/`:
`daemon`, `replay.py`, `scroll.py`, and copied `completed-evidence/` checkpoints. They are not committed
reports or durable CI artifacts. Preserve or rerun them before making a release
decision; temporary files may be removed by the operating system.

## Measured results

The note replay completed **3/3 independently reset attempts**. Each attempt
verified the exact intermediate insertion, deletion back to the original body,
list/detail route transitions, and the unchanged guard note. This qualifies
only the fixed tap, one ASCII key, Backspace, visible Back and reopen sequence
on this simulator/tool path. It is not a model capability score.

The forward/reverse Scroll Lab swipe replay also passed **3/3 fresh fixtures**.
From first-visible ordinal 1, a swipe from `(500,750)` to `(500,350)` with
`duration_ms=700` advanced to ordinal 4 (scroll offsets 368, 370 and 370).
Reversing those endpoints reduced the first-visible ordinal on every attempt.
This qualifies direction and observable movement for that fixed swipe only;
item selection, overshoot policy, nested lists and native inertia remain outside
this replay. Inputs, screenshots and state are in `scroll.py` and the same raw
artifact directory.

## Scope limits and next qualification

The daemon correctly returns HTTP 404 for direct `keyboard_text`: that internal
primitive is absent from the public tool catalog. Do not expose it or call the
bridge directly to bypass the path being measured. Use public `keyboard_tap`
for primitive qualification and `enter_text` for high-level text-entry testing.

This fixture has not qualified `enter_text`, mixed Chinese/ASCII composition,
select-all/clipboard behavior, keyboard obstruction, time wheels, real native
app lifecycle, or interruption recovery. Those require the working model and
environment prerequisites in the acceptance workload. The three experimental
workflow tasks remain excluded from daily CI.

The current judge receives pre/post images and a structured trace, not the
pixels of every intermediate screenshot. Reopen verification therefore needs
ordered route/state checkpoints or preserved intermediate visual evidence in
addition to the final record. A rubric asking for reopen is useful but does
not independently prove persistence when those checkpoints are absent.

## Operation candidate suite validation

This follow-up adds three executable candidate suites (20 tasks): navigation
(5), editing (7), and controls (8). They are explicitly selectable in the CI
catalog, with daily scheduling disabled pending reference qualification and
an agent baseline. This supersedes the earlier statement that the round added
no suites. The three existing workflow experiments remain separate.

All 20 fixtures were applied to the pinned simulator, read back after
foregrounding, and checked against their initial state. Screenshots were
captured and each initial state was rejected by the full outcome oracle.
This validates fixture loading and observation, not completion of those tasks.

Visual inspection found that foreground setup forced an external `/` route
after Clock's default redirect, leaving its content blank. The bridge now
launches the app through `openApp(appId)` without an external route. Repeating
all fixture checks preserved Notes/Settings/Scroll Lab starts and put all four
Clock tasks on `/alarm` with their seeded alarms visible. No vendor files were
changed.

Seven candidate tasks were then replayed with fixed inputs through the current
daemon's public `touch_gesture` API. Each passed final route/state assertions
on **three fresh fixtures** (21/21 replays):

| Task | Fixed normalized taps | Result |
| --- | --- | --- |
| `similar_titles_target_first` | `(250,420)` | Correct target ID, all notes unchanged |
| `similar_titles_target_last` | `(250,580)` | Same target at a different position, all notes unchanged |
| `back_and_open_other_record` | `(250,420)`, `(80,80)`, `(250,580)` | Target detail → list → guard detail, all notes unchanged |
| `wifi_enable` | `(450,490)`, `(850,208)` | Wi-Fi on; Bluetooth and airplane mode unchanged |
| `wifi_disable` | `(450,490)`, `(850,208)` | Wi-Fi off; Bluetooth and airplane mode unchanged |
| `wifi_already_enabled` | `(450,490)` | Wi-Fi page opened without toggling the already-on radio |
| `alarm_disable` | `(860,407)` | Target disabled, its other fields and the guard alarm unchanged |

The daemon binary hash, simulator image and pinned submodule are the same as
in the earlier reference record. The current bridge startup fix and the new
suite files were mounted into the simulator. Full states, screenshots and
ordered tool outputs were saved outside the checkout under
`/tmp/aiden-operation-suite-evidence` and `/tmp/aiden-operation-suite-replays`.
These local paths are investigation artifacts, not CI-published reports.

No model selected these actions and no judge scored these reference replays.
The remaining 13 tasks have fixture validation only: no complete reference
replay or agent result is claimed. In particular, the seven editing tasks do
not yet qualify `enter_text` or Chinese IME; the two time-boundary tasks do not
qualify wheel adjustment. All 20 tasks still require an actual agent baseline.

The reverse-scroll prompt deliberately stays on the list until the final item
is opened: returning from a detail remounts Scroll Lab's list at the top and
would otherwise bypass the intended reverse gesture. Ordered navigation,
long press, save/reopen and avoiding transient Wi-Fi toggles remain judge/trace
requirements; final-state assertions alone do not prove them.
