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

Coordinates are normalized to 0–1000 on both axes. After the Agent runs device
actions or `wait 0` in a foreground shell, the shell result includes one
screenshot sheet with up to the last five captured frames, in order from left to
right, then top to bottom. Each panel marks only its own operation. There is no
footer; a `!` next to a marker indicates a failed operation. Normalize new
coordinates within the last panel,
not across the entire sheet. A foreground shell can dispatch up to 64 operations.
An unchanged display is not an error. A CLI failure exits nonzero, so `&&` stops
the chain. Without an active shell scope, the executable accepts `--url` to
target a daemon directly (default `http://127.0.0.1:8080`).

## Viewing and saving screenshots

```sh
device-cli wait 0                                      # Attach the current screen without waiting for stability
device-cli look                                        # Save a JPEG in /tmp
device-cli look --dir /tmp --region 100 200 900 800     # Save a cropped JPEG
```

`look` prints the absolute path to a new JPEG. The destination directory must
already exist and be writable; `/tmp` is the default and recommended choice.
`--region X1 Y1 X2 Y2` sets the top-left and bottom-right corners in normalized
integer coordinates (0–1000). `look` does not contribute a frame to the shell's
screenshot sheet. If a chain also runs commands that attach frames, those
commands still contribute their own screenshots.
