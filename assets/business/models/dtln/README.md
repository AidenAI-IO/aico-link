# Hybrid DTLN for RV1106

These deployment inputs were copied from the prepared RNN-Noise experiment on
2026-10-30. Only the validated hybrid path is included. The direct INT8 full
models failed the source experiment's quality check and are not used.

- `s1l0.rknn`, `s1l1.rknn`, `s2l0.rknn`, `s2l1.rknn`: LSTM affine gates,
  RKNN Toolkit2 2.5.0, RV1106, w8a8. Hidden/cell states stay CPU float32.
- `weights.f32`: 987,777 little-endian float32 values exported by the experiment's
  `scripts/export_cpu_weights.py`; offsets are in `src/dtln_weight_offsets.h`.
- `upstream.json`: pinned DTLN ONNX provenance. Model weights are MIT licensed;
  see `LICENSE` (Nils L. Westhausen, 2020).
- `librknnmrt.so`: private ARM hard-float RKNN Mini Runtime 2.5.0,
  `74ea630ce8@2026-09-30T09:43:19`. Rockchip copyright and upstream terms apply;
  the DTLN MIT license does not apply to this library.

Runtime source: [airockchip/rknn-toolkit2](https://github.com/airockchip/rknn-toolkit2/tree/2c723177a94fac65cbc036d722907ddbc91980cf/rknpu2/runtime/Linux/librknn_api),
commit `2c723177a94fac65cbc036d722907ddbc91980cf`,
`armhf/librknnmrt.so`, Git blob `99b78f9dacd63559120228feb59437c4a6c99231`.
The matching header is in `third_party/rknpu2/v2.5.0/include/rknn_api.h`.

The business package installs this directory at `/usr/lib/aico/models/dtln`.
Audio service loads the library by that private path with `RTLD_LOCAL`; it does
not replace the system runtime or the static RKNN runtime used by `rknn_vad`.
`SHA256SUMS` pins the deployment binaries and is checked during packaging.

Source experiment evidence: 16 kHz mono, 512-sample blocks, 128-sample hops.
The 60-second calibration recording scored 15.24 dB SI-SDR versus 15.32 dB
floating point; a 30-second holdout scored 8.32 versus 8.37 dB. With NEON and
FIFO priority 10, all 7,503 / 3,753 measured offline frames met the 8 ms budget.
These measurements excluded live capture/playback and concurrent load; they do
not establish live audio-service latency or sustained real-time performance.
