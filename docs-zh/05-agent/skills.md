---
sidebar_position: 4
---

# Skill 与 RoleProfile 机制

Agent 会自动从配置目录中发现 `SKILL.md` 文件，在系统提示词中展示 Available skills 目录，并通过 `skill_read` 按需加载相关 Skill 的完整说明。被激活的 Skill 会注入 Agent 的 `RoleProfile`，其中包含系统提示词、Skill 说明和可用工具。

## 目录结构

```text
/userdata/agent/
└── skills/
    └── my-skill/
        └── SKILL.md
```

扫描规则：

```text
configDir/skills/**/SKILL.md
```

## 示例

```markdown
---
name: ui-operator
description: Prefer screenshot-driven UI inspection before clicking.
metadata:
  device_types: [iOS, Android]
  allowed_tools: [screenshot, touch_gesture, keyboard_tap, enter_text]
---

Take a screenshot before interacting with an unfamiliar UI.
Prefer describing what you see before clicking.
```

`[basic_settings.device].device_type` 和 `metadata.device_types` 使用相同的规范值：`iOS`、`Android`、`macOS`、`windows` 和 `linux`。为了兼容，也接受小写别名，但文档和新的 Skill 元数据应使用规范形式。

## 当前已实现的能力

- 自动发现 `SKILL.md`；
- 在提示词中展示 Available skills，并在运行时通过 `skill_read` 加载完整的 `SKILL.md`；
- 通过 `RoleProfile` 将 Skill 说明注入 Agent 的系统提示词；
- 解析 `allowed_tools` 元数据用于校验和未来兼容，但目前工具可用性由静态的 Agent 工具目录控制，而不是由激活的 Skill 决定；
- 根据全局 `device_type` 状态，按 `metadata.device_types` 过滤自定义 Skill；
- 向 Agent 提供 `skill_list` / `skill_read` / `skill_manage` / `skill_mark_used`；HTTP 工具 API 只暴露非维护类的工具（`skill_list` / `skill_read`）；
- `skill_read` 支持读取 `SKILL.md`，以及 `references/`、`templates/`、`scripts/`、`assets/` 下的 UTF-8 辅助文件；
- 当文件能放入模型上下文时，`skill_read` 会完整加载该文件；每个文件上限为 64 KiB。上下文紧张时，Agent 会在下一次模型调用前修剪或压缩较早的上下文，且当前轮次的修剪不会把已完整加载的 Skill 替换为占位符。如果该文件无法与当前请求及本轮已加载的 Skill 一起放入上下文，其完整结果会保存为一个 artifact。Agent 会收到一个标记为不完整的有限预览，并可以按有限范围读取该 artifact；它不得把预览当作完整的 Skill 说明；
- 在 `usage.json` 中记录查看/使用/修改统计；
- 支持 `active` / `stale` / `archived` 生命周期状态；`skill_list` 默认过滤掉 archived；
- 对于带有 `source: agent` 或 `created_by: agent` 的 Skill，`skill_list` 会根据最后使用时间自动执行生命周期：未使用 90 天后进入 `stale`，180 天后进入 `archived`；这一自动扫描每 24 小时最多运行一次；
- `skill_mark_used` 会自动把 `stale` / `archived` Skill 恢复为 `active`。

## 内置 Skill

业务软件包会把内置 Skill 安装到 `/usr/share/aico/skills/` 下。启动时，Agent 会把它们复制到 `/userdata/agent/skills/`，并在 `/userdata/agent/skill-state/.bundled_manifest.json` 中记录同步状态。

- `SKILL.md` 在缺失时复制，在未被编辑时更新；当内置副本和本地副本都发生变化时，会借助所配置的模型进行合并。
- `references/`、`templates/`、`scripts/` 和 `assets/` 下的辅助文件在缺失时复制，并在本地副本仍与上次同步的版本一致时更新。用户编辑或删除过的文件保持不动；从内置包中移除的文件仅在未被编辑时才会删除。辅助文件不会合并。
- 恢复某个内置 Skill 会用软件包中的副本覆盖 `SKILL.md` 和所有内置辅助文件。

内置的 `dataviz` Skill 是带辅助文件的 Skill 示例：它按需从 `references/` 加载图表指南，并通过 `shell` 运行 `scripts/validate_palette.py` 来检查调色板对比度和色盲区分度。它把每张图表写成 `/share/charts/` 下一个独立的 HTML 文件，以供下载。

## Agent 执行

Agent 使用精简的执行循环，在单一统一流程中处理工具调用并生成响应。整个执行过程中 Agent 都可以使用所有工具，并直接管理自己的任务拆分和执行策略。

工具执行集成在主循环中，并通过回调处理器实现流式响应和 episode 记录。

## 设备专用 Skill

自定义 Skill 可以声明它们支持哪些已配置的目标设备类型：

```markdown
---
name: wechat-login-android
description: WeChat login flow for Android.
metadata:
  device_types: [Android]
  allowed_tools: [screenshot, touch_gesture, enter_text, quick_action]
---

Follow the Android-specific login flow.
```

规则：

- 适用于所有设备类型的通用 Skill 应省略 `metadata.device_types`；
- 支持的值与 `[basic_settings.device].device_type` 一致：`iOS`、`Android`、`macOS`、`windows` 和 `linux`；
- 加载 Skill 时，会对 `ios`、`android`、`mac`、`macos` 和 `win` 等常见别名进行规范化；
- `Available skills` 只显示与当前全局 `device_type` 兼容的 Skill；
- `skill_list` 和 `skill_read` 默认也会隐藏不兼容的 Skill；
- 如需显式查看或维护，可向 `skill_list` 或 `skill_read` 传入 `include_incompatible: true`；
- `skill_manage` 会在 create/edit/patch 时校验 `metadata.device_types`，但仍可管理 `configDir/skills` 下的任意 Skill 文件。

对于平台专用的工作流，如果说明差异较大，优先使用独立的 Skill，例如 `wechat-login-android` 和 `wechat-login-ios`。只有当大部分流程相同、且正文可以根据全局 `device_type` 清晰分支时，才使用一个带多个 `device_types` 的 Skill。

## 已解析但未完全强制执行的字段

- `preferred_model`
- `allowed_tools`
- `allowed_children`

这些字段可作为未来扩展的元数据，但当前运行时不应依赖它们来强制执行。

## HTTP 工具 Skill 导出

Agent 提供：

```text
GET /api/tool-skills
```

该端点会生成一个描述 Aico HTTP 工具 API 的 Skill 包，方便 Codex 等外部 Agent 以统一方式调用设备工具。

## 使用建议

- Skill 内容应描述高层策略；不要硬编码易变的坐标；
- 对于 UI 操作类 Skill，建议要求在点按前先截图；
- 为了文档清晰和向前兼容，`allowed_tools` 应保持精简，但当前运行时的工具可用性不会因激活的 Skill 而扩展；
- `allowed_tools` 只能引用当前已注册的工具，或 `delegate_<child>` 形式的子 Agent 委托伪工具；
- 不要把一次性任务进度、临时状态、密钥、原始日志或个人事实写入 Skill；这些内容不属于可复用的流程。
