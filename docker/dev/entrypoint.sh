#!/bin/bash
set -eu

agent_dir="${AICO_AGENT_DIR:-/userdata/agent}"
system_env="${AICO_SYSTEM_ENV:-/userdata/system/env}"
agent_service="${AICO_AGENT_INIT_SCRIPT:-/usr/local/bin/aico-agent-service}"
wifi_proxy_service="${AICO_WIFI_PROXY_INIT_SCRIPT:-/usr/local/bin/aico-wifi-proxy-service}"
env_run_bin="${AICO_ENV_RUN_BIN:-/usr/local/bin/aico-env-run}"
quick_actions_bin="${AICO_QUICK_ACTIONS_BIN:-/usr/local/bin/aico-quick-actions}"
config_web_pid=""
ttyd_pid=""

truthy() {
    case "${1:-}" in
        1|true|TRUE|True|yes|YES|Yes|on|ON|On) return 0 ;;
        *) return 1 ;;
    esac
}

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

# The user Python base persists in the volume. Packages with native code only
# load on the architecture that installed them, so set the old tree aside when
# the image architecture changes (for example amd64 -> arm64 on Apple Silicon).
python_base="${PYTHONUSERBASE:-/userdata/agent/python}"
current_arch="$(uname -m)"
if [ -f "$python_base/.aico-arch" ]; then
    previous_arch="$(cat "$python_base/.aico-arch")"
    if [ "$previous_arch" != "$current_arch" ]; then
        mv "$python_base" "$python_base.$previous_arch.$(date +%s)"
    fi
fi
mkdir -p "$python_base"
[ -f "$python_base/.aico-arch" ] || printf '%s\n' "$current_arch" > "$python_base/.aico-arch"

if [ ! -f "$agent_dir/agent.toml" ]; then
    cp /opt/aico/defaults/agent.toml "$agent_dir/agent.toml"
fi
if [ ! -f "$system_env" ]; then
    : > "$system_env"
fi
if [ ! -f /userdata/wpa_supplicant.conf ]; then
    : > /userdata/wpa_supplicant.conf
fi

# Install, replace, or remove the launcher-managed quick_actions.json for
# AICO_QUICK_ACTIONS_PRESET before the Agent reads it at startup. A failure
# here only leaves the Agent on its current quick actions.
AICO_AGENT_DIR="$agent_dir" "$quick_actions_bin" ||
    echo "aico-quick-actions failed; keeping the current quick actions" >&2

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

# The browser terminal is opt-in. Without ttyd, Agent Web /webtty/ answers
# 502 with a short page explaining how to enable it (AICO_ENABLE_TERMINAL=1).
if truthy "${AICO_ENABLE_TERMINAL:-0}"; then
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
fi

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

# Tie Config Web, and ttyd when enabled, to the container lifecycle so
# Compose restarts the whole sandbox when either one stops.
web_pids=("$config_web_pid")
if [ -n "$ttyd_pid" ]; then
    web_pids+=("$ttyd_pid")
fi

set +e
wait -n "${web_pids[@]}"
status="$?"
set -e
exit "$status"
