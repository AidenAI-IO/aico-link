#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BENCHMARK_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
REPO_ROOT="$(cd "$BENCHMARK_ROOT/.." && pwd)"
LABEL="com.aico.mobilegym-local-launcher"
HELPER_LABEL="com.aico.mobilegym-local-launcher-helper"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
HELPER_PLIST="$HOME/Library/LaunchAgents/$HELPER_LABEL.plist"
LOG_DIR="$HOME/Library/Logs/Aico"
UV_BIN="$(command -v uv)"

if [[ -z "$UV_BIN" ]]; then
  echo "Error: uv not found in PATH" >&2
  exit 1
fi

mkdir -p "$(dirname "$PLIST")" "$LOG_DIR"

ENVIRONMENT_XML=""
LAUNCHER_PATH="/usr/local/bin:/opt/homebrew/bin:/Applications/Docker.app/Contents/Resources/bin:/usr/bin:/bin:/usr/sbin:/sbin"
ENVIRONMENT_XML="${ENVIRONMENT_XML}    <key>PATH</key>
    <string>${LAUNCHER_PATH}</string>
"
if [[ -n "${MOBILEGYM_DOCKER_PROXY:-}" ]]; then
  ENVIRONMENT_XML="${ENVIRONMENT_XML}    <key>MOBILEGYM_DOCKER_PROXY</key>
    <string>${MOBILEGYM_DOCKER_PROXY}</string>
"
fi
ENVIRONMENT_XML="${ENVIRONMENT_XML}    <key>MOBILEGYM_PLAYWRIGHT_CHROMIUM_DOWNLOAD_HOST</key>
    <string>${MOBILEGYM_PLAYWRIGHT_CHROMIUM_DOWNLOAD_HOST:-https://playwright-akamai.azureedge.net}</string>"

cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>-c</string>
    <string>cd "$REPO_ROOT" &amp;&amp; exec "$UV_BIN" run --project benchmark python benchmark/mobilegym/scripts/local_launcher.py --host 127.0.0.1 --port 4174</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>EnvironmentVariables</key>
  <dict>
$ENVIRONMENT_XML
  </dict>
  <key>WorkingDirectory</key>
  <string>$REPO_ROOT</string>
  <key>StandardOutPath</key>
  <string>$LOG_DIR/mobilegym-local-launcher.out.log</string>
  <key>StandardErrorPath</key>
  <string>$LOG_DIR/mobilegym-local-launcher.err.log</string>
</dict>
</plist>
EOF

cat > "$HELPER_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$HELPER_LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>-c</string>
    <string>cd "$REPO_ROOT" &amp;&amp; exec "$UV_BIN" run --project benchmark python benchmark/mobilegym/scripts/local_launcher_helper.py --host 127.0.0.1 --port 4175</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>WorkingDirectory</key>
  <string>$REPO_ROOT</string>
  <key>StandardOutPath</key>
  <string>$LOG_DIR/mobilegym-local-launcher-helper.out.log</string>
  <key>StandardErrorPath</key>
  <string>$LOG_DIR/mobilegym-local-launcher-helper.err.log</string>
</dict>
</plist>
EOF

launchctl bootout gui/$UID "$PLIST" >/dev/null 2>&1 || true
launchctl bootout gui/$UID "$HELPER_PLIST" >/dev/null 2>&1 || true
launchctl bootstrap gui/$UID "$PLIST"
launchctl bootstrap gui/$UID "$HELPER_PLIST"
launchctl kickstart -k gui/$UID/$LABEL
launchctl kickstart -k gui/$UID/$HELPER_LABEL

echo "Installed and started $LABEL"
echo "Endpoint: http://127.0.0.1:4174"
echo "Logs: $LOG_DIR/mobilegym-local-launcher.{out,err}.log"
echo "Installed and started $HELPER_LABEL"
echo "Helper endpoint: http://127.0.0.1:4175"
echo "Helper logs: $LOG_DIR/mobilegym-local-launcher-helper.{out,err}.log"
