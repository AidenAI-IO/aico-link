---
sidebar_position: 3
---

# 工具 HTTP API

在所有输入模式下，Agent 都会通过 HTTP 暴露可安全调用的 Agent 自有工具，供外部 Agent 或手动调用使用。内部维护工具（例如 `skill_manage`）不会通过 HTTP 暴露。

关于同一个 Agent 上的 MCP Streamable HTTP（`POST /mcp`），参见 [mcp.md](./mcp.md)。

## 端点

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/tools` | 列出 HTTP 可见的工具，包括说明、输入模式、示例和 HTTP 绑定 |
| `POST` | `/api/tools/{tool_name}` | 调用指定的工具 |
| `GET` | `/api/tool-skills` | 生成适合外部 Agent 使用的 `SKILL.md` 包 |

## 请求格式

JSON 对象输入：

```json
{
  "input": {"command": "pwd"}
}
```

原始字符串输入：

```json
{
  "raw_input": "{\"command\":\"pwd\"}"
}
```

需要传入原始字符串时，使用 `raw_input`。大多数工具（包括 Skill 工具）在目录中用 JSON 示例描述其输入：

```json
{
  "raw_input": "planner"
}
```

## 响应格式

```json
{
  "tool": {
    "name": "shell",
    "category": "system",
    "description": "...",
    "input_mode": "json",
    "example_input": "{\"command\":\"pwd\"}",
    "args_schema": {
      "type": "object",
      "properties": {
        "command": {"type": "string"}
      },
      "additionalProperties": false
    },
    "http": {
      "method": "POST",
      "path": "/api/tools/shell"
    },
    "agent_exposed": true,
    "ui_operation": false
  },
  "raw_input": "{\"command\":\"pwd\"}",
  "output": "...",
  "is_error": false,
  "duration_ms": 12,
  "called_at": "2026-05-18T12:34:56Z"
}
```

工具执行失败同样以 JSON 格式返回。请检查：

- `is_error`
- `output` 是否包含错误信息
- HTTP 传输是否成功

## 目录范围

HTTP 目录在运行时根据已注册的 Agent 自有工具生成。它可能包含诊断工具和面向外部 Agent 的工具，这些工具有意不出现在默认的对话 Agent 提示词中。

`current_time` 和 `calculator` 未注册，因此不会出现在对话工具目录或 HTTP 工具目录中。请使用 `shell` 获取控制器本地的精确时间、时区以及进行确定性计算。

## 按平台区分的对话目录

对话 Agent 的工具目录会按全局 `[basic_settings.device].device_type` 状态过滤。有效的设备类型配置值为 `iOS`、`Android`、`macOS`、`windows` 和 `linux`；运行时会从这些值派生出小写的平台标识符（`ios`、`android`、`macos`、`windows`、`linux`）用于过滤工具目录。没有平台元数据的工具被视为可移植。

平台专用的工具应保持注册且 HTTP 可见，以便手动诊断，除非直接调用它们不安全或不可能。平台拆分只作用于面向模型的目录，这样模型就不会学到当前设备上无法运行的工具。

新增或修改工具时，遵循以下规则：

- 当某项能力在各平台上相同时，优先使用一个在内部读取运行时 `device_type` 的语义工具。
- 当工具无法在所有已配置的设备类型上运行、其 schema 或说明会暗示错误的平台行为，或支持的操作集存在实质差异时，在 `builtInToolSpecMetadata` 中添加平台元数据。
- 在 `builtInToolSpecMetadata` 中使用派生的平台标识符：手机配套能力使用 `ios` 和 `android`，Mac 桌面绑定使用 `macos`，只有在工具已有经过验证的对应平台绑定后，才添加 `windows` 或 `linux`。不要把这些小写标识符用作文档中的 `[basic_settings.device].device_type` 值。
- 不要要求模型为选择平台而传入 platform/device/os 参数；运行时会根据全局 `device_type` 状态推导平台。
- 如果工具有平台专用的子命令，应让其运行时 `ArgsSchema()` 只列出当前 `device_type` 下生效的子命令（例如 `quick_action.action` 和 `touch_gesture.type`）。
- 当一个名义上平台专用的命名空间存在可移植的子集时，在其他平台上只暴露该子集，而不是完全隐藏该命名空间；例如非 Android 平台的 `keyboard_tap` 可以只列出媒体键 `KEYCODE_*` 中的音量、媒体、截图和亮度别名。
- 每当引入某个工具或在平台分组之间移动工具时，为 `AgentToolsForPlatform`、`Runtime.availableTools()` 或平台专用的工具 schema 添加针对性的测试。

影响 HID 的工具调用一旦被接受，其执行就独立于客户端套接字，并有五分钟的服务端超时。当客户端通过 USB ECM 连接开发板时，这一点很重要：挂载或卸载 iOS 键盘会让 USB 复合设备短暂重新枚举，并可能断开 HTTP 连接，但工具会继续运行，并在结束时仍然卸载键盘。如果响应套接字已丢失，调用方可能需要重新连接并观察结果屏幕。这适用于键盘、指针/触摸、复合文本/搜索，以及可能参与串行化 HID 流程的快捷操作工具。在一次 HTTP 调用内，第一个键盘操作会挂载键盘，后续操作复用它；触摸输入不会卸载键盘。各次独立的 HTTP 调用各自管理自己的键盘作用域。其他 HTTP 工具保留正常的客户端取消行为。

HTTP 目录采用单独的策略。它会连同 `args_schema` 一起暴露已注册的操作类工具和专用工具，但 `skill_manage` 和 `skill_mark_used` 等内部维护工具永远不会通过默认的 HTTP 工具 API 列出或调用。

每个描述符还会报告对话模型如何看到该工具：`agent_exposed` 表示模型能否调用它，`agent_platforms` 列出显示该工具的设备平台（在所有平台上都显示时省略），`ui_operation` 标记用于观察或操作可见设备 UI 的工具，例如 `screenshot`、`touch_gesture` 和 `quick_action`。需要获取某个平台下模型 UI 操作面的客户端（例如基准测试框架的对比）应使用这三个字段筛选工具，而不是自行维护列表。

对于启动 App 的请求，先检查最新截图。如果所请求 App 的图标或卡片清晰可见、唯一且未被遮挡，就对其可见且不重叠的中心点使用 `touch_gesture`，而不是 `open_app`；仅当目标无法清晰点按，或直接点按没有产生经过验证的结果时，才使用 `open_app`。`open_app` 和 `open_url` 都会返回操作后的截图；回答或继续之前务必检查它。`ok:true` 仅表示操作系统接受了启动请求，并不表示所请求的 App 或页面已经可见。

Android 可见 UI 回退流程从记住的启动器搜索入口开始，首次使用时则从应用抽屉开始。其入口截图检查也会查找所请求的 App 图标。清晰识别出的启动器图标会被直接点按，并在返回成功之前验证打开后的 App 屏幕。如果看不到目标图标，则继续现有的搜索框和备选入口流程。这样可以避免搜索一个已经显示在抽屉中的 App。

缺少标准手势字段的对话 `touch_gesture` 调用会在捕获基线之前被拒绝。滑动必须包含 `type:"swipe"`、`start`，并同时提供 `end` 或 `direction` 之一。每个提供的 `point`、`start` 或 `end` 都必须是同时包含数值 `x` 和 `y` 的对象；零是有效值，而缺失或为 null 的坐标会在捕获之前被拒绝。错误信息中包含完整的点按和滑动示例，以便重试时保留所有必填字段。

## curl 示例

```bash
curl http://127.0.0.1:8080/api/tools

curl -X POST http://127.0.0.1:8080/api/tools/shell \
  -H 'Content-Type: application/json' \
  -d '{"input":{"command":"pwd"}}'

curl -X POST http://127.0.0.1:8080/api/tools/keyboard_text \
  -H 'Content-Type: application/json' \
  -d '{"input":{"text":"hello from API"}}'

curl -X POST http://127.0.0.1:8080/api/tools/screenshot \
  -H 'Content-Type: application/json' \
  -d '{"input":{}}'

curl -X POST http://127.0.0.1:8080/api/tools/weather \
  -H 'Content-Type: application/json' \
  -d '{"input":{"location":"Shanghai"}}'
```

成功的 `screenshot` 输出通常包含 `width`、`height`、`format`、`size` 以及 base64 编码的 JPEG `data`。
`screenshot` 接受可选的 `{"grounding":true}`；不传时，无论 UI Grounding 设置如何，截图都是普通截图。当 `ui_grounding_enabled` 开启时（默认），`touch_gesture`、`keyboard_tap`、`quick_action`、`mouse_move`、`mouse_scroll`、`enter_text`、`open_app` 和 `open_url` 的操作后截图也会以同样方式进行 grounding。此时 Agent 会在截图上运行 UI-DETR 组件检测器（参见 [UI grounding](ui-grounding.md)），在返回的图像上绘制带编号的 Set-of-Mark 框，并添加一个 `grounding` 对象 `{"elements":[{"id":1,"center":[x,y],"box":[x1,y1,x2,y2]},...]}`，使用 0-1000 归一化坐标（即 `touch_gesture` 接受的单位），最多 50 个元素，按检测器得分从高到低排列。`action_output` 保持为 `ok`。低分、重复以及与屏幕等大的框会被排除，因此某些控件可能不会被标注。如果检测失败，会返回普通截图，并由 `grounding.error` 给出原因；在没有检测器的设备上，错误会说明 grounding 不可用。
成功的 `wait_for_stable_screen` 输出包含稳定性字段 `ok`、`stable`、`elapsed_ms`、`frames_differed`，以及可选的 `ignored_motion_blocks` 和 `last_diff`，同时还返回一张包含 `width`、`height`、`format`、`size` 和 base64 JPEG `data` 的截图。该等待使用与操作后结构检测器相同的 40×80 块网格比较连续帧（忽略顶部 8%，范围限定在手机屏幕的有效区域内）；当某块的平均亮度变化超过 `screen_stable_diff_threshold`（默认 6）时视为该块发生变化，当持续 `screen_stable_ms` 期间发生变化的块不超过 1% 时，屏幕即视为稳定。在连续三次比较中都发生变化的块被视为持续运动（视频、轮播、时钟）并被排除，记为 `ignored_motion_blocks`，除非它们覆盖网格的 20% 以上，那种情况属于需要等待结束的过渡。`frames_differed=false` 表示在等待窗口内没有观察到帧变化；它不是操作后 `screen_changed` 的前后对比信号。`stable=false` 表示屏幕仍在变化，但该截图仍可作为当前观察结果使用。
在执行 `keyboard_tap`、`mouse_move`、`mouse_scroll` 或 `touch_gesture` 之前，系统会尽力捕获一个基线。执行成功后，它会等待屏幕稳定（或直到超时），并自动拍摄最终截图。它们的 `output` 是 JSON，包含原始操作结果 `action_output`、`screen_stable`、`stable_wait_ms`、可选的 `screen_changed`，以及最终截图的 `width`、`height`、`format`、`size` 和 base64 JPEG `data`。`enter_text` 同样会尽力捕获基线，并返回一张短延迟的操作后截图，但不执行稳定屏幕等待；其 `screen_stable` 和 `stable_wait_ms` 字段会被省略。存在时，操作后的 `screen_changed` 使用 PR #442 的结构检测器比较基线和最终截图：它忽略图像顶部 8%，对 40×80 块网格取平均，并要求超过 1% 的块超过颜色差异阈值。在绝对鼠标模式（iOS）下，指针之前所在位置以及操作结束后指针所在位置周围的块（每侧为截图宽度 5% 的正方形）不计入，因为即使点按没有命中任何东西，单是移动指针也会改变十几个甚至更多的块。该正方形也覆盖了在触摸点绘制的反馈，例如选中圈、放下的棋子或确认标记，因此 `tap`、`double_tap` 或 `long_press` 还会在那里额外检查：当点按点周围、截图宽度 3% 到 5% 之间的环形区域内的平均绝对亮度差超过 4（0-255）时，`screen_changed` 为 true。iOS 指针圆点始终位于内侧 3% 之内，在开发板上移动或悬停它对该环形区域造成的变化不到 1。如果尽力捕获的基线不可用，则省略 `screen_changed`；只有在该字段存在时，才把 `screen_changed=false` 视为没有有意义的变化。`last_diff` 存在时，仍只是稳定等待的诊断信息。
对于 `touch_gesture`，普通的点按、长按、滑动、滚动和可拖动目标都使用标准的 `type` 语法。`type:"tap_sequence"` 在一次调用中按顺序点按 `points`（2 到 32 个具名 `{x,y}` 对象），每次点按之间等待 `interval_ms`（默认 250，范围 80 到 2000）。仅当所有目标都在最新截图中可见、且点按不会改变布局时才使用它，例如数字键盘、计算器按键、PIN 码键盘或多个复选框。运行时总是会在两次点按之间检查屏幕；该检查无法关闭。它会把绝对模式指针停在第一个目标上，捕获一帧原始基线，并在每次点按后，把每个尚未点按的目标周围的区块（约为手机屏幕宽度的 7%，允许 2 像素偏移）与基线进行比较。当任何剩余目标（包括序列会再次点按的目标）的平均绝对亮度差超过 30 时，剩余的点按不会发送；刚被点按的按键上的指针和按下高亮远低于这一阈值。作为兜底，当手机屏幕进度网格中超过一半的块与基线不同时，序列也会停止。区块定位在手机屏幕的有效区域内，因此镜像手机画面周围的黑边不受影响。如果无法捕获基线帧，则不发送任何点按。`action_output` 是包含 `requested`、`executed`、`stopped_early` 和 `stop_reason` 的 JSON，操作后截图会标出每个已执行的点按。该序列是一次工具调用，因此只获得一张操作后截图和一次循环保护的进度检查。原子 `actions`（`touch_down`、`move_to`、`wait`、`touch_up`）是一个低频使用的高级选项，仅用于任何标准手势类型都无法表达的、不间断的自定义接触时序；这些操作作为一个保持接触的输入程序运行，每个以 `touch_down` 开始的序列都必须以 `touch_up` 结束。原子 `move_to` 接受可选的 `speed`（以每秒归一化坐标单位计）；其时长根据前一个点计算，而显式的 `duration_ms` 优先。两者都省略时保持瞬时移动。移动可拖动目标绝不能使用原子操作。在目标当前位置调用 `drag_start`；它会在保持接触的同时在内部执行屏幕稳定等待，并返回带有 `screen_stable` 的最终截图。当 `screen_stable=true` 时，根据该截图确认目的地，并在确认的点调用 `drag_release`。当 `screen_stable=false` 时，运行时会自动移回最初的 `drag_start` 点并释放接触；检查返回的截图，然后重试完整的拖动流程，而不是调用 `drag_release`。在正常的拖动流程中，不要单独调用 `wait_for_stable_screen`，也不要根据 `screen_stable=false` 的结果确定目的地。开始调用会按住 500ms，然后以每秒 500 归一化单位的速度（一次 400ms 的插值移动）移动 200 归一化单位，屏幕稳定后不释放；释放调用会直接移动，保持 200ms，然后释放。之前的单次调用 `type:"drag"` 已不再支持。带参数的滑动使用 `type:"swipe"`：提供 `start`，再加上 `end` 或 `direction` 之一；`speed` 默认为每秒 2500 归一化坐标单位。`duration_ms` 可选：对于 `start` + `end` 它会覆盖时长，对于 `start` + `direction` 它控制移动距离（`speed * duration_ms / 1000`）。不指定时，方向滑动会一直移动到相应边缘。`hold_before_ms` 和 `hold_after_ms` 是可选的停留时间（毫秒），分别位于移动之前和释放之前（默认 0）。`steps` 可选，用于设置 HID 插值次数（默认 24；值越大运动越平滑）。返回/主页回退是显式的贴边滑动，例如 `start.x=1` 或 `start.y=999`。
`weather` 支持地名或经纬度坐标，在运行时从 Open-Meteo 获取地理编码、当前天气和短期预报。
`wait_for_wakeup` 是一个终止型运行时工具。工具调用成功后，它会立即结束当前的 Agent 运行，并让语音交互回到等待下一次唤醒的状态；它不会要求模型再给出额外的最终回答。运行结果会设置 `wait_for_wakeup_requested` / `wait_for_wakeup_reason`；旧字段 `sleep_requested` / `sleep_reason` 仅作为兼容别名保留。应把道别、“没别的了”、停止聆听、进入空闲、待机、休眠或等待下一次唤醒视为针对该工具的对话控制请求，而不是设备或后台服务的操作请求。在同一个响应中说一句简短的告别，并在该轮次中调用 `wait_for_wakeup`；仅口头告别并不会结束 Classic 会话。
## 给外部 Agent 的建议

- 优先通过 `GET /api/tools` 发现能力
- 需要操作屏幕时，先 `screenshot`，再点击/输入
- 点击/输入操作成功后，直接检查该工具返回的操作后截图；无需立即再次调用 `screenshot`
- 鼠标和触摸使用 0-1000 归一化坐标
- 通过私有 IP 或 USB 网络适配器访问时，注意绕过代理：设置 `NO_PROXY` / `no_proxy`
- 对于长时间运行的 `shell` 任务，按照工具文档中的说明使用后台会话，并在完成后停止它们
- 当用户道别或要求休眠/停止聆听/进入空闲/等待下一次唤醒时，在同一轮次中伴随一句简短的告别调用 `wait_for_wakeup`；不要仅用普通文本回复假装进入待机
