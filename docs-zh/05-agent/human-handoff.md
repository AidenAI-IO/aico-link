---
sidebar_position: 14
---

# 人工转交

当下一步需要隐私输入、用户批准、人工判断，或可用工具无法安全执行的操作时，人工转交可让 Agent 暂停任务。

转交以对话方式进行，而不是阻塞等待。工具会立即返回，Agent 向用户说明需要做什么，然后结束当前轮次。用户报告完成后，Agent 会再次观察屏幕，然后才继续。

## 交互流程

1. Agent 观察到某一步需要人工操作。
2. 它调用 `request_user_action`，并提供原因、详情以及可选的建议操作。
3. Agent 告诉用户该做什么，但不会在聊天中索要隐私凭据。
4. 用户直接在手机上完成操作，完成后回复。
5. Agent 重新截图或使用其他合适的观察方式来验证新状态。
6. 任务在新的对话轮次中继续。

## 转交原因

| 原因 | 用途 |
| --- | --- |
| `authentication` | 需要输入密码或其他隐私凭据。 |
| `login_method_selection` | 用户需要选择账号或登录方式。 |
| `captcha` | 需要人工完成 CAPTCHA 验证。 |
| `verification_code` | 需要短信、邮件、身份验证器或其他验证码。 |
| `sensitive_operation` | 支付、删除、授权或其他有重大后果的操作需要批准。 |
| `redirect_confirmation` | 用户需要批准离开某个 App 或打开外部目标。 |
| `permission_confirmation` | 操作系统权限提示需要用户审阅。 |
| `black_screen` | 无法观察屏幕，通常是因为某个 App 阻止截屏。 |
| `ambiguous_situation` | 存在多个有效选项，需要用户的偏好或判断。 |
| `unsupported_action` | 所请求的步骤超出了可用工具或平台的能力范围。 |
| `stuck` | 观察和安全的回退尝试都无法取得进展。 |
| `other` | 其他确实需要人工介入的情况。 |

## 工具契约

工具名称：

```text
request_user_action
```

输入：

```json
{
  "reason": "authentication",
  "details": "The App Store login screen requires the account password.",
  "suggested_action": "Enter the password on the phone, submit the form, and tell me when it is complete."
}
```

字段：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `reason` | 是 | 上文列出的原因值之一。 |
| `details` | 是 | 解释 Agent 为何无法安全继续的具体上下文。 |
| `suggested_action` | 否 | 关于用户应执行操作的简明指示。 |

工具会立即返回一个结构化标记：

```json
{
  "status": "HUMAN_HANDOFF_REQUESTED",
  "reason": "authentication",
  "details": "The App Store login screen requires the account password.",
  "suggested_action": "Enter the password on the phone, submit the form, and tell me when it is complete."
}
```

对于后台任务，同一个工具会感知模式，并向编排层返回 `USER_ACTION_REQUESTED`。任务保持 `running` 状态，而其 Agent 循环结束。实时前台 Agent 向用户呈现该操作；用户确认完成后，它调用 `response_user_action`。普通的旧式运行仍返回 `HUMAN_HANDOFF_REQUESTED`。

## Agent 行为

- 说明为什么需要帮助，并指明需要执行的具体操作。
- 请用户直接在被控设备上输入密码、验证码或支付信息，绝不要在聊天中输入。
- 请求转交后结束当前轮次，而不是反复轮询或重试被阻塞的操作。
- 用户返回后，先观察再操作。仅凭“完成了”之类的说法本身不足以作为证据。
- 如果仍无法截图，尽可能使用其他可见证据或系统证据，并说明仍存在的不确定性。
- 不要把人工转交作为对可恢复工具错误的第一反应。先尝试安全的观察和受支持的回退路径。

## 示例

### 身份验证

```json
{
  "reason": "authentication",
  "details": "The login screen requires a password that the Agent does not have.",
  "suggested_action": "Enter the password on the phone and submit the login form."
}
```

### 敏感操作确认

```json
{
  "reason": "sensitive_operation",
  "details": "The checkout screen is ready to confirm a payment of ¥299.",
  "suggested_action": "Verify the recipient and amount, then approve or cancel the payment."
}
```

### 截屏被阻止

```json
{
  "reason": "black_screen",
  "details": "The banking app returns a black screenshot, so the current fields and controls cannot be verified.",
  "suggested_action": "Complete the required step manually in the banking app and return to a screen that can be observed."
}
```

## 验证

在 `src/agent` 中运行针对性的单元测试：

```bash
go test ./internal/agent -run TestHumanHandoff
```

实现和原因允许列表定义在 `internal/agent/tools_human_handoff.go` 中。
