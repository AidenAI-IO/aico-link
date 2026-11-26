---
sidebar_position: 1
---

# 故障排查

## `example_camera_capture` 报告 `Device or resource busy`

原因：`frame_service` 正在独占 `/dev/video0`。

解决方法：

```bash
systemctl stop aico-frame.service
example_camera_capture
systemctl start aico-frame.service
```

日常截图和帧测试请使用：

```bash
frame_service_cli screenshot --out /tmp/screenshot.bmp
```

## `screenshot` 工具失败

检查：

```bash
systemctl status aico-frame.service --no-pager
frame_service_cli --socket /run/frame_service/frame_service.sock health
ls -l /run/frame_service/frame_service.sock
```

常见原因：

- `frame_service` 未运行；
- `agent.toml` 中 `[advanced_settings.hardware.hid].frame_socket` 路径不一致；
- HDMI 输入未同步；
- RK628D 或 TC358743 HDMI 子设备状态异常。

在 Debian 固件上，可以用以下命令区分前两种情况：

```bash
systemctl status aico-frame.service --no-pager
frame_service_cli --socket /run/frame_service/frame_service.sock health
```

即使没有 HDMI 信号源，服务也会有意让套接字处于就绪状态。此时 `health` 返回
`state=RECOVERING`（或 `STARTING`），在收到帧之前无法截图；套接字不存在则说明服务或启动确实
失败。之后接入 HDMI 时，采集管理器会自动重试并转为 `RUNNING`。

## Frame Service 不断重启

查看日志：

```bash
tail -f /var/log/frame_service/frame_service.log
```

检查：

- `/usr/lib/aico/frame_service` 是否存在且可执行；
- `/dev/video0` 以及 RK628D 或 TC358743 子设备是否存在；
- EDID / HDMI 信号是否正常；
- 是否有其他进程在使用 `/dev/video0`。

查找 HDMI 桥接子设备时不要假设固定的编号。RK628D 报告为 `rk628-csi`；TC358743 报告为
`tc358743`：

```bash
for f in /sys/class/video4linux/v4l-subdev*/name; do echo "$f: $(cat "$f")"; done
```

如果没有选中匹配的 `rk628-csi` 或 `tc358743` 节点，请在 `/etc/aico_frame_service.conf` 中
将 `FRAME_SERVICE_SUBDEV=/dev/v4l-subdevX` 设为该节点，并重启 `aico-frame.service`。

## HDMI 桥接芯片已 probe 但采集无法发现它

共享设备树将 RK628 和 TC358743 描述为二选一。每块开发板上只有一个桥接芯片。厂商的 V4L2
notifier 会同时等待两个端点，直到未使用的那个被清除。如果没有完成这一步，已焊接的桥接芯片
可以 probe 成功，但不会创建它的 `/dev/v4l-subdev*` 节点，导致 Frame Service 在桥接发现阶段
停留在 `RECOVERING`。

`aico-media-modules` 会检查 CIF 媒体拓扑中是否存在从任一桥接芯片到 CSI D-PHY 的已启用链路，
然后依次写入 CIF 和 ISP 的 `clr_unready_dev` 参数。D-PHY 的 bound 回调会在 notifier 完成之前
创建这条链路，因此该检查不依赖子设备节点或实时 HDMI 信号。如果两个桥接芯片都没有链路，辅助
脚本会保留处于等待状态的端点，并继续加载独立的媒体/音频模块。在没有已绑定桥接芯片的情况下
清除端点，可能会在厂商 CIF 输入格式检查中触发 Oops。

辅助脚本会以 `[aico-media-modules]` 前缀，把待完成的初始化同时报告到系统日志和启动控制台。
它会区分“缺少已启用的桥接链路”与“拓扑查询失败（或缺少 `media-ctl`）”，并明确说明 CIF/ISP
notifier 仍处于等待状态、已跳过清理、独立模块会继续加载。查询失败时会包含媒体设备路径和诊断
输出；这并不意味着桥接芯片不存在。

使用 `media-ctl -d /dev/media0 -p` 检查拓扑图（选择驱动为 `rkcif` 的媒体设备）。如果桥接芯片
在初次运行模块辅助脚本之后才完成绑定，请停止采集并重新运行该脚本，以便它安全地补全拓扑图：

```bash
sudo systemctl stop aico-frame.service
sudo /usr/lib/aico/aico-media-modules
sudo systemctl start aico-frame.service
```

仅有 I2C driver 符号链接并不能证明绑定已完成：Linux 会在调用驱动的 probe 函数之前创建它。
不要把这个符号链接当作强制完成 notifier 的条件。

只有在 CIF 参数写入成功后才会尝试 ISP 清理。如果 CIF 参数不存在、不可写或拒绝写入，辅助脚本会
记录警告，让 ISP 保持等待状态，并继续加载独立模块。

## RK628 或 I2C 超时而 Frame Service 仍保持运行

每行原生服务日志都包含 `monotonic_ms`、`pid` 和原生 `tid`。monotonic 值可以与 `dmesg` 行首的
时间戳对比，TID 则对应 `/proc/<pid>/task/<tid>` 条目。

Frame Service 记录了从请求到驱动的完整边界：

- `capture_request_received` / `capture_request_resolved`：请求序列号、Unix 套接字对端 PID、
  UID、GID、进程名、最终状态和耗时；
- `request_queued`、`cycle_started`、`warmup_started`、`frame_read_started` 和
  `cycle_completed`：当前处于哪一代采集、工作是否进入了预热或等待帧阶段，以及每个阶段的耗时；
- `camera_source open_started`、`bridge_selected` 和 `policy_resolved`：所选的桥接芯片以及
  实际生效的 EDID/强制触发策略；
- `v4l2 ioctl_started` / `ioctl_completed`：具体的 ioctl 操作、设备、fd、返回值、errno 和
  耗时。操作包括 `subdev_query_dv_timings`、`subdev_set_dv_timings`、`subdev_set_edid`、
  `video_set_format`、`video_stream_on` 和 `video_stream_off`；
- `recovery_started` / `recovery_backoff_completed`：恢复原因和实际生效的重试延迟。

有 `ioctl_started` 行却没有对应的 `ioctl_completed` 行，说明该用户态线程当前阻塞在内核中。
`errno=110` 且耗时接近 I2C 控制器超时的完成记录，指明了是哪个 V4L2 操作碰到了故障总线。如果
内核 I2C 错误发生时，Frame Service 最近的事件是 `worker_idle` 且 `stream_active=0`，附近也
没有 `capture_request_received` 或 `ioctl_started`，那么这次访问并非来自 Frame Service 的请求
循环。如果错误完全没有对应的用户态边界，剩下的调用方就在内核内部（例如 RK628 workqueue 或
其他 V4L2 使用方）；用户态日志无法定位该调用点。下一层诊断手段是内核 tracepoint，或临时添加
包含 workqueue 名称和调用点操作的 `rk628`/`rk3x-i2c` 日志。

收集故障现场时，请同时保留两种时钟和实际生效的运行时配置：

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
cp /etc/aico_frame_service.conf /tmp/rk628-incident/
awk '
  /^\[advanced_settings\.hardware\.frame_service\]$/ { copy = 1 }
  copy && /^\[/ && $0 !~ /^\[advanced_settings\.hardware\.frame_service\]$/ { exit }
  copy { print }
' /userdata/agent/agent.toml \
  > /tmp/rk628-incident/frame-agent-config.txt 2>/dev/null || true
```

不要对已经在超时的总线运行 `i2cdetect`、`i2cget` 或 `i2cdump`。这些命令会增加总线事务，可能
掩盖是哪个生产操作最先遇到故障。`i2cdetect` 的 `UU` 标记只表示该地址被某个内核驱动占用，并不
是 ACK 测试。

## Agent Web UI 无法打开

检查：

```bash
systemctl status aico-agent.service --no-pager
tail -f /userdata/agent/log/agent.log
ss -lntp | grep 8080
```

确认：

- 守护进程正在监听 `8080` 端口；`text` 和 `stt` 两种模式都会提供 Web UI；
- `agent.toml` 的 TOML 语法正确；
- 模型配置和 API key 可用；
- 防火墙、USB 网络或 Wi-Fi IP 正确。

## Wi-Fi 连接不稳定

解决方法：

- 外接 2.4 GHz 天线，天线接口为 IPEX 一代；
- 或尝试切换 Wi-Fi 信道，避开干扰严重的信道。

## 语音模式没有声音或无法录音

检查：

```bash
systemctl status aico-audio.service --no-pager
audio_service_cli --socket /run/audio_service/audio_service.sock health
audio_service_cli --socket /run/audio_service/audio_service.sock get-volume
amixer sget 'DAC HPMIX'
amixer sget 'DAC LINEOUT'
```

建议：

- 先运行 `scripts/setup_audio_volume.sh`；
- 确认 `[voice_settings.classic.audio].socket` 路径与服务一致；
- 使用 `record-stream` 和 `play-stream` 分别验证录音和播放；
- TTS 失败时检查 `ffmpeg` 是否存在。

## RKNN VAD 推理失败

先在开发板上直接运行辅助程序自检：

```bash
/usr/lib/aico/rknn_vad --model /usr/lib/aico/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn --weights /usr/lib/aico/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
/usr/lib/aico/cpu_vad --weights /usr/lib/aico/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
/usr/lib/aico/smart_turn_helper --model /usr/lib/aico/models/smart-turn-v3.2-dyt-4s-rv1106-int8-mmse.rknn --self-test
```

成功时，辅助程序输出 `P <probability>`。当前的 RV1106 辅助程序使用 RKNN 零拷贝 IO；如果输出
`rknn_set_io_mem failed`、`rknn_run failed`，或旧版辅助程序输出 `rknn_inputs_set failed`，
请检查：

- `/usr/lib/aico/rknn_vad` 和 `/usr/lib/aico/smart_turn_helper` 中内嵌的 RKNN mini runtime 是否与模型匹配；
- `silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn` / `smart-turn-v3.2-dyt-4s-rv1106-int8-mmse.rknn` 是否为针对 RV1106 目标重新转换的模型；
- 辅助程序日志中输入/输出张量的类型、大小、scale、zero-point 是否正常。

### 为什么内嵌 mini runtime

RV1106 VAD 编码器/解码器模型是 Rockchip **mini runtime split** 格式。官方 armhf/glibc 2.3.2
版本只提供 **full** runtime，它可以打开 NPU，但会拒绝这些模型并报错：

```text
Verify ModelBuffer failed!
Invalid RKNN format
Import rknn model failed!
```

因此 `/usr/lib/aico/rknn_vad` 静态内嵌了 armhf-uclibc mini runtime（`librknnmrt.a`，2.3.2），
而不是链接动态的 `librknnrt.so`。由于该静态库是基于 uClibc ctype 表构建的，
`src/rknn_glibc_compat.c` 从 glibc locale 表提供了它引用的两个数据符号（`__ctype_b`、
`__ctype_tolower`）。不要将其替换为 full runtime 或动态 RKNN 库，否则模型加载失败的问题会
再次出现。运行时该辅助程序只需要 `/dev/rknpu`。

## NPU 或媒体设备无法访问

`/dev/rknpu` 和 `/dev/mpi/*` 应为 `0660 root:video`，且 `aico` 用户必须属于 `video` 和
`audio` 组：

```bash
ls -l /dev/rknpu /dev/mpi/ 2>/dev/null
id aico
```

- Udev 规则将 NPU 和媒体节点设为 `0660 root:video`。
- `aico-media-modules.service` 会在帧服务和音频服务启动之前加载 Rockchip 媒体/NPU 模块，并
  创建 DMA heap 节点和链接，包括 `/dev/dma_heap/system`。
- mini RKNN runtime 只需要 `/dev/rknpu`；缺少 `/dev/dma_heap/system` 只影响 full runtime 路径。

## HID 输入无效

检查：

```bash
ls -l /dev/hidg*
mount | grep configfs
lsmod | grep -E 'dwc2|libcomposite'
```

尝试重新初始化：

```bash
systemctl restart aico-usb-gadget.service
```

对于 iOS 目标设备，请确认已开启“缩放”（Zoom）并关闭“辅助触控”（AssistiveTouch）
（`Settings > Accessibility`）。其他任何组合下触控输入都会被忽略；键盘输入仍然有效，因此看起来
像是只有指针出了问题。

如果主机报告 `unknown main item tag`、`item fetching failed` 或 `hid-generic ... error -22`，
说明 HID 报告描述符是以 ASCII 文本而非原始字节写出的。描述符必须使用 POSIX 八进制转义写入，
才能在 `dash` 下正确输出；参见 [USB HID 与 ECM](../02-architecture/boot-services.md#usb-hid-与-ecm)。
单独出现 `error -71`（`device not accepting address`）属于更底层的 USB 控制传输失败，问题指向
线缆、供电或主机端口，而不是描述符。

## 无法从 iPhone 16e 采集屏幕

原因：iPhone 16e 的 USB-C 接口存在兼容性问题。

解决方法：

- 目前无法支持从 iPhone 16e 采集屏幕；
- 换用其他兼容机型进行测试。

## 连接手机后开发板频繁重启

原因：供电不足。

解决方法：

- 换用供电能力更强的 USB hub；
- 或使用支持外部供电的 USB hub。

## HTTP Tool API 访问异常

- 为设备私有 IP / USB 网卡地址设置 `NO_PROXY`；
- 先访问 `GET /api/tools` 确认服务可达；
- 工具调用失败时，检查 `POST /api/tools/{tool_name}` 响应中的 `is_error` 和 `output`；
- 将传输失败与工具失败分开判断。

## 某个 Python 依赖不可用

Debian 镜像不使用 opkg，也没有可变的 `/opt` 软件包根目录。对于 Agent 侧的 Python 依赖，先检查
存储健康状况，然后把一个精确版本的 wheel 安装到持久化的 userbase 中，并验证环境：

```bash
cat /run/agent/storage_level
/usr/bin/python3 -m pip install --only-binary=:all: 'packaging==24.2'
/usr/bin/python3 -m pip check
```

软件包存放在 `/userdata/agent/python` 下，重启和 A/B 升级后仍会保留。不要用 pip 替换固件自带的
pip、setuptools 或 wheel，也不要用 `apt` 修改生产环境的 A/B rootfs。清理和存储行为参见
[持久化 Python 软件包](../04-agent/python-packages.md)。

## Docker 构建失败

检查：

```bash
docker buildx version
docker info
```

Apple Silicon 推荐：

```bash
colima start --vm-type vz --vz-rosetta
./debian_build.sh
```

确认没有使用 `--arch x86_64` 启动 Colima 虚拟机。
