---
sidebar_position: 6
---

# Phone operation coverage and validation

The current benchmark does not establish broad phone-operation capability.
This document separates defined tasks, scheduled execution, environment
validation and outcome evidence. It describes the working-tree catalog;
unmerged changes are not evidence of default-branch CI coverage.

## Current execution boundary

`benchmark/ci/suites.json` and `ci.plan.select_cases` determine selection.
The daily workflow selects `runnable`: 13 cases and 104 task definitions in
this revision, before repeat overrides. Only five tasks operate a changing
simulator UI: three in `mobilegym_basic` and two in
`mobilegym_scroll_regression`. The 12 `perception_v1` tasks use static images
and mocked action responses. Bridge-policy tasks are also scripted.

Manual `runnable` selection can explicitly run the otherwise excluded
`phone_control_v1`, `mobilegym_scroll_sweep`, experimental
`mobilegym_workflow_v1`, and the three operation candidates below. Native device cases require manual `hardware`/`all`
execution and configured bridges. A suite's presence in the catalog does not
mean its device is available, its input path works, or its tasks passed.

CI green means complete result artifacts. Capability success must be read
from task results and metrics. Do not combine memory, static targeting,
scripted policies, simulator UI and native-device outcomes into a claim about
general phone operation.

## Capability inventory

Task IDs below refer to [benchmark suites](../../benchmark/suites/).
An existing task is partial coverage of a dimension, not validation of all
its variants. Reference replay results are scoped to the exact environment,
control backend, fixture and tool path; native Android, iOS and physical HID
need separate validation.

| Dimension | Existing task evidence | Automatic execution | Outcome evidence and gaps |
| --- | --- | --- | --- |
| Target identification and tapping | `perception_v1`: all 12 tasks; `phone_control_v1`: `find_and_tap_specific_item` | Static targeting only | Coordinate bounds are deterministic for static images. Dynamic target movement, similar labels, hidden/disabled controls and screen changes are not systematically sampled. |
| Navigation and app lifecycle | `mobilegym_basic`: `swipe_navigation`; `phone_control_v1`: `open_settings`, `open_clock`, `tap_back`, `swipe_between_pages`; `quick_action_v1`: Home, app switching, recents, browser tabs; ADB/VPhone basics | One simulator app-drawer task | Mostly pre/post screenshots and rubric. Several starts are prepared by agent prompts. Draft preservation across switching, wrong-page recovery and restart persistence lack controlled cases. |
| Scrolling and search | `mobilegym_scroll_regression`: `find_item_024`, `find_item_083`; optional 100-depth sweep; `phone_control_v1`: `scroll_page_down`, `scroll_to_bottom`, `settings_search_bluetooth`; `skill_discovery_v1`: scrolling navigation | Two fixed Scroll Lab tasks | Scroll Lab checks route, selected ID and irreversible overshoot. It does not validate nested lists, reverse search, horizontal lists or native scroll inertia. |
| Text entry and editing | `phone_control_v1`: `type_in_search`, `type_long_mixed_text`, `select_all_and_delete`, `copy_paste_text`; `quick_action_v1`: type/undo/select/copy/cut/paste; `adb_android_basic`: `type_english_text`; experimental `note_replace_one_item` | None | Editing experiment checks exact saved body and preservation. Other cases mainly use rubric. `type_long_mixed_text` actually contains ASCII only. Chinese composition exists in manual iOS tasks but has no validated replay here. Cursor edits, multiline input, keyboard occlusion and clipboard restoration need controlled acceptance. |
| Complex controls and gestures | `phone_control_v1`: `add_clock_alarm`, `long_press_app_icon`, `drag_app_icon`; `app_workflow_v1`: `calendar_create_full_event`, photo selection | None | Wheel precision, reverse adjustment, dropdowns, date/time boundaries and gesture timing lack validated controlled outcomes. Use daemon `drag_start`/`drag_release`; legacy bridge `drag` does not establish compatibility. |
| System controls and phone services | `mobilegym_basic`: `settings_check_wifi`; `phone_control_v1`: Wi-Fi toggles and notification shade; manual `quick_action_v1` panels; Aiden App connection tasks | Simulator Wi-Fi inspection; scripted service policy | Reading Wi-Fi is not toggling it. Real service round trips require an actual App connection. ADB intents/keyevents, companion APIs and physical input are distinct execution paths. |
| Interruption and failure recovery | Scripted phone-bridge restoration policies; manual task starts; no controlled in-task recovery suite | Scripted policy only | No systematic permission denial, incoming overlay, keyboard covering Save, rotation, slow loading, disconnect/reconnect or retry-after-save cases. Starting on the wrong page is not an interruption during execution. |
| Complete user workflows | Three `mobilegym_workflow_v1` experiments; `app_workflow_v1`: messaging, posting, notes, browser, maps, camera, calendar, shopping and cross-app comparison | None | Simulator experiments check records and unrelated-data preservation. External workflows lack equivalent independent state readers and controlled fixtures. Reopen checks require temporal evidence; a correct final state alone is insufficient. |

## Implemented operation candidates

Three additional suites contain **20 executable tasks**. All use independent
`initial_environment_state` fixtures, the existing UI runner and deterministic
`environment_assertions`; none uses mocked actions or agent-driven setup.
They are registered in the CI catalog and selectable with profile `runnable`
and an explicit suite path. They are **not in the daily automatic selection**
until reference qualification and an agent baseline are recorded.

| Suite | Tasks | Implemented scenarios | Independent final checks |
| --- | ---: | --- | --- |
| `mobilegym_navigation_v1` | 5 | Similar titles with target first/last; Settings → Home → Notes; Back then another record; scroll until item 24 is visible then reverse to item 3 | Exact record route, unchanged records/settings; final scroll selection and bounded maximum descent |
| `mobilegym_editing_v1` | 7 | ASCII punctuation/multiline; append; replace middle line; delete last character; clear body; rename title; mixed Chinese/ASCII | Exact full title/body and target ID; preserved metadata/guard record; no duplicates; final target detail route |
| `mobilegym_controls_v1` | 8 | Wi-Fi off→on/on→off/already on; alarm +1/−1 minute across hour boundary; disable alarm; weekday repeat; long-press pin | Actual OS Wi-Fi state with other radios preserved; complete alarm records; target-only pinning and unchanged note contents |

These are outcome-oriented candidate workloads, not additional static targeting
screens. The unchanged-state Wi-Fi case intentionally requires a visual check
and no unnecessary toggle. The fixture resets the next attempt's baseline;
results are collected before reset. Fixture state paths are MobileGym-specific.

Final state cannot prove a temporal sequence: visiting Home, reversing within the list
before opening item 3, leaving/reopening an edit, avoiding a transient Wi-Fi toggle, or
using long press still requires the enabled judge and successful action trace.
Do not use `--no-judge` to claim those requirements passed. The current judge
receives pre/post images and structured tool results, not every intermediate
image; ambiguous traces require manual inspection, not assumed success.

The Chinese task now has a real mixed-language target and exact Unicode oracle,
but does not seed a wrong IME mode or qualify native IME behavior. Clipboard
restoration, keyboard obstruction, drag/drop, draft interruption, permission
variants, delayed-save/disconnect recovery, and native app workflows remain
gaps. The existing three workflow experiments are retained separately.

## Execution-path validation

The reference path is:

```text
fixed tool inputs -> Go daemon /api/tools/<name>
  -> daemon MNK and screenshot providers
  -> environment bridge -> target UI
  -> screenshots plus independent state readback
```

Read tool inputs from the daemon's `/api/tools` catalog. In bridge mode the
daemon owns high-level tools such as `enter_text`; its implementation can
perform vision checks and keyboard operations. Directly calling a bridge's
same-named tool, Playwright fill, DOM clicks, ADB shell or a companion API
does not validate the daemon path unless that is the path under evaluation.
Fixture seeding and independent observation are harness operations and must
not substitute for the UI mutation being tested.

For each replay, record firmware SHA and worktree changes, daemon binary hash,
simulator image digest and pinned submodule SHA (or native OS/app versions),
control backend, viewport, fixture, ordered tool inputs and outputs, and
screenshots at meaningful checkpoints. Store raw artifacts under ignored
`benchmark/runs/` or outside the checkout. Do not commit generated reports,
credentials or personal phone data.

| Reference case | Controlled start | Fixed operation sequence | Required observation |
| --- | --- | --- | --- |
| R1 tap/navigation | Seeded Notes list with two known records | Tap Shopping list; tap visible Back; reopen same note | Detail/list/detail routes match; both records unchanged |
| R2 input | Open seeded note; focus a known editable field | Insert a known ASCII character through public `keyboard_tap`; separately validate `enter_text` with a configured vision model | Exact intermediate committed text and saved state; tool acceptance alone fails acceptance |
| R3 delete | Field containing `Aiden-123!` with cursor at the end | Delete final character; select all via supported UI and clear | Exact intermediate and empty field, no change to another field; unsupported shortcuts are blockers |
| R4 scroll | Scroll Lab at top, 100 fixed records | One known swipe; inspect; tap a now-visible known item | Viewport advances and expected item opens; record overshoot; verify reverse swipe separately |
| R5 Back/Home | Known note detail with committed edit | Back to list; Home; return to Notes; reopen record | Expected route sequence and unchanged saved contents |

Run each replay from a fresh fixture three times before labelling that exact
path repeatable. This is environment qualification, not agent success or a
statistical reliability claim. A no-model keyboard replay cannot qualify
`enter_text`'s vision/IME composition path. Preserve the failed replay when
an operation returns success without the expected change.

## Minimum agent acceptance workload

These acceptance specifications include variants beyond the implemented tasks.
The table below remains the target scope, not a claim that every variant is
implemented or scheduled. The candidate workloads above implement parts of
A1–A6, A9–A11; existing workflows address A15. Reuse existing tasks where they
meet the criteria; implement missing fixtures and readers before adding entries. Each task starts independently. Check target
identity and unrelated-data preservation for every mutation. Use a known
reference replay to distinguish an environment blocker from agent failure.

| ID / priority | User goal and essential variants | Acceptance |
| --- | --- | --- |
| A1 / P0 | Open a specified record among similar labels; change its position between fixtures | Correct identity and route; no other record touched |
| A2 / P0 | Navigate from Home or a non-target detail screen to the target, then return | Correct destination and return state; verified transitions |
| A3 / P0 | Find an off-screen record in medium/deep lists; repeat with a target above the current viewport | Correct selection, bounded actions and overshoot evidence; stable initial scroll position |
| A4 / P0 | Enter ASCII punctuation and multiline text into one of two editable fields | Exact committed text in intended field; other field unchanged |
| A5 / P0 | Append, replace one line, delete a character, clear the whole field | Exact full saved body after reopening; original identity retained |
| A6 / P0 | Enter mixed Chinese/ASCII with the device IME; switch from an initially wrong keyboard mode | Exact committed characters, no uncommitted composition or pinyin residue |
| A7 / P0 | Copy from one field and paste into another; preserve original clipboard | Exact destination plus clipboard restoration; copy tool success is insufficient |
| A8 / P0 | Edit and save while the keyboard covers the action area | Dismiss or navigate correctly; reopen confirms save; unrelated data unchanged |
| A9 / P1 | Change a time wheel by one minute in both directions; cross an hour boundary | Exact final hour/minute; repeat, enabled state and other alarms preserved |
| A10 / P1 | Long press for a context menu; drag one icon to a known empty slot | Correct menu or final position; no accidental folder, deletion or displaced guard |
| A11 / P1 | Change a system setting that is on/off/already satisfied | Correct final state, no accidental toggle in satisfied case, restore fixture baseline |
| A12 / P1 | Switch apps with a draft; handle a controlled overlay; return and continue | Draft survives, overlay resolved as requested, exactly one final record |
| A13 / P1 | Permission already granted versus first-run prompt versus denied | Follows the stated permission goal; checks actual result and reports unavailability honestly |
| A14 / P1 | Delay Save acknowledgement or disconnect after Save, then reconnect | Check persisted result before retry; no duplicate side effect; fixture cleaned up |
| A15 / P1 | Note edit, contact form and note-to-contact transfer | All requested fields survive save/reopen; source and guard records preserved |
| A16 / P2 | Messaging, photo-to-gallery, calendar and browser/map workflows on native OS | Dedicated fixtures/accounts and independent destination evidence; no live public posting or purchase required for this baseline |

P0 qualifies the common control loop first. P1 covers complex controls, system
changes and recovery. P2 broadens application coverage after those paths work.
These priorities do not imply that P0 alone is comprehensive phone control.
Qualify Android first where a dedicated native environment is available, then
run the same goals with iOS-specific fixtures and expected OS behavior. Keep
ADB and physical HID results separate.

## Reporting and CI promotion

Report environment qualification separately from agent results. A complete
report should expose planned attempts, runnable/blocked attempts, reason for
blockage, per-dimension agent success, full-workflow success, preservation
violations, duplicate effects, human handoffs, latency and action count.
Unknown failure attribution stays unknown until evidence resolves it.

The runner already records task results, eligibility, failure classes, timings
and handoff evidence, but it does not automatically classify every bridge
failure or aggregate this eight-dimension matrix. Those are implementation
gaps, not current dashboard features.

Promote a task to daily execution only after its reference replay, isolated
setup, outcome reader and cleanup work on the chosen environment. Establish
an agent baseline over repeated runs with model, judge, task version, platform
and skills fixed. Then choose explicit regression limits per dimension rather
than using workflow green. Keep larger native app flows periodic until their
environment availability and execution time support daily runs.

## Validation record

The current investigation's measured outcomes and limitations are recorded in
[Reference operation validation](./phone-operation-validation.md). This is a
manual evidence record; it must not be read as an automatic environment gate.
