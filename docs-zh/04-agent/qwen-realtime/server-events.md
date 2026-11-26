Qwen-Audio Realtime API 的服务端事件参考。所有服务端事件都包含公共字段 `event_id`（由服务端自动生成）和 `type`（事件类型）。

**用户指南**：[实时语音对话（Qwen-Audio-Realtime）](https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime)。事件交互时序请参见 [WebSocket API](https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime-websocket-api)。

## **error**

**说明**：请求出错或服务故障时返回。客户端错误（`invalid_request_error`）不会中断连接；服务端错误（`server_error`）会终止连接。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "error", "error": { "type": "invalid_request_error", "code": "invalid_value", "message": "Cannot create response while another response is in progress.", "param": "response.create" } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `error`。 |
| **error** `*object*` 错误详情。 **属性** **type** `*string*` 错误类型，例如 `invalid_request_error`（客户端错误）或 `server_error`（服务端错误）。 **code** `*string*` 错误码。 **message** `*string*` 错误信息。 **param** `*string*` 与错误相关的参数。 |

## **session.created**

**说明**：连接建立后服务端发送的第一个事件，包含默认会话配置。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_KiKZC2zrhNsKFPZ5cTpyA", "type": "session.created", "session": { "object": "realtime.session", "model": "qwen-audio-3.0-realtime-plus", "modalities": ["text", "audio"], "voice": "longanqian", "input_audio_transcription": { "model": "fun-asr" }, "turn_detection": { "type": "server_vad", "threshold": 0.5, "silence_duration_ms": 800 }, "id": "sess_A1LbG2D63WELBSawRbpq8" } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `session.created`。 |
| **session** `*object*` 会话配置。 **属性** **object** `*string*` 固定为 `realtime.session`。 **model** `*string*` 模型名称。 **modalities** `*array*` 模型输出模态。 **voice** `*string*` 模型生成音频所用的音色：系统音色名称，或复刻音色的 `voice_id`。 **input\\_audio\\_transcription** `*object*` 音频转写配置。 **属性** **model** `*string*` 音频转写模型，例如 `fun-asr`。 **turn\\_detection** `*object*` 轮次检测（VAD）配置。 **id** `*string*` 会话的唯一标识。 |

## **session.updated**

**说明**：`session.update` 请求处理成功后返回，包含更新后的完整会话配置。失败时改为返回 `error` 事件。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_FMG6kiHbILCGiqXFPA98e", "type": "session.updated", "session": { "id": "sess_A1LbG2D63WELBSawRbpq8", "object": "realtime.session", "model": "pre-fun-realtime-audiochat-beta-3", "modalities": ["text", "audio"], "voice": "longanqian", "input_audio_transcription": { "model": "fun-asr" }, "turn_detection": { "type": "smart_turn", "threshold": 0.1, "silence_duration_ms": 900 } } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `session.updated`。 |
| **session** `*object*` 更新后的完整会话配置。结构与 `session.created` 中的 session 对象相同。 |

## **input\_audio\_buffer.speech\_started**

**说明**：VAD 检测到语音开始（server\_vad / smart\_turn 模式）。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "input_audio_buffer.speech_started", "audio_start_ms": 1200, "item_id": "item_xxx" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `input_audio_buffer.speech_started`。 |
| **audio\\_start\\_ms** `*integer*` 语音开始时间戳，单位为毫秒。 |
| **item\\_id** `*string*` 该语音片段提交后将要创建的对话项 ID。 |

## **input\_audio\_buffer.speech\_stopped**

**说明**：VAD 检测到语音结束（server\_vad / smart\_turn 模式）。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "input_audio_buffer.speech_stopped", "audio_end_ms": 3400, "item_id": "item_xxx", "reason": "turn_invalid" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `input_audio_buffer.speech_stopped`。 |
| **audio\\_end\\_ms** `*integer*` 语音结束时间戳，单位为毫秒。 |
| **item\\_id** `*string*` 将要创建的用户消息对话项 ID。 |
| **reason** `*string*` 仅在 smart\\_turn 模式下返回。值为 `turn_invalid` 表示当前轮次被判定为无效（没有语义内容），不会触发推理。有效轮次不返回此字段。 |

## **input\_audio\_buffer.committed**

**说明**：音频缓冲区已提交为一条用户消息（push-to-talk 手动提交或 VAD 自动提交）。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "input_audio_buffer.committed", "previous_item_id": "item_xxx", "item_id": "item_xxx" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `input_audio_buffer.committed`。 |
| **previous\\_item\\_id** `*string*` 前一个对话项的 ID。 |
| **item\\_id** `*string*` 已创建的用户消息对话项 ID。 |

## **input\_audio\_buffer.cleared**

**说明**：音频缓冲区已清空（仅 push-to-talk 模式）。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "input_audio_buffer.cleared" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `input_audio_buffer.cleared`。 |

## **conversation.item.created**

**说明**：新对话项创建成功。在用户音频提交、客户端手动创建对话项或助手回复开始时触发。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "conversation.item.created", "previous_item_id": "item_xxx", "item": { "id": "item_xxx", "object": "realtime.item", "type": "message", "status": "in_progress", "role": "assistant", "content": [] } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `conversation.item.created`。 |
| **previous\\_item\\_id** `*string*` 前一个对话项的 ID。 |
| **item** `*object*` 已创建的对话项。 **属性** **id** `*string*` 对话项的唯一标识。 **object** `*string*` 固定为 `realtime.item`。 **type** `*string*` 对话项类型：`message`（普通消息）或 `function_call`（函数调用）。 **status** `*string*` 对话项状态，例如 `in_progress` 或 `completed`。 **role** `*string*` 消息角色，例如 `user` 或 `assistant`。仅 `message` 对话项包含。 **content** `*array*` 消息内容元素列表。仅 `message` 对话项包含。 |

## **conversation.item.deleted**

**说明**：对话项已删除。客户端发送 `conversation.item.delete` 后返回此确认事件。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "conversation.item.deleted", "item_id": "item_xxx" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `conversation.item.deleted`。 |
| **item\\_id** `*string*` 已删除的对话项 ID。 |

## **conversation.item.retrieved**

**说明**：对话项获取成功。客户端发送 `conversation.item.retrieve` 后返回。音频内容只包含转写文本，不包含原始音频数据。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "conversation.item.retrieved", "item": { "id": "item_xxx", "object": "realtime.item", "type": "message", "role": "user", "content": [ { "type": "input_audio", "transcript": "你好" } ] } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `conversation.item.retrieved`。 |
| **item** `*object*` 获取到的完整对话项。 **属性** **id** `*string*` 对话项的唯一标识。 **object** `*string*` 固定为 `realtime.item`。 **type** `*string*` 对话项类型：`message`（普通消息）或 `function_call`（函数调用）。 **role** `*string*` 消息角色，例如 `user` 或 `assistant`。仅 `message` 对话项包含。 **content** `*array*` 消息内容元素列表。音频内容只包含转写文本，不包含原始音频数据。 |

## **conversation.item.input\_audio\_transcription.delta**

**说明**：语音识别过程中流式返回的 ASR 增量转写结果，包含情感和语种检测信息。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "conversation.item.input_audio_transcription.delta", "item_id": "item_xxx", "content_index": 0, "text": "你好", "stash": "世界" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `conversation.item.input_audio_transcription.delta`。 |
| **item\\_id** `*string*` 关联的对话项 ID。 |
| **content\\_index** `*integer*` 内容部分的索引。 |
| **text** `*string*` 已确认的转写文本。 |
| **stash** `*string*` 尚未确认的暂定文本。 |

## **conversation.item.input\_audio\_transcription.completed**

**说明**：ASR 最终转写结果。转写文本会写入对应对话项的 `transcript` 字段。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "conversation.item.input_audio_transcription.completed", "item_id": "item_xxx", "content_index": 0, "transcript": "你好世界" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `conversation.item.input_audio_transcription.completed`。 |
| **item\\_id** `*string*` 关联的对话项 ID。 |
| **content\\_index** `*integer*` 内容部分的索引。 |
| **transcript** `*string*` 完整的转写文本。 |

## **conversation.item.input\_audio\_transcription.failed**

**说明**：ASR 转写失败。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "conversation.item.input_audio_transcription.failed", "item_id": "item_xxx", "content_index": 0, "error": { "type": "transcription_error", "code": "transcription_failed", "message": "ASR transcription failed" } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `conversation.item.input_audio_transcription.failed`。 |
| **item\\_id** `*string*` 关联的对话项 ID。 |
| **content\\_index** `*integer*` 内容部分的索引。 |
| **error** `*object*` 错误详情。 **属性** **type** `*string*` 错误类型，例如 `transcription_error`。 **code** `*string*` 错误码，例如 `transcription_failed`。 **message** `*string*` 错误信息。 |

## **conversation.item.ambient\_audio\_transcription.delta**

**说明**：**仅 smart\_turn 模式。** 环境音频的增量转写结果。当 VAD 检测到语音活动、但语义分析判定其为无效轮次（例如噪声或“嗯”“啊”这类无语义的声音）时，ASR 结果会以环境事件的形式传给客户端。此事件不关联对话上下文中的任何对话项；`item_id` 是独立生成的临时 ID。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "conversation.item.ambient_audio_transcription.delta", "item_id": "item_xxx", "content_index": 0, "text": "嗯", "stash": "" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `conversation.item.ambient_audio_transcription.delta`。 |
| **item\\_id** `*string*` 独立生成的临时 ID，不关联对话上下文中的任何对话项。 |
| **content\\_index** `*integer*` 内容部分的索引。 |
| **text** `*string*` 已确认的转写文本。 |
| **stash** `*string*` 尚未确认的暂定文本。 |

## **conversation.item.ambient\_audio\_transcription.completed**

**说明**：**仅 smart\_turn 模式。** 环境音频的最终转写结果。与 delta 事件配对，标志一段环境音频转写结束。结果不会写入对话上下文。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "conversation.item.ambient_audio_transcription.completed", "item_id": "item_xxx", "content_index": 0, "transcript": "嗯" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `conversation.item.ambient_audio_transcription.completed`。 |
| **item\\_id** `*string*` 独立生成的临时 ID，不关联对话上下文。 |
| **content\\_index** `*integer*` 内容部分的索引。 |
| **transcript** `*string*` 完整的转写文本。 |

## **response.created**

**说明**：一次模型推理已开始。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.created", "response": { "id": "resp_xxx", "object": "realtime.response", "status": "in_progress", "modalities": ["text", "audio"], "voice": "longanqian", "output": [] } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.created`。 |
| **response** `*object*` 回复对象。 **属性** **id** `*string*` 回复的唯一标识。 **object** `*string*` 固定为 `realtime.response`。 **status** `*string*` 回复状态，例如 `in_progress`。 **modalities** `*array*` 模型输出模态。 **voice** `*string*` 模型生成音频所用的音色：系统音色名称，或复刻音色的 `voice_id`。 **output** `*array*` 回复输出项列表，初始为空数组。 |

## **response.output\_item.added**

**说明**：回复中新增了一个输出项。普通回复使用 `message` 类型的输出项；Function Calling 使用 `function_call`。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.output_item.added", "response_id": "resp_xxx", "output_index": 0, "item": { "id": "item_xxx", "object": "realtime.item", "type": "message", "status": "in_progress", "role": "assistant", "content": [] } } ``` Function Call 输出项示例：对于函数调用输出项，`response.output_item.added` / `conversation.item.created` / `response.output_item.done` 中的 `item` 结构如下： ``` { "id": "item_xxx", "object": "realtime.item", "type": "function_call", "status": "completed", "call_id": "call_xxx", "name": "get_weather", "arguments": "{\\"city\\":\\"杭州\\"}" } ``` **注意** 一个回复可能包含多个 `function_call` 输出项，也可能同时包含普通 `message` 输出和 `function_call` 输出。Function Call 内容不会送入 TTS 播放。 |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.output_item.added`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **item** `*object*` 新增的输出项。 **属性** **id** `*string*` 输出项的唯一标识。 **object** `*string*` 固定为 `realtime.item`。 **type** `*string*` 输出项类型：`message`（普通消息）或 `function_call`（函数调用）。 **status** `*string*` 输出项状态，例如 `in_progress`。 **role** `*string*` 消息角色，固定为 `assistant`。仅 `message` 输出项包含。 **content** `*array*` 消息内容元素列表。仅 `message` 输出项包含。 |

## **response.content\_part.added**

**说明**：输出项中新增了一个内容部分。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.content_part.added", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "content_index": 0, "part": { "type": "audio", "text": "" } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.content_part.added`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **content\\_index** `*integer*` 内容部分在输出项中的索引。 |
| **part** `*object*` 新增的内容部分。 **属性** **type** `*string*` 内容类型，例如 `audio` 或 `text`。 **text** `*string*` 文本内容，初始为空字符串。 |

## **response.text.delta**

**说明**：纯文本模式下的文本增量事件，流式返回文本片段。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.text.delta", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "content_index": 0, "delta": "你好" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.text.delta`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **content\\_index** `*integer*` 内容部分在输出项中的索引。 |
| **delta** `*string*` 增量文本片段。 |

## **response.text.done**

**说明**：表示纯文本模式下的文本输出已完成。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.text.done", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "content_index": 0, "text": "你好，有什么可以帮助你的吗？" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.text.done`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **content\\_index** `*integer*` 内容部分在输出项中的索引。 |
| **text** `*string*` 完整的文本输出。 |

## **response.audio\_transcript.delta**

**说明**：音频模式下的转写文本增量事件，流式返回转写片段。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.audio_transcript.delta", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "content_index": 0, "delta": "你好" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.audio_transcript.delta`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **content\\_index** `*integer*` 内容部分在输出项中的索引。 |
| **delta** `*string*` 增量转写片段。 |

## **response.audio\_transcript.done**

**说明**：表示音频模式下的转写文本输出已完成。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.audio_transcript.done", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "content_index": 0, "transcript": "你好，有什么可以帮助你的吗？" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.audio_transcript.done`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **content\\_index** `*integer*` 内容部分在输出项中的索引。 |
| **transcript** `*string*` 完整的转写文本。 |

## **response.audio.delta**

**说明**：音频模式下的音频数据增量事件。`delta` 字段包含 Base64 编码的 PCM 音频数据。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.audio.delta", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "content_index": 0, "delta": "<base64 编码的音频数据>" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.audio.delta`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **content\\_index** `*integer*` 内容部分在输出项中的索引。 |
| **delta** `*string*` Base64 编码的 PCM 音频数据块。 |

## **response.audio.done**

**说明**：表示音频模式下的音频输出已完成。不包含音频数据。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.audio.done", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "content_index": 0 } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.audio.done`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **content\\_index** `*integer*` 内容部分在输出项中的索引。 |

## **response.content\_part.done**

**说明**：输出项中某个内容部分的输出已完成。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.content_part.done", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "content_index": 0, "part": { "type": "audio", "text": "你好，有什么可以帮助你的吗？" } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.content_part.done`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **content\\_index** `*integer*` 内容部分在输出项中的索引。 |
| **part** `*object*` 已完成的内容部分。 **属性** **type** `*string*` 内容类型，例如 `audio` 或 `text`。 **text** `*string*` 文本内容或音频转写文本。 |

## **response.output\_item.done**

**说明**：回复中某个输出项的输出已完成。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.output_item.done", "response_id": "resp_xxx", "output_index": 0, "item": { "id": "item_xxx", "object": "realtime.item", "type": "message", "status": "completed", "role": "assistant", "content": [ { "type": "text", "text": "你好，有什么可以帮助你的吗？" } ] } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.output_item.done`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **item** `*object*` 已完成输出项的完整信息。 **属性** **id** `*string*` 输出项的唯一标识。 **object** `*string*` 固定为 `realtime.item`。 **type** `*string*` 输出项类型：`message`（普通消息）或 `function_call`（函数调用）。 **status** `*string*` 输出项状态，例如 `completed`。 **role** `*string*` 消息角色，固定为 `assistant`。仅 `message` 输出项包含。 **content** `*array*` 消息内容元素列表。仅 `message` 输出项包含。 |

## **response.function\_call\_arguments.delta**

**说明**：Function Calling 参数增量。当模型决定调用工具时，服务端先发送 `response.output_item.added`（`item.type=function_call`）及对应的 `conversation.item.created`，再通过此事件流式返回参数片段。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.function_call_arguments.delta", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "call_id": "call_xxx", "delta": "{\\"city" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.function_call_arguments.delta`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **call\\_id** `*string*` 函数调用的唯一标识。 |
| **delta** `*string*` 函数调用参数的增量片段（JSON 字符串片段）。 |

## **response.function\_call\_arguments.done**

**说明**：Function Calling 参数输出完成。收到此事件后，客户端应执行对应的工具，用 `conversation.item.create` 提交 `function_call_output`，然后发送 `response.create` 触发第二次推理。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_xxx", "type": "response.function_call_arguments.done", "response_id": "resp_xxx", "item_id": "item_xxx", "output_index": 0, "call_id": "call_xxx", "name": "get_weather", "arguments": "{\\"city\\":\\"杭州\\"}" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.function_call_arguments.done`。 |
| **response\\_id** `*string*` 关联的回复 ID。 |
| **item\\_id** `*string*` 关联的输出项 ID。 |
| **output\\_index** `*integer*` 输出项在回复中的索引。 |
| **call\\_id** `*string*` 函数调用的唯一标识。 |
| **name** `*string*` 被调用函数的名称。 |
| **arguments** `*string*` 完整的函数调用参数（JSON 字符串）。 |

## **response.done**

**说明**：一次推理已结束。`status` 表示结束原因。

| **event\\_id** `*string*` 本事件的唯一标识。 | ## 正常完成 ``` { "event_id": "event_xxx", "type": "response.done", "response": { "id": "resp_xxx", "object": "realtime.response", "status": "completed", "modalities": ["text", "audio"], "voice": "longanqian", "output": [ { "id": "item_xxx", "object": "realtime.item", "type": "message", "status": "completed", "role": "assistant", "content": [ { "type": "audio", "transcript": "你好，有什么可以帮助你的吗？" } ] } ], "usage": { "total_tokens": 377, "input_tokens": 336, "output_tokens": 41, "input_tokens_details": { "text_tokens": 228, "audio_tokens": 108 }, "output_tokens_details": { "text_tokens": 9, "audio_tokens": 32 } } } } ``` ## 被打断取消 ``` { "event_id": "event_xxx", "type": "response.done", "response": { "id": "resp_xxx", "status": "cancelled", "status_details": { "type": "cancelled", "reason": "turn_detected" } } } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `response.done`。 |
| **response** `*object*` 完整的回复对象。 **属性** **id** `*string*` 回复的唯一标识。 **object** `*string*` 固定为 `realtime.response`。 **status** `*string*` 回复的最终状态。取值： - `completed`：正常完成。 - `cancelled`：被打断取消。`status_details.reason` 为 `turn_detected`（VAD 打断）或 `client_cancelled`（客户端取消）。 - `failed`：LLM 或 TTS 出错。 **status\\_details** `*object*` 状态详情，仅在 `cancelled` 或 `failed` 时出现。 **属性** **type** `*string*` 状态类型，例如 `cancelled`。 **reason** `*string*` 取消原因：`turn_detected`（VAD 打断）或 `client_cancelled`（客户端取消）。 **modalities** `*array*` 模型输出模态。 **voice** `*string*` 模型生成音频所用的音色：系统音色名称，或复刻音色的 `voice_id`。 **output** `*array*` 回复输出项列表，包含完整的输出项对象。 **usage** `*object*` 本次回复的 token 用量。仅在 `status` 为 `completed` 时返回。 **属性** **total\\_tokens** `*integer*` 本次回复消耗的 token 总数。 **input\\_tokens** `*integer*` 输入 token 数。 **output\\_tokens** `*integer*` 输出 token 数。 **input\\_tokens\\_details** `*object*` 输入 token 明细，包含 `text_tokens`（文本 token 数）和 `audio_tokens`（音频 token 数，仅在使用音频模态时出现）。 **output\\_tokens\\_details** `*object*` 输出 token 明细，包含 `text_tokens`（文本 token 数）和 `audio_tokens`（音频 token 数，仅在使用音频模态时出现）。 **plugins** `*object*`（可选）插件用量指标。启用联网搜索（`enable_search`）时返回。 **属性** **search** `*object*` 联网搜索用量指标。 **属性** **count** `*integer*` 搜索次数。 **strategy** `*string*` 搜索策略。 |

## **voiceprint\_audio\_list.in\_progress**

**说明**：异步声纹注册正在进行。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_PaEcN7CCrlhE8q4MM2yND", "type": "voiceprint_audio_list.in_progress", "item_id": "vp_Y12cA986j1KZ9O9YmAXOA" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `voiceprint_audio_list.in_progress`。 |
| **item\\_id** `*string*` 声纹注册任务的唯一标识。 |

## **voiceprint\_audio\_list.completed**

**说明**：声纹注册已完成。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_PaEcN7CCrlhE8q4MM2yND", "type": "voiceprint_audio_list.completed", "item_id": "vp_Y12cA986j1KZ9O9YmAXOA" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `voiceprint_audio_list.completed`。 |
| **item\\_id** `*string*` 声纹注册任务的唯一标识。 |

## **voiceprint\_audio\_list.failed**

**说明**：声纹注册失败。不会阻塞正常的对话请求。

| **event\\_id** `*string*` 本事件的唯一标识。 | ``` { "event_id": "event_PaEcN7CCrlhE8q4MM2yND", "type": "voiceprint_audio_list.failed", "item_id": "vp_Y12cA986j1KZ9O9YmAXOA", "reason": "" } ``` |
| --- | --- |
| **type** `*string*` 事件类型，固定为 `voiceprint_audio_list.failed`。 |
| **item\\_id** `*string*` 声纹注册任务的唯一标识。 |
| **reason** `*string*` 失败原因说明。 |
