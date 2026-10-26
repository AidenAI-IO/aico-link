#!/bin/sh
set -eu

source_config_dir="${AICO_SOURCE_CONFIG_DIR:-/config}"
runtime_config_dir="${AICO_RUNTIME_CONFIG_DIR:-/tmp/aico-config}"
config_file="$runtime_config_dir/agent.toml"

rm -rf "$runtime_config_dir"
mkdir -p "$runtime_config_dir"
cp -a "$source_config_dir"/. "$runtime_config_dir"/
mkdir -p "$runtime_config_dir/log" "$runtime_config_dir/memory" "$runtime_config_dir/skill-state"

if [ ! -f "$config_file" ]; then
    echo "missing agent.toml in $source_config_dir" >&2
    exit 1
fi

if grep -q '^[[:space:]]*bridge_token_file[[:space:]]*=' "$config_file"; then
    sed -i 's#^[[:space:]]*bridge_token_file[[:space:]]*=.*#bridge_token_file = "/config/bridge_token"#' "$config_file"
fi

if grep -q '^[[:space:]]*control_token_file[[:space:]]*=' "$config_file"; then
    sed -i 's#^[[:space:]]*control_token_file[[:space:]]*=.*#control_token_file = "/config/control_token"#' "$config_file"
fi

set -- daemon -dir "$runtime_config_dir" -addr "${AICO_DAEMON_ADDR:-0.0.0.0:8080}"

if [ -n "${AICO_BENCHMARK_TOKEN_FILE:-}" ]; then
    set -- "$@" --benchmark-token-file "$AICO_BENCHMARK_TOKEN_FILE"
fi

if [ -n "${AICO_DEVICE_TYPE:-}" ]; then
    set -- "$@" --device-type "$AICO_DEVICE_TYPE"
fi

bridge_enabled=0
if [ "${AICO_ENVIRONMENT_BRIDGE_MODE:-}" = "1" ] || [ "${AICO_ENVIRONMENT_BRIDGE_MODE:-}" = "true" ]; then
    bridge_enabled=1
    if [ -z "${ENVIRONMENT_BRIDGE_ENDPOINT:-}" ]; then
        echo "ENVIRONMENT_BRIDGE_ENDPOINT is required when AICO_ENVIRONMENT_BRIDGE_MODE is enabled" >&2
        exit 1
    fi
    set -- "$@" --environment-bridge-mode --environment-bridge-endpoint "$ENVIRONMENT_BRIDGE_ENDPOINT"
    if [ -n "${AICO_BENCHMARK_TASK_ID:-}" ]; then
        set -- "$@" --benchmark-task-id "$AICO_BENCHMARK_TASK_ID"
    fi
fi

if [ "${AICO_BENCHMARK_MOCK_TOOLS:-0}" = "1" ]; then
    set -- "$@" --benchmark-mock-tools
fi

# These containers have no physical device services. Real-device CLI runs do not
# use this entrypoint; bridged and scripted environments retain their tool catalog.
if [ -n "${AICO_BENCHMARK_TOKEN_FILE:-}" ] && [ "$bridge_enabled" -eq 0 ]; then
    set -- "$@" --benchmark-headless
fi

exec "$@"
