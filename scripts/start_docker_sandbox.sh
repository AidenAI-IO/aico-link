#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
WAIT_TIMEOUT="${AICO_SANDBOX_WAIT_TIMEOUT:-180}"
CONFIG_WEB_PORT="${AICO_CONFIG_WEB_PORT:-8000}"
AGENT_WEB_PORT="${AICO_AGENT_WEB_PORT:-8080}"
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"

if [[ $# -gt 0 ]]; then
    echo "Usage: $0" >&2
    exit 2
fi

cd "$ROOT_DIR"

if ! command -v docker >/dev/null 2>&1; then
    echo "Docker is required to start the sandbox." >&2
    exit 1
fi
if ! docker compose version >/dev/null 2>&1; then
    echo "Docker Compose is required to start the sandbox." >&2
    exit 1
fi
if ! compose_up_help="$(docker compose up --help 2>&1)"; then
    echo "Could not inspect Docker Compose startup options." >&2
    exit 1
fi
for required_option in --wait --wait-timeout; do
    if ! grep -Eq "(^|[[:space:]])${required_option}([=[:space:]]|$)" \
        <<<"$compose_up_help"; then
        echo "Docker Compose v2.17.0 or newer is required (${required_option} is unavailable)." >&2
        exit 1
    fi
done

if [[ "$CONFIG_WEB_PORT" == 0 || "$AGENT_WEB_PORT" == 0 ]]; then
    read -r CONFIG_WEB_PORT AGENT_WEB_PORT < <(
        python3 "$SCRIPT_DIR/select_docker_web_ports.py" \
            "$CONFIG_WEB_PORT" "$AGENT_WEB_PORT"
    )
fi
export AICO_CONFIG_WEB_PORT="$CONFIG_WEB_PORT"
export AICO_AGENT_WEB_PORT="$AGENT_WEB_PORT"

compose_args=(
    --detach
    --wait
    --wait-timeout "$WAIT_TIMEOUT"
    --build
)

echo "Building the current source and starting the Aico Docker sandbox..."

if ! docker compose up "${compose_args[@]}" aico; then
    echo "The sandbox did not become healthy. Recent logs:" >&2
    docker compose logs --tail=100 aico >&2 || true
    exit 1
fi

echo "Aico Docker sandbox is ready."
echo "Config Web: http://localhost:${CONFIG_WEB_PORT}"
echo "Agent Web:  http://localhost:${AGENT_WEB_PORT}"
echo "Terminal:   http://localhost:${AGENT_WEB_PORT}/webtty/ (ttyd)"
echo "Logs:       docker compose logs -f aico"
