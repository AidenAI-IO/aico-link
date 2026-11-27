---
sidebar_position: 4
---

# Device CLI

后台 Agent 和独立实时会话通过前台 `shell` 调用使用 `device-cli`。CLI 会将操作转发给现有的 [Tools HTTP API](tools-http-api.md)。支持图片的实时提供商会随 shell 结果收到最终截图。

```sh
device-cli tap 500 650
device-cli swipe 500 800 500 300
device-cli tap 500 650 && device-cli app type 'Hello'  # when a companion route is available
```

横纵坐标均归一化为 0–1000。一个前台 shell 最多执行 64 项设备操作，并附加一张最终截图。开启 UI Grounding 且检测器可用时，截图会包含带编号的框和归一化坐标。每次成功点按后固定等待 50 毫秒。下一步依赖新界面时，请拆成单独调用。命令链失败或超时后，先检查最终画面，因为前面的操作可能已经执行。若画面已旋转，方向检查可在派发前拒绝坐标输入。

使用任一文本命令前，先聚焦目标编辑框。两种命令都不接受焦点坐标。

- `device-cli type TEXT` 使用设备输入，优先使用原生输入，其他情况下使用 HID/IME。
- `device-cli app type TEXT` 使用 Phone Bridge 剪贴板路径，写入请求文本或确认已准备的剪贴板，再向当前焦点发送粘贴快捷键。命令报告剪贴板操作和粘贴派发结果，前台 shell 会附加最终截图。若派发失败且文本可能已插入，先检查画面再重试。iOS 未启用 PiP 时，需要最近准备的匹配剪贴板和可用的直接读取路径。

当 `app_connected`、`app_pip_enabled` 或 `app_fgs_enabled` 为真时，优先考虑 `device-cli app type`。多行文本使用一个包含实际换行的单引号参数。shell 工具的 JSON 示例为 `{"command":"device-cli app type 'First line\nSecond line'"}`。避免 Bash 风格的 `$'...'` 和作为文本传入的字面量 `\n`。

支持的 App 数据操作还可以使用 BLE Wake 或返回入口。没有活动 shell 作用域时，使用 `--url` 直接指定 daemon，默认地址为 `http://127.0.0.1:8080`。

`drag` 在控制器上完成两个阶段。shell 失败或取消且触点仍按下时，会尝试在起点释放触点。成功的 `drag-start` 会保留接触，供之后调用 `drag-release`。直接 HTTP 拖动保留原有验证流程。

## 命令帮助

运行 `device-cli --help` 查看命令。将 `--help` 紧跟在命令、命令组或操作之后可查看对应语法，例如 `device-cli app contacts --help` 或 `device-cli app calendar create --help`。帮助写入标准输出，无需访问 daemon。要传入字面量 `--help`，使用 `device-cli type -- --help`、`device-cli app type -- --help` 或 `device-cli app clipboard write -- --help`。

## 退出状态与标准输出

退出状态 `0` 表示命令成功。失败返回非零状态，并将错误写入标准错误。操作成功后仍可能需要检查画面。标准输出承载结果数据。

| 结果 | 标准输出 |
| --- | --- |
| 只有确认信息 | 无，例如 `wait`、成功的点按、输入和剪贴板写入。 |
| 单个字符串字段 | 原始字符串，无 JSON 引号和附加换行，例如剪贴板文本或新事件 ID。 |
| 单个非字符串字段 | 单独的 JSON 值，例如 `app calendar query` 或 `app contacts query` 返回的数组，无匹配时为 `[]`。 |
| 多个数据字段 | JSON 对象，例如 `quick` 的路径和操作详情。 |

CLI 从结构化结果中省略 `ok` 和内部字段 `restored_from_return_entry`。`look` 单独输出保存的 JPEG 路径。Tools HTTP API 保留现有响应格式。

## 配套 App 数据

这些命令按操作检查可用路径和权限，无附加截图。

```sh
device-cli app clipboard read
device-cli app clipboard write 'Hello'
device-cli app calendar query --from 2026-11-23T00:00:00+08:00 --to 2026-11-24T00:00:00+08:00
device-cli app calendar create --title 'Team sync' --start-at 2026-11-23T09:00:00+08:00 --end-at 2026-11-23T09:30:00+08:00
device-cli app calendar delete --event-id EVENT_ID
device-cli app contacts query --query Alice --limit 5
device-cli app contacts create --name Alice --phone-number '+15550100'
device-cli app contacts update --contact-id CONTACT_ID --organization 'Example'
device-cli app notification send --title Reminder --body 'Check the calendar' --sound=false
```

读取剪贴板时，标准输出只包含文本，不额外添加换行。空剪贴板无输出且成功退出。读取失败以非零状态退出。将结果存入 shell 变量时，先检查赋值的退出状态，再用引号包裹变量作为参数。shell 的命令替换会移除末尾换行。

日历、联系人和通知发送使用命名参数。日历查询的 `--start` 和 `--end` 分别等同于 `--from` 和 `--to`，每个边界只指定一次。多个电话号码或邮箱可重复使用 `--phone-number` 或 `--email`。日历时间和通知的 `--schedule-at` 使用带时区的 RFC3339。省略 `--schedule-at` 会立即发送。创建或删除日历事件前请确认详情。

模型通过这些命令操作剪贴板、日历、联系人和发送通知。底层 Bridge handler 仍供 CLI、Tools HTTP API 和旧调用方使用。查询通知继续通过 `bridge_notification` 的 `action=query` 读取开发板的通知缓冲区。顶层 `open` 和 `url` 保留现有的路径选择和 UI 回退。

## 键盘快捷键

`device-cli key meta+a` 和 `device-cli key meta a` 都会同时按下 Meta 和 A。支持多个修饰键，例如 `key ctrl+shift+tab`。全选等平台语义操作优先使用 `device-cli quick select_all`。

未知按键或格式错误的组合键会返回非零状态。在 Agent shell 中，即使管道或后续成功命令掩盖了 CLI 的退出状态，失败的 `key` 请求也会将工具结果标记为失败。需要在失败后停止时，使用 `&&` 并避免掩盖退出状态的管道。

## 查看与保存截图

```sh
device-cli wait 0                                      # Attach the final shell screenshot without a delay
device-cli look                                        # Save a JPEG in /tmp
device-cli look --dir /tmp --region 100 200 900 800     # Save a cropped JPEG
```

`look` 输出新 JPEG 文件的绝对路径。目标目录必须已经存在且可写，默认且推荐使用 `/tmp`。Agent 可使用 `read_image` 和输出的路径查看图片。`--region X1 Y1 X2 Y2` 使用归一化整数坐标（0–1000）设置左上角和右下角。`look` 只保存文件，shell 最终截图由原有流程提供。

`wait N` 固定等待毫秒数（0–30000，默认 2000），不会轮询画面稳定性或拍摄中间帧。在前台 Agent shell 中，退出时会捕获最终截图。独立 `wait` 只等待，无需访问 Agent。

## ConAct 与 Skill-UTG

在连接设备的后台 Device CLI 运行中，ConAct 和 Skill-UTG 始终启用，包括 MobileGym 等环境桥接。独立 CLI、独立实时会话、无设备和脚本化基准测试不启用这两项功能。

- **`fold_subtask`** 在明确的子任务边界记录经验证的事实和下一目标。它用这些内容和一张新截图替换当前子任务，保留磁盘上的原始会话修订版本以及用户消息和附件。自上一个边界以来至少需要派发一次 UI 操作。当已验证的视觉子任务完成且还有目标待执行时，系统提示会要求 Agent 折叠。截图观察达到十四张时，备用机制保留最新的六张。提供商溢出和输入预算保护继续生效。
- **`utg_search` 和 `utg_goto`** 提供查找结果和路线指导。图谱区分 `nav`、`create`、`edit`、`commit`、`cancel` 和 `delete`。默认路线仅使用导航边，其他指导需要显式指定末段意图。只有观察到的应用启动和回主页操作才可建立导航边。执行操作前，Agent 必须在实时画面中验证地标。可用地标的简短列表只出现在新对话的初始状态消息。成功的记忆工具调用使用单独的有限额度。

运行生成最终回复后，UTG 可以发起一次尽力而为的后台反思请求，超时为 30 秒。同一个 Agent 会跳过重叠的反思。反思使用已完成对话的快照和有限的、可归因到单个操作的控制器转换记录。命令链中的多个 UI 操作不会被当成独立边学习。固定词表内的标签和指导以及粗粒度截图指纹存放在 `<config-dir>/memory/skill-utg/<platform>.json`，按本地 Agent 配置和目标平台隔离。图谱不保存原始截图或命令参数。原生和环境桥接运行均可用，建议为不同设备和敏感工作使用单独的配置。图谱不会自动返回主页或执行建议路线。

Agent Web UI 的调试链接打开 `/debug`。在 **Coordinate** 中可检查截图或发送归一化点按，在 **UTG**（`/debug#utg`）中可查看已保存的地标、转换和提示。UTG 页面只读，可在有近期截图时与其匹配。刷新页面不会触发截图或设备操作。
