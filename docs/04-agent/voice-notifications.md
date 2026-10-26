---
sidebar_position: 6
---

# Voice Notifications

Voice notifications let the Agent attach short system reminders to a normal spoken reply or announce a pending reminder through an active Realtime voice session. The implementation lives in `internal/agent/voice_notification.go` and is shared by every speech path through the Agent runtime.

## Delivery modes

The manager produces one of four spoken-text modes:

| Mode | When it is used | Effect |
| --- | --- | --- |
| `normal` | The turn succeeded and no persistent reminder is eligible | Speak the normal Agent reply unchanged |
| `tail` | The turn succeeded and an active persistent condition is pending | Append at most one short reminder before TTS |
| `replacement` | The final LLM request failed | Replace the missing reply with a fixed network, quota, or service-error message |
| `notification` | Realtime is idle and an active persistent condition is pending | Send the reminder to Realtime as a private speech response |

A response tail changes only the text sent to TTS. It does not change the LLM output, assistant history, session summary, or response shown by the Web UI or companion app.

In `input_mode = "realtime"`, pending notifications are consumed when the Realtime session is idle. Realtime is the primary speech path; when no Realtime session is active, a configured standalone TTS provider is used as a fallback. When that provider is also unavailable — the common case, because `input_mode = "realtime"` does not require `tts.provider` — the prerecorded TTS-unavailable clip plays instead, so the failure is still audible. The clip only announces that speech is unavailable and does not carry the notification text, so the reminder stays pending and the clip plays at most once per standby period. If no path is available at all, the reminder stays pending until it expires, resolves, or a later speech path can carry it.

A Realtime provider session that fails outright (network loss, rejected credentials) cannot voice its own error, so the Agent announces the turn failure through the same standalone path, falling back to the prerecorded clip. Local audio, persistence, and other daemon failures are not presented as model failures. This matches what `input_mode = "stt"` does with a replacement-mode turn failure.

## Publishing persistent conditions

Background condition producers use the runtime's shared `VoiceNotificationSink`:

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

`DedupeKey` must use `<Code>:<ScopeID>`, for example `storage:device`. Severity is not part of the key. A producer publishes `active` heartbeats while a condition exists and publishes `resolved` when it recovers.

```go
sink := runtime.VoiceNotificationSink()
err := sink.Publish(ctx, agent.VoiceNotificationEvent{
    Code:      "storage",
    Severity:  agent.SeverityWarning,
    State:     agent.VoiceNotificationActive,
    DedupeKey: "storage:device",
})
```

Each active cycle is isolated by an internal cycle ID:

- the first `active` event creates a pending record;
- same-severity heartbeats renew the lease without repeating delivery;
- a severity increase becomes pending again;
- a severity decrease does not repeat a level already covered by a higher delivery;
- `resolved` deletes the active cycle;
- a later `active` event starts a new cycle and may be delivered again.

## Preparing and confirming speech

Final reply paths call `PrepareSpokenText` immediately before TTS. Selecting a tail marks it in flight and returns a delivery token. The caller must report the playback result with the same token:

```go
prepared := runtime.PrepareSpokenText(ctx, agent.SpokenTextInput{
    ResponseText:   responseText,
    TurnFailure:    turnFailure,
    TailAppendable: tailAppendable,
})
err := dialog.SpeakFinal(ctx, prepared.Text, nil)
runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, err)
```

Only completed playback records a reminder as delivered. Failed or canceled playback makes the current condition eligible again. Delivery tokens include the active cycle and selected severity snapshot, so a delayed callback cannot acknowledge a newer cycle or a severity upgrade that occurred during playback.

Realtime notification consumers call `PrepareNotification` when the session is idle. The returned text is sent as a private Realtime item and the consumer reports the Realtime response status with the same delivery token after playback completes. Notification responses do not enter local user history or execute tools. A user interruption or a non-completed Realtime response leaves the notification pending for retry.

Streaming replies that have already emitted speech are not modified. Their pending reminder remains available for the next non-streamed, appendable reply.

## Local TTS-unavailable fallback

Official firmware bundles prerecorded Chinese and English messages under:

```text
/usr/share/aico/audio/voice-notifications/tts-unavailable.zh-CN.wav
/usr/share/aico/audio/voice-notifications/tts-unavailable.en-US.wav
```

When a final reply cannot use the configured TTS provider, the Agent bypasses TTS and plays the matching WAV directly through `audio_service`. This also covers startup-time TTS initialization failure and providers that complete without producing audio.

The fallback is deliberately limited:

- it runs for final reply speech, standalone voice notifications, and Realtime session failures — never for tool-progress speech;
- for standalone voice notifications it plays at most once per standby period, because the clip does not carry the notification text and therefore leaves the reminder pending;
- it runs only before any PCM from the failed TTS attempt has started playing;
- cancellation and preemption never trigger it;
- playing the fallback does not acknowledge a pending response-tail notification, because the original reply and reminder were not spoken;
- disabling `[memory_settings.notification]` also disables this fallback.

Locale selection follows the device-level `locale`: `en-US` uses the English file and `zh-CN` uses the Chinese file. The same locale drives built-in notification text, response-tail punctuation, and prerecorded fallback selection. Development and custom images may override the asset directory with `AICO_TTS_FALLBACK_DIR`.

## Selection rules

One reply carries at most one persistent reminder. Eligible records are ordered by:

1. direct relevance to the current task, when related codes are supplied;
2. higher severity;
3. more recent severity change;
4. longer wait time.

The built-in persistent policy currently provides Chinese and English storage messages for warning, critical, and emergency severity. Additional scenarios can register local text policies by code, severity, and locale.

Final turn failures do not enter the persistent queue. They are classified as network unavailable, token/quota insufficient, or generic LLM unavailable and produce a response replacement for that turn only.

## Configuration

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

`0` for `default_ttl_seconds` disables automatic expiration. Per-code TTL values override it; active heartbeats renew the lease. Expiration is only a producer-failure fallback and does not replace an explicit `resolved` event.

The Config Web service preserves these sections when saving other settings. The current page does not render dedicated voice-notification controls, so edit them directly in `agent.toml`.

## Current limitations

- The manager does not detect storage conditions; StorageMonitor or another producer must publish them.
- Network and quota failures are derived only from the current turn's final error, not background health checks.
- A tail is not injected after streaming TTS has already produced audio.
- Realtime notifications are consumed by an active Realtime session; when no session is active, standalone TTS is used when configured.
