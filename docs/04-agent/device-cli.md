---
sidebar_position: 4
---

# Device CLI

The backend agent operates the target screen using `device-cli` in a foreground
`shell` call. The CLI forwards to the existing [Tools HTTP API](tools-http-api.md);
it does not implement another device driver. The backend agent no longer receives
the legacy direct UI tools. The HTTP API and the standalone realtime voice path
still expose their existing device tools. Set `AICO_LEGACY_DEVICE_TOOLS=1` before
starting the daemon to opt out of CLI mode.

```sh
device-cli tap 500 650
device-cli swipe 500 800 500 300
device-cli type 'Hello' && device-cli enter
```

Coordinates are normalized to 0–1000 on both axes. After a foreground shell has
actually invoked the scoped CLI, the shell result is followed by one screenshot
sheet containing up to the last five captured frames, in order from left to
right, then top to bottom. Each panel marks only its own operation. There is no
footer; a `!` next to a marker indicates a failed operation. Normalize new
coordinates within the last panel,
not across the entire sheet. A foreground shell can dispatch up to 64 operations.
An unchanged display is not an error. A CLI failure exits nonzero, so `&&` stops
the chain. Without an active shell scope, the executable accepts `--url` to
target a daemon directly (default
`http://127.0.0.1:8080`); use the HTTP screenshot endpoint to retrieve the image
outside the agent.
