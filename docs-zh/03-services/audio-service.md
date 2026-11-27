---
sidebar_position: 5
---

# Audio Service：音频录制与播放服务

`audio_service` 是一个常驻运行的 C++ 服务，提供录音、播放和音量控制能力。Go Agent 的设备语音管线和 `audio_volume` 工具通过 Unix 域 socket 调用该服务。

## 默认参数

| 参数 | 默认值 |
| --- | --- |
| Socket | `/run/audio_service/audio_service.sock` |
| 日志 | `/var/log/audio_service/audio_service.log` |
| 二进制文件 | `/usr/lib/aico/audio_service` |
| 音量状态文件 | `/userdata/audio_service/playback_volume` |

## 命令行参数

```text
audio_service [--socket PATH] [--volume-state PATH]
```

| 参数 | 说明 |
| --- | --- |
| `--socket PATH` | 服务通信使用的 Unix 域 socket 路径 |
| `--volume-state PATH` | 用于在重启后保留播放音量的持久化文件路径；默认为 `/userdata/audio_service/playback_volume` |

在开发过程中直接运行时，它同样默认使用 `/run/audio_service/audio_service.sock`，可以通过参数或环境变量覆盖：

```bash
AUDIO_SERVICE_SOCKET=/tmp/audio_service.sock /usr/lib/aico/audio_service
/usr/lib/aico/audio_service --socket /tmp/audio_service.sock
```

## 启动

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

示例：

```bash
audio_service_cli --socket /run/audio_service/audio_service.sock health
audio_service_cli --socket /run/audio_service/audio_service.sock get-volume
audio_service_cli --socket /run/audio_service/audio_service.sock set-volume --volume 80
```

录音：

```bash
audio_service_cli --socket /run/audio_service/audio_service.sock record-stream --seconds 3 > /tmp/record.pcm
```

播放：

```bash
cat /tmp/record.pcm | audio_service_cli --socket /run/audio_service/audio_service.sock play-stream --rate 16000 --ch 1 --bits 16
```

## Agent 配置

```toml
[voice_settings.classic.audio]
socket = "/run/audio_service/audio_service.sock"
sample_rate = 16000
channels = 1
bit_width = 16
```

在语音模式下，Agent 使用音频服务完成以下步骤：

1. 启动录音会话；
2. 读取 PCM 数据块；
3. 使用 RKNN Silero VAD 检测语音边界；
4. 需要播放时，启动播放会话并写入 PCM。

## 采集语音处理（VQE）

每个录音会话都会启用由 `/usr/share/aico/audio/config_aivqe.json`
（`overlay-debian/usr/share/aico/audio/config_aivqe.json`）配置的 Rockchip VQE 管线。
AEC 使用播放通路的数字回环作为参考信号。其他已启用的阶段包括残余回声抑制（`aes`）、
降噪（`anr`）、自动增益控制（`agc`）和去混响（`dereverb`）。

该文件中的若干设置针对板载麦克风的采集质量进行了调优：

- `howl` 已禁用。设备从不会通过扬声器实时回放麦克风声音，而在扬声器静音时，
  该阶段会引入稳态音调（约 2.4、4.0、5.6 和 7.2 kHz），并将底噪抬高约 25 dB。
- `anr.fGmin` 为 `0.2`（约 -14 dB），而不是 `0.01`。更低的下限会把背景声压成数字静音，
  并切掉较弱的音节和词尾。
- `agc.max_gain` 为 `12` dB，而不是 `25` dB，以避免放大残余噪声。

重新调优时，请对比同一段语音在扬声器静音和正在播放两种情况下的录音，
并同时检查词与词之间的背景声和轻声说话的效果。

## DTLN 降噪（默认启用）

对于 16 或 24 kHz 的单声道 PCM16 录音会话，采集管线如下：

1. 硬件采集以及现有的 Rockchip VQE/AEC 管线；
2. 转换为 16 kHz 单声道并进行直流高通滤波；
3. 混合式 DTLN 降噪（四个 RKNN 门控图，加上 CPU float32 状态和 DSP）；
4. 转换为请求的 16 或 24 kHz 采样率，并通过 socket 输出。

这适用于 Agent 录音和 `audio_service_cli record-stream`。播放和 AEC 参考通路保持不变。
其他录音格式会绕过 DTLN，并记录一条 `dtln_format_bypassed` 日志。VQE 降噪仍然启用；
VQE/DTLN 组合链路仍需在设备上进行试听和负载验证。

该模型使用 512 个采样点的块和 128 个采样点的跳步（8 ms）。其 384 个采样点的算法延迟为 24 ms，
另外还有不完整跳步的缓冲、采集缓冲和处理时间。启动阶段的填充会被丢弃，延迟的尾部会在 EOF
之前全部输出，因此短录音和词尾都能保留。每个录音会话和每次采集重启时都会重置状态。

`stop_recording` 会请求关闭采集；客户端必须持续读取直到收到 `end_of_stream`，才能关闭会话。
CLI 会将最后这段 PCM 写到 stdout，Agent 则会在完成流式 STT 之前以及在回退的 WAV/音频归档中包含它。
缺少 EOF 会被报告为排空（drain）错误，而不是静默接受被截断的录音。

业务软件包在 `/usr/lib/aico/models/dtln/` 下包含模型、浮点权重以及经过测试的私有
RKNN Mini Runtime 2.5.0，并在打包时校验它们的 SHA256。该运行时仅为 DTLN 加载，
不会替换 VAD 使用的 RKNN 运行时。参见[模型来源](../../assets/business/models/dtln/README.md)。

`/etc/aico_audio_service.conf` 中的设置（直接运行服务时也可以作为环境变量传入）：

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `AUDIO_SERVICE_DENOISE` | `1` | 设为 `0` 可绕过 DTLN |
| `AUDIO_SERVICE_DTLN_DIR` | `/usr/lib/aico/models/dtln` | 包含全部四个模型、权重和私有运行时的目录 |
| `AUDIO_SERVICE_CAPTURE_RT_PRIORITY` | `10` | 采集工作线程的 FIFO 优先级；设为 `0` 使用普通调度 |

修改配置后请重启 `aico-audio.service`。服务单元设置了 `LimitRTPRIO=10`；只有采集工作线程会请求实时调度。
如果请求失败，录音会继续进行，并记录一条 `capture_rt_unavailable` 警告。源实验在 NEON 和
FIFO 优先级 10 下达到了离线 8 ms 跳步的时间预算，但实际端到端延迟和持续并发负载仍需在设备上测量。

初始化失败时会记录 `dtln_unavailable`，并保留仅 VQE 的采集。推理失败时会记录 `dtln_failed`，
返回已缓冲的原始音频，并在该会话剩余时间内绕过 DTLN。下一个录音会话会重新尝试初始化。
初始化成功时会记录 `dtln_enabled`。

如需与仅 VQE 的采集进行对比，请设置 `AUDIO_SERVICE_DENOISE=0`，重启服务，然后重复上面的录音 CLI 示例。
在默认配置下检查服务日志中是否有 `dtln_enabled`，并在播放静音和播放进行中两种情况下试听轻声说话和词尾。
主机测试覆盖了流式处理和失败处理；它们无法验证 NPU 推理或麦克风质量。

## 音量

`audio_service` 对外提供 `0..100` 的逻辑音量。底层设备的最大音量可以通过以下命令初始化：

```bash
scripts/board/setup_audio_volume.sh
```

详情参见[音量初始化与调整](../07-operations/audio-volume.md)。
