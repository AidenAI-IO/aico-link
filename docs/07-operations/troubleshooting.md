---
sidebar_position: 1
---

# Troubleshooting

## `example_camera_capture` reports `Device or resource busy`

Cause: `frame_service` is exclusively using `/dev/video0`.

Solution:

```bash
systemctl stop aiden-frame.service
example_camera_capture
systemctl start aiden-frame.service
```

For daily screenshots and frame testing, use:

```bash
frame_service_cli screenshot --out /tmp/screenshot.bmp
```

## `screenshot` tool fails

Check:

```bash
systemctl status aiden-frame.service --no-pager
frame_service_cli --socket /run/frame_service/frame_service.sock health
ls -l /run/frame_service/frame_service.sock
```

Common causes:

- `frame_service` is not running;
- `[advanced_settings.hardware.hid].frame_socket` path in `agent.toml` is inconsistent;
- HDMI input is not synced;
- RK628D or TC358743 HDMI subdevice status is abnormal.

On the Debian firmware, the first two cases are distinguishable with:

```bash
systemctl status aiden-frame.service --no-pager
frame_service_cli --socket /run/frame_service/frame_service.sock health
```

The service is intentionally socket-ready even when no HDMI source is present.
In that state `health` returns `state=RECOVERING` (or `STARTING`) and a
screenshot is unavailable until a frame arrives; a missing socket indicates a
real service/startup failure. Connecting HDMI later causes the capture manager
to retry automatically and transition to `RUNNING`.

## Frame Service keeps restarting

View logs:

```bash
tail -f /var/log/frame_service/frame_service.log
```

Check:

- Whether `/usr/lib/aiden/frame_service` exists and is executable;
- Whether `/dev/video0` and an RK628D or TC358743 subdevice exist;
- Whether EDID / HDMI signal is normal;
- Whether other processes are using `/dev/video0`.

Find the HDMI bridge subdevice without assuming a fixed index. RK628D reports
`rk628-csi`; TC358743 reports `tc358743`:

```bash
for f in /sys/class/video4linux/v4l-subdev*/name; do echo "$f: $(cat "$f")"; done
```

If the matching `rk628-csi` or `tc358743` node was not selected, set
`FRAME_SERVICE_SUBDEV=/dev/v4l-subdevX` in
`/etc/aiden_frame_service.conf` to that node and restart `aiden-frame.service`.

## HDMI bridge probes but capture cannot discover it

The shared device tree describes RK628 and TC358743 as alternatives. Each
board has exactly one bridge. The vendor V4L2 notifier waits for both endpoints
until the unused alternative is cleared. Without that completion, the fitted
bridge can probe successfully but its `/dev/v4l-subdev*` node is not created,
leaving Frame Service in `RECOVERING` at bridge discovery.

`aiden-media-modules` checks the CIF media topology for an enabled link from
either bridge to the CSI D-PHY, then writes the CIF and ISP `clr_unready_dev`
parameters in that order. The D-PHY bound callback creates this link before
notifier completion, so the check does not depend on a subdevice node or a live
HDMI signal. If neither bridge is linked, the helper preserves the waiting
endpoints and continues loading the independent media/audio modules. Clearing
them without a bound bridge can Oops in the vendor CIF input-format check.

The helper reports pending initialization to both the system journal and the
boot console with an `[aiden-media-modules]` prefix. It distinguishes a missing
enabled bridge link from a failed topology query (or missing `media-ctl`),
and explicitly states that CIF/ISP notifiers remain pending, cleanup was
skipped, and independent modules will continue loading. Query failures include
the media device path and diagnostic output; they do not imply a bridge is
absent.

Inspect the graph with `media-ctl -d /dev/media0 -p` (select the media device
whose driver is `rkcif`). If the bridge binds after the initial module helper
has finished, stop capture and rerun the helper so it can safely complete the
graph:

```bash
sudo systemctl stop aiden-frame.service
sudo /usr/lib/aiden/aiden-media-modules
sudo systemctl start aiden-frame.service
```

An I2C driver symlink alone does not prove binding has finished: Linux creates
it before calling the driver's probe function. Do not use that symlink as the
condition for forcing notifier completion.

ISP cleanup is attempted only after a successful CIF parameter write. If the
CIF parameter is missing, not writable, or rejects the write, the helper logs
a warning, leaves ISP pending, and continues loading independent modules.

## RK628 or I2C times out while Frame Service stays active

Every native service log line includes `monotonic_ms`, `pid`, and the native
`tid`. The monotonic value can be compared with the timestamp at the start of a
`dmesg` line, and the TID identifies the corresponding
`/proc/<pid>/task/<tid>` entry.

Frame Service records the complete request-to-driver boundary:

- `capture_request_received` / `capture_request_resolved`: request sequence,
  Unix-socket peer PID, UID, GID, process name, final status, and duration;
- `request_queued`, `cycle_started`, `warmup_started`,
  `frame_read_started`, and `cycle_completed`: which capture generation is
  active, whether work reached warm-up or a frame wait, and each stage's
  elapsed time;
- `camera_source open_started`, `bridge_selected`, and `policy_resolved`: the
  selected bridge and effective EDID/force-trigger policy;
- `v4l2 ioctl_started` / `ioctl_completed`: the exact ioctl operation,
  device, fd, return value, errno, and elapsed time. Operations include
  `subdev_query_dv_timings`, `subdev_set_dv_timings`, `subdev_set_edid`,
  `video_set_format`, `video_stream_on`, and `video_stream_off`;
- `recovery_started` / `recovery_backoff_completed`: recovery reason and the
  effective retry delay.

An `ioctl_started` line without its matching `ioctl_completed` line identifies
the userspace thread currently blocked in the kernel. A completion with
`errno=110` and an elapsed time near the I2C controller timeout identifies the
specific V4L2 operation that reached the failing bus. If kernel I2C errors occur
while the most recent Frame Service event is `worker_idle` with
`stream_active=0`, and there is no nearby `capture_request_received` or
`ioctl_started`, the access is not coming from Frame Service's request loop.
If the error has no matching userspace boundary at all, the remaining caller is
inside the kernel (for example an RK628 workqueue or another V4L2 consumer);
userspace logs cannot identify that call site. The next diagnostic layer is a
kernel tracepoint or a temporary `rk628`/`rk3x-i2c` log containing the
workqueue name and call-site operation.

Preserve both clocks and the effective runtime configuration when collecting an
incident:

```bash
mkdir -p /tmp/rk628-incident
pid=$(pidof frame_service | awk '{print $1}')
dmesg > /tmp/rk628-incident/dmesg.txt
cp /var/log/frame_service/frame_service.log /tmp/rk628-incident/
frame_service_cli --socket /run/frame_service/frame_service.sock health \
  > /tmp/rk628-incident/frame-health.txt 2>&1
tr '\0' ' ' < "/proc/$pid/cmdline" \
  > /tmp/rk628-incident/frame-cmdline.txt
ps -eLo pid,tid,ppid,stat,wchan:32,comm \
  > /tmp/rk628-incident/tasks.txt
for task in "/proc/$pid"/task/*; do
  {
    echo "=== $task ==="
    cat "$task/status"
    cat "$task/wchan"
    cat "$task/stack"
  } >> /tmp/rk628-incident/frame-thread-stacks.txt 2>&1
done
cp /etc/aiden_frame_service.conf /tmp/rk628-incident/
awk '
  /^\[advanced_settings\.hardware\.frame_service\]$/ { copy = 1 }
  copy && /^\[/ && $0 !~ /^\[advanced_settings\.hardware\.frame_service\]$/ { exit }
  copy { print }
' /userdata/agent/agent.toml \
  > /tmp/rk628-incident/frame-agent-config.txt 2>/dev/null || true
```

Do not run `i2cdetect`, `i2cget`, or `i2cdump` against a bus that is already
timing out. Those commands add transactions and can obscure which production
operation first encountered the fault. The `UU` marker from `i2cdetect` only
means a kernel driver owns the address; it is not an ACK test.

## Agent Web UI won't open

Check:

```bash
systemctl status aiden-agent.service --no-pager
tail -f /userdata/agent/log/agent.log
ss -lntp | grep 8080
```

Verify:

- The daemon is listening on port `8080`; both `text` and `stt` modes serve the Web UI;
- `agent.toml` TOML syntax is correct;
- Model configuration and API key are available;
- Firewall, USB network, or Wi-Fi IP is correct.

## Wi-Fi connection is unstable

Solution:

- Connect an external 2.4 GHz antenna, antenna connector uses IPEX1 generation;
- Or try switching Wi-Fi channel to avoid heavily interfered channels.

## Voice mode has no sound or cannot record audio

Check:

```bash
systemctl status aiden-audio.service --no-pager
audio_service_cli --socket /run/audio_service/audio_service.sock health
audio_service_cli --socket /run/audio_service/audio_service.sock get-volume
amixer sget 'DAC HPMIX'
amixer sget 'DAC LINEOUT'
```

Recommendations:

- First run `scripts/setup_audio_volume.sh`;
- Confirm `[voice_settings.classic.audio].socket` path matches the service;
- Use `record-stream` and `play-stream` to verify recording/playback separately;
- Check if `ffmpeg` exists when TTS fails.

## RKNN VAD inference fails

First run helper self-test directly on the board:

```bash
/usr/lib/aiden/rknn_vad --model /usr/lib/aiden/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn --weights /usr/lib/aiden/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
/usr/lib/aiden/cpu_vad --weights /usr/lib/aiden/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
```

On success, it will output `P <probability>`. Current RV1106 helper uses RKNN zero-copy IO; if it outputs `rknn_set_io_mem failed`, `rknn_run failed`, or old helper outputs `rknn_inputs_set failed`, check:

- Whether the RKNN mini runtime embedded in `/usr/lib/aiden/rknn_vad` matches the model;
- Whether `silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn` is the encoder model re-converted for RV1106 target;
- Whether input/output tensor type, size, scale, zero-point in helper logs are normal.

### Why the mini runtime is embedded

The RV1106 VAD encoder/decoder models are Rockchip **mini runtime split**
format. The official armhf/glibc 2.3.2 release only ships a **full** runtime,
which opens the NPU but rejects these models with:

```text
Verify ModelBuffer failed!
Invalid RKNN format
Import rknn model failed!
```

`/usr/lib/aiden/rknn_vad` therefore statically embeds the armhf-uclibc mini
runtime (`librknnmrt.a`, 2.3.2) instead of linking a dynamic `librknnrt.so`.
Because that archive was built against uClibc ctype tables,
`src/rknn_glibc_compat.c` provides the two data symbols it references
(`__ctype_b`, `__ctype_tolower`) from the glibc locale tables. Do not replace
this with the full runtime or a dynamic RKNN library, or the model-load failure
returns. At runtime this helper only needs `/dev/rknpu`.

## NPU or media devices are not accessible

`/dev/rknpu` and `/dev/mpi/*` are expected to be `0660 root:video`, and the
`aiden` user must be in the `video` and `audio` groups:

```bash
ls -l /dev/rknpu /dev/mpi/ 2>/dev/null
id aiden
```

- Udev rules set the NPU and media nodes to `0660 root:video`.
- `aiden-media-modules.service` loads the Rockchip media/NPU modules and
  creates the DMA heap nodes and links, including `/dev/dma_heap/system`,
  before the frame and audio services start.
- The mini RKNN runtime only needs `/dev/rknpu`; a missing
  `/dev/dma_heap/system` only affects the full runtime path.

## HID input is ineffective

Check:

```bash
ls -l /dev/hidg*
mount | grep configfs
lsmod | grep -E 'dwc2|libcomposite'
```

Try reinitializing:

```bash
systemctl restart aiden-usb-gadget.service
```

For iOS target devices, confirm AssistiveTouch is enabled.

If the host reports `unknown main item tag`, `item fetching failed`, or
`hid-generic ... error -22`, the HID report descriptor was emitted as ASCII
text instead of raw bytes. The descriptors must be written with POSIX octal
escapes so they survive `dash`; see
[USB HID and ECM](../02-architecture/boot-services.md#usb-hid-and-ecm). A bare
`error -71` (`device not accepting address`) is a lower-level USB
control-transfer failure and points at cabling, power, or the host port rather
than the descriptor.

## Cannot capture screen from iPhone 16e

Cause: iPhone 16e's USB-C port has compatibility issues.

Solution:

- Currently unable to support screen capture from iPhone 16e;
- Switch to other compatible models for testing.

## Board reboots frequently after connecting phone

Cause: Insufficient power supply.

Solution:

- Switch to a USB hub with better power supply capability;
- Or use a USB hub with external power support.

## HTTP Tool API access exception

- Set `NO_PROXY` for device private IP / USB network adapter address;
- First access `GET /api/tools` to confirm service is reachable;
- When a tool invocation fails, check `is_error` and `output` in the response
  from `POST /api/tools/{tool_name}`;
- Separate transport failure from tool failure judgement.

## A Python dependency is unavailable

The Debian image does not use opkg or a mutable `/opt` package root. For an
Agent-side Python dependency, first check storage health, then install one exact
wheel into the persistent userbase and validate the environment:

```bash
cat /run/agent/storage_level
/usr/bin/python3 -m pip install --only-binary=:all: 'packaging==24.2'
/usr/bin/python3 -m pip check
```

Packages are stored under `/userdata/agent/python` and survive reboot and A/B
updates. Do not use pip to replace firmware-provided pip, setuptools, or wheel,
and do not use `apt` to mutate the production A/B rootfs. See
[Persistent Python Packages](../04-agent/python-packages.md) for cleanup and
storage behavior.

## Docker build fails

Check:

```bash
docker buildx version
docker info
```

Recommended for Apple Silicon:

```bash
colima start --vm-type vz --vz-rosetta
./debian_build.sh
```

Confirm not using `--arch x86_64` to start Colima VM.
