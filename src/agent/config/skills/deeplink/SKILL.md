---
name: deeplink
description: Launch phone apps and webpages by name or URL with device-cli.
metadata:
  preferred_model: primary
  allowed_tools: [shell]
---

Use with the `device-cli` skill. Tap an app's icon if clearly visible. Otherwise run `device-cli open 'WeChat'`, `device-cli open '支付宝'`, or `device-cli open browser` in the foreground shell.

Use `device-cli url 'https://example.com'` for a webpage. Supported URL schemes also include `sms:`, `mailto:`, and `tel:`. Quote the whole URL as one shell argument. Opening a composer is not proof that a message was sent.

Verify the post-action screenshot before reporting success. For a specific in-app page, use the visible app controls after launch.
