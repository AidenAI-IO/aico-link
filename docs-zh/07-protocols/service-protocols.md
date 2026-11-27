---
sidebar_position: 2
---

# Frame / Audio Service 协议参考

本页概述基于 UDS 信封的应用层协议。通用信封参见 [Unix 域套接字协议](uds-protocol.md)。

## Frame Service

默认套接字：

- 开发时直接运行：`/tmp/frame_service.sock`
- Debian 服务：`/run/frame_service/frame_service.sock`

### 操作

| op / command | 说明 |
| --- | --- |
| `health` | 返回服务健康状态、采集模式、最新请求序列号、错误信息、平均延迟等 |
| `latest_frame` | 启动采集，返回一帧新画面，然后再次暂停采集 |
| `get_frame` | 兼容性操作；由于按需采集的帧不会保留，返回 `FRAME_NOT_FOUND` |
| `list_frames` | 兼容性操作；由于生产环境没有帧历史，返回空列表 |
| `restart` | 请求服务重启采集管理器 |

`latest_frame` 接受 `format: "jpeg"` 或 `format: "raw"`、可选的布尔值 `crop_black`、可选的旧版
`minimal_width`，以及可选且成对出现的 `screen_width` / `screen_height` 字段。当 `crop_black`
为 true 且提供了当前屏幕尺寸时，服务会按该宽高比计算最大的居中裁剪区域：宽或高之一保持与源帧
相同。没有屏幕尺寸时，JPEG 和 raw 响应都会检测水平和垂直方向上居中的黑边，并且最多只裁剪一个
方向。`minimal_width` 保留旧版仅水平方向的居中裁剪。raw 裁剪会保留源像素格式，并将边界对齐到
完整的色度采样。省略 `crop_black` 或为 false 时不做裁剪。响应中包含 `source_width`、
`source_height` 以及 `crop_*` 矩形，用于坐标映射。

生产服务在 `health` 中报告 `capture_mode: "on_demand"` 以及 `ring_buffer_size: 0`、
`ring_buffer_used: 0`。为了协议兼容，`since_seq` 仍被接受，但每个成功的 `latest_frame`
请求都会分配一个新的序列号并触发一次新的采集。并发的采集请求会被串行化。

### FrameMetadata

核心字段：

| 字段 | 说明 |
| --- | --- |
| `seq` | 帧序列号 |
| `capture_ts_ns` | 采集时间戳（纳秒） |
| `width` / `height` | 分辨率 |
| `pixel_format` | 像素格式，如 `nv12`（frame_service 默认）或 `uyvy` |
| `stride` | 行跨度 |
| `bytes` | 数据字节数 |
| `planes` | 多平面元数据：offset / stride / bytes |
| `stale` | 帧是否已过期 |

CLI 会原样写出 `latest-frame` 的数据；`screenshot` 会先将帧编码为 BMP 再写出。

## Audio Service

默认套接字：`/run/audio_service/audio_service.sock`

### AudioFormat

```json
{
  "sample_rate": 16000,
  "channels": 1,
  "bit_width": 16
}
```

### 操作

| op / CLI command | 说明 |
| --- | --- |
| `health` | 返回录音/播放会话状态 |
| `start_recording` | 创建录音会话，返回 `session_id` |
| `read_record_chunk` | 长轮询读取 PCM 数据块 |
| `stop_recording` | 停止录音会话 |
| `start_playback` | 创建播放会话，返回 `session_id` |
| `write_play_chunk` | 写入 PCM 数据；可标记流结束 |
| `stop_playback` | 停止播放会话 |
| `get_playback_volume` | 获取逻辑音量 `0..100` |
| `set_playback_volume` | 设置逻辑音量 `0..100` |

### 响应状态

Audio Service 使用共享的 `AicoServiceStatus`。录音长轮询超时时可能返回 `TIMEOUT`；会话不存在时返回 `SESSION_NOT_FOUND`。

## 版本兼容建议

- 新增字段时保持 JSON 向后兼容；
- payload 保持为原始二进制数据，不要转换为 base64；
- 客户端应忽略未知字段；
- 服务端出错时也应返回有效的信封和 `status`；只有套接字连接/读写失败才视为传输失败。
