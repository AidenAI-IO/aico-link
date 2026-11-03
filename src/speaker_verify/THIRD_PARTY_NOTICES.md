# Third-party materials and model modifications

## ERes2Net / 3D-Speaker

- Upstream project: https://github.com/modelscope/3D-Speaker
- Upstream model: `iic/speech_eres2net_base_sv_zh-cn_3dspeaker_16k` / related ERes2Net-base
  releases under Apache License 2.0
- Board helpers and model packaging in this tree are adapted from
  [aico-speaker-verification](https://github.com/AidenAI-IO/aico-speaker-verification)
- Apache License 2.0 text for the vendored helper sources: `LICENSE` in this
  directory
- Deployed board models (hashes in
  `assets/business/models/eres2net-SHA256SUMS`):
  - `eres2net_backbone_rv1106_mmse.rknn`
  - `eres2net_tail.f32`

### Changes made for board deployment

The original `.pt` state dict is not fine-tuned here. Derived artifacts split the
trained model into a fixed-frame ONNX/RKNN backbone and a native CPU
statistics/projection tail, then quantize the backbone for RV1106 W8A8 with
MMSE. Helper sources in this directory implement Fbank extraction, RKNN
inference, and the CPU tail used by the Agent.

## Runtime and toolchain

RKNN Toolkit2 / `rknn_api.h` / `librknnmrt.so` are Rockchip components with their
own terms. They are **not bundled in this directory**; supplying their
filesystem paths to the build does not license redistribution. The tested Debian
board uses ARM hard-float glibc.

The model's Apache-2.0 grant does not grant trademark rights, user voice consent,
or licenses to unrelated dependencies. Before shipping a complete device image,
review the licenses of every included SDK/runtime and the applicable voice-data
privacy requirements independently.
