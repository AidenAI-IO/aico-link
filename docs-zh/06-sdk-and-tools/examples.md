---
sidebar_position: 2
---

# 示例程序

CMake 会构建多个 `example_*` 可执行程序，用于验证 SDK 和设备能力。Debian 交叉构建会把它们
输出到 `output/debian-apps/apps/bin/`；诊断类示例不会复制到生产业务软件包中。以下命令假设
你已将所选二进制文件复制到开发板并放入 `PATH`。

## 唤醒

```bash
example_wakeup
```

监听 GPIO 33/GPIO 32 下降沿。典型硬件连接：按键将 GPIO 接到 GND，使用内部上拉。

## 音频采集

```bash
example_audio_capture [device_name]
```

采集音频并打印帧信息。可选传入 ALSA 设备名。

## 音频播放

```bash
example_audio_play [device_name]
```

通过指定的 ALSA 设备播放 PCM 音频文件（`audio_capture_debug.pcm`）。该 PCM 文件必须位于当前目录或二进制文件的运行路径下。

## 摄像头采集

```bash
example_camera_capture
example_camera_capture --output /mnt/tmp/frame.raw
example_camera_capture --edid /usr/share/aico/edid/hdmi_1080p30_cta.hex
```

常用参数：

```text
example_camera_capture [device_path] [width] [height] [pixfmt] [skip_frames] [options]
```

支持 `--device`、`--subdev`、`--edid`、`--force-trigger`、`--capture-retries`、`--output` 等选项。详见 `--help`。

默认单次采集流程：

- 先查询当前 DV 时序；
- 必要时推送内置的仅 1080p60 CTA EDID；
- 等待 `/dev/v4l-subdev2` HDMI 同步；
- 开启流后丢弃过渡帧；
- 通过 V4L2 MMAP 从 `/dev/video0` 采集；
- 默认保存可查看的 `/mnt/tmp/frame.ppm`。

> 如果 `frame_service` 正在运行，请先停止它，否则 `/dev/video0` 会处于占用状态。

## USB HID

```bash
sudo example_usb_hid setup composite
sudo example_usb_hid keyboard tap ENTER
sudo example_usb_hid keyboard text "hello from pico"
sudo example_usb_hid touch click 16000 16000
sudo example_usb_hid cleanup
```

更多信息参见 [USB HID 与设备控制](../04-services/usb-hid.md)。

## 其他工具

| 程序 | 说明 |
| --- | --- |
| `hello` | 基础 C 程序验证 |
| `trigger` | GPIO / 触发相关工具 |
| `audio_stream` | 音频流示例工具 |
| `agent config-web` | 配置 Web 服务 |
| `frame_service_cli` | Frame Service 调试 CLI |
| `audio_service_cli` | Audio Service 调试 CLI |
