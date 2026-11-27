Client event reference for the Qwen-Audio Realtime API.

**User guide**: [Real-time voice conversations (Qwen-Audio-Realtime)](https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime). For event interaction sequences, see [WebSocket API](https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime-websocket-api).

## **session.update**

**Description**: After establishing a connection, send this event to update the default session configuration. Include only the fields to change; omitted fields retain their existing values. The server validates the parameters, returning an error if they are invalid or applying the changes and returning the full configuration if they are valid.

**Note**

`turn_detection` can only be changed before audio is first sent (in the IDLE state).

| **type** `*string*`** (required)** Event type, fixed to `session.update`. | ``` { "type": "session.update", "session": { "modalities": [ "text", "audio" ], "voice": "longanqian", "turn_detection": { "type": "server_vad", "threshold": 0.5, "silence_duration_ms": 800 } } } ``` Function Calling: ``` { "type": "session.update", "session": { "modalities": [ "text", "audio" ], "voice": "longanqian", "tools": [ { "type": "function", "function": { "name": "get_weather", "description": "查询指定城市天气", "parameters": { "type": "object", "properties": { "city": { "type": "string", "title": "城市" } }, "required": ["city"] } } } ], "turn_detection": { "type": "server_vad", "threshold": 0.5, "silence_duration_ms": 800 } } } ``` Voiceprint registration (voiceprint_audio_urls): ``` { "type": "session.update", "session": { "turn_detection": { "type": "smart_turn", "voiceprint_audio_urls": [ "https://example.com/speaker1.pcm", "https://example.com/speaker2.wav" ] } } } ``` |
| --- | --- |
| **session** `*object*` (optional) Session configuration. **Properties** **modalities** `*array*` (optional) Model output modalities. Valid values: - `["text"]`: Text only. - `["audio", "text"]` (default): Both text and audio. **voice** `*string*` (optional) TTS voice name. Default: `longanqian`. Two types are supported. This can only be set in the first `session.update`; subsequent values are ignored. - **System voices**: Valid values: `longanqian`, `longanlingxin`, `longanlingxi`, `longanxiaoxin`, `longanlufeng`. - **Cloned voices**: Create a voice using the voice cloning API and set this parameter to the returned `voice_id`. See [Voice configuration](https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime#fc60h311). **enable_speech_emotion** `*boolean*` (optional) Whether to enable emotion enhancement, which makes emotional variation in the response voice more pronounced. Default: `true`. Valid values: `true`, `false`. **instructions** `*string*` (optional) System instructions defining the model's role, response style, and behavioral preferences. Apply to the entire session. **input_audio_format** `*string*` (optional) Input audio format. Currently, only `pcm` (16kHz, 16-bit, mono) is supported and is the default. Can only be changed before audio is first sent (in the IDLE state). **output_audio_format** `*string*` (optional) Output audio format. Currently, only `pcm` (24kHz, 16-bit, mono) is supported and is the default. **max_history_turns** `*integer*` (optional) Maximum number of historical question-answer turns allowed per request. Range: 1-50. Default: 20. **tools** `*array*` (optional) List of Function Calling tool definitions. When configured, the model can decide whether to call a tool based on user input. **Properties** **type** `*string*`** (required)** Fixed to `function`. **function.name** `*string*`** (required)** Tool function name. **function.description** `*string*` (optional) Description of the tool function, used by the model to decide whether to call it. **function.parameters** `*object*` (optional) Description of the function's input parameters, used by the model to extract the required arguments. May be omitted if the function requires no arguments. **Properties** **type** `*string*`** (required)** Fixed to `object`. **properties** `*object*` (optional) Names, data types, and descriptions of the input parameters. **required** `*array*` (optional) Specifies which input parameters are required. **turn_detection** `*object\\|null*` (optional) Turn detection configuration. Set to `null` to switch to push-to-talk mode (manually submit audio and trigger inference). If omitted, the system enables VAD with default parameters. **Properties** **type** `*string*` (optional) VAD type. Valid values: - `server_vad` (default): Detects speech boundaries using acoustic features and automatically triggers inference. - `smart_turn`: Intelligent turn detection combining acoustic perception and semantic understanding to identify turn boundaries. Sounds without semantic meaning (such as "um" or "ah") do not trigger a conversation turn or interrupt model speech. **threshold** `*float*` (optional) VAD sensitivity. Applies only in server_vad mode (has no effect in smart_turn mode). Lower values make VAD more sensitive and more likely to recognize faint sounds, including background noise, as speech. Higher values require clearer, louder speech to trigger detection. Range: \\[-1.0, 1.0\\]. Default: 0.5. **silence_duration_ms** `*integer*` (optional) Minimum silence duration in milliseconds after speech ends. Applies only in server_vad mode (has no effect in smart_turn mode). When this duration elapses, a model response is triggered. Lower values produce faster responses but may trigger prematurely during brief pauses. Range: \\[200, 6000\\]. Default: 800. Recommended for conversations: 400-800. **voiceprint_audio_urls** `*array*` (optional) **Applies only in smart_turn mode.** List of publicly accessible URLs of prerecorded audio from the target user, used for speaker enhancement. When provided, the model precisely identifies the target speaker in full-duplex conversations and ignores other speakers and background noise. Supports up to 5 URLs. Required audio format: 16kHz PCM or WAV. **Important** This parameter can only be configured in the **first** `session.update` event; subsequent values are ignored. |

## **input\_audio\_buffer.append**

**Description**: Append audio data to the input buffer. Send continuously at a high frequency (for example, one frame every 20~40 ms). The server does not acknowledge this event.

| **type** `*string*`** (required)** Event type, fixed to `input_audio_buffer.append`. | ``` { "type": "input_audio_buffer.append", "audio": "<base64 编码的音频数据>" } ``` |
| --- | --- |
| **audio** `*string*`** (required)** Base64-encoded audio data. |

## **input\_audio\_buffer.commit**

**Description**: **Push-to-talk mode only.** Commit buffered audio as a user message. Committing does not automatically trigger inference; send `response.create` to trigger it manually.

This event is ignored in server\_vad / smart\_turn mode.

| **type** `*string*`** (required)** Event type, fixed to `input_audio_buffer.commit`. | ``` { "type": "input_audio_buffer.commit" } ``` |
| --- | --- |

## **input\_audio\_buffer.clear**

**Description**: **Push-to-talk mode only.** Clear uncommitted audio from the buffer. This event is ignored in server\_vad / smart\_turn mode. The server responds with an `input_audio_buffer.cleared` event.

| **type** `*string*`** (required)** Event type, fixed to `input_audio_buffer.clear`. | ``` { "type": "input_audio_buffer.clear" } ``` |
| --- | --- |

## **conversation.item.create**

**Description**: Manually insert an item into the conversation context. Use this to inject historical context, add text information, or submit tool execution results for Function Calling.

**Note**

If `item.id` already exists in the conversation, creation is rejected with an error.

| **type** `*string*`** (required)** Event type, fixed to `conversation.item.create`. | Inject a user text message: ``` { "type": "conversation.item.create", "previous_item_id": "item_xxx", "item": { "id": "my_item_001", "type": "message", "role": "user", "content": [ { "type": "input_text", "text": "请帮我总结一下上次的对话" } ] } } ``` Submit Function Calling results: ``` { "type": "conversation.item.create", "item": { "type": "function_call_output", "call_id": "call_xxx", "output": "{\"temperature\":18,\"condition\":\"晴\"}" } } ``` |
| --- | --- |
| **previous_item_id** `*string*` (optional) Specifies the conversation item after which to insert the new item. If omitted, the new item is appended to the conversation. |
| **item** `*object*`** (required)** Conversation item to create. **Properties** **id** `*string*` (optional) Unique identifier of the conversation item. Automatically generated by the server if omitted. If the specified ID already exists in the conversation, an error is returned. **type** `*string*`** (required)** Conversation item type. Valid values: - `message`: A regular conversation message. - `function_call`: A function call request. Usually generated by the server; the client can also use it to add historical context. - `function_call_output`: Tool execution results. After receiving a `function_call`, the client executes the tool and submits results using this type. **role** `*string*` (required for `message`) Message role. Valid values: `system`, `user`, `assistant`. **content** `*array*` (required for `message`) List of message content elements. Each contains a `type` and the corresponding data fields. **Supported content types by role** **system** `input_text`: System message; requires `text`. **user** - `input_text`: User text input; requires `text`. - `input_audio`: User audio input; requires `audio` (Base64-encoded). **assistant** `output_text`: Assistant text output; requires `text`. **call_id** `*string*` (required for `function_call` / `function_call_output`) Unique function call identifier linking the request and result. **name** `*string*` (required for `function_call`) Name of the function to call. **arguments** `*string*` (required for `function_call`) Function call arguments as a JSON string. **output** `*string*` (required for `function_call_output`) Tool execution results as a JSON string. |

## **conversation.item.retrieve**

**Description**: Retrieve a conversation item stored on the server. Returned audio content contains only the transcription (`transcript`), not raw audio data.

| **type** `*string*`** (required)** Event type, fixed to `conversation.item.retrieve`. | ``` { "type": "conversation.item.retrieve", "item_id": "item_xxx" } ``` |
| --- | --- |
| **item_id** `*string*`** (required)** ID of the conversation item to retrieve. The server returns the result in a `conversation.item.retrieved` event. |

## **conversation.item.delete**

**Description**: Delete the specified item from the conversation context. The server acknowledges deletion with a `conversation.item.deleted` event.

| **type** `*string*`** (required)** Event type, fixed to `conversation.item.delete`. | ``` { "type": "conversation.item.delete", "item_id": "item_xxx" } ``` |
| --- | --- |
| **item_id** `*string*`** (required)** ID of the conversation item to delete. |

## **response.create**

**Description**: Explicitly trigger model inference. Behavior by mode:

-   **push-to-talk mode**: Must be called manually after committing buffered audio with `input_audio_buffer.commit` or submitting `function_call_output`. Cannot be triggered again while a response is being generated.

-   **server\_vad mode**: Usually triggered automatically by the server. The client may also call it manually when no response is being generated; it cannot be triggered again while a response is being generated.

-   **smart\_turn mode**: May be called while waiting for the user's next input. Cannot be triggered again during an active turn (between receiving `input_audio_buffer.speech_started` and `response.done`).


The optional `response` field overrides the default session configuration for this inference. In Function Calling scenarios, the client also uses this event to trigger a second inference after submitting `function_call_output`.

**Note**

In server\_vad / smart\_turn mode, new speech can still interrupt manually triggered inference.

| **type** `*string*`** (required)** Event type, fixed to `response.create`. | ``` { "type": "response.create", "response": { "modalities": ["audio", "text"] } } ``` |
| --- | --- |
| **response** `*object*` (optional) Overrides the default session configuration for this inference. If omitted, the current session configuration is used. **Properties** **modalities** `*array*` (optional) Overrides the output modalities for this turn. Valid values are the same as `modalities` in `session.update`. **voice** `*string*` (optional) Overrides the TTS voice for this turn. |

## **response.cancel**

**Description**: Cancel the ongoing inference. Text already output is written to the item linked list, and the server returns `response.done` with `status=cancelled`.

Returns an error if no inference is in progress.

| **type** `*string*`** (required)** Event type, fixed to `response.cancel`. | ``` { "type": "response.cancel" } ``` |
| --- | --- |
