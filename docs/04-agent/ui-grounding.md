---
sidebar_position: 19
---

# UI Grounding

The `screenshot` tool can mark the interface components it detects on the
screen, so the model can pick a target by number and tap a measured center
instead of estimating pixels by eye. This is opt-in per call:

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
2. Draws each detected component as a colored box with its number at the inner
   top-left corner (Set-of-Mark). Tags shrink to fit small boxes; a box too
   small for any tag gets its tag just above it.
3. Returns the marked image as the observation, and sets the tool's
   `action_output` to the element list:

```json
{"ok":true,"elements":[{"id":1,"center":[248,281],"box":[31,250,477,312]}]}
```

Coordinates are pixels of the returned screenshot, the space `touch_gesture`
takes under the visual coordinate protocol. Elements are ordered by detector
score, at most 50. The marked image also becomes the latest screenshot for the
shell export (`$AICO_SCREENSHOT_JPEG`/`PPM`) and for coordinate checks.

If detection fails, the plain screenshot is returned with `grounding_error` in
`action_output`, so the observation is never lost. On a host or benchmark
container without the helper and model, the error says grounding is
unavailable.

## Detector

The detector is the UI-DETR V6 student: a single-class YOLOv5 model with 960×960
input, quantized to INT8 for the RV1106 NPU. It finds "interface components"
only. It does not read text or classify controls, and it can miss a control or
box one control several times, so the model still chooses the target by reading
the image. The model, anchors, and provenance are in
[`assets/business/models/ui-detr/`](../../assets/business/models/ui-detr/README.md).

Post-processing matches the validated board runner: half-pixel bilinear
letterbox padded with 114, score equal to objectness times class probability,
no confidence threshold, NMS at IoU 0.8 on normalized source coordinates, and at
most 50 boxes.

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
| startup | `READY <model_width> <model_height>` |
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
decoding. Grounding is therefore off by default; the device-operator skill asks
for it when targets are small or dense, or after a missed tap.
