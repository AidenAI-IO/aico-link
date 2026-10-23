# Native Android benchmark coverage

The `adb` CI profile has two suites: general Android UI operations and text
editing diagnostics. Both run over the same ADB bridge on an emulator or a
physical device. The suites organize capabilities; they do not imply different
Android environments.

## Compatibility and selection

| Suite / CI case | Tasks per repeat | What runs on the ADB device |
| --- | ---: | --- |
| `adb_android_basic` | 16 | Screen reading, Settings, Wi-Fi status, app drawer, Back, scrolling, quick-action contracts, cross-app navigation, panel recovery, app resume, and recents |
| `adb_android_text_editing` | 4 | ASCII spaces, punctuation, replacing a query, clearing a query |

These are 20 task executions per repeat. CI and direct local runs use the same
suite contents, without task-ID allowlists. Both CI case IDs match their suite names. `adb_android_basic` incorporates the
earlier `adb-android`, `adb-phone-control`, `adb-quick-action`, and
`adb-navigation` cases. The navigation tasks now live in `adb_android_basic.json`; there is no
separate navigation suite. Text editing stays separate because it needs an
editable Settings search interface and exercises keyboard/input limitations.

The basic suite is version 2.1 and text editing is version 1.1. Both suites use
English for task prompts, setup instructions, and judging criteria. Basic 2.1
translates the remaining Chinese instructions from 2.0; account for that language
change when comparing results. These workloads
are not directly comparable with the earlier eight-task smoke or 32-task profile.
Shared cross-platform suites retain their original definitions. The three
`quick_action_*` tasks require a successfully executed named shortcut; ordinary
UI tasks accept any supported route, except when recents is explicitly requested.
All tasks forbid shell commands and user handoff. Skill-read observations remain
informational.

Setup leaves evidence needed for judging in the pre-screenshot: device information
before reading a model and returning Back, notifications before dismissing the
panel to Settings, and the exact original query before replacement or clearing.
The rubric requires that precondition to be visible; an attempted setup alone
is insufficient. The shortcut notification task leaves its panel open so the
post-screenshot verifies its content. The recents shortcut leaves Settings visible
in recents; it does not claim to verify global removal of an app card from one
carousel viewport. No minimum of three scroll calls is imposed when fewer scrolls
reach the bottom.

The bridge resets Home before each task. Per-task setup then prepares a Settings
page or input field where required. Opening a panel or app during setup is not
counted as a scored action. Cross-app and recents-return tasks still exercise
navigation and recovery; their traces establish action order while final images
establish the resulting UI. Intermediate screenshots are not supplied to the
judge, so these traces do not independently prove every intermediate screen's
contents.

Other suites are not automatically compatible:

- `mobilegym_basic` and `mobilegym_scroll_*` depend on synthetic applications,
  fixture IDs, or MobileGym state assertions. ADB setup currently only returns
  Home and does not install those apps, seed fixtures, or expose `/api/state`.
- Remaining `phone_control` tasks can require a notes app, two editable fields,
  multiple populated launcher pages, clipboard support, persistent alarm/layout
  changes, or network changes. They need explicit fixtures and restoration
  before joining a repeated native-Android run.
- Remaining `quick_action` tasks often target iOS/macOS bindings. They cannot be
  converted by simply changing `target_platform`.
- `app_workflow` needs installed apps, accounts, data, network access, and, for
  some tasks, externally visible actions. It is not a stock-emulator baseline.
- Aiden App connection suites require the actual companion app and connection
  capabilities. ADB transport does not reproduce USB HID or Phone Bridge.
- Memory/reflection suites already have isolated or mock environments; running
  them next to an emulator would not add device coverage.

## Preconditions and isolation

Use a dedicated, unlocked Android test device with a working Home screen,
Settings with an editable search interface, and Clock with an alarm list.
Finish first-run onboarding and dismiss unrelated permission/login screens
before recording a baseline. Record the Android version, launcher name/version
and mode, locale, screen size/density, input method, ADB serial, and Agent/Judge
models alongside each run. Compare identical configurations first.

ADB reset returns Home and verifies keyboard dismissal. It is not a data or
launcher-layout reset. The new tasks avoid network toggles, adding alarms,
moving icons, clearing app cards, accounts, and publishing content. Text entry
clears any existing query; replacement/clearing setup seeds the exact original
query. Inspect setup failures and pre-screenshots separately from scored task
execution. A visible wrong seed fails the precondition rubric rather than
counting as successful editing.

The new tasks name outcomes and accept localized/equivalent Android screens.
They do not specify package names, icon positions, a launcher brand, or an
app-drawer route. The basic suite includes an explicit app-drawer task and
therefore requires a launcher mode supporting that interaction. Report that
precondition when comparing launchers; do not claim the entire profile supports
every launcher mode.

## Run

From a branch containing these changes, dispatch `benchmark.yml` with:

```text
profile: adb
suite: (empty)
repeats: 3
```

This selects only ADB cases, avoiding the external iOS/macOS environments in the
broader `hardware` profile. The runner must already have a booted device and
`BENCHMARK_ANDROID_SERIAL`; the workflow does not create one. Cases are serialized
by the hardware lock, and each ADB case has task concurrency 1.

Preview selection without a device or model credentials, from the repository root:

```bash
python3 benchmark/ci/plan.py validate
python3 benchmark/ci/plan.py matrix --profile adb
python3 benchmark/ci/plan.py matrix --profile adb --suite suites/adb_android_basic.json
```

For a focused local run, start the bridge as described in `README.md`, then:

```bash
cd benchmark
uv run python -m runner run \
  --suite suites/adb_android_basic.json \
  --environment-url http://127.0.0.1:8899 \
  --auto-agent-setup --target-platform android --max-concurrency 1 --repeats 3

uv run python -m runner run \
  --suite suites/adb_android_basic.json \
  --task-id tap_back --task-id about_device_and_back \
  --environment-url http://127.0.0.1:8899 \
  --auto-agent-setup --target-platform android --max-concurrency 1 --repeats 3
```

Configure Agent and Judge credentials through the existing runner configuration.
Judging must be enabled to assess UI outcomes; `--no-judge` only exercises the
pipeline and must not be used as evidence that these tasks succeeded.

## Evaluation and remaining gaps

Keep eligible tasks, setup/environment failures, timeouts, and scored failures
separate. Compare task success together with wall time, model time, tool errors,
and actual traces. Composite tools such as `open_app` contain internal visual
decisions and device actions: fewer outer calls alone do not prove less work.

The navigation and editing tasks allow a one-call solution and do not require
a particular tool route except when the user goal explicitly asks for recents.
Visual rubrics use the pre/post screenshots. Action ordering uses successful
tool results and observation flags; those flags establish an observation occurred,
not its visual contents. The shared legacy suites retain their original scoring
contracts. Report setup and input-backend failures alongside task outcomes.

Text cases are diagnostic even when the backend cannot complete them. The current
bridge lacks some keyboard keys (including `space` for `keyboard_tap`) and does
not support native Chinese/IME composition. Do not skip failed ASCII cases or
switch the agent to a privileged input shortcut to make them pass. Input support
fixes should be reviewed separately and compared against this baseline.

Adding tasks on one emulator broadens scenarios, not devices. Validate multiple
launcher modes, locales, display sizes, and Android versions independently, then
repeat a representative subset on physical devices using the production input
path. Until those runs exist, this profile provides no evidence of OEM-launcher
or USB-HID compatibility. No new Agent performance claim follows from the suite
definitions or their unit tests alone.
