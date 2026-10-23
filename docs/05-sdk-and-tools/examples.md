---
sidebar_position: 2
---

# Example Programs

CMake builds multiple `example_*` executables to verify SDK and device
capabilities. The Debian cross-build writes them to
`output/debian-apps/apps/bin/`; diagnostic examples are not copied into the
production business package. The commands below assume the selected binary has been
copied to the board and placed on `PATH`.

## Wakeup

```bash
example_wakeup
```

Listens for GPIO 33/GPIO 32 falling edge. Typical hardware connection: button connects GPIO to GND with internal pull-up.

## Audio Capture

```bash
example_audio_capture [device_name]
```

Captures audio and prints frame information. Optionally pass in ALSA device name.

## Audio Playback

```bash
example_audio_play [device_name]
```

Plays back a PCM audio file (`audio_capture_debug.pcm`) through the specified ALSA device. The PCM file must exist in the current directory or the path where the binary is run.

## Camera Capture

```bash
example_camera_capture
example_camera_capture --output /mnt/tmp/frame.raw
example_camera_capture --edid /usr/share/aico/edid/hdmi_1080p30_cta.hex
```

Common parameters:

```text
example_camera_capture [device_path] [width] [height] [pixfmt] [skip_frames] [options]
```

Supports options like `--device`, `--subdev`, `--edid`, `--force-trigger`, `--capture-retries`, `--output`, etc. See `--help` for details.

Default one-shot flow:

- First queries current DV timings;
- Pushes built-in 1080p60-only CTA EDID if necessary;
- Waits for `/dev/v4l-subdev2` HDMI sync;
- Discards transitional frames after stream-on;
- Captures from `/dev/video0` via V4L2 MMAP;
- Saves viewable `/mnt/tmp/frame.ppm` by default.

> If `frame_service` is running, stop it first, otherwise `/dev/video0` will be busy.

## USB HID

```bash
sudo example_usb_hid setup composite
sudo example_usb_hid keyboard tap ENTER
sudo example_usb_hid keyboard text "hello from pico"
sudo example_usb_hid touch click 16000 16000
sudo example_usb_hid cleanup
```

For more information, see [USB HID and Device Control](../03-services/usb-hid.md).

## Other Tools

| Program | Description |
| --- | --- |
| `hello` | Basic C program verification |
| `trigger` | GPIO / trigger-related utility |
| `audio_stream` | Audio stream example tool |
| `agent config-web` | Configuration web service |
| `frame_service_cli` | Frame Service debugging CLI |
| `audio_service_cli` | Audio Service debugging CLI |
