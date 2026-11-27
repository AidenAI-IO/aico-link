---
sidebar_position: 1
---

# Agent 概览

Aico 是运行在 Aico Link 硬件上的 AI Agent。它的 Go 实现位于 `src/agent/`，基于 `github.com/tmc/langchaingo` 构建。它既是一个长期运行的守护进程，也是设备端的工具控制平面。

## 可执行文件

| 入口  | 说明                                                     |
| ------------ | --------------------------------------------------------------- |
| `cmd/daemon` | 长期运行的守护进程，提供 Web UI 和可选的设备端语音循环 |

交叉编译后，守护进程的可执行文件为：

```text
output/debian-apps/apps/bin/agent
```

在固件中，它默认安装到：

```text
/usr/lib/aico/agent
```

## 当前能力

- 已注册的模型提供商：`openai`、`anthropic`、`openrouter`、`kimi`、`kimi-cn`、`volcengine`、`deepseek`、`gemini`、`ollama`
- 内置工具调用：HID、截图、音量、shell
- 供 Web UI、外部 Agent 或手动调用的 HTTP 工具 API
- 从 `SKILL.md` 自动发现并在运行时激活 Skill
- 单 Agent 执行循环，精简了工具调用和响应处理
- 对话记忆持久化、会话记忆压缩；参见[会话记忆压缩](session-memory.md)
- 设备记忆和任务 Episode 记忆；参见[记忆平面](memory-plane.md)
- Web UI：聊天历史、浏览器录音、附件、检查器侧边栏（已连接的设备和屏幕、硬件资源、后台任务、共享文件）、Skill 导出
- iOS 实时活动 / 灵动岛任务状态；参见[实时活动 / 灵动岛](live-activity.md)
- 设备端语音流水线：VAD / STT / TTS；[实时语音](realtime-voice.md)

### Web UI 硬件资源

页面可见时，侧边栏每秒刷新一次硬件资源：Wi-Fi 接口 IP 地址、RX/TX 速率和累计字节计数、电池电量百分比和电压、CPU 和 NPU 利用率、内存和 CMA 使用量，以及根文件系统和已挂载本地块设备（包括 userdata 和已挂载的 SD 卡）的磁盘使用量。电池、CPU、NPU、内存、CMA 和磁盘使用量以紧凑的仪表条显示。
电压显示在电池电量旁边；完整的网络地址、流量总计和精确的存储大小可在 **Details** 中查看。同一块设备的多个 bind mount 共用一个磁盘条目。终端通过 `/webtty/` 在单独的标签页中打开。

`GET /api/resources` 返回一个只读、不可缓存的快照。Linux procfs 和 sysfs 提供 CPU 计数器、内存和 Wi-Fi 统计；Rockchip 的 `/sys/kernel/debug/rknpu/load`（或 `/proc/rknpu/load`）提供 NPU 利用率。内存使用量不含可用内存，磁盘使用量不含空闲块，NPU 利用率是所有上报核心的平均值。CMA 使用量是从预留 CMA 池中分配的连续内存（用于摄像头、NPU 及其他 DMA 缓冲区）；它取自 `/proc/meminfo` 中的 `CmaAllocated`，因为 RV1106 内核让 CMA 保持非活动状态（`CONFIG_CMA_INACTIVE`），导致 `CmaFree` 始终为零。没有 `CmaAllocated` 的内核会回退为 `CmaTotal - CmaFree`。电池读数来自 MCU 服务，并带有超时上限。缺失或无法读取的指标显示为 `Unavailable`；CPU 和网络速率需要两次采样。请求不会重叠，页面隐藏时暂停轮询，请求失败会清除过期读数并重试。
资源快照可以通过与 Web UI 相同的连接获取；现有的 MCU 控制端点仍仅限 USB 和回环访问。

在硬件资源旁边，检查器列出了 **Core services**（Agent、Frame、Audio、BLE、Config Web、MCU、Wi-Fi Proxy、Terminal）。该面板在可见时每五秒轮询一次 `GET /api/diagnostics/services`，显示每个单元的严重程度、CPU/内存采样和重启次数。故障历史来自 `GET /api/diagnostics/incidents`，其数据源为 `/userdata/diagnostics/service-events.jsonl`，该文件在核心单元的 `OnFailure=` 触发时由 `aico-service-incident` 写入。支持日志导出会包含该事件文件以及各单元的 `last/` 日志尾部。

## 运行模式

HTTP 服务器和 Web UI 在所有输入模式下都会启动。`input_mode` 控制守护进程是否同时运行设备端语音循环：

| 模式   | 行为                                                                    |
| ------ | --------------------------------------------------------------------------- |
| `text` | 仅 HTTP 服务器和 Web UI                                                  |
| `stt`  | HTTP 服务器和 Web UI，外加设备录音 → VAD → STT → LLM → TTS 循环    |
| `realtime` | HTTP 服务器和 Web UI，外加所配置的实时语音模型会话 |

在 `stt` 模式下，Web UI 请求和设备语音交互共享同一个 Agent 运行时，并在同一个守护进程中运行。

Web UI 会逐字显示流式输出的回答和推理文本，遇到大段突发内容时会加快播放。它在内存中保留完整的已接收文本，并等待播放结束后再与已完成的历史记录对齐。伴随工具调用的文本仍作为普通的助手消息显示在工具详情之前，历史记录重新加载后也是如此。在工具边界处它保留待处理动画；显式的流重置会丢弃当前草稿。
加载历史记录、后台标签页以及开启了减少动态效果偏好时，文本会直接显示而不带动画。

## 拟议的架构工作

[流式优先 Agent 重构计划](streaming-first-refactor-plan.md)对比了当前的语音 Agent 框架与 Aico 的现有实现，并提出在现有 Go/C++ 运行时内，分阶段迁移 LLM、STT、TTS、实时、播放和客户端流式处理。拟议的语音输入源仅限开发板麦克风；不包括浏览器和手机的实时语音。
这是一份研究/设计提案，并非对已发布行为的描述。

## 启动

本地开发：

```bash
cd src/agent
go run ./cmd/daemon -dir ./config -addr :8080
```

设备服务：

```bash
/usr/lib/aico/aico-managed-env-run /usr/lib/aico/agent -dir /userdata/agent -addr :8080
```

## 内置工具

对话 Agent 会获得记忆、设备操作、Skill 管理、shell 和网络调研以及手机数据所需的所有已注册且当前可用的工具。

`current_time` 和 `calculator` 不是已注册的内置工具；Agent 使用 `shell` 获取控制器本地的精确时间、时区以及进行确定性计算。`skill_manage` 和 `skill_mark_used` 仍无法通过 HTTP 工具 API 使用。

关于工具详情和 HTTP 调用方法，参见 [Tools HTTP API](tools-http-api.md)。
关于通过 MCP Streamable HTTP（`POST /mcp`）使用相同的 Agent 工具，参见 [MCP](mcp.md)。

## Python 软件包环境

固件提供的 pip、Agent 安装到 `/userdata` 下的持久化软件包、基于 shell 的直接安装，以及有限的 StorageMonitor 清理，均记录在[持久化 Python 软件包环境](python-packages.md)中。该实现复用现有的 `shell` 工具，没有新增内置的软件包安装工具。
