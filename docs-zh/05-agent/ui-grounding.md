---
sidebar_position: 19
---

# UI Grounding

Agent 可以标注它在屏幕上检测到的界面组件，让模型按编号选择目标并点按测量得到的中心点，而不必凭目测估算像素。

## 设置

`[conversation_settings.agent].ui_grounding_enabled`（默认 `true`）控制 Agent 自行为模型截取的每一张截图。Config Web 在 Conversation & tools 页面中将其显示为 "Mark tappable elements automatically"（自动标注可点击元素）开关。Agent 实时读取该设置，因此修改后无需重启，下一张截图即生效。

| 截图 | 设置开启 | 设置关闭 |
| --- | --- | --- |
| 附加在每条用户消息上 | 带 grounding | 普通 |
| 在 `touch_gesture`、`keyboard_tap`、`quick_action`、`mouse_move`、`mouse_scroll`、`enter_text`、`open_app`、`open_url` 之后 | 带 grounding | 普通 |
| `screenshot` 工具 | 仅在 `{"grounding": true}` 时 | 仅在 `{"grounding": true}` 时 |
| `wait_for_stable_screen` | 普通 | 普通 |

`screenshot` 工具是一个普通的截图工具：无论设置如何，只有在模型传入 `grounding: true` 时才进行 grounding。

操作后的 grounding 在 `screen_changed` 比较（需要普通图像）之后、绘制触摸标记之前运行，因此检测器永远不会看到这些标记。在没有检测器的设备上，Agent 自行截取的截图保持普通，不带 grounding 也不带错误；只有显式的 `screenshot` 请求才会报告 grounding 不可用。

## 观察

对于带 grounding 的截图，Agent 会：

1. 解码裁剪后的手机截图，并以 RGB888 格式发送给 `ui_grounding_helper`。
2. 丢弃覆盖超过半个屏幕的框（那是页面或面板，而不是控件），然后把剩余的每个组件绘制为彩色框，并在框左上角正上方标注编号（Set-of-Mark），使标签不会遮挡控件。在屏幕顶部边缘，标签会放在角内。标签大小随截图尺寸缩放。
3. 将标注后的图像作为观察结果返回，并在工具输出中添加一个 `grounding` 字段。工具的 `action_output` 不变，模型读取的工具结果也不变。元素列表放在携带图像的状态消息中，位于其说明文字之后（对于用户消息截图，位于设备状态行之后）：

```text
UI grounding: the numbered boxes in this image are these elements, with center and box in touch_gesture coordinates: {"elements":[{"id":1,"center":[500.0,260.4],"box":[62.5,231.7,961.7,289.2]}]}
```

坐标遵循 `touch_gesture`，因此中心点可以原样传给它。截图工具报告的是返回截图的 0-1000 归一化坐标（`pixel / (size - 1) × 1000`，保留一位小数），这也是 `touch_gesture` 和 HTTP API 所接受的格式。当视觉坐标协议生效时（Anthropic 模型），`touch_gesture` 改为接受截图像素，Agent 会在模型读取之前把 grounding 字段转换为相同的像素坐标。元素按检测器得分排序，最多 50 个，通常更少。标注后的图像也会成为 shell 导出（`$AICO_SCREENSHOT_JPEG`/`PPM`）和坐标检查所用的最新截图。

如果检测失败，会返回普通截图，并由 `grounding.error` 给出原因，元素列表为空；状态消息会说明 grounding 失败，因此观察结果绝不会丢失。在没有辅助程序和模型的主机或基准测试容器上进行显式请求时，错误会说明 grounding 不可用。

## 检测器

检测器是 UI-DETR V6 学生模型：一个单类别 YOLOv5 模型，输入为 960×960，针对 RV1106 NPU 量化为 INT8。它只查找“界面组件”。它不识别文字，也不对控件分类，并且可能漏掉某个控件，或对同一个控件框出多次，因此模型仍需通过阅读图像来选择目标。模型、锚框和来源信息位于 [`assets/business/models/ui-detr/`](../../assets/business/models/ui-detr/README.md)。

预处理和解码与经过验证的开发板运行器一致：半像素双线性 letterbox，填充值为 114，得分等于 objectness 乘以类别概率，在归一化的源坐标上做 NMS，最多 50 个框。该运行器针对召回率调优（置信度 0，NMS IoU 0.8），会在一个控件上留下多个几乎相同的框。Agent 改为传入 `--confidence 0.5
--nms-iou 0.5`，使每个标记都彼此区分。部分控件可能不会被标注。

## 辅助进程

`/usr/lib/aico/ui_grounding_helper` 使用 `/usr/lib/aico/models/ui-detr/` 中私有的 RKNN Mini Runtime 2.5.0 加载模型，因为该模型是用 Toolkit2 2.5.0 编译的。音频辅助程序使用的静态 2.3.2 运行时与此无关。

当 UI Grounding 设置开启时，模型常驻内存：Agent 在启动时或设置被打开时（先于第一个请求）加载辅助程序，并保持运行、不会因空闲退出。协议错误或推理失败会停止辅助程序，Agent 会在后台重新加载它。
关闭该设置会停止辅助程序，并释放其约 40 MB 的 CMA。设置关闭时，显式的 `screenshot` grounding 请求会按需启动辅助程序，并在两分钟内无请求后退出。

### 模型加载与内存规整

NPU 缓冲区来自 CMA，CMA 有足够空间容纳它们，但内核还需要一块物理连续的 order-4（64 KB）普通内存块来跟踪它们。在空闲内存只有几 MB 的开发板上，这样的内存块经常缺失：此时 `rknn_init` 返回 `-1` 失败，内核记录 `page allocation failure: order:4` 和 `RKNPU: dmabuf alloc failed`，单纯重试无济于事。因此，辅助程序在加载失败后会规整内存（`/proc/sys/vm/compact_memory`，需要 root，Agent 正是以 root 运行），然后重试，最多尝试三次。在开发板上，三次普通重试都失败了，而规整后的第一次加载就成功了。

协议（stdin 为二进制，小端序；stdout 为文本行）：

| 请求 | 响应 |
| --- | --- |
| 启动（`--confidence C --nms-iou T`，均为 0 到 1，默认 0 和 0.8） | `READY <model_width> <model_height>` |
| `'D'` u32 宽度、u32 高度、RGB888 字节 | `OK <n> <inference_ms>`，然后是 `n` 行源像素坐标 `<x1> <y1> <x2> <y2> <score>` |
| `'Q'` | 退出 |
| 任何失败 | `ERR <message>` |

在设备上检查辅助程序和模型：

```bash
/usr/lib/aico/ui_grounding_helper --runtime /usr/lib/aico/models/ui-detr/librknnmrt.so \
  --model /usr/lib/aico/models/ui-detr/model.rknn \
  --anchors /usr/lib/aico/models/ui-detr/RK_anchors.txt --self-test
```

自检会让一张空白页面经过 NPU，并打印 `OK` 响应。

## 开销

在 RV1106G3 上的源测量显示，每张截图的稳态中位数为 666 ms，P95 为 816 ms，不含初始化（中位数 233 ms）和图像解码。设置开启时，每条用户消息以及每个输入、文本录入和启动操作都要付出一次这样的开销。关闭该设置可以节省这部分时间；模型仍然可以通过 `screenshot` 工具请求 grounding。系统提示词中的角色规则和 device-operator Skill 会告诉模型：当某个框覆盖目标时，原样点按该编号元素的中心点。
