---
sidebar_position: 5
---

# Audio Service: Audio Recording and Playback Service

`audio_service` is a long-running C++ service that provides recording, playback, and volume control capabilities. The Go Agent's device voice pipeline and the `audio_volume` tool call this service through a Unix domain socket.

## Default Parameters

| Parameter | Default Value |
| --- | --- |
| Socket | `/run/audio_service/audio_service.sock` |
| Log | `/var/log/audio_service/audio_service.log` |
| Binary | `/usr/lib/aico/audio_service` |
| Volume state file | `/userdata/audio_service/playback_volume` |

## Command-line Parameters

```text
audio_service [--socket PATH] [--volume-state PATH]
```

| Parameter | Description |
| --- | --- |
| `--socket PATH` | Unix domain socket path for service communication |
| `--volume-state PATH` | File path for persisting playback volume across restarts; default `/userdata/audio_service/playback_volume` |

When run directly during development, it also defaults to `/run/audio_service/audio_service.sock`, which can be overridden via a parameter or environment variable:

```bash
AUDIO_SERVICE_SOCKET=/tmp/audio_service.sock /usr/lib/aico/audio_service
/usr/lib/aico/audio_service --socket /tmp/audio_service.sock
```

## Startup

```bash
systemctl start aico-audio.service
systemctl status aico-audio.service --no-pager
systemctl restart aico-audio.service
```

## CLI

```text
audio_service_cli [--socket PATH] <command>

Commands:
  health
  record-stream [--seconds N]
  play-stream [--rate N] [--ch N] [--bits N]
  get-volume
  set-volume --volume N
```

Examples:

```bash
audio_service_cli --socket /run/audio_service/audio_service.sock health
audio_service_cli --socket /run/audio_service/audio_service.sock get-volume
audio_service_cli --socket /run/audio_service/audio_service.sock set-volume --volume 80
```

Recording:

```bash
audio_service_cli --socket /run/audio_service/audio_service.sock record-stream --seconds 3 > /tmp/record.pcm
```

Playback:

```bash
cat /tmp/record.pcm | audio_service_cli --socket /run/audio_service/audio_service.sock play-stream --rate 16000 --ch 1 --bits 16
```

## Agent Configuration

```toml
[voice_settings.classic.audio]
socket = "/run/audio_service/audio_service.sock"
sample_rate = 16000
channels = 1
bit_width = 16
```

In voice mode, the Agent uses the audio service to:

1. Start a recording session;
2. Read PCM chunks;
3. Detect speech boundaries with RKNN Silero VAD;
4. Start a playback session and write PCM when playback is needed.

## Capture Voice Processing (VQE)

Every recording session enables the Rockchip VQE pipeline configured by
`/usr/share/aico/audio/config_aivqe.json`
(`overlay-debian/usr/share/aico/audio/config_aivqe.json`). AEC uses the
digital loopback of the playback path as its reference. The other enabled
stages are residual echo suppression (`aes`), noise reduction (`anr`),
automatic gain control (`agc`), and dereverberation (`dereverb`).

Several settings in this file are tuned for capture quality with the onboard
microphone:

- `howl` is disabled. The device never plays the microphone back through the
  speaker in real time, and with the speaker silent this stage added stationary
  tones (about 2.4, 4.0, 5.6, and 7.2 kHz) and raised the noise floor by about
  25 dB.
- `anr.fGmin` is `0.2` (about -14 dB) instead of `0.01`. The lower floor
  gated the background to digital silence and cut weak syllables and word
  endings.
- `agc.max_gain` is `12` dB instead of `25` dB to avoid amplifying residual
  noise.

When retuning, compare recordings of the same speech with the speaker silent
and while playing, and check both the background between words and quiet
speech.

## DTLN Noise Suppression (Enabled by Default)

For mono PCM16 recording sessions at 16 or 24 kHz, the capture pipeline is:

1. Hardware capture and the existing Rockchip VQE/AEC pipeline;
2. Mono conversion to 16 kHz and DC high-pass filtering;
3. Hybrid DTLN noise suppression (four RKNN gate graphs plus CPU float32 state
   and DSP);
4. Conversion to the requested 16 or 24 kHz rate and delivery over the socket.

This applies to Agent recording and `audio_service_cli record-stream`. Playback
and the AEC reference path are unchanged. Other recording formats bypass DTLN
with a `dtln_format_bypassed` log entry. VQE noise reduction remains enabled;
the combined VQE/DTLN chain still needs device listening and load validation.

The model uses 512-sample blocks and 128-sample hops (8 ms). Its 384-sample
algorithmic delay is 24 ms, plus partial-hop buffering, capture buffers and
processing time. Startup padding is withheld and the delayed tail is flushed
before EOF so short recordings and word endings are retained. State resets on
each recording session and capture restart.

`stop_recording` requests capture shutdown; clients must continue reading until
`end_of_stream` before closing the session. The CLI writes this final PCM to
stdout, and the Agent includes it before finalizing streaming STT and in the
fallback WAV/audio archive. A missing EOF is reported as a drain error rather
than silently accepting a truncated recording.

The business package includes the models, float weights and tested private
RKNN Mini Runtime 2.5.0 under `/usr/lib/aico/models/dtln/`. It checks their
SHA256 sums when packaging. The runtime is loaded only for DTLN and does not
replace the RKNN runtime used by VAD. See the
[model provenance](../../assets/business/models/dtln/README.md).

Settings in `/etc/aico_audio_service.conf` (also accepted as environment
variables when running the service directly):

| Variable | Default | Purpose |
| --- | --- | --- |
| `AUDIO_SERVICE_DENOISE` | `1` | Set to `0` to bypass DTLN |
| `AUDIO_SERVICE_DTLN_DIR` | `/usr/lib/aico/models/dtln` | Directory containing all four models, weights and private runtime |
| `AUDIO_SERVICE_CAPTURE_RT_PRIORITY` | `10` | Capture worker FIFO priority; set to `0` for normal scheduling |

Restart `aico-audio.service` after changing its configuration. The service unit
sets `LimitRTPRIO=10`; only the capture worker requests real-time scheduling.
If the request fails, recording continues with a `capture_rt_unavailable`
warning. The source experiment met its offline 8 ms hop budget with NEON and
FIFO priority 10, but live end-to-end latency and sustained concurrent load
must be measured on the device.

Initialization failure logs `dtln_unavailable` and preserves VQE-only capture.
Inference failure logs `dtln_failed`, returns buffered original audio and
bypasses DTLN for the rest of that session. The next recording session retries
initialization. Successful initialization logs `dtln_enabled`.

To compare with VQE-only capture, set `AUDIO_SERVICE_DENOISE=0`, restart, and
repeat the recording CLI example above. Check service logs for `dtln_enabled`
in the default configuration and listen to quiet speech and word endings with
playback both silent and active. Host tests cover streaming and failure
handling; they cannot validate NPU inference or microphone quality.

## Volume

`audio_service` exposes a logical volume of `0..100`. The underlying device maximum volume can be initialized with:

```bash
scripts/setup_audio_volume.sh
```

See [Volume Initialization and Adjustment](../07-operations/audio-volume.md) for details.
