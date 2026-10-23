# Native Android benchmark coverage

The eight-task `adb_android_basic` suite is a smoke test. It cannot establish
launcher independence, text editing reliability, or recovery across app and
system-panel transitions. The `adb` CI profile adds coverage without changing
Agent prompts, routing logic, or the existing suites' scoring contracts.

## Compatibility and selection

| Suite / CI case | Tasks per repeat | What runs on the ADB device |
| --- | ---: | --- |
| `adb_android_basic` / `adb-android` | 8 | Original smoke baseline, unchanged |
| `phone_control_v1` / `adb-phone-control` | 7 | Settings, Clock, Back, Settings scrolling, notification shade, bottom of list, device information |
| `quick_action_v1` / `adb-quick-action` | 3 | Home, removing Settings from recents, notification panel and dismissal |
| `skill_discovery_v1` / `adb-skill-discovery` | 2 | Settings entry and screenshot-guided device-information navigation |
| `adb_android_navigation` / `adb-navigation` | 8 | Cross-app entry, panel recovery, Back, scroll round trip, app resume context, recents selection |
| `adb_android_text_editing` / `adb-text-editing` | 4 | ASCII spaces, punctuation, replacing a query, clearing a query |

These are 32 task executions per repeat, not 32 independent capabilities: some
existing suites deliberately overlap while evaluating different tool policies.
The authoritative task-ID allowlists are in `../ci/suites.json`. CI passes these
IDs through to the normal runner rather than duplicating existing task JSON.

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
moving icons, accounts, and publishing content; each text task clears the query
before typing. The reused recents task removes a Settings card, so run it only
on the dedicated test device. Some reused tasks use Agent-driven setup; inspect
setup failures and pre-screenshots separately from scored task execution.

The new tasks name outcomes and accept localized/equivalent Android screens.
They do not specify package names, icon positions, a launcher brand, or an
app-drawer route. The old smoke suite still has an explicit app-drawer task and
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
python3 benchmark/ci/plan.py matrix --profile adb --suite suites/phone_control_v1.json
```

For a focused local run, start the bridge as described in `README.md`, then:

```bash
cd benchmark
uv run python -m runner run \
  --suite suites/adb_android_navigation.json \
  --environment-url http://127.0.0.1:8899 \
  --auto-agent-setup --target-platform android --max-concurrency 1 --repeats 3

uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --task-id tap_back --task-id find_and_tap_specific_item \
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

New tasks allow a one-call solution and do not require a particular tool route
except when the user goal explicitly asks for recents. Final-state rubrics rely
on the device screenshot; intermediate behavior is assessed from available tool
trace and observation evidence. The unchanged legacy suites retain their original
minimum-call and tool-policy rules. Report legacy-policy failures separately;
do not lower their thresholds in order to improve the new run's headline score.

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

## Reading results by execution environment

Actions Summary lists task outcomes separately under **Android (ADB)**,
**MobileGym**, and other recorded environments. Case completion means the runner
finished; consult the passed, failed, timeout, skipped, and judge-error counts.
Each environment's attempt success rate includes diagnostic UI task outcomes.

In `dashboard.html`, use **Execution environment → Android (ADB)** for native
Android results or **MobileGym** for synthetic app scenarios. Cards show each
environment/platform's latest workflow run. The same suite executed through two
backends has separate histories and comparison baselines. Weekly reports also
separate environments and platforms; their capability rates exclude skill
discovery diagnostics, as before. They do not report a pooled cross-environment
pass rate. Older records without environment metadata remain **Unknown**.

The ADB label identifies a native Android execution backend. It does not prove
whether the connected target was an emulator or a physical phone. That distinction
still requires device metadata; do not infer it from a suite name.

The existing `app_workflow_v1` suite is a separate, external-device suite with
13 end-to-end workflows: messaging/publishing, notes persistence, browser search,
maps, camera/gallery, calendar, commerce, and cross-app price comparison. It is
not included in the stock ADB profile. Notes, browser, and camera/gallery are
candidates for future native Android coverage after verifying installed apps,
onboarding, network/camera prerequisites, input support, and repeatable data
setup/cleanup. Account-dependent and publishing workflows need dedicated test
accounts and fixtures. Their absence from this profile is a coverage gap, not
evidence that those workflows pass on Android.
