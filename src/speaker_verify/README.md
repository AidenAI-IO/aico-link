# Classic speaker verification helpers

Board-side tools vendored from
[aico-speaker-verification](https://github.com/AidenAI-IO/aico-speaker-verification)
(ERes2Net-base on RV1106). The Agent shells these out the same way as the
upstream web demo:

1. `speaker_verify_fbank` — PCM16 WAV → Fbank `[1,1,80,200]`
2. `speaker_verify_rknn` — RKNN backbone → frames
3. `speaker_verify_tail` — CPU linear → 512-d embedding

Go owns multi-profile storage, L2 normalization, cosine scoring, and the
Classic utterance gate. See `src/agent/internal/agent/speaker_*.go`.

Models (installed under `/usr/lib/aico/models/`):

- `eres2net_backbone_rv1106_mmse.rknn`
- `eres2net_tail.f32`
