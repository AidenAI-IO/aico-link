# Third-party materials and model modifications

## ERes2Net / 3D-Speaker

- Source: https://github.com/modelscope/3D-Speaker
- Pinned source revision: `065629c313eaf1a01c65c640c46d77e61e9607b4`
- Copyright notices retained in all copied Python model files.
- License: Apache License 2.0; text: `licenses/3D-Speaker-LICENSE`.
- Upstream model: `iic/speech_eres2net_base_200k_sv_zh-cn_16k-common`, revision `v1.0.0`.
- Model card explicitly labels the model Apache License 2.0; a copy is preserved in
  `models/eres2net/UPSTREAM_MODEL_CARD.md`. That unmodified upstream text contains
  historical links to shared example clips; those links do not add another model
  implementation to this repository. Trailing blank lines were trimmed only.
- The three public WAV fixtures come from that model's `examples/` directory. They
  are provided only to reproduce the published smoke tests, not as held-out data or
  as permission to identify/impersonate their speakers.

### Changes made for this deployment

The original `.pt` state dict is retained without modification. Derived artifacts
split the trained model into a fixed 200-frame ONNX backbone and a native CPU
statistics/projection tail, then quantize the backbone for RV1106 W8A8 with MMSE.
No learned weights were fine-tuned in this work. Details and hashes are in
`models/eres2net/README.md`; source implementation is under `vendor/3d-speaker/`.

## Runtime and toolchain

RKNN Toolkit2 / `rknn_api.h` / `librknnmrt.so` are Rockchip components with their
own terms. They are **not bundled here**; supplying their filesystem paths to the
build does not license redistribution. The tested Debian board uses ARM hard-float
glibc. Runtime libraries from uClibc firmware are not interchangeable.

The model's Apache-2.0 grant does not grant trademark rights, user voice consent,
or licenses to unrelated dependencies. Before shipping a complete device image,
review the licenses of every included SDK/runtime and the applicable voice-data
privacy requirements independently.
