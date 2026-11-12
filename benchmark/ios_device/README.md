# Direct iOS Device Bridge

Connect a real iPhone to the native Agent over USB using
[pymobiledevice3](https://github.com/doronz88/pymobiledevice3) and
[WebDriverAgent](https://github.com/appium/WebDriverAgent) (WDA/XCTest).
This path captures the phone directly, supports Unicode input, and can select
system permission dialogs that iPhone Mirroring cannot reliably control.
It does not require macOS Accessibility or Screen Recording permission.

Start with [Local Agent with a Real Phone](../../docs/01-getting-started/local-agent-real-phone.md)
for the Agent and Config Web setup. Use a separate Agent data directory for
this target, especially its `quick_actions.json`.

## Prerequisites and Installation

The supported setup uses macOS, full Xcode, an Apple Development signing team,
and a USB-connected iPhone with Developer Mode enabled. Unlock the phone,
approve **Trust This Computer**, and finish any developer authorization shown
by iOS/Xcode. These initial trust and developer prompts require the user;
the bridge reports connection failures instead of attempting to approve them.
Xcode must support the connected iOS version and prepare its developer image
and CoreDevice tunnel. Open the device in Xcode's Devices and Simulators first.
On macOS, runner startup prefers Apple's native developer tunnel; the upstream
tunnel selector falls back to userspace if the native tunnel is unavailable.

From `benchmark/`, create a separate Python environment:

```bash
export AICO_IOS_ENV="$HOME/.cache/aico-ios/venv"
uv venv --python 3.12 "$AICO_IOS_ENV"
uv pip install --python "$AICO_IOS_ENV/bin/python" -r ios_device/requirements.txt
```

WDA must be signed and installed before running the bridge. Obtain an upstream
WDA checkout and record the revision used. The implementation was exercised
with WDA revision `3d6a4e03cdd673e540289c610f7e189f8ffea434` and
pymobiledevice3 `11.52.1` on iOS 27.2.1. The helper copies the checkout before
changing its runner bundle ID; it does not modify the original source:

```bash
"$AICO_IOS_ENV/bin/python" -m ios_device.setup \
  --team-id YOUR_TEAM_ID --udid YOUR_DEVICE_ID \
  --wda-source /path/to/WebDriverAgent \
  --output /path/to/new-wda-build --install
```

The default runner ID is `io.aiden.aico.WebDriverAgentRunner.xctrunner`.
Use `--bundle-id YOUR_UNIQUE_RUNNER_ID` if your team needs another app ID;
append `.xctrunner` when starting the installed runner. Signing/provisioning
errors must be resolved in Xcode with your own account. No signing keys or
account credentials belong in this repository.

## Start the Bridge and Agent

After installing the signed runner, start the complete local workflow from
the repository root with `python3 scripts/local_agent.py ios`. It builds the
Agent, prepares dependencies, opens Config Web, and starts this bridge with
the correct quick actions and task route. The manual commands below are for
debugging the bridge separately.

Keep the phone unlocked. Run only one XCTest/WDA controller for the device:

```bash
"$AICO_IOS_ENV/bin/python" -m ios_device \
  --xctrunner io.aiden.aico.WebDriverAgentRunner.xctrunner \
  --port 8896 --task-id ios-device
```

The bridge starts and owns that runner, then stops it on exit. Omit
`--xctrunner` to attach to a WDA instance already running through Xcode or
pymobiledevice3. Add `--udid` when multiple USB devices are attached.
`--probe` checks connectivity and lock state without sending touch/text input
or applying permission rules. With `--xctrunner`, probing also starts/stops WDA.

Copy the direct-device quick actions into this target's Agent data directory
before its first run. These bindings use native Home navigation and touch
gestures. The override replaces the embedded quick-action catalog:

```bash
cp ios_device/quick_actions.json "$AICO_AGENT_DATA/quick_actions.json"
"$AICO_LAB/agent" \
  -dir "$AICO_AGENT_DATA" -addr 127.0.0.1:8080 \
  --device-type iOS --environment-bridge-mode \
  --environment-bridge-endpoint http://127.0.0.1:8896 \
  --benchmark-task-id ios-device
```

The bridge claims its exclusive task lease at startup. Its `/health` response
reports `bridge_type: ios_device`, backend `ios-device`, and `input_available`.
A locked phone remains connected but rejects input. After a USB disconnection
or WDA exit, restore the connection and restart the bridge; requests are never
automatically replayed. Keep the existing Agent endpoint and task ID when
restarting. A timeout may mean input already reached iOS; inspect before retrying.

## Inspect and Select Permission Dialogs

The bridge exposes system and application alerts through WDA, including
notification, location, and camera prompts. Read the exact localized text and
button labels first:

```bash
curl --fail http://127.0.0.1:8896/api/ios/alert \
  -H 'benchmark-task-id: ios-device'
```

The result is `{"ok":true,"data":{"present":false}}` when no alert is open,
or includes `alert_id`, `text`, and `buttons` when one is present. Select a
previously inspected alert using its fingerprint and one exact button label:

```bash
curl --fail http://127.0.0.1:8896/api/ios/alert \
  -H 'benchmark-task-id: ios-device' -H 'Content-Type: application/json' \
  -d '{"alert_id":"FINGERPRINT_FROM_INSPECTION","button":"Allow Once"}'
```

A changed alert returns HTTP 409. A button must appear exactly once. Nested
accessibility aliases are collapsed only when their label, identifier, and
screen bounds match; genuinely ambiguous labels remain unselectable. After
sending a selection, the bridge checks that the alert disappeared or changed.
An unverified selection is an error and is not repeated automatically.
The endpoints require the active task lease, as do screenshot and input calls.
They are bridge extensions; the Agent retains its own normal tool catalog.

## Opt-in Automatic Permission Rules

Without a policy, permission dialogs stay visible for the Agent/user to
inspect and select. To automate known prompts, save an explicit policy and
start the bridge with `--permission-policy /path/to/permissions.json`:

```json
{
  "rules": [
    {
      "text_contains": "“Aico Permission Probe” Would Like to Send You Notifications",
      "buttons": ["Don’t Allow", "Allow"],
      "button": "Allow"
    }
  ]
}
```

Use the actual text and labels returned by your phone, including punctuation
and locale. Include the app name and permission purpose in `text_contains`;
avoid generic fragments such as `Allow`. The entire button list must match,
including duplicate labels (location precision toggles may appear twice).
Order does not matter. Only one matching rule may apply, and the selected label
must be unique. Unknown or ambiguous prompts are left visible.

Policies run before a screenshot is returned, so the normal Agent observation
loop can continue after a known dialog. The bridge records an attempt before
sending input, verifies dismissal, and never retries an uncertain selection.
It permits a fresh occurrence after observing an interval with no alert.
Rules apply to WDA alerts generally; scope them to the intended app and prompt.
There is no blanket auto-accept or real-device entitlement bypass.

## Optional Permission Test App

Build and install the included **Aico Permission Probe** to test notifications,
location, camera, and text input without changing another app's permissions:

```bash
"$AICO_IOS_ENV/bin/python" -m ios_device.setup \
  --team-id YOUR_TEAM_ID --udid YOUR_DEVICE_ID \
  --probe-app --output /path/to/new-probe-build --install
```

The app requests authorization and displays its current states. It does not
capture images, read location, or send notifications. iOS persists permission
choices. Use the app's Settings page to change permissions. Settings changes do
not restore the first-use notification prompt; use a fresh test bundle ID to
repeat first-use permission tests. The bridge does not reset other
applications' permissions.

## Input and Capture Limits

- Screenshots are JPEG at the device's actual pixel dimensions. Normalized
  coordinates map to WDA's logical screen size, including display rotation.
- Taps, double taps, holds, two-point swipes/drags, vertical scrolling, and
  complete single-contact atomic touch programs are supported. Atomic programs
  are limited to 128 actions and 10 seconds; partial drag contacts and
  multi-finger gestures are not supported.
- Native `insert_text` uses WDA Unicode keys. Replacement clears the focused
  accessible field before typing. `keyboard_text` retains the bridge's ASCII
  contract. Enter, Tab, and Backspace/Delete are supported; desktop modifier
  chords and arbitrary physical keys are not.
  Direct iOS text input accepts at most 512 UTF-16 units per request (256
  supplementary-plane emoji or 512 BMP characters). Oversized input is rejected
  before clearing or typing. WDA types at an explicit 60 keys/s, leaving time
  for keyboard synchronization within the 25s WDA and 30s Agent deadlines.
  Mirroring and other backends retain their existing text limits.
- This target's Home binding uses WDA's native `/wda/pressButton` command
  with `name: home`, which also dismisses Spotlight;
  Spotlight first uses the same command, then swipes down on Home. App Switcher
  still uses gesture navigation. Synthetic bottom-edge swipes may scroll an app
  instead of returning Home. Only this target's `quick_actions.json` opts into
  native system navigation; Mirroring, Android, and HID mappings are unchanged.
  The Agent's search/launch flow replaces its query through native insertion;
  public `enter_text` inserts at the current caret/selection. Direct bridge
  clients may use `insert_text` with `replace: true` to clear old field text.
- WDA can control permission alerts but cannot bypass locked-device security,
  protected authentication surfaces, signing, USB trust, or Developer Mode.
  Unsupported secure surfaces may not expose usable screenshots or elements.

See [Environment Bridge Protocol](../environment_bridge.md) for shared
setup/release, screenshot, and MNK request formats. The optional dependency is
host-only; normal Docker unit tests use a fake WDA transport and do not need
Apple hardware, signing credentials, or pymobiledevice3 installed.
