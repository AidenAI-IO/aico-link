#!/bin/bash
set -eu

agent_dir="${AICO_AGENT_DIR:-/userdata/agent}"
system_env="${AICO_SYSTEM_ENV:-/userdata/system/env}"
agent_service="${AICO_AGENT_INIT_SCRIPT:-/usr/local/bin/aico-agent-service}"
wifi_proxy_service="${AICO_WIFI_PROXY_INIT_SCRIPT:-/usr/local/bin/aico-wifi-proxy-service}"
env_run_bin="${AICO_ENV_RUN_BIN:-/usr/local/bin/aico-env-run}"
config_web_pid=""
ttyd_pid=""

mkdir -p \
    "$agent_dir/log" \
    "$agent_dir/memory" \
    "$agent_dir/skill-state" \
    "$agent_dir/cache" \
    /userdata/audio \
    /userdata/ota \
    /userdata/system \
    /userdata/tmp \
    /run/agent
chmod 1777 /userdata/tmp

if [ ! -f "$agent_dir/agent.toml" ]; then
    cp /opt/aico/defaults/agent.toml "$agent_dir/agent.toml"
fi
if [ ! -f "$system_env" ]; then
    : > "$system_env"
fi
if [ ! -f /userdata/wpa_supplicant.conf ]; then
    : > /userdata/wpa_supplicant.conf
fi

shutdown() {
    trap - INT TERM EXIT
    if [ -n "$ttyd_pid" ]; then
        kill "$ttyd_pid" 2>/dev/null || true
        wait "$ttyd_pid" 2>/dev/null || true
    fi
    if [ -n "$config_web_pid" ]; then
        kill "$config_web_pid" 2>/dev/null || true
        wait "$config_web_pid" 2>/dev/null || true
    fi
    "$agent_service" stop || true
    "$wifi_proxy_service" stop || true
}

trap shutdown INT TERM EXIT

"$wifi_proxy_service" start
"$agent_service" start

"$env_run_bin" ttyd \
    --port=3000 \
    --base-path=/webtty/ \
    --max-clients="${TTYD_MAX_CLIENTS:-2}" \
    --client-option "titleFixed=${TTYD_TITLE:-Aico Link Shell}" \
    --client-option "rendererType=${TTYD_RENDERER:-canvas}" \
    --client-option "fontSize=${TTYD_FONT_SIZE:-24}" \
    --client-option "scrollback=${TTYD_SCROLLBACK:-500}" \
    --client-option "cursorStyle=${TTYD_CURSOR_STYLE:-bar}" \
    --client-option "disableResizeOverlay=${TTYD_DISABLE_RESIZE_OVERLAY:-true}" \
    /bin/bash \
    >>/userdata/agent/log/ttyd.log 2>&1 &
ttyd_pid="$!"

"$env_run_bin" "${AICO_AGENT_BIN:-/usr/lib/aico/agent}" config-web \
    --bind=0.0.0.0 \
    --port=80 \
    --config="$agent_dir/agent.toml" \
    --wifi-config=/userdata/wpa_supplicant.conf \
    --ota-state=/userdata/ota/state.json \
    --cmdline=/proc/cmdline \
    --system-env="$system_env" \
    --storage-state=/run/aico/storage.state &
config_web_pid="$!"

set +e
# Keep both web frontends tied to the container lifecycle so Compose can restart them together.
wait -n "$config_web_pid" "$ttyd_pid"
status="$?"
set -e
exit "$status"
