---
sidebar_position: 6
---

# OTA GitHub 代理配置

## 概述

从 GitHub 下载 OTA 更新时，某些地区的网络连接可能不稳定。OTA 系统现已支持配置 GitHub 代理服务来加速下载。

## 在 Config Web 中配置

通过 USB 连接设备，打开 `http://192.168.42.1`，找到 **OTA → GitHub Proxy URL**。输入代理服务的完整 HTTPS URL，例如：

```text
https://gh-proxy.com/
```

保存配置即可生效。将该字段留空即可禁用代理。

## 在文件中配置

### OTA 配置文件（`/userdata/debian/ota/config.json`）

在 OTA 配置中添加 `github_proxy_url` 字段：

```json
{
  "github_proxy_url": "https://gh-proxy.com/"
}
```

### Agent 配置文件（`/userdata/agent/agent.toml`）

在 Agent 配置中添加 OTA 部分：

```toml
[advanced_settings.runtime.ota]
github_proxy_url = "https://gh-proxy.com/"
```

## 代理 URL 格式

你可以使用任何遵循以下标准模式的 GitHub 代理服务：

```text
https://your-proxy.example.com/https://github.com/...
```

示例：
```json
{
  "github_proxy_url": "https://my-proxy.example.com/"
}
```

## 工作原理

配置 `github_proxy_url` 后：

1. OTA 升级程序识别 GitHub URL（github.com、api.github.com、raw.githubusercontent.com、objects.githubusercontent.com）
2. 在原始 GitHub URL 前面加上代理 URL
3. 通过代理服务下载

转换示例：
```
Original: https://github.com/AidenAI-IO/aico-link/releases/download/v1.0.0/update.tar.gz
With proxy: https://gh-proxy.com/https://github.com/AidenAI-IO/aico-link/releases/download/v1.0.0/update.tar.gz
```

非 GitHub URL 不受此设置影响。

URL 必须是绝对 HTTPS URL。常见示例包括 `https://gh-proxy.com/` 和 `https://ghfast.top/`，但你可以使用任何可信、且接受以原始 GitHub URL 作为后缀的服务。

## 禁用代理

要禁用代理，删除 `github_proxy_url` 字段或将其设为空字符串：

```json
{
  "github_proxy_url": ""
}
```

## 故障排查

如果配置代理后下载失败：

1. 确认设备可以访问该代理 URL
2. 检查代理 URL 是否为完整的 HTTPS URL
3. 尝试其他代理服务
4. 暂时禁用代理，以确认不是 GitHub 连接问题

## 安全提示

代理服务可以看到被访问的 URL。请只使用可信的代理服务，尤其是在生产部署中。
