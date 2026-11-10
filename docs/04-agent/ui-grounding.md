---
sidebar_position: 19
---

# UI Grounding

The Agent can mark the interface components it detects on the screen, so the
model can pick a target by number and tap a measured center instead of
estimating pixels by eye.

## Setting

`[conversation_settings.agent].ui_grounding_enabled` (default `true`) controls
every screenshot the Agent takes by itself for the model. Config Web shows it
as the "Mark tappable elements automatically" (自动标注可点击元素) switch on the
Conversation & tools page. The Agent reads it
live, so a change applies to the next screenshot without a restart.

| Screenshot | Setting on | Setting off |
| --- | --- | --- |
| Attached to each user message | Grounded | Plain |
| After `touch_gesture`, `keyboard_tap`, `quick_action`, `mouse_move`, `mouse_scroll`, `enter_text`, `open_app`, `open_url` | Grounded | Plain |
| `screenshot` tool | Only with `{"grounding": true}` | Only with `{"grounding": true}` |
| `wait_for_stable_screen` | Plain | Plain |

The `screenshot` tool is a plain capture tool: it grounds only when the model
passes `grounding: true`, whatever the setting.

Post-action grounding runs after the `screen_changed` comparison, which needs
the plain image, and before the touch markers are drawn, so the detector never
sees them. On a device without the detector, the screenshots the Agent takes
by itself stay plain and carry no grounding or error; only an explicit
`screenshot` request reports that grounding is unavailable.

## Observation

For a grounded screenshot, the Agent:

1. Decodes the cropped phone screenshot and sends it as RGB888 to
   `ui_grounding_helper`.
2. Drops boxes that cover more than half the screen (a page or panel, not a
   control), then draws each remaining component as a colored box with its
   number just above the top-left corner (Set-of-Mark), so the tag does not
   cover the control. At the top edge of the screen the tag goes inside the
   corner. Tags scale with the screenshot size.
3. Returns the marked image as the observation and adds a `grounding` field to
   the tool output. The tool's `action_output` is unchanged, and so is the
   tool result the model reads. The element list goes into the state message
   that carries the image, after its caption (for the user-message screenshot,
   after the device state lines):

```text
UI grounding: the numbered boxes in this image are these elements, with center and box in touch_gesture coordinates: {"elements":[{"id":1,"center":[500.0,260.4],"box":[62.5,231.7,961.7,289.2]}]}
```

Coordinates follow `touch_gesture`, so a center can be passed to it unchanged.
The screenshot tool reports normalized 0-1000 coordinates of the returned
screenshot (`pixel / (size - 1) × 1000`, one decimal), which is what
`touch_gesture` and the HTTP API take. When the visual coordinate protocol is
active (Anthropic models), `touch_gesture` takes screenshot pixels instead, and
the Agent converts the grounding field to the same pixels before the model
reads it. Elements are ordered by detector
score, at most 50, usually fewer. The marked image also becomes the latest screenshot for the
shell export (`$AICO_SCREENSHOT_JPEG`/`PPM`) and for coordinate checks.

If detection fails, the plain screenshot is returned and `grounding.error`
gives the reason, with an empty element list; the state message says grounding
failed, so the observation is never lost. For an explicit request on a host or
benchmark container without the helper and model, the error says grounding is
unavailable.

## Detector

The detector is the UI-DETR V6 student: a single-class YOLOv5 model with 960×960
input, quantized to INT8 for the RV1106 NPU. It finds "interface components"
only. It does not read text or classify controls, and it can miss a control or
box one control several times, so the model still chooses the target by reading
the image. The model, anchors, and provenance are in
[`assets/business/models/ui-detr/`](../../assets/business/models/ui-detr/README.md).

Preprocessing and decoding match the validated board runner: half-pixel
bilinear letterbox padded with 114, score equal to objectness times class
probability, NMS on normalized source coordinates, at most 50 boxes. The runner
was tuned for recall (confidence 0, NMS IoU 0.8), which leaves several
near-identical boxes on one control. The Agent passes `--confidence 0.3
--nms-iou 0.6` instead, so each mark is distinct, accepting that some controls
go unmarked. On the UI-DETR example screenshot this keeps 32 of the runner's
50 boxes.

## Helper process

`/usr/lib/aico/ui_grounding_helper` loads the model with the private RKNN Mini
Runtime 2.5.0 in `/usr/lib/aico/models/ui-detr/`, because the model was compiled
with Toolkit2 2.5.0. The static 2.3.2 runtime used by the audio helpers is not
involved.

The Agent starts the helper on the first grounding request and keeps it
resident, so later requests skip model initialization. The loaded model holds
about 40 MB of CMA, so the helper exits after two minutes without a request.
A protocol error or failed inference also stops it; the next request starts a
new one.

Protocol (stdin binary, little-endian; stdout text lines):

| Request | Response |
| --- | --- |
| startup (`--confidence C --nms-iou T`, both 0 to 1, default 0 and 0.8) | `READY <model_width> <model_height>` |
| `'D'` u32 width, u32 height, RGB888 bytes | `OK <n> <inference_ms>`, then `n` lines `<x1> <y1> <x2> <y2> <score>` in source pixels |
| `'Q'` | exit |
| any failure | `ERR <message>` |

Check the helper and model on a device:

```bash
/usr/lib/aico/ui_grounding_helper --runtime /usr/lib/aico/models/ui-detr/librknnmrt.so \
  --model /usr/lib/aico/models/ui-detr/model.rknn \
  --anchors /usr/lib/aico/models/ui-detr/RK_anchors.txt --self-test
```

The self-test runs one blank page through the NPU and prints the `OK` response.

## Cost

Source measurements on an RV1106G3 show a steady-state median of 666 ms and a
P95 of 816 ms per screenshot, excluding initialization (median 233 ms) and image
decoding. With the setting on, every user message and every input, text-entry
and launch action pays this once. Turn the setting off to save that time; the
model can still ask for grounding with the `screenshot` tool. The system
prompt's role rules and the device-operator skill tell the model to tap a
numbered element's center, unchanged, when one box covers the target.
