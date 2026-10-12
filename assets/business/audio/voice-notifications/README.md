# Voice Notification Audio Assets

Prerecorded audio clips for voice notifications when cloud TTS is unavailable.

## Audio Specifications

All WAV files in this directory must follow these specifications:

- **Format**: WAV (RIFF)
- **Encoding**: PCM, signed 16-bit little-endian
- **Sample Rate**: 16000 Hz
- **Channels**: 1 (mono)

These specifications match the PCM format used by the agent's audio pipeline and are extracted directly by `extractPCMFromWAV()` in `tts_fallback.go`.

## Voice Selection

To maintain consistent audio identity across all notification clips:

- **Chinese (zh-CN)**: `zh-CN-XiaoxiaoNeural` (warm, friendly female voice)
- **English (en-US)**: `en-US-AriaNeural` (positive, confident female voice)

Both voices are from Microsoft Azure TTS, available via the free `edge-tts` tool.

## Generation Workflow

### Prerequisites

Install `edge-tts` and ensure `ffmpeg` is available:

```bash
pip3 install edge-tts
brew install ffmpeg  # macOS, or equivalent on other platforms
```

### Step 1: Generate MP3 from text

```bash
edge-tts \
  --voice zh-CN-XiaoxiaoNeural \
  --text "你的中文文案" \
  --write-media output.mp3
```

For English:

```bash
edge-tts \
  --voice en-US-AriaNeural \
  --text "Your English text" \
  --write-media output.mp3
```

### Step 2: Convert to 16kHz mono PCM WAV

```bash
ffmpeg -i output.mp3 \
  -ar 16000 \
  -ac 1 \
  -sample_fmt s16 \
  -y output.wav
```

### Step 3: Verify specifications

```bash
file output.wav
# Expected: RIFF (little-endian) data, WAVE audio, Microsoft PCM, 16 bit, mono 16000 Hz
```

Or with Python:

```python
import wave
w = wave.open('output.wav')
print(f'{w.getnframes()/w.getframerate():.2f}s  '
      f'{w.getsampwidth()*8}bit/{w.getframerate()}Hz/{w.getnchannels()}ch')
# Expected: X.XXs  16bit/16000Hz/1ch
```

### Step 4: Rename and place

Follow the naming convention:

- **Failure-specific clips**: `<failure-code>.<locale>.wav`
  - Example: `session_idle.zh-CN.wav`, `session_idle.en-US.wav`
  - The failure code matches the `TurnFailure.Code` field from `voice_notification.go`
  
- **Generic fallback**: `tts-unavailable.<locale>.wav`
  - Used when no failure-specific clip exists

Supported locales: `zh-CN`, `en-US` (standard BCP 47 format)

## Current Clips

| Failure Code | Chinese Text | English Text | Duration (zh/en) |
|---|---|---|---|
| `session_idle` | 长时间无对话，会话已结束。 | Session ended due to inactivity. | 3.2s / 3.0s |
| _(generic)_ | 当前智能服务暂时不可用，请稍后再试。 | The assistant service is temporarily unavailable. Please try again later. | 4.2s / 6.1s |

## Adding New Clips

When adding a new failure-specific clip:

1. **Check the failure code** in `internal/agent/voice_notification.go` — the `TurnFailureCode` constant that `TurnFailureFromError()` maps to.

2. **Get the localized text** from `PrepareSpokenText()` — the replacement message for that failure code.

3. **Generate both locales** (zh-CN and en-US) using the workflow above.

4. **Verify consistency**:
   ```bash
   file *.wav | grep -v "16 bit, mono 16000 Hz"
   # Should return nothing — all files match the spec
   ```

5. **Update this README** — add a row to the "Current Clips" table with the failure code, text, and duration.

## Fallback Behavior

The audio selection logic in `ttsUnavailableFallbackPath()`:

1. Determine the user's locale from device configuration (`zh-CN` or `en-US`).
2. If a failure code is provided, look for `<failure-code>.<locale>.wav`.
3. If not found (or no failure code), fall back to `tts-unavailable.<locale>.wav`.

There is no cross-locale fallback: the generic clip is always chosen from the
resolved locale, so both locales' generic clips must ship with the package.

This means:
- Adding a new failure-specific clip is **opt-in** — existing behavior is unchanged until the file is placed.
- The generic `tts-unavailable.*.wav` files must always exist as the ultimate fallback.

## Maintenance Notes

- **Do not edit these files manually** — always regenerate from text using the documented workflow to ensure audio quality and consistency.
- **Duration variance is acceptable** — shorter clips (2–3s) are fine as long as the message is clear.
- **Keep the voice consistent** — all new clips must use `XiaoxiaoNeural` (zh-CN) and `AriaNeural` (en-US). Mixing voices confuses users.

---

Generated: 2026-10-09
