---
name: device-operator
description: Use when controlling a visible target device UI through screenshots, touch, mouse, keyboard, text entry, scrolling, app switching, or capture recovery.
metadata:
  preferred_model: primary
  allowed_tools:
    [
      screenshot,
      wait_for_stable_screen,
      quick_action,
      touch_gesture,
      mouse_move,
      mouse_scroll,
      keyboard_tap,
      enter_text,
      open_app,
      open_url,
      request_user_action,
      recall_memory,
      save_memory,
      skill_read,
      shell,
    ]
---

Use this skill when the task requires operating a visible connected device UI. This is the complete generic device-operation playbook; do not split routine app switching, text entry, scrolling, picker, or screenshot recovery work into child skills.

## Core Loop

Always operate through a visual feedback loop:

1. Observe the current screen with `screenshot` or `wait_for_stable_screen`.
2. Decide the smallest next UI action that could make progress.
3. Act with one input or semantic tool.
4. Inspect the resulting screen.
5. Continue only after confirming what changed.

Do not perform multiple blind UI actions in a row. Base every coordinate, tap, swipe, and typed input on the latest visual state.

For actions that were expected to visibly change the UI, treat `screen_changed=false` in a post-action screenshot as "effect not yet verified". Post-action `screen_changed` compares the immediate pre-action screenshot with the final settled screenshot using structural change detection that ignores the top status area and minor image noise. The standalone `wait_for_stable_screen` tool instead reports whether motion occurred during its own wait window. In either case, do not say the action succeeded just because `action_output` is `ok`; inspect the screenshot, compare it with the expected target change, and continue checking or choose a different action if the UI still looks unchanged.

If `touch_gesture` returns `screen_changed=false` and the configured touch mode does not match the target platform, stop instead of retrying blind touches: Android expects `[basic_settings.device].device_type="Android"` (derived `hid.pointer_mode="touchscreen"`), while iOS/iPadOS expects `[basic_settings.device].device_type="iOS"` (derived `hid.pointer_mode="absolute"`). Ask the user to switch `device_type` and restart the agent before continuing.

For cross-app tasks that require extracting data from a source app and entering it into a target app, you must first visually confirm each required value from the source app's latest valid visual observations, such as `screenshot` or `wait_for_stable_screen` results. You may not switch away from the source app or enter any of that data into the target app until this verification is complete. Never invent or fabricate data that was not observed in the source app's UI.

## Tool Choice

Prefer the highest-level reliable tool for the job:

- When the user's intent clearly matches a cataloged semantic action, you MUST use `quick_action`. This includes back, home, app switching or switching back, system/global search, copy, paste, cut, select all, semantic backward/forward deletion, undo/redo, find, send, and browser actions. Use documented fallback fields such as `alternative` and `alternative_index` only when selecting an alternative binding. Use `{"action":"list"}` to inspect the configured device catalog when availability is uncertain.
  - A `ctrl`/`meta` `keyboard_tap` chord is allowed only when the user explicitly asks to press those exact physical keys, the shortcut is app-specific or not cataloged, or a `quick_action` result in the current run explicitly reports the matching action as `reserved`/unavailable before executing a binding.
  - Do not infer that `quick_action` is unavailable from an unrelated tool failure, text-entry failure, stale screenshot, HID problem, or your own assumption.
  - If an active quick action executed but returned failure or produced no visible effect, use a listed alternative or a non-shortcut UI strategy. Never replay the same binding through an equivalent `keyboard_tap` modifier chord.
  - If `ok=true` but the screenshot shows no expected change, treat it as ineffective: try `alternative=true` once when alternatives are listed, otherwise switch tools. Never loop on the same binding.
- When an app icon, app card, or requested control is clearly visible, unique, and unobscured in the latest screenshot, you MUST use `touch_gesture` to tap its visible non-overlapping center. This direct visible-target rule takes priority over `open_app` and system/app search, even when the user phrases the request as "open <app>". Do not call `open_app` merely because it is semantically available.
- Use the standard `touch_gesture` `type` forms for normal taps, long presses, swipes, and drags on mobile or desktop targets, and as a listed or non-shortcut fallback for back/home gestures. Back/home gesture fallbacks are ordinary `type:"swipe"` calls with explicit edge coordinates. Atomic `actions` are a low-frequency advanced option only for an uninterrupted custom contact sequence that no standard gesture type can express; do not use them for ordinary taps, long presses, swipes, scrolling, or draggable targets.
  - Decide whether the request moves a draggable target before selecting the gesture form. Moving an app icon, card, widget, list item, or other draggable UI target MUST use this sequence: call `drag_start` at its current center and let its internal screen-stability wait finish. `drag_start` presses for 500ms, then moves 200 normalized units at 500 normalized units per second (a 400ms interpolated move) in a bounded direction and keeps the contact down only when the wait succeeds. Do not call `wait_for_stable_screen` separately in the normal drag flow. Only when the `drag_start` result reports `screen_stable=true`, inspect its returned stable screenshot and confirm the final destination, then call `drag_release` at that confirmed point. When it reports `screen_stable=false`, runtime has automatically moved back to the original point and released the contact; inspect the returned screenshot and retry the complete `drag_start` flow instead of calling `drag_release`. Never determine or guess the destination from an intermediate or `screen_stable=false` result, translate this sequence into atomic `actions`, use the removed one-call `drag` type, or perform another input action while the contact remains down.
  - For a home-screen icon move to an empty spot, locate both source and destination before `drag_start`. Empty grid cells may continue below the last occupied icon row: use visible column centers and row spacing to locate a candidate inside the launcher workspace, above search, dock, and navigation. Confirm that cell is still vacant in the stable `drag_start` screenshot before releasing. Never release over another icon or its label; that creates a folder. Inspect the final screenshot to confirm the icon occupies the empty cell and no folder opened.
- For a numeric picker, use `touch_gesture` with `type:"swipe"`, starting inside the intended visible column and within its visible number rows. For finger-up, start near the lower visible row; for finger-down, start near the upper one. Convert vertical bounds using screenshot height. The end may extend beyond the column while staying inside the screen. Inspect the centered value in each returned screenshot; if it did not change, move the next start inside the wheel instead of repeating the path. Do not use keyboard entry or tap picker rows to expose an editor.
- Use `enter_text` for normal text input into fields, including Chinese/CJK, emoji, IME, and verified field entry.
- Use `keyboard_tap` for literal keys such as enter, escape, tab, and arrows; for exact physical chords the user explicitly asks to press; for app-specific shortcuts not represented by `quick_action`; and only for the evidence-gated reserved/unavailable fallback above. When a familiar Ctrl/Cmd chord merely describes a cataloged semantic goal, `quick_action` is mandatory.
- Use `mouse_move` and `mouse_scroll` only when pointer movement or wheel input is specifically appropriate; use `touch_gesture` with `type:"tap"` for coordinate clicks.

If a semantic tool fails, read the message and choose a different approach. Do not retry the same binding unless the tool explicitly offers a distinct alternative.

## Coordinate Discipline

Before using coordinates:

- Inspect the screenshot and identify the intended target visually.
- Use normalized 0-1000 coordinates: `(0,0)` is top-left, `(1000,1000)` is bottom-right, `(500,500)` is center.
- Never pass screenshot pixels directly to a coordinate tool. Convert point measurements from the latest returned image first: `x_normalized = pixel_x / max(screenshot_width - 1, 1) * 1000` and `y_normalized = pixel_y / max(screenshot_height - 1, 1) * 1000`.
- For taps and long presses, choose the visual center of the target. For small controls, estimate the control bounds and aim for the midpoint, biased slightly inward. For scrolling, use the scrollable-region bounds described below.
- For ordinary scrolling, start inside the intended scrollable region near its edge, with a small margin based on the current UI. The end may extend beyond the region while staying inside the screen. Physical screen-edge coordinates are reserved for system gestures: left-edge `back` starts at normalized `x=1`, and bottom-edge `home` starts at normalized `y=999`.
- Do not guess a coordinate if the target is not visible or the screen is stale.
- If a tap misses, observe again before adjusting. Do not repeat the exact same coordinate blindly.

## Text Entry

Use `enter_text` for normal input boxes such as search fields, forms, and chat composers.

Required pattern:

```json
{
  "text": "你好",
  "focus": { "x": 450, "y": 105 }
}
```

- Focus coordinates must come from the latest screenshot.
- Before calling `enter_text`, the latest screenshot must clearly show the actual editable field or composer, and `focus` must be inside that visible field. An app home screen, folder/list view, blank area, or screen that only shows a create/new button is not input-ready; first create/open the document or message and observe its editor.
- Treat `open_app` success as app-open confirmation only. It does not prove an in-app editor or input field is ready.
- Treat `ok:true` as successful text entry. When visual confirmation matters, also inspect the screenshot returned with the tool result.
- `ok:false` includes a next-step suggestion; follow it instead of inferring internal IME state from fields that are not part of the public result.
- For Chinese/CJK composition, provide only the exact target text. `enter_text` derives IME parts and keystrokes internally.
- Never pass Chinese, emoji, or romanization blobs to `keyboard_text`.
- When English text must be entered while a Chinese IME is active, switch to the English/Latin keyboard first, commonly via the globe/input-method key. Do not leave the English text in Chinese IME preedit/candidate state.
- If text remains in the IME candidate/preedit area instead of the field, retry once with corrected focus or follow the returned suggestion.

For copy and paste between two visible fields, identify both field bounds in the latest screenshot before editing. Enter text in the first field, use `quick_action` with `select_all` and then `copy` while that field remains focused, and inspect the selection or clipboard feedback. Tap the center of the distinct second field, confirm its caret or keyboard focus, then use `quick_action` with `paste`. Verify the text appears in the second field. If focus or selection is uncertain, take a fresh screenshot and correct that step; do not repeatedly tap the same point or copy without an active selection.

`enter_text` automatically prefers a usable Phone Bridge clipboard path, then falls back to ordered ASCII and IME runs. If its structured result conflicts with its attached screenshot, treat this as uncertain verification rather than immediate input failure. Call `wait_for_stable_screen` once and compare the requested text with the fresh observation. Preserve the current field while evidence conflicts; do not perform corrective input until the fresh observation identifies a concrete mismatch.

For simple keys:

- Use `keyboard_tap` for literal enter, escape, tab, arrows, or backspace. If the intent is semantic submit/send, use `quick_action`; if the user explicitly asks to press Enter, use `keyboard_tap` with `{"keys":["enter"]}`.
- For semantic backward/forward deletion, use `quick_action` with `delete_backward` or `delete_forward`. Use `keyboard_tap` with `backspace` or `delete` only for an explicitly requested literal key press or the controlled unavailable/reserved fallback above; `delete` is forward-delete.

If text does not appear or appears in the wrong place, stop typing, take a fresh screenshot, re-check focus and field identity, then retry once with corrected focus or input method. If still failing, summarize observed field state and ask for help or use bridge if appropriate.

If `enter_text` reports missing HID devices such as `/dev/hidg0` or `/dev/hidg1`, treat direct local text entry as unavailable in this environment. Take at most one fresh screenshot to confirm the current state; unless the target is already clearly visible and reachable without text entry, stop and report the blocker with the exact tool error and ask for help.

## App Switching and Launch

Use this flow for app switcher, recents, returning to Aiden, and cross-app navigation workflows.

1. Use global `[basic_settings.device].device_type` as the platform authority. Do not re-classify iOS/Android from screenshots; use screenshots only to locate visible controls and verify results.
2. Observe the screen.
3. If the target app icon or app card is clearly visible, unique, and unobscured, tap its visible non-overlapping center with `touch_gesture`.
4. If the target is not directly visible, try `quick_action` for `app_switch`, home, back, or app search when that semantic navigation step is appropriate.
5. Use `open_app` only when the target app is not clearly and reliably tappable in the latest screenshot, or when one direct visible-target tap produced no verified effect. It selects Phone Bridge or visible system search internally.
6. Verify the result with the post-action screenshot before continuing.

Before probing app-switch behavior, call `recall_memory` with tags such as `["app-switch", "device"]`. If a matching calibration exists for the configured `[basic_settings.device].device_type`, use it directly.

If no known quick action or cached method works:

1. Try bottom-edge swipe-and-hold: start near `y≈990`, end around `y≈550`, hold after gesture. Screenshot after the gesture.
2. If Android 3-button nav is visible, tap Recents.
3. If still on the same app, go home once, then retry app switcher/search from the home screen.

After each probe, verify whether the switcher appeared before trying the next method.

Selecting an app:

- If the target app card is visible, tap the visible non-overlapping center of that card.
- If not visible, swipe within the switcher to bring it into view.
- If still not found, dismiss the switcher and use `open_app`, system search, or the home/app drawer.
- If multiple plausible cards appear, ask the user to choose instead of guessing.

After successfully opening the switcher via a non-obvious method, call `save_memory` with the configured `[basic_settings.device].device_type`, method, gesture coordinates, and tags `["app-switch", "device"]`.

On iOS, if Phone Bridge context says the companion app is backgrounded/inactive and `return_entry=dynamic_island`, treat Dynamic Island as the fastest way back to the Aiden App. Do not blind-tap lock-screen Live Activity cards; use screenshot/HID fallback or visual confirmation for those cases. Opening the Aiden App restores the companion app shortcut channel.

## Scrolling and Picker Controls

Swipe `direction` describes finger movement, not content direction:

- `direction:"up"`: finger moves up → viewport scrolls down → reveals content below.
- `direction:"down"`: finger moves down → viewport scrolls up → reveals content above.

Scrollable region discipline:

- Many scroll controls continue receiving the same gesture after the initial press, even when the pointer moves outside their visible bounds.
- Identify the intended scrollable region from the latest screenshot. Use `type:"swipe"` with explicit `start` and `end`. Put the start inside that region near the appropriate edge, leaving a small margin based on the current UI.
- The end may extend beyond the scrollable region along the scroll direction while remaining inside the screen with a margin from physical screen edges. Use the available travel when far from the target and shorter travel for fine adjustments. A small picker does not require a short swipe confined to its visible height.
- Read the returned screenshot after every swipe to confirm that the intended control moved. If moving outside the region stops or cancels movement, shorten the next path for that control. Leave timing at defaults; the tool handles acceleration, braking, and release.

Calibration loop:

1. Identify the intended region and put the start just inside its appropriate edge. Choose an explicit end for the needed travel, which may be outside the region but must remain inside the screen; use default timing.
2. Read the gesture result's automatic post-action screenshot.
3. Use the returned screenshot and its `screen_changed` field to confirm movement.
   An omitted `screen_changed` means the baseline comparison was unavailable; judge the returned screenshot directly.
4. Adapt travel to the observed movement: longer when far from the target, shorter near it. Repeating a successful swipe is allowed while the content or selected value continues progressing. Recompute the start region if the layout changes.
5. Do not repeat the same path and speed after a failed attempt.

If the same list boundary appears again, stop searching in that direction. Try search/filter, a different tab, or ask the user.

For picker/wheel controls, use this observation-and-swipe loop. Keep the user's final target fixed and finish one column before moving to the next. Read each value from the latest screenshot rather than assuming hour/minute positions or reusing an earlier task's state.

1. Identify the selected CENTER value, the values immediately above and below it, the intended column's visible bounds, and adjacent row spacing. A target merely visible above or below the center is not selected yet. For numeric values, count remaining ROWS using the observed value change per row: 10 to 20 with rows 10,15,20 is two rows. Use a shorter wrapped route only when the column's cyclic range is known; do not assume every wheel wraps.
2. Move the desired row TOWARD the selected center. A row above center requires finger-DOWN (`end.y > start.y`); a row below requires finger-UP (`end.y < start.y`). When values increase downward, decreasing 08 to 07 therefore requires finger-down. If visible ordering is unclear but the current value is readable, use one short probe and inspect its result before a larger move.
3. Use `touch_gesture` with `type:"swipe"`, constant X inside the intended column, and explicit start/end. Start near the upper inner edge for finger-down or lower inner edge for finger-up; the end may extend beyond the column while staying inside the screen. Choose each swipe's start inside the column from the latest screenshot. Follow the active visual coordinate protocol; vertical pixel distances are normalized with screenshot HEIGHT, never width.
4. Choose travel from the remaining rows and the observed response to the last swipe in this same column. Initially use visible row spacing as an estimate, then calibrate from actual value changes. For example, if 120 normalized units moved three rows and only one remains, try about 40 units instead of repeating 120. When the target is visibly adjacent, use a one-row correction, not the previous multi-row path. Longer travel is allowed when many rows remain; do not restrict the end to the picker height. These are estimates: inspect the result after every swipe. Leave timing at defaults and omit `hold_after_ms`.
5. Read the centered value in the automatic post-action screenshot and recompute the remaining rows. A changed readable value is movement even when `screen_changed=false`; check whether it moved toward the target. Repeat while making progress, shortening travel near the target. If the value did not change, first recheck the start and column bounds; do not move the start progressively into the settings below. Discard earlier movement estimates after a layout change, keyboard edit, manual intervention, or switching columns.
6. Stop moving a column when its centered value equals the target. Recheck all requested columns before saving or confirming. Do not infer success solely from `ok`. Save or confirm only when requested by the user.

Do not tap picker rows or switch to keyboard/text entry: a row tap may open an editor instead of selecting that row. Do not use atomic actions or `drag_start` for a picker. Stop and report the visible state if values cannot be read or repeated shorter corrections do not converge.

## Screenshot and Capture Recovery

Use this only when `screenshot`, post-action screenshots, or capture-related tools fail and visual operation cannot continue.

If `screenshot` fails, output mentions `SERVICE_RECOVERING`, socket errors, empty image data, invalid screenshot JSON, or repeated post-action screenshot failure:

Immediately pause all UI actions (tapping, typing, swiping, navigation) before executing the recovery sequence; no further UI actions may be taken until the capture service is restored and a valid screenshot is obtained.

- Stop tapping, typing, swiping, and guessing from stale visual state.
- Do not claim a UI task is complete without a fresh screenshot proving the target screen.

Recovery sequence:

1. Retry `screenshot` once if the error suggests transient recovery.
2. If it fails again, diagnose frame service with the service manager used by
   the device:

```bash
# Debian
systemctl status --no-pager aiden-frame.service

frame_service_cli --socket /run/frame_service/frame_service.sock health
ls -l /run/frame_service/frame_service.sock
```

3. If health reports a bad state or recovering capture manager, request capture-manager restart first:

```bash
frame_service_cli --socket /run/frame_service/frame_service.sock restart
```

4. Verify recovery with health, then call `screenshot` again.
5. If CLI restart fails, the socket is missing, or the service is not running,
   restart it with the matching service manager:

```bash
# Debian
systemctl restart aiden-frame.service
```

6. After service restart, verify in order: service status, health, then `screenshot`.

If recovery still fails, inspect recent logs before asking the user to intervene:

```bash
tail -n 80 /var/log/frame_service/frame_service.log
```

When reporting a blocker, include the screenshot error, recovery commands tried, and latest health or log signal.

## Failed Attempt Handling

Treat an attempt as failed when the expected change did not happen, text was not entered, navigation did not move, the screen changed unexpectedly, or a tool result reports an error.

If an action was expected to change the UI and its returned post-action observation says `screen_changed=false`, no meaningful structural difference was detected between the pre-action and final settled screenshots. Treat that as a failed or unverified attempt until the screenshot itself proves otherwise. Do not report success from tool output alone.

After a failed attempt:

1. Observe with `screenshot`.
2. Compare expected vs observed result.
3. Avoid repeating the exact same failed action; if one repeat is justified, change one variable and verify the result before trying again.
4. Change one variable at a time: target location, gesture type, navigation path, input method, or semantic shortcut.
5. After 2 failed attempts on the same goal, change strategy instead of retrying the same path.
6. After 3 failed attempts total on the same goal, pause repeated UI actions, summarize what changed, then switch to diagnosis, a different path, user-facing blocker, or human handoff if no new evidence suggests progress.

Keep an internal attempt log with goal, attempt, expected result, observed result, and next adjustment. Report it only when blocked or asked.

If the device is detected to be locked and standard unlock gestures (swipe up from bottom, home quick_action) fail twice consecutively, stop repeating unlock gestures; switch to diagnosis or report the locked device as a blocker with the attempts tried. Do not keep repeating unlock gestures unless fresh evidence shows a different unlock path.

## Navigation and Search

- First identify the current screen and app when possible.
- Prefer visible buttons, semantic shortcuts, and search/filter controls over blind scrolling.
- When looking for an app, contact, setting, file, item, or page content, search before long manual browsing.
- Try one alternate search term when reasonable before switching to manual browsing.
- Check each relevant tab, list, or section once before repeating any of them.
- Do not repeatedly search or scroll the same unchanged list.
- If multiple plausible matches appear, ask the user to choose instead of guessing.
- After back, home, app switch, or navigation, verify the destination with a fresh observation.
- If navigation loops or returns to the same screen twice, stop and reassess.

## Sensitive Actions

Stop and ask the user, or call `request_user_action`, before actions involving:

- payment, purchase, order placement, transfer, or subscription
- deleting data or changing account/security settings
- login, verification code, captcha, biometric, or identity verification
- privacy permissions for contacts, photos, microphone, camera, location, or files
- sending messages, emails, posts, comments, or starting calls on behalf of the user

Do not tap a privacy permission switch, checkbox, Allow button, or equivalent control just to inspect what happens. If the visible target is a privacy permission toggle, ask before touching the switch. If a row and its switch are not clearly separable, treat the whole row as sensitive and ask first.

When the next required step is user confirmation for a sensitive action, call `request_user_action` immediately with the specific control/action and suggested user reply. Do not ask in prose and then continue using tools.

Do not confirm sensitive dialogs unless the user explicitly asked for that exact final action and the target/action still matches the current screen.

## Verification Checklist

Before reporting success:

- [ ] The latest observation proves the requested state or action completed.
- [ ] No sensitive/irreversible action was taken without explicit confirmation.
- [ ] Text entry was only reported successful when the tool returned committed success or the latest screen visibly confirms it.
- [ ] Any failed attempts were not repeated blindly.
- [ ] If blocked, the response says what was tried and what evidence blocked progress.
- [ ] Each required cross-app source value was visually confirmed from the source app's UI before being entered into a target app, with no invented or fabricated values.
