---
sidebar_position: 2
---

# Frame / Audio Service Protocol Reference

This page outlines the application protocols based on UDS envelope. For the common envelope, see [Unix Domain Socket Protocol](uds-protocol.md).

## Frame Service

Default socket:

- Direct development run: `/tmp/frame_service.sock`
- Debian service: `/run/frame_service/frame_service.sock`

### Operations

| op / command | Description |
| --- | --- |
| `health` | Returns service health status, capture mode, latest request sequence number, error messages, average latency, etc. |
| `latest_frame` | Starts capture, returns one fresh frame, then pauses capture again |
| `get_frame` | Compatibility operation; returns `FRAME_NOT_FOUND` because on-demand frames are not retained |
| `list_frames` | Compatibility operation; returns an empty list because there is no production frame history |
| `restart` | Requests service to restart capture manager |

`latest_frame` accepts `format: "jpeg"` or `format: "raw"`, an optional
`crop_black` boolean, an optional legacy `minimal_width`, and optional paired
`screen_width` / `screen_height` fields. When `crop_black` is true and current
screen dimensions are present, the service computes the largest centered crop
with that aspect ratio: either width or height remains equal to the source
frame. Without screen dimensions, both JPEG and raw responses detect centered
black bars on the horizontal and vertical axes and crop at most one axis.
`minimal_width` retains the legacy centered horizontal-only crop. Raw crops
preserve the source pixel format and align bounds to complete chroma samples.
When `crop_black` is omitted or false, no cropping is performed. The response
includes `source_width`, `source_height`, and the `crop_*` rectangle for
coordinate mapping.

The production service reports `capture_mode: "on_demand"` and
`ring_buffer_size: 0`, `ring_buffer_used: 0` in `health`. `since_seq` remains
accepted for protocol compatibility, but every successful `latest_frame`
request receives a newly assigned sequence number and triggers a fresh capture.
Concurrent capture requests are serialized.

### FrameMetadata

Core fields:

| Field | Description |
| --- | --- |
| `seq` | Frame sequence number |
| `capture_ts_ns` | Capture timestamp in nanoseconds |
| `width` / `height` | Resolution |
| `pixel_format` | Pixel format, e.g., `nv12` (frame_service default) or `uyvy` |
| `stride` | Row stride |
| `bytes` | Payload byte count |
| `planes` | Multi-plane metadata: offset / stride / bytes |
| `stale` | Whether frame is stale |

CLI will write the `latest-frame` payload as-is; `screenshot` will encode the frame as BMP before writing.

## Audio Service

Default socket: `/run/audio_service/audio_service.sock`

### AudioFormat

```json
{
  "sample_rate": 16000,
  "channels": 1,
  "bit_width": 16
}
```

### Operations

| op / CLI command | Description |
| --- | --- |
| `health` | Returns recording/playback session status |
| `start_recording` | Creates recording session, returns `session_id` |
| `read_record_chunk` | Long-polls to read PCM chunk |
| `stop_recording` | Stops recording session |
| `start_playback` | Creates playback session, returns `session_id` |
| `write_play_chunk` | Writes PCM payload; can mark end-of-stream |
| `stop_playback` | Stops playback session |
| `get_playback_volume` | Gets logical volume `0..100` |
| `set_playback_volume` | Sets logical volume `0..100` |

### Response Status

Audio Service uses shared `AicoServiceStatus`. Recording long-poll may return `TIMEOUT` on timeout; returns `SESSION_NOT_FOUND` when session does not exist.

## Version Compatibility Recommendations

- Maintain JSON backward compatibility when adding new fields;
- Keep payload as raw binary data, do not convert to base64;
- Clients should ignore unknown fields;
- Server errors should also return valid envelope and `status`; only socket connection/read-write failures are considered transport failures.
