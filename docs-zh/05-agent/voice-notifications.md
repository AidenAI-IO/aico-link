---
sidebar_position: 6
---

# 语音通知

语音通知让 Agent 可以在正常的语音回复后附加简短的系统提醒，或者通过活跃的 Realtime 语音会话播报待处理的提醒。实现位于 `internal/agent/voice_notification.go`，由 Agent 运行时中的所有语音路径共享。

## 投递模式

管理器会产出以下四种播报文本模式之一：

| 模式 | 使用时机 | 效果 |
| --- | --- | --- |
| `normal` | 本轮成功，且没有符合条件的持久提醒 | 原样播报 Agent 的正常回复 |
| `tail` | 本轮成功，且有一个活跃的持久状况待播报 | 在 TTS 之前最多追加一条简短提醒 |
| `replacement` | 最终的 LLM 请求失败 | 用固定的网络、额度或服务错误消息替代缺失的回复 |
| `notification` | Realtime 空闲，且有一个活跃的持久状况待播报 | 将提醒作为私有语音回复发送给 Realtime |

回复末尾提醒只改变发送给 TTS 的文本。它不会改变 LLM 输出、助手历史、会话摘要，也不会改变 Web UI 或配套 App 显示的回复。

在 `input_mode = "realtime"` 下，待处理的通知会在 Realtime 会话空闲时被消费。Realtime 是主要的语音路径；没有活跃的 Realtime 会话时，会回退到已配置的独立 TTS 提供商。如果该提供商也不可用（这是常见情况，因为 `input_mode = "realtime"` 不要求配置 `tts.provider`），则改为播放预录的 TTS 不可用提示音，让故障仍然能被听到。该提示音只告知语音不可用，不携带通知文本，因此提醒会保持待处理状态，且该提示音在每个待机周期内最多播放一次。如果完全没有可用路径，提醒会一直保持待处理，直到过期、被解除，或之后有某条语音路径可以承载它。

如果 Realtime 提供商会话直接失败（网络中断、凭据被拒），它无法播报自身的错误，因此 Agent 会通过同一条独立路径播报本轮失败，并回退到预录提示音。本地音频、持久化以及其他守护进程故障不会被呈现为模型故障。这与 `input_mode = "stt"` 处理 replacement 模式的轮次失败的方式一致。

## 发布持久状况

后台状况生产者使用运行时共享的 `VoiceNotificationSink`：

```go
type VoiceNotificationSink interface {
    Publish(ctx context.Context, event VoiceNotificationEvent) error
}

type VoiceNotificationEvent struct {
    Code      string
    Severity  NotificationSeverity
    State     string
    DedupeKey string
    Params    map[string]string
}
```

`DedupeKey` 必须采用 `<Code>:<ScopeID>` 格式，例如 `storage:device`。严重级别不属于该键的一部分。生产者在状况存在期间发布 `active` 心跳，并在恢复时发布 `resolved`。

```go
sink := runtime.VoiceNotificationSink()
err := sink.Publish(ctx, agent.VoiceNotificationEvent{
    Code:      "storage",
    Severity:  agent.SeverityWarning,
    State:     agent.VoiceNotificationActive,
    DedupeKey: "storage:device",
})
```

每个活跃周期通过内部的周期 ID 相互隔离：

- 第一个 `active` 事件会创建一条待处理记录；
- 相同严重级别的心跳会续租，但不会重复投递；
- 严重级别升高时会重新变为待处理；
- 严重级别降低时，不会重复播报已被更高级别投递覆盖的级别；
- `resolved` 会删除活跃周期；
- 之后的 `active` 事件会开始一个新周期，并可能再次投递。

## 准备与确认播报

最终回复路径在 TTS 之前立即调用 `PrepareSpokenText`。选中尾巴时会将其标记为投递中，并返回一个投递 token。调用方必须用同一个 token 报告播放结果：

```go
prepared := runtime.PrepareSpokenText(ctx, agent.SpokenTextInput{
    ResponseText:   responseText,
    TurnFailure:    turnFailure,
    TailAppendable: tailAppendable,
})
err := dialog.SpeakFinal(ctx, prepared.Text, nil)
runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, err)
```

只有播放完成才会把提醒记为已投递。播放失败或被取消会让当前状况重新符合投递条件。投递 token 包含活跃周期和所选严重级别的快照，因此延迟到达的回调无法确认更新的周期，也无法确认播放期间发生的严重级别升级。

Realtime 通知消费者在会话空闲时调用 `PrepareNotification`。返回的文本作为私有 Realtime 对话项发送，消费者在播放完成后用同一个投递 token 报告 Realtime 回复状态。通知回复不会进入本地用户历史，也不会执行工具。用户打断或 Realtime 回复未完成时，通知保持待处理状态以便重试。

已经输出语音的流式回复不会被修改。其待处理的提醒仍保留给下一条非流式、可追加的回复。

## 本地 TTS 不可用回退

官方固件在以下位置内置了预录的中文和英文提示：

```text
/usr/share/aico/audio/voice-notifications/tts-unavailable.zh-CN.wav
/usr/share/aico/audio/voice-notifications/tts-unavailable.en-US.wav
```

当最终回复无法使用已配置的 TTS 提供商时，Agent 会绕过 TTS，直接通过 `audio_service` 播放对应的 WAV。这也覆盖了启动时 TTS 初始化失败，以及提供商完成请求却没有产出音频的情况。

该回退被刻意限制：

- 它只用于最终回复语音、独立语音通知和 Realtime 会话失败，绝不用于工具进度语音；
- 对于独立语音通知，它在每个待机周期内最多播放一次，因为提示音不携带通知文本，提醒因此仍保持待处理；
- 它只在失败的 TTS 尝试尚未开始播放任何 PCM 之前运行；
- 取消和抢占永远不会触发它；
- 播放回退提示音不会确认待处理的回复末尾提醒通知，因为原始回复和提醒都没有被播报；
- 禁用 `[memory_settings.notification]` 也会禁用该回退。

语言选择遵循设备级的 `locale`：`en-US` 使用英文文件，`zh-CN` 使用中文文件。同一 locale 还决定内置通知文本、回复末尾提醒的标点以及预录回退文件的选择。开发镜像和自定义镜像可以用 `AICO_TTS_FALLBACK_DIR` 覆盖资源目录。

## 选择规则

一条回复最多携带一条持久提醒。符合条件的记录按以下顺序排序：

1. 与当前任务的直接相关性（提供了相关代码时）；
2. 更高的严重级别；
3. 更近的严重级别变化；
4. 更长的等待时间。

内置的持久策略目前为 warning、critical 和 emergency 三个严重级别提供中文和英文的存储提示。其他场景可以按代码、严重级别和 locale 注册本地文本策略。

最终的轮次失败不会进入持久队列。它们被归类为网络不可用、token/额度不足或通用 LLM 不可用，并且只为该轮生成替代回复。

## 配置

```toml
[basic_settings.language_timezone]
locale = "en-US"

[memory_settings.notification]
enabled = true
max_pending = 8

[memory_settings.notification.response_tail]
enabled = true
max_items = 1
max_text_chars = 40

[memory_settings.notification.expiration]
default_ttl_seconds = 0

[memory_settings.notification.expiration.code_ttl_seconds]
storage = 900
```

`default_ttl_seconds` 为 `0` 时禁用自动过期。按代码设置的 TTL 会覆盖它；活跃心跳会续租。过期只是生产者失效时的兜底，不能替代显式的 `resolved` 事件。

Config Web 服务在保存其他设置时会保留这些配置段。当前页面没有提供专门的语音通知控件，因此请直接在 `agent.toml` 中编辑。

## 当前限制

- 管理器本身不检测存储状况；必须由 StorageMonitor 或其他生产者发布。
- 网络和额度故障只根据当前轮次的最终错误推断，而不是后台健康检查。
- 流式 TTS 已经产出音频之后，不会再注入尾巴。
- Realtime 通知由活跃的 Realtime 会话消费；没有活跃会话时，若已配置独立 TTS，则使用独立 TTS。
