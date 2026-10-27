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

## Volume

`audio_service` exposes a logical volume of `0..100`. The underlying device maximum volume can be initialized with:

```bash
scripts/setup_audio_volume.sh
```

See [Volume Initialization and Adjustment](../07-operations/audio-volume.md) for details.
