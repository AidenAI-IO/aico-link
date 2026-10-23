---
sidebar_position: 15
---

# Swipe Interaction

Aico controls phone scrolling and picker wheels through USB HID while observing the result through HDMI screenshots. Because it does not receive native accessibility or scroll events, every gesture must be verified from the visible screen.

## Constraints

| Capability | Aico behavior |
| --- | --- |
| Touch confirmation | Infer success from the screenshot returned after the action. |
| Widget value reading | Read visible values from the screen; no native picker value API is available. |
| Scroll boundary detection | Compare before/after screenshots and inspect the resulting page. |
| Inertia control | Use bounded gestures and re-observe instead of assuming an exact displacement. |

JPEG noise, animation frames, and repeated content make exact pixel displacement unreliable. Treat scrolling as an iterative observe-act-verify loop.

## Tools

### `touch_gesture`

Use the standard `type` form for normal touch interaction, including taps, long
presses, swipes, scrolling, and two-phase drags. Atomic `actions` are a
low-frequency advanced option for an uninterrupted custom contact sequence
that the standard gesture types cannot express. Actions execute in order in
one input session, so a contact remains down across waits and moves:

```json
{
  "actions": [
    {"action": "touch_down", "point": {"x": 500, "y": 700}},
    {"action": "wait", "ms": 80},
    {"action": "move_to", "point": {"x": 500, "y": 300}, "speed": 2500},
    {"action": "touch_up"}
  ]
}
```

The action vocabulary is deliberately small:

- `touch_down`: starts a contact and requires `point`.
- `move_to`: moves to `point`, preserving the current contact state. Optional `speed` uses normalized coordinate units per second and derives movement time from the preceding point. `duration_ms` overrides `speed`; omitting both keeps the existing immediate move.
- `wait`: waits for `ms` milliseconds without changing contact state.
- `touch_up`: releases the current contact; `point` is optional.

Coordinates use the normalized `0..1000` range. A program must contain at least one action and must end with `touch_up`; each declared action duration is bounded to 30 seconds, the sum of declared durations is bounded to 60 seconds, and programs are limited to 128 actions. HID and ADB check these limits on both the submitted program and the expanded motion profile, including its release tails, before sending any input. Supported one-object `type` forms remain the default for normal interactions; `type:"drag"` is not supported.

Moving a draggable target intentionally spans two tool calls. Never replace
this flow with atomic `actions`. Always use this sequence:

1. Call `{"type":"drag_start","point":{"x":400,"y":500}}` at the target's current center.
2. Let `drag_start` finish its internal screen-stability wait. When its result
   reports `screen_stable=true`, inspect the returned stable screenshot and
   confirm the final destination point. Do not choose a destination from an
   intermediate or `screen_stable=false` result. On `screen_stable=false`,
   runtime automatically returns the contact to the original `drag_start`
   point and releases it; inspect the returned screenshot and retry this flow
   from step 1 instead of calling `drag_release`.
3. Call `{"type":"drag_release","point":{"x":750,"y":500}}` with that confirmed point.

`drag_start` presses for 500ms, then moves exactly 200 normalized units at 500
normalized units per second (a 400ms interpolated move) in a bounded axis
direction to activate dragging, and does not release when the screen becomes
stable. `drag_release` moves
directly to the destination, holds for 200ms, then releases. Do not issue an
unrelated input action between the pair. The stable-screen wait and final
screenshot capture are internal to `drag_start`; a separate
`wait_for_stable_screen` call is not part of the normal flow. The former one-call `type:"drag"`
gesture has been removed.

On Android ADB backends, the provider discovers the physical touchscreen and its absolute coordinate range with `getevent -lp`, then emits `sendevent` programs that preserve contact across atomic waits/moves and across the `drag_start`/`drag_release` boundary. This requires the Android shell user to have write access to the selected `/dev/input/event*` device. When device permissions or SELinux prohibit raw injection, the provider falls back to Android's `input touchscreen motionevent DOWN|MOVE|UP` primitive; if that is also unavailable, atomic actions return `module_unavailable`. HID is a separately selected alternative through `input_backend=hid`; the ADB provider does not switch to HID automatically.

Use `type:"swipe"` for ordinary lists, carousels, maps, and other free-scrolling surfaces:

```json
{
  "type": "swipe",
  "start": {"x": 500, "y": 650},
  "end": {"x": 500, "y": 350}
}
```

`speed` is optional and defaults to `2500` normalized coordinate units per second. A swipe accepts either `start` + `end`, or `start` + `direction` (`up`, `down`, `left`, `right`). `duration_ms` is optional: with an explicit `end` it overrides the calculated timing; with a `direction` it determines travel as `speed * duration_ms / 1000`. Without a duration, a directional swipe travels to the corresponding screen edge. `hold_before_ms` and `hold_after_ms` optionally add dwell after press and before release (default 0), while `steps` optionally controls HID interpolation (default 24). For example, `{"type":"swipe","start":{"x":500,"y":800},"direction":"up","speed":2500,"duration_ms":300}` ends at `{"x":500,"y":50}`.

Normalized coordinates use a `0..1000` range on each axis. HID action tools return a post-action screenshot after the screen settles.

## Picker Workflow

Picker columns use the standard `touch_gesture` swipe. The specialized
`wheel_nudge` tool, row-spacing detector and picker-only execution policy
have been removed.

1. Read the selected center value and adjacent row order from the latest
   screenshot. A visible value outside the center is not selected.
2. Move the desired row toward the center: a row above it needs finger-down;
   a row below it needs finger-up. Keep the final requested target fixed.
3. Start inside the intended column near its appropriate inner edge. Many
   controls retain the contact after the pointer leaves their bounds, so the
   endpoint may extend outside the column while remaining inside the screen
   with a margin from physical edges.
4. Estimate travel from visible row spacing and calibrate from the observed
   value change. Use longer travel when far away and shorter corrections near
   the target. Leave timing at defaults and omit `hold_after_ms`.
5. Verify the centered value in every returned screenshot. A changed readable
   value is progress even when `screen_changed=false`. If moving outside the
   region cancels the gesture, shorten the next path for that control.
6. Verify all requested columns before saving. Do not tap picker rows or use
   text entry to expose an editor. Stop if values are unreadable or repeated
   shorter corrections do not converge.

## Shared Motion Profile

`src/agent/internal/agent/mnk/motion_profile.go` owns the quintic acceleration/
braking curve, release eligibility and the normalized endpoint split. Standard
HID swipes, timed HID atomic contact moves, and ADB touch programs reuse it.
New gesture tools should call `SwipeWithOptions` or `TouchActions` instead of
implementing their own report trajectories. Backends retain event encoding and
scheduling: HID uses a monotonic clock; ADB preserves requested sleep intervals
but process/injection overhead can increase elapsed time.

An eligible content gesture uses at least 180ms of main motion followed by
100ms of real low-speed movement over the last two normalized units (bounded
to half the final segment). This approaches the exact endpoint rather than
holding a stationary coordinate. Screen-edge origins and explicit end holds
are excluded from the release tail. Atomic immediate moves and releases that
change the endpoint keep their existing semantics. Mouse-wheel events and the
dedicated two-call drag API are separate operations.

The iOS `app_switch` shortcut uses 80ms after pressing, a 350ms upward move,
and 200ms before release: 630ms of programmed contact time instead of 1750ms.
Stable-screen observation and HID overhead are additional.

The iOS `close_app_from_recents` shortcut starts near the bottom of the centered
card at `(500,780)` and moves to `(500,50)` over a 180ms main segment, keeping
the shared low-speed release profile. Including the initial 80ms wait and 100ms
moving release tail, programmed contact time is 360ms. The longer path provides
more travel for dismissal with a slow release. The previous `(500,500)` to
`(500,120)` path left the card in place in the local iPhone check; verify the
card disappears in the returned screenshot rather than relying on tool `ok`.

## Ordinary Scrolling

### Lists

1. Prefer a visible search field over blind scrolling.
2. Use a moderate swipe for exploration.
3. Inspect the returned screenshot and `screen_changed`.
4. Switch to a shorter swipe when the target approaches the viewport.
5. Stop when the screen no longer changes or a visible boundary is reached.

### Horizontal carousels

Swipe within the carousel rather than across global navigation or system gesture areas. Confirm the page or selected item changed before continuing.

### Maps and canvases

Use short pans, re-observe the viewport, and adjust direction iteratively. Screenshot interpretation is more useful than global image-diff ratios for these surfaces.

## Reusing Stable Parameters

When a widget has been operated successfully across repeated observations, the Agent can save a procedure memory containing the app, page, picker location, row spacing, and effective interaction pattern. Recalled values are hints only: current screenshot geometry always takes precedence.

## Operational Boundaries

- Visual reading can misidentify similar picker values; verify the centered result.
- Exact scroll distance is not available.
- Layouts can change across device size, language, OS version, and app version.
- A successful gesture does not prove the task succeeded; verify the resulting page or value separately.
