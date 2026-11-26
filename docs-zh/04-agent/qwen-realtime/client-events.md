Qwen-Audio Realtime API 的客户端事件参考。

**用户指南**：[实时语音对话（Qwen-Audio-Realtime）](https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime)。事件交互时序请参见 [WebSocket API](https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime-websocket-api)。

## **session.update**

**说明**：建立连接后，发送此事件以更新默认会话配置。只需包含要修改的字段；省略的字段保留现有值。服务端会校验参数：参数无效时返回错误，参数有效时应用修改并返回完整配置。

**注意**

`turn_detection` 只能在首次发送音频之前（IDLE 状态下）修改。

| **type** `*string*`** （必填）** 事件类型，固定为 `session.update`。 | ``` { "type": "session.update", "session": { "modalities": [ "text", "audio" ], "voice": "longanqian", "turn_detection": { "type": "server_vad", "threshold": 0.5, "silence_duration_ms": 800 } } } ``` Function Calling： ``` { "type": "session.update", "session": { "modalities": [ "text", "audio" ], "voice": "longanqian", "tools": [ { "type": "function", "function": { "name": "get_weather", "description": "查询指定城市天气", "parameters": { "type": "object", "properties": { "city": { "type": "string", "title": "城市" } }, "required": ["city"] } } } ], "turn_detection": { "type": "server_vad", "threshold": 0.5, "silence_duration_ms": 800 } } } ``` 声纹注册（voiceprint\\_audio\\_urls）： ``` { "type": "session.update", "session": { "turn_detection": { "type": "smart_turn", "voiceprint_audio_urls": [ "https://example.com/speaker1.pcm", "https://example.com/speaker2.wav" ] } } } ``` |
| --- | --- |
| **session** `*object*`（可选）会话配置。 **属性** **modalities** `*array*`（可选）模型输出模态。取值： - `["text"]`：仅文本。 - `["audio", "text"]`（默认）：文本和音频。 **voice** `*string*`（可选）TTS 音色名称。默认值：`longanqian`。支持两类音色。只能在第一次 `session.update` 中设置；后续设置的值会被忽略。 - **系统音色**：取值：`longanqian`、`longanlingxin`、`longanlingxi`、`longanxiaoxin`、`longanlufeng`。 - **复刻音色**：通过声音复刻 API 创建音色，并将此参数设置为返回的 `voice_id`。参见[音色配置](https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime#fc60h311)。 **enable\\_speech\\_emotion** `*boolean*`（可选）是否启用情感增强，使回复语音的情感变化更明显。默认值：`true`。取值：`true`、`false`。 **instructions** `*string*`（可选）系统指令，用于定义模型的角色、回复风格和行为偏好。作用于整个会话。 **input\\_audio\\_format** `*string*`（可选）输入音频格式。目前仅支持 `pcm`（16kHz、16-bit、单声道），也是默认值。只能在首次发送音频之前（IDLE 状态下）修改。 **output\\_audio\\_format** `*string*`（可选）输出音频格式。目前仅支持 `pcm`（24kHz、16-bit、单声道），也是默认值。 **max\\_history\\_turns** `*integer*`（可选）每次请求允许携带的最大历史问答轮数。取值范围：1-50。默认值：20。 **tools** `*array*`（可选）Function Calling 工具定义列表。配置后，模型可根据用户输入决定是否调用工具。 **属性** **type** `*string*`** （必填）** 固定为 `function`。 **function.name** `*string*`** （必填）** 工具函数名称。 **function.description** `*string*`（可选）工具函数的描述，模型据此决定是否调用该工具。 **function.parameters** `*object*`（可选）函数入参的描述，模型据此提取所需参数。如果函数不需要参数，可以省略。 **属性** **type** `*string*`** （必填）** 固定为 `object`。 **properties** `*object*`（可选）入参的名称、数据类型和描述。 **required** `*array*`（可选）指定哪些入参为必填。 **turn\\_detection** `*object\\|null*`（可选）轮次检测配置。设置为 `null` 可切换到 push-to-talk 模式（手动提交音频并触发推理）。如果省略，系统将以默认参数启用 VAD。 **属性** **type** `*string*`（可选）VAD 类型。取值： - `server_vad`（默认）：基于声学特征检测语音边界，并自动触发推理。 - `smart_turn`：结合声学感知与语义理解来识别轮次边界的智能轮次检测。没有语义的声音（例如“嗯”“啊”）不会触发对话轮次，也不会打断模型说话。 **threshold** `*float*`（可选）VAD 灵敏度。仅在 server\\_vad 模式下生效（在 smart\\_turn 模式下无效）。值越低，VAD 越灵敏，越容易把微弱的声音（包括背景噪声）识别为语音；值越高，需要更清晰、更响亮的语音才能触发检测。取值范围：\\[-1.0, 1.0\\]。默认值：0.5。 **silence\\_duration\\_ms** `*integer*`（可选）语音结束后的最短静音时长，单位为毫秒。仅在 server\\_vad 模式下生效（在 smart\\_turn 模式下无效）。静音达到该时长后触发模型回复。值越低回复越快，但可能在短暂停顿时过早触发。取值范围：\\[200, 6000\\]。默认值：800。对话场景建议：400-800。 **voiceprint\\_audio\\_urls** `*array*`（可选）**仅在 smart\\_turn 模式下生效。** 目标用户预先录制音频的公网可访问 URL 列表，用于说话人增强。提供后，模型在全双工对话中可精确识别目标说话人，并忽略其他说话人和背景噪声。最多支持 5 个 URL。音频格式要求：16kHz PCM 或 WAV。 **重要** 此参数只能在**第一次** `session.update` 事件中配置；后续设置的值会被忽略。 |

## **input\_audio\_buffer.append**

**说明**：向输入缓冲区追加音频数据。以较高频率持续发送（例如每 20~40 ms 一帧）。服务端不会确认此事件。

| **type** `*string*`** （必填）** 事件类型，固定为 `input_audio_buffer.append`。 | ``` { "type": "input_audio_buffer.append", "audio": "<base64 编码的音频数据>" } ``` |
| --- | --- |
| **audio** `*string*`** （必填）** Base64 编码的音频数据。 |

## **input\_audio\_buffer.commit**

**说明**：**仅用于 push-to-talk 模式。** 将缓冲区中的音频提交为一条用户消息。提交不会自动触发推理；需发送 `response.create` 手动触发。

在 server\_vad / smart\_turn 模式下，此事件会被忽略。

| **type** `*string*`** （必填）** 事件类型，固定为 `input_audio_buffer.commit`。 | ``` { "type": "input_audio_buffer.commit" } ``` |
| --- | --- |

## **input\_audio\_buffer.clear**

**说明**：**仅用于 push-to-talk 模式。** 清除缓冲区中尚未提交的音频。在 server\_vad / smart\_turn 模式下，此事件会被忽略。服务端以 `input_audio_buffer.cleared` 事件响应。

| **type** `*string*`** （必填）** 事件类型，固定为 `input_audio_buffer.clear`。 | ``` { "type": "input_audio_buffer.clear" } ``` |
| --- | --- |

## **conversation.item.create**

**说明**：手动向对话上下文中插入一个对话项。可用于注入历史上下文、添加文本信息，或在 Function Calling 中提交工具执行结果。

**注意**

如果 `item.id` 已存在于对话中，创建会被拒绝并返回错误。

| **type** `*string*`** （必填）** 事件类型，固定为 `conversation.item.create`。 | 注入一条用户文本消息： ``` { "type": "conversation.item.create", "previous_item_id": "item_xxx", "item": { "id": "my_item_001", "type": "message", "role": "user", "content": [ { "type": "input_text", "text": "请帮我总结一下上次的对话" } ] } } ``` 提交 Function Calling 结果： ``` { "type": "conversation.item.create", "item": { "type": "function_call_output", "call_id": "call_xxx", "output": "{\"temperature\":18,\"condition\":\"晴\"}" } } ``` |
| --- | --- |
| **previous\\_item\\_id** `*string*`（可选）指定将新对话项插入到哪个对话项之后。如果省略，新对话项会追加到对话末尾。 |
| **item** `*object*`** （必填）** 要创建的对话项。 **属性** **id** `*string*`（可选）对话项的唯一标识。省略时由服务端自动生成。如果指定的 ID 已存在于对话中，将返回错误。 **type** `*string*`** （必填）** 对话项类型。取值： - `message`：普通对话消息。 - `function_call`：函数调用请求。通常由服务端生成；客户端也可以用它补充历史上下文。 - `function_call_output`：工具执行结果。收到 `function_call` 后，客户端执行工具并以此类型提交结果。 **role** `*string*`（`message` 必填）消息角色。取值：`system`、`user`、`assistant`。 **content** `*array*`（`message` 必填）消息内容元素列表。每个元素包含一个 `type` 及对应的数据字段。 **各角色支持的内容类型** **system** `input_text`：系统消息；需要 `text`。 **user** - `input_text`：用户文本输入；需要 `text`。 - `input_audio`：用户音频输入；需要 `audio`（Base64 编码）。 **assistant** `output_text`：助手文本输出；需要 `text`。 **call\\_id** `*string*`（`function_call` / `function_call_output` 必填）函数调用的唯一标识，用于关联请求与结果。 **name** `*string*`（`function_call` 必填）要调用的函数名称。 **arguments** `*string*`（`function_call` 必填）函数调用参数，为 JSON 字符串。 **output** `*string*`（`function_call_output` 必填）工具执行结果，为 JSON 字符串。 |

## **conversation.item.retrieve**

**说明**：获取服务端存储的对话项。返回的音频内容只包含转写文本（`transcript`），不包含原始音频数据。

| **type** `*string*`** （必填）** 事件类型，固定为 `conversation.item.retrieve`。 | ``` { "type": "conversation.item.retrieve", "item_id": "item_xxx" } ``` |
| --- | --- |
| **item\\_id** `*string*`** （必填）** 要获取的对话项 ID。服务端通过 `conversation.item.retrieved` 事件返回结果。 |

## **conversation.item.delete**

**说明**：从对话上下文中删除指定的对话项。服务端以 `conversation.item.deleted` 事件确认删除。

| **type** `*string*`** （必填）** 事件类型，固定为 `conversation.item.delete`。 | ``` { "type": "conversation.item.delete", "item_id": "item_xxx" } ``` |
| --- | --- |
| **item\\_id** `*string*`** （必填）** 要删除的对话项 ID。 |

## **response.create**

**说明**：显式触发模型推理。不同模式下的行为：

-   **push-to-talk 模式**：在用 `input_audio_buffer.commit` 提交缓冲音频或提交 `function_call_output` 之后，必须手动调用。正在生成回复时不能再次触发。

-   **server\_vad 模式**：通常由服务端自动触发。客户端也可以在没有回复正在生成时手动调用；正在生成回复时不能再次触发。

-   **smart\_turn 模式**：可以在等待用户下一次输入时调用。在活跃轮次期间（收到 `input_audio_buffer.speech_started` 到 `response.done` 之间）不能再次触发。


可选的 `response` 字段会为本次推理覆盖默认会话配置。在 Function Calling 场景中，客户端在提交 `function_call_output` 后也用此事件触发第二次推理。

**注意**

在 server\_vad / smart\_turn 模式下，新的语音仍然可以打断手动触发的推理。

| **type** `*string*`** （必填）** 事件类型，固定为 `response.create`。 | ``` { "type": "response.create", "response": { "modalities": ["audio", "text"] } } ``` |
| --- | --- |
| **response** `*object*`（可选）为本次推理覆盖默认会话配置。如果省略，则使用当前会话配置。 **属性** **modalities** `*array*`（可选）覆盖本轮的输出模态。取值与 `session.update` 中的 `modalities` 相同。 **voice** `*string*`（可选）覆盖本轮的 TTS 音色。 |

## **response.cancel**

**说明**：取消正在进行的推理。已输出的文本会写入对话项链表，服务端返回 `status=cancelled` 的 `response.done`。

如果没有正在进行的推理，则返回错误。

| **type** `*string*`** （必填）** 事件类型，固定为 `response.cancel`。 | ``` { "type": "response.cancel" } ``` |
| --- | --- |
