---
sidebar_position: 6
---

# OTA GitHub Proxy Configuration

## Overview

When downloading OTA updates from GitHub, network connectivity can be unstable in some regions. The OTA system now supports configuring GitHub proxy services to accelerate downloads.

## Configure in Config Web

Connect the device over USB, open `http://192.168.42.1`, and find **OTA → GitHub Proxy URL**. Enter the complete HTTPS URL for the proxy service, for example:

```text
https://gh-proxy.com/
```

Save the configuration to apply it. Leave the field empty to disable the proxy.

## Configure in a File

### OTA Config File (`/userdata/debian/ota/config.json`)

Add the `github_proxy_url` field to your OTA configuration:

```json
{
  "github_proxy_url": "https://gh-proxy.com/"
}
```

### Agent Config File (`/userdata/agent/agent.toml`)

Add the OTA section to your agent configuration:

```toml
[advanced_settings.runtime.ota]
github_proxy_url = "https://gh-proxy.com/"
```

## Proxy URL Format

You can use any GitHub proxy service that follows the standard pattern:

```text
https://your-proxy.example.com/https://github.com/...
```

Example:
```json
{
  "github_proxy_url": "https://my-proxy.example.com/"
}
```

## How It Works

When `github_proxy_url` is configured:

1. The OTA updater detects GitHub URLs (github.com, api.github.com, raw.githubusercontent.com, objects.githubusercontent.com)
2. It prepends the proxy URL to the original GitHub URL
3. Downloads are fetched through the proxy service

Example transformation:
```
Original: https://github.com/AidenAI-IO/aico-link/releases/download/v1.0.0/update.tar.gz
With proxy: https://gh-proxy.com/https://github.com/AidenAI-IO/aico-link/releases/download/v1.0.0/update.tar.gz
```

Non-GitHub URLs are not affected by this setting.

The URL must be an absolute HTTPS URL. Common examples include `https://gh-proxy.com/` and `https://ghfast.top/`, but you can use any trusted service that accepts the original GitHub URL as a suffix.

## Disabling the Proxy

To disable the proxy, remove the `github_proxy_url` field or set it to an empty string:

```json
{
  "github_proxy_url": ""
}
```

## Troubleshooting

If downloads fail with a proxy configured:

1. Verify the proxy URL is accessible from your device
2. Check that the proxy URL is a complete HTTPS URL
3. Try a different proxy service
4. Temporarily disable the proxy to verify it's not a GitHub connectivity issue

## Security Note

Proxy services can see the URLs being accessed. Only use trusted proxy services, especially for production deployments.
