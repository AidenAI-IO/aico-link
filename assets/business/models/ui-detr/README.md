# UI-DETR V6 component detector for RV1106

These deployment inputs were copied from the UI-DETR-1 final bundle
(`models/final/`, manifest `ui_component_final_delivery_v2`, created
2026-10-31). The Agent's `screenshot` tool uses them when the model passes
`grounding: true`.

- `model.rknn`: single-class YOLOv5 student, 13,937,158 parameters, 960×960
  NHWC uint8 input, three sigmoid probability heads at strides 8, 16 and 32.
  Trained from random initialization on UI-DETR pseudo-labels. RKNN Toolkit2
  2.5.0, RV1106, `asymmetric_quantized-8`, mean 0 and std 255 folded into the
  model.
- `RK_anchors.txt`: the anchors fitted for this model only; never mix with
  anchors from another version.
- `librknnmrt.so`: private ARM hard-float RKNN Mini Runtime 2.5.0,
  `74ea630ce8@2026-09-30T09:43:19`, identical to the copy in `../dtln/`. See
  that directory's README for its upstream source. Rockchip copyright and
  upstream terms apply.

The business package installs this directory at `/usr/lib/aico/models/ui-detr`.
`ui_grounding_helper` loads the runtime by that private path, so it does not
replace the system runtime or the static 2.3.2 runtime linked into the audio
helpers. `SHA256SUMS` pins the files and is checked during packaging.

Decoding follows the validated board runner: half-pixel bilinear letterbox
padded with 114, `score = objectness × class probability`, NMS on normalized
source coordinates, at most 50 boxes, highest score first. The runner's
confidence 0 and NMS IoU 0.8 favor recall; the Agent uses confidence 0.3 and
IoU 0.6 so marked boxes do not duplicate each other. That setting was not
part of the source evaluation.

Source measurements on an RV1106G3 (steady state, excluding initialization
and image decoding): median 666 ms and P95 816 ms per screenshot, sampled
CMA increase up to 42.7 MB, initialization median 233 ms. The detector finds
one class, "interface component"; it does not output text, control types or
click intent. On 26 previously reviewed screenshots it matched about 85% of
AI-labelled components at IoU 0.5, with many extra and duplicate boxes. These
are not independent test results or click success rates.
