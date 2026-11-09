---
sidebar_position: 19
---

# UI Grounding

The `screenshot` tool can mark the interface components it detects on the
screen, so the model can pick a target by number and tap a measured center
instead of estimating pixels by eye. It is for correcting coordinates: when a
`touch_gesture` seems to have missed its target, the model takes a grounded
screenshot, finds the target's box, and retries at that element's center.
It is opt-in per call:

```json
{"grounding": true}
```

## Observation

Without `grounding`, `screenshot` behaves as before. A user message no longer
triggers a screenshot by itself: the model calls `screenshot` when it needs to
see the screen.

With `grounding: true`, the Agent captures the screenshot as usual, then:

1. Decodes the cropped phone screenshot and sends it as RGB888 to
   `ui_grounding_helper`.
2. Drops boxes that cover more than half the screen (a page or panel, not a
   control), then draws each remaining component as a colored box with its
   number just above the top-left corner (Set-of-Mark), so the tag does not
   cover the control. At the top edge of the screen the tag goes inside the
   corner. Tags scale with the screenshot size.
3. Returns the marked image as the observation, and adds a `grounding` field
   with the element list beside the unchanged `action_output` of `ok`. The model
   reads both in the tool message:

```json
{"action_output":"ok","grounding":{"elements":[{"id":1,"center":[500.0,260.4],"box":[62.5,231.7,961.7,289.2]}]}}
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
gives the reason, with an empty element list, so the observation is never
lost. On a host or benchmark
container without the helper and model, the error says grounding is
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
decoding. Grounding is therefore off by default. The system prompt's role
rules, the tool description and the device-operator skill reserve it for one
case: when a `touch_gesture` result does not match what the model expected,
it takes a grounded screenshot and retries at the target element's center,
instead of adjusting coordinates from the same screenshot.
