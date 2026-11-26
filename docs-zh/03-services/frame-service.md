---
sidebar_position: 4
---

# Frame Service：HDMI 帧采集服务

`frame_service` 是一个常驻运行的 C++ 服务，独占 `/dev/video0`，并通过 Unix 域 socket 提供最新截图。它会保持设备和 V4L2 MMAP 缓冲区处于打开状态。默认情况下，它在两次请求之间暂停数据流，而不是持续出队帧；可以通过 `agent.toml` 中的一项设置让 STREAMON 一直保持激活，以便进行延迟和功耗测量。

## 为什么需要 Frame Service

允许多个进程直接打开 `/dev/video0` 可能导致资源冲突和状态不稳定。`frame_service` 对视频设备进行集中管理：

- 在启动时完成 HDMI 同步 / EDID / DV timings / VI 初始化；
- 空闲时调用 `VIDIOC_STREAMOFF`，每次帧请求时重新入队缓冲区并调用 `VIDIOC_STREAMON`；
- 原始帧字节只在当前请求的生命周期内保留，而不维护生产环境的环形缓冲区；
- 通过 socket 向消费者（Agent、CLI、HTTP 示例等）提供帧；
- 支持在出错时重启采集管理器。

## 默认参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| Socket（开发时直接运行） | `/tmp/frame_service.sock` | `frame_service_main.cpp` 中的默认值 |
| Socket（Debian 服务） | `/run/frame_service/frame_service.sock` | systemd 配置中的默认值 |
| EDID | 按桥接芯片区分 | RK628D 保留驱动提供的 1080p60 EDID；TC358743 加载 `/usr/share/aico/edid/hdmi_1080p30_cta.hex` |
| 采集模式 | `on_demand` | 每个 `latest_frame` / 截图请求进行一次全新采集 |
| 预热帧数 | 按模式区分 | 持续 STREAMON 时为 `6`；每次请求重启数据流时为 `0` |
| 生产环境环形缓冲区占用 | `0` | 健康检查报告 `ring_buffer_size=0`、`ring_buffer_used=0` |
| 像素格式 | `auto` | RK628 使用 NV16 采集并输出 NV12。TC358743 和未识别的桥接芯片使用原生 NV12。 |
| 截图最大边长 | `960` | 与 Go 截图工具的默认压缩策略相关 |

## 启动

开发模式：

```bash
/usr/lib/aico/frame_service --socket /tmp/frame_service.sock
```

服务只初始化一次 HDMI/V4L2，然后暂停数据流并等待采集请求。

Debian 服务：

```bash
systemctl start aico-frame.service
```

Debian 服务使用相同的按需采集生命周期。`/etc/aico_frame_service.conf` 中的
`FRAME_SERVICE_PIXEL_FORMAT` 可取 `auto`、`nv16-to-nv12`、`nv12`、`nv16`、`uyvy` 或 `yuyv`，
默认为 `auto`。`FRAME_SERVICE_FPS` 为了配置兼容仍被接受，但在按需模式下会被忽略。

## 参数

```text
frame_service [--socket PATH] [--device PATH] [--width N] [--height N]
              [--pixel-format FMT] [--subdev PATH|--auto-subdev] [--edid PATH]
              [--ring-size N] [--fps N] [--no-hdmi-sync]
              [--force-trigger|--no-force-trigger]
              [--warmup-frames N]
              [--keep-streamon|--pause-between-captures]
              [--allow-uniform-frames|--reject-uniform-frames]
              [--require-exact-resolution|--allow-resolution-mismatch]
```

| 参数 | 说明 |
| --- | --- |
| `--socket PATH` | UDS socket 路径 |
| `--device PATH` | V4L2 采集设备，默认为 `/dev/video0` |
| `--width N` / `--height N` | 要求的 HDMI 分辨率，默认为 1920x1080 |
| `--pixel-format FMT` | `auto`（默认）、`nv16-to-nv12`、`nv12`、`nv16`、`uyvy` 或 `yuyv` |
| `--subdev PATH` | 显式指定 HDMI 桥接子设备。Debian 服务通常使用 `--auto-subdev`，因为桥接芯片可能在启动后才出现，且其 `/dev/v4l-subdevX` 编号可能变化。 |
| `--auto-subdev` | 每次恢复采集时重新发现 `rk628-csi` 或 `tc358743`。这是 Debian 服务所用的模式，可在没有 HDMI 时保持 IPC 端点在线。 |
| `--edid PATH` | 自定义 EDID hex。Debian 启动辅助脚本会自动为 TC358743 选择 1080p30 CTA EDID，并让 RK628D 使用其驱动自带的 1080p60 EDID；显式指定路径会覆盖该策略 |
| `--force-trigger` / `--no-force-trigger` | 启用或禁用启动时一次性的 EDID/HPD 重新协商。Debian 启动辅助脚本默认使用按桥接芯片区分的 `auto`：RK628D 禁用，TC358743 启用。在 TC358743 上开始采集之前，辅助脚本还会将 HPD 拉低 2 秒，并留出 5 秒让 HDMI 信号源稳定在 1080p30 |
| `--ring-size N` | 已弃用的兼容选项；生产环境的按需采集会忽略它 |
| `--fps N` | 已弃用的兼容选项；生产环境的按需采集会忽略它 |
| `--warmup-frames N` | 覆盖在复制响应之前出队/释放的帧数。未覆盖时，`--keep-streamon` 下默认为 `6`，`--pause-between-captures` 下默认为 `0` |
| `--keep-streamon` / `--pause-between-captures` | 在两次请求之间保持采集 STREAMON，或恢复默认的每次请求 STREAMON/STREAMOFF 生命周期 |
| `--allow-uniform-frames` | 预热后接受全黑或纯色帧；这是默认行为，因为它们是有效的截图 |
| `--reject-uniform-frames` | 预热后拒绝所有像素相同的紧凑 UYVY/YUYV 帧，用于诊断 |
| `--no-hdmi-sync` | 跳过 HDMI 同步辅助流程 |
| `--require-exact-resolution` / `--allow-resolution-mismatch` | 要求或放宽与 `--width/--height` 的匹配；默认启用精确匹配 |

也可以使用环境变量：

```bash
export FRAME_SERVICE_SOCKET=/tmp/frame_service.sock
```

Debian 启动辅助脚本默认将 `FRAME_SERVICE_SUBDEV` 留空，并自动发现 sysfs 名称中包含
`rk628-csi` 或 `tc358743` 的子设备。服务会在采集初始化之前创建其 Unix socket。
如果桥接芯片、视频节点、HDMI 信号、EDID 或 DV timings 不可用，采集管理器会保持在
`RECOVERING` 状态，并以有上限的退避间隔重试；socket 和健康检查端点仍然可用。
一旦连接了信号源，管理器会在无需 systemd 重启的情况下转入 `RUNNING`。`RUNNING`
在信号源打开后立即发布，而不是在提供第一帧之后：按需客户端会以该状态作为发出第一个
`latest_frame` 的前提，因此如果服务要等到采集一帧之后才推进状态，就永远等不到请求。
只有在自动发现不适用时，才在 `/etc/aico_frame_service.conf` 中设置显式路径。

在 `auto` 模式下，frame_service 在识别 HDMI 桥接芯片后，每次打开采集时选择格式。
TC358743 和未识别的桥接芯片保留原生 NV12 采集。在 RK628 上，服务向 V4L2 请求单平面 NV16，
并通过 IPC 输出紧凑的 NV12。这样可以避免在 RV1106 上持续使用原生 NV12 采集时间歇出现的垂直色度错位。
采集复制过程会去除行填充，并直接从映射的 NV16 帧中隔行采样交织的 UV 行。负载大小为
`width * height * 3 / 2` 字节。显式指定 `nv16` 会请求原生 NV16 输出，`nv12` 会请求原生 NV12 采集。

对于紧凑 NV12，JPEG 请求默认使用 RV1106 Rockit MPI VENC 硬件编码器。服务会按厂商计算的水平和垂直对齐方式
将帧复制到 MMZ 缓冲区，同步缓存，并在请求之间保留编码通道。JPEG 请求共享编码器锁（包括后台预热期间），
因此并发客户端仍会使用硬件编码。尺寸变化会重新创建通道。在 `/etc/aico_frame_service.conf` 中设置
`FRAME_SERVICE_JPEG_ENCODER=software` 可禁用硬件编码。编码器不可用和临时失败时会保留软件回退，
并带有重试冷却时间。可选地将 `FRAME_SERVICE_VENC_CHANNEL` 设置为 0 到 63 之间的首选通道；
如果通道冲突，服务会自动尝试其他通道。

硬件编码需要匹配的 Debian 媒体内核模块。厂商的汇编代码假定内核 `struct page` 对象为 32 字节，
而启用了 `CONFIG_MEMCG` 的 Debian 内核使用 36 字节。Debian BSP 构建会暂存并重新构建
`mpp_vcodec.ko` 和 `rockit.ko`，加入针对所配置内核头文件编译的转换代码；同时还禁用了未使用的厂商内部 MCU 加载器。
服务在使用 VENC 之前会检查两个模块的只读参数 `aico_page_size`。在旧版 BSP 上只升级应用时，会记录
`venc_kernel_abi_unsupported`，并在平台模块更新之前使用软件编码。这样可以避免之前出现的
`jpege_seek_bits` 内核故障以及随后的 `RK_ERR_VENC_BUF_EMPTY` 超时。

要验证一块开发板，请在设备上运行 `scripts/board_jpeg_smoke.py`。它会测试冷启动的 JPEG 请求、质量变化、
完整帧和裁剪帧（包括需要行填充的宽度）、反复的通道尺寸变化，以及并发的原始帧/JPEG 客户端。
将保存的 JPEG 样本复制到主机上进行完整解码和图像检查。确认存在 `venc_encoded` 记录且没有
`software_fallback`，检查 `/proc/vcodec/enc/venc_info` 中已完成的硬件数据包，并检查内核日志中是否有故障。
仅凭 JPEG 输出有效并不能证明是硬件在执行编码。


## 按需采集生命周期

在默认的 `keep_streamon = false` 下，服务启动和每次请求遵循以下生命周期。

服务启动时：

1. 照常执行 HDMI timing/EDID 同步。
2. 打开 `/dev/video0`，申请并映射 V4L2 缓冲区。
3. 服务立即调用 `VIDIOC_STREAMOFF`，并在不出队任何帧的情况下等待。

对于每个 `latest_frame` 请求：

1. 将已映射的缓冲区入队，并通过 `VIDIOC_STREAMON` 重新开始采集。
2. 摄像头层丢弃其初始的过渡帧。在默认的 pause-between-captures 模式下不会额外丢弃帧。
3. 复制一帧，按需裁剪/编码，然后发送给请求方。默认接受内容均匀的帧，因此黑屏或纯色页面不会被报告为采集失败。
4. 服务再次调用 `VIDIOC_STREAMOFF`。

这样可以避免长时间空闲后拿到旧的已完成缓冲区，并消除之前的排空循环所造成的持续 VI/DDR 流量。
请求是串行处理的，因此多个客户端不会争抢 V4L2 队列。

Debian 启动辅助脚本从 `/userdata/agent/agent.toml` 读取持久化设置：

```toml
[advanced_settings.hardware.frame_service]
keep_streamon = false
```

设为 `true` 时，数据流在初始化后保持激活，请求会跳过 STREAMOFF/STREAMON 这一对操作。
每个请求在响应之前会出队六帧：其中四帧用于清空在 RK628D 上观察到的已完成 V4L2 MMAP 队列，
另外两帧作为余量。在 Config Web 中保存修改后的值会重启 Frame Service，使新策略立即生效。
显式的 `--warmup-frames` 值会覆盖这一按模式区分的默认值，用于诊断。

## 资源与功耗边界

按需模式去掉了保留的生产环境帧副本以及持续的帧出队/复制工作。它有意保留视频 fd 和四个驱动 MMAP 缓冲区的分配，
这样每次截图时无需对 HDMI 进行完整的 EDID/timing 重新初始化。HDMI 桥接芯片本身也保持配置状态。因此：

- 用户态环形缓冲区内存和持续的内存带宽占用应当下降；
- 截图延迟会因数据流重启、按模式区分的预热策略以及所请求的裁剪/编码工作而增加；
- `keep_streamon = true` 去掉了每次请求重启数据流的延迟，但空闲时采集硬件仍保持激活；
- 整板功耗的节省取决于当前使用的 RK628D/TC358743 和 VI 驱动在 `VIDIOC_STREAMOFF` 之后是否关闭时钟，必须在硬件上实测。

## CLI

```bash
frame_service_cli [--socket PATH] <health|latest-frame|screenshot|list-frames|restart> [--out PATH]
```

示例：

```bash
frame_service_cli --socket /tmp/frame_service.sock health
frame_service_cli --socket /tmp/frame_service.sock screenshot --out /tmp/screenshot.bmp
frame_service_cli --socket /tmp/frame_service.sock latest-frame --out /tmp/frame.raw
frame_service_cli --socket /tmp/frame_service.sock list-frames
frame_service_cli --socket /tmp/frame_service.sock restart
```

在按需模式下，`list-frames` 返回空列表。`get_frame` 为了兼容仍保留在线路协议中，但会返回
`FRAME_NOT_FOUND`，因为原始帧在其请求连接结束后就会被释放。

## 与 Agent 的关系

Go Agent 的 `screenshot` 工具通过 `FrameServiceClient` 访问 `frame_service`。配置：

```toml
[advanced_settings.hardware.hid]
frame_socket = "/run/frame_service/frame_service.sock"

[advanced_settings.hardware.frame_service]
keep_streamon = false
```

当健康检查报告 `capture_mode=on_demand` 时，Agent 会将较旧的 `frame_age_ms` 视为正常的空闲状态，
并发出 `latest_frame` 以触发一次全新采集。缓冲模式下的过期帧检查保持不变。

理解截图内容需要模型/提供商支持图像输入。纯文本模型可以调用 `screenshot`，但无法理解像素内容。

## 与直接使用摄像头的示例冲突

`frame_service` 运行时独占 `/dev/video0`。如需运行 `example_camera_capture`：

```bash
systemctl stop aico-frame.service
example_camera_capture
systemctl start aico-frame.service
```
