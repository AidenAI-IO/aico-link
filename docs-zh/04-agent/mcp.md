---
sidebar_position: 3.5
---

# MCP

## 为 Agent 安装 MCP 工具

打开 **Config Web → Advanced → MCP → Install MCP for Agent → Install MCP**。在本地 Agent 设置中，从导航中打开 **MCP**。粘贴服务器提供商给出的 `mcpServers` JSON，然后选择 **Install and connect**。Agent 会先初始化每个服务器并发现其工具，然后再保存导入结果。导入成功后，对话 Agent 在下一次请求时即可使用这些工具，无需重启。导入失败不会改变已安装的目录。服务器名称必须由 1–32 个字母、数字、下划线或连字符组成。已存在的名称必须先卸载，才能导入替换项。

远程 Streamable HTTP 示例：

```json
{
  "mcpServers": {
    "remote": {
      "type": "http",
      "url": "https://your-provider.example/mcp",
      "headers": {"Authorization": "Bearer YOUR_PROVIDER_TOKEN"}
    }
  }
}
```

本地 stdio 示例：

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem@2.4.0", "/path/to/allowed/files"]
    }
  }
}
```

该命令**在 Agent 主机上**以 Agent 的权限直接运行，不经过 shell。只安装可信的服务器。Node.js/npm（`npx`）、Python/uv（`uvx`）或其他所需运行时必须已经安装，并且 Agent 服务可以访问。桌面路径并不指向开发板的文件系统。软件包启动器可能会下载依赖，因此首次启动需要网络访问；如果启动时间超过 30 秒的连接超时，请预先安装软件包并使用可执行文件的绝对路径。`env` 为 stdio 服务器提供额外的环境变量。传输方式可以省略：有 `url` 时选择 HTTP，否则由 `command` 选择 stdio。`streamable-http` 也可作为 HTTP 类型使用。`disabled: true` 会保存条目但不启动它。

选择一个已安装的服务器，即可查看其工具、启用/禁用、重新连接或卸载。卸载会删除保存的连接并关闭会话；软件包缓存或提供商侧的数据不会被删除。Agent 启动时，已启用的服务器会在后台重新连接。如果连接失败，在修复运行时、端点或凭据后使用 **Reconnect**。目录在连接/重新连接时刷新；工具列表的动态变化需要重新连接才能生效。

凭据存储在 `agent.toml` 旁边的 `mcp-servers.json` 中，权限为 `0600`。管理接口的响应只包含状态和工具名称，绝不包含已保存的环境变量或 HTTP 头。不要分享此文件。配置导入限制为 64 KiB 和 16 个已安装服务器；每个服务器最多可暴露 256 个工具。工具调用在 60 秒后超时。导入的名称使用稳定的 `mcp_<server>_<tool>_<hash>` 命名空间，并保留服务器的 JSON 参数 schema、文本/结构化内容以及工具错误结果。其他 MCP 内容块以 JSON 形式保留；此版本不会把 MCP 图像/音频作为原生多模态模型输入附加。这些工具可供对话 Agent 使用，但不会通过设备的 `/mcp` 或 `/api/tools` 端点再次导出。

该客户端支持基于 stdio 和 Streamable HTTP（JSON 或 SSE 响应）的工具服务器。不支持旧版 HTTP+SSE 端点、OAuth 登录、resources、prompts、sampling 和 elicitation。对于需要 token 的提供商，请配置其 HTTP 头或 stdio 环境变量。仅提供 resources 或仅提供 prompts 的服务器会被拒绝，并附带说明性错误。

### 三个参考服务器

集成测试使用具有不同能力的独立参考实现：

| 服务器 | 能力 | 已验证的操作 |
| --- | --- | --- |
| `@modelcontextprotocol/server-filesystem@2.4.0` | 限定在允许目录内的文件 | 读取一个临时文本文件 |
| `@modelcontextprotocol/server-memory@2.2.1` | 持久化知识图谱 | 创建一个带观察记录的实体 |
| `@modelcontextprotocol/server-sequential-thinking@2.2.0` | 有状态的推理步骤 | 提交最终想法 |

为了可重复验证，请把这些版本安装到检出目录之外的临时 npm prefix 中，然后在固定版本的测试镜像中、挂载 Node.js 24+ 和该 prefix 的情况下运行针对性测试。这些参考软件包需要 Node.js 24+，而标准测试镜像为常规测试套件提供的是 Node.js 22。对于这个可选测试，也把 Node.js 安装到临时 prefix 中：

```bash
npm install --prefix /tmp/aico-mcp-node --no-save --package-lock=false node@24.13.1
export PATH="/tmp/aico-mcp-node/node_modules/node/bin:$PATH"
npm install --prefix /tmp/aico-mcp-reference --no-save --package-lock=false \
  @modelcontextprotocol/server-filesystem@2.4.0 \
  @modelcontextprotocol/server-memory@2.2.1 \
  @modelcontextprotocol/server-sequential-thinking@2.2.0
# Inside the pinned test container, from src/agent:
AICO_MCP_REFERENCE_ROOT=/tmp/aico-mcp-reference \
  go test -count=1 -v -run '^TestMCPReferenceServers$' ./internal/mcpclient
```

该测试会自行创建允许的目录和记忆文件。它会发起真实的 MCP 工具调用，但不需要 LLM 提供商或物理设备。确定性测试还覆盖了 stdio、JSON HTTP 和 SSE HTTP、凭据、禁用/卸载、过期的工具句柄、持久化/重启、导入回滚以及取消。关于 Docker 测试流程，参见[测试与验证](../01-getting-started/testing.md)。

### 管理 API

Config Web 提供以下需要认证的端点（本地设置要求同源的回环请求）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/mcp/servers` | 状态和已发现的工具名称 |
| `POST` | `/api/mcp/servers` | 导入 `mcpServers` JSON 文档 |
| `PATCH` | `/api/mcp/servers/<name>` | `{"enabled": true}` 连接/重新连接；`false` 禁用 |
| `DELETE` | `/api/mcp/servers/<name>` | 卸载已保存的连接 |

Config Web 只代理到 Agent 固定的回环管理端点。设备的 MCP 访问 token 无权进行安装或管理。

## 将外部客户端连接到设备

Agent HTTP 服务器在设备上的单个端点上，通过 [Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http) 暴露 Model Context Protocol。

## 端点

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/mcp` | MCP JSON-RPC（initialize、tools/list、tools/call 等） |

URL 示例：

- Wi-Fi：`https://<board-ip>:443/mcp`（Config Web HTTPS 反向代理）
- USB ECM：`https://192.168.42.1:443/mcp`
- Config Web HTTPS 禁用时的 USB 直连回退：`http://192.168.42.1:8080/mcp`

Wi-Fi 客户端需要启用 Config Web HTTPS。连接前，请安装并信任开发板的本地 TLS CA。HTTPS 端点会把 MCP 代理到回环地址上的 Agent，因此无需启用 LAN 访问。如果开发板 IP 发生变化（DHCP），请更新客户端 URL。对设备地址绕过 HTTP 代理（`NO_PROXY` / `no_proxy`）。

## 启用 / 禁用

| 设置 | 默认值 | 位置 |
| --- | --- | --- |
| `mcp_enabled` | `true` | `agent.toml` 中的 `[conversation_settings.agent]`，或 Config Web → Advanced → MCP |
| `mcp_shell_enabled` | `false` | 仅 `[conversation_settings.agent]`（TOML；Config Web 中没有） |
| `mcp_config_web_base_url` | `http://127.0.0.1` | 仅限回环 HTTP；供 `device_status` / `support_bundle` 使用 |

当 `mcp_enabled=false` 时，`POST /mcp` 返回 404。语音和其他 HTTP API 不受影响。`mcp_shell_enabled` 只控制通过 `/mcp` 访问 `shell`；`POST /api/tools/shell` 仍可在同一 Agent 端口上按其自身规则单独访问，但永远不接受 MCP 访问 token。

## 认证

在开发板上，`/mcp` 要求每个客户端都提供 MCP 访问 token：直接插在 Aico Link 上的电脑、通过 Wi-Fi 连接的电脑，以及开发板自身上的进程都是如此。这与 LAN 访问无关。在 Config Web → Advanced → MCP → Create token 中创建 token；密钥只显示一次，并附带可直接粘贴的配置。token 只能访问 `/mcp`，不会过期，修改设备密码后依然有效，并可在同一页面上单独吊销。`POST /api/auth/login` 返回的会话 token 不能用于 `/mcp`。API 参见 [MCP 访问 token](../03-services/device-management-api.md#mcp-访问-token)。

| 客户端 | 需要 |
| --- | --- |
| 同一 Wi-Fi（`https://<board-ip>:443/mcp`），LAN 访问开启或关闭 | 受信任的开发板 TLS CA 和 MCP 访问 token |
| 直接插在 Aico Link 上（`https://192.168.42.1:443/mcp`） | 受信任的开发板 TLS CA 和 MCP 访问 token |
| USB 直连回退（`http://192.168.42.1:8080/mcp`） | 物理 USB 连接和 MCP 访问 token |
| 不带 `-lan-guard` 运行的 Docker 或主机 | 无 |

没有有效 token 时，`/mcp` 返回 `401`，并带有 `"error": "mcp_token_required"`。
其他 Agent 路由保持各自的规则：开发板上的客户端无需凭据；设置了设备密码时，其他客户端需要登录（LAN）或配对（USB）。

Claude Code：

```bash
claude mcp add --transport http aico https://<board-ip>:443/mcp \
  --header "Authorization: Bearer aico_mcp_..."
```

使用 JSON 配置的客户端：

```json
{
  "mcpServers": {
    "aico": {
      "type": "http",
      "url": "https://<board-ip>:443/mcp",
      "headers": {"Authorization": "Bearer aico_mcp_..."}
    }
  }
}
```

请通过 IP 访问开发板：Agent 会拒绝 `Host` 为主机名的请求（参见[跨站请求](../03-services/device-management-api.md#跨站请求)）。

## 允许列表

`tools/list` 只返回以下工具（当 `mcp_shell_enabled=true` 时另加 `shell`）：

| MCP 工具 | 后端 |
| --- | --- |
| `phone_bridge_status` | Agent 手机桥接状态 |
| `screenshot` | `POST /api/tools/screenshot` |
| `wait_for_stable_screen` | `POST /api/tools/wait_for_stable_screen` |
| `touch_gesture` | `POST /api/tools/touch_gesture` |
| `enter_text` | `POST /api/tools/enter_text` |
| `keyboard_tap` | `POST /api/tools/keyboard_tap` |
| `open_app` | `POST /api/tools/open_app` |
| `open_url` | `POST /api/tools/open_url` |
| `quick_action` | `POST /api/tools/quick_action` |
| `bridge_clipboard` | `POST /api/tools/bridge_clipboard` |
| `shell` | `POST /api/tools/shell`（受开关控制） |
| `device_status` | config-web `GET /api/device/status` |
| `support_bundle` | config-web `GET /api/logs/support`（嵌入式资源 blob；同一时间只允许一个进行中的调用，多余的调用会被拒绝） |

使用 MCP `tools/list` 发现工具。`server/discover` 返回 2026-07-28 版的 `DiscoverResult`（`supportedVersions`、`resultType`，以及 `_meta` 下的 `serverInfo`）。不要指望存在单独的 `list_tools` 工具。`GET /health` 不通过 MCP 暴露。

## 传输说明（2026-07-28 Streamable HTTP）

- 双时代兼容：客户端可以只发送 `MCP-Protocol-Version`（包括 `2026-07-28`）而不带 `params._meta`；`tools/list` / `ping` 在这条路径上可以工作。规范要求的请求体 `_meta` 字段在存在时会被强制校验；为了简化设备端客户端，仅带请求头的新式流量也会被接受。
- 当 `params._meta` 携带 `2026-07-28` 或客户端发送 `Mcp-Method` 时，适用严格的新式检查：此时必须提供 `MCP-Protocol-Version`，`Mcp-Method` 必须与请求体中的 method 一致，并且（对于 `tools/call`）`Mcp-Name` 必须一致。如果 `params._meta` 包含协议版本，它必须与请求头一致。不一致时返回 HTTP 400，并带有 JSON-RPC `-32020`。
- 公布的协议版本（`server/discover` 的 `supportedVersions` 和 initialize）为 `2026-07-28` 和 `2025-11-25`。在 `initialize` 时，较旧或无法识别的版本字符串（例如 `2025-03-26`）会被有意协商为 `2025-11-25`，因为该端点的单 POST JSON-RPC 工具传输形态在这些旧修订版之间是一致的；较旧的字符串不会被公布，因为历史上它们意味着支持 JSON-RPC 批处理。
- 成功的 JSON-RPC 结果包含 `resultType: "complete"`（公布 `2026-07-28` 时为必需）。
- 不支持 JSON-RPC 批处理数组（每次 POST 只能有一个请求对象）；批处理请求体返回 HTTP 400。
- 未知方法返回 HTTP 404，并带有 JSON-RPC `-32601`。
- 响应为 `application/json`（暂未使用 SSE 流）。此版本没有 `subscriptions/listen` / MRTR。

## Origin

省略 `Origin` 的客户端（典型的 MCP 宿主应用 / curl）会被接受。

如果客户端发送了 `Origin` 头（浏览器请求），它必须：

1. 使用不可重绑定的主机名：IP 字面量或 `localhost`
2. 与请求的 `Host` 匹配（不区分 scheme 的 `host:port`）

这会阻止普通的跨源浏览器调用，并且即使 `Origin` 和 `Host` 一致，也会拒绝被重绑定的攻击者域名。在开发板上，LAN guard 对所有 Agent 和 Config Web 路由应用相同的规则，并额外拒绝 `Sec-Fetch-Site: cross-site` 以及主机名形式的 `Host` 头（参见[跨站请求](../03-services/device-management-api.md#跨站请求)）。`Origin` 检查并不是认证；参见[认证](#认证)。

## 相关内容

- 工具 HTTP API（相同的 Runtime 工具）：[tools-http-api.md](./tools-http-api.md)
