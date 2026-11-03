#!/bin/sh
set -eu

sandbox_project="aico-sandbox-smoke-$$"
config_port="${AICO_SANDBOX_TEST_CONFIG_PORT:-0}"
agent_port="${AICO_SANDBOX_TEST_AGENT_PORT:-0}"
bridge_port="${AICO_SANDBOX_TEST_BRIDGE_PORT:-}"
sandbox_bridge_endpoint=""
sandbox_device_type=""
bridge_pid=""
bridge_log=""
script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"

if grep -q 'scripts/debian-apps/debian.sources' "$script_dir/../docker/test/Dockerfile"; then
    echo "docker test image must not depend on the regional Debian apps mirror" >&2
    exit 1
fi

compose() {
    AICO_CONFIG_WEB_PORT="$config_port" \
    AICO_AGENT_WEB_PORT="$agent_port" \
    AICO_DEVICE_TYPE="$sandbox_device_type" \
    AICO_ENVIRONMENT_BRIDGE_ENDPOINT="$sandbox_bridge_endpoint" \
    AICO_BENCHMARK_TASK_ID=docker-sandbox-smoke \
    AICO_BRIDGE_EPISODE_ID=docker-sandbox-smoke \
        docker compose -p "$sandbox_project" "$@"
}

cleanup() {
    trap - INT TERM EXIT
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    if [ -n "$bridge_pid" ]; then
        kill "$bridge_pid" 2>/dev/null || true
        wait "$bridge_pid" 2>/dev/null || true
    fi
    if [ -n "$bridge_log" ] && [ -f "$bridge_log" ]; then
        rm -f "$bridge_log"
    fi
}

# curl -f collapses every failure into a bare "curl: (22)" with no hint which
# request produced it, and this script makes ten of them against two servers.
# Name each asserted request so a failure says what it asked for. The readiness
# polls below stay silent on purpose: they are expected to fail while waiting.
request() {
    label=$1
    shift
    if ! curl "$@"; then
        printf 'smoke test request failed: %s\n' "$label" >&2
        exit 1
    fi
}

agent_pid() {
    compose exec -T aico /usr/local/bin/aico-agent-service status 2>/dev/null \
        | sed -n 's/^agent=running pid=\([0-9][0-9]*\).*/\1/p'
}

wait_for_agent() {
    attempt=1
    while [ "$attempt" -le 45 ]; do
        if curl -fsS --max-time 5 "http://127.0.0.1:$config_port/" >/dev/null 2>&1 \
            && curl -fsS --max-time 5 "http://127.0.0.1:$agent_port/api/tools" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
        attempt="$((attempt + 1))"
    done
    compose logs
    return 1
}

trap cleanup INT TERM EXIT

selected_ports="$(python3 "$script_dir/select_docker_web_ports.py" "$config_port" "$agent_port")"
config_port="${selected_ports%% *}"
agent_port="${selected_ports#* }"

compose up -d --build
wait_for_agent

compose exec -T aico sh -ec '
python3 --version
python3 -m pip --version
ttyd --version
rg --version
fq --version
yq --version
'

config_page="$(request 'GET config web /' \
    -fsS --max-time 10 "http://127.0.0.1:$config_port/")"
agent_page="$(request 'GET agent web /' \
    -fsS --max-time 10 "http://127.0.0.1:$agent_port/")"
terminal_page="$(request 'GET agent web /webtty/' \
    -fsSL --max-time 10 "http://127.0.0.1:$agent_port/webtty/")"
terminal_headers="$(request 'HEAD agent web /webtty/' \
    -fsSIL --max-time 10 "http://127.0.0.1:$agent_port/webtty/")"
terminal_token="$(request 'GET agent web /webtty/token' \
    -fsS --max-time 10 "http://127.0.0.1:$agent_port/webtty/token")"
reported_agent_port="$(
    request 'GET config web /api/device/status' \
        -fsS --max-time 10 "http://127.0.0.1:$config_port/api/device/status" \
        | python3 -c 'import json, sys; print(json.load(sys.stdin)["agent_status"]["public_port"])'
)"
test "$reported_agent_port" -eq "$agent_port"
case "$config_page" in
    *'<!DOCTYPE html>'*) ;;
    *) printf 'Config Web did not return HTML.\n' >&2; exit 1 ;;
esac
case "$agent_page" in
    *'<!DOCTYPE html>'*) ;;
    *) printf 'Agent Web did not return HTML.\n' >&2; exit 1 ;;
esac
case "$terminal_page" in
    *'<!doctype html>'*|*'<!DOCTYPE html>'*) ;;
    *) printf 'ttyd did not return HTML through Agent Web.\n' >&2; exit 1 ;;
esac
case "$terminal_headers" in
    *'Server: ttyd/'*) ;;
    *) printf 'ttyd response did not include its server header.\n' >&2; exit 1 ;;
esac
case "$terminal_page" in
    *'ttyd'*'xterm'*) ;;
    *) printf 'ttyd did not return its terminal client page through Agent Web.\n' >&2; exit 1 ;;
esac
case "$terminal_token" in
    *'"token"'*) ;;
    *) printf 'ttyd token endpoint failed through Agent Web.\n' >&2; exit 1 ;;
esac

container_id="$(compose ps -q aico)"
restart_count="$(docker inspect --format '{{.RestartCount}}' "$container_id")"
ttyd_pid="$(compose exec -T aico sh -ec '
for process in /proc/[0-9]*; do
    executable="$(readlink "$process/exe" 2>/dev/null || true)"
    if [ "$executable" = /usr/local/bin/ttyd ]; then
        basename "$process"
        exit 0
    fi
done
exit 1
')"
test -n "$ttyd_pid"
compose exec -T aico sh -c 'kill "$1"' sh "$ttyd_pid"

attempt=1
while [ "$attempt" -le 30 ]; do
    current_restart_count="$(docker inspect --format '{{.RestartCount}}' "$container_id")"
    if [ "$current_restart_count" -gt "$restart_count" ]; then
        break
    fi
    sleep 1
    attempt="$((attempt + 1))"
done
test "$current_restart_count" -gt "$restart_count"
wait_for_agent
request 'GET agent web /webtty/ after ttyd restart' \
    -fsSL --max-time 10 "http://127.0.0.1:$agent_port/webtty/" >/dev/null

before_pid="$(agent_pid)"
test -n "$before_pid"

# The marker has to be a key isAllowedSystemEnvKey() accepts, so it is a proxy
# variable rather than something named after this test. no_proxy takes an
# arbitrary token -- only HTTP_PROXY/HTTPS_PROXY/ALL_PROXY get URL-validated --
# and nothing in the sandbox routes through a proxy, so the value is inert.
request 'PUT config web /api/system/environment' \
    -fsS --max-time 15 -X PUT \
    -H 'Content-Type: application/json' \
    --data '{"system_env":"no_proxy=aico-docker-sandbox-smoke\n"}' \
    "http://127.0.0.1:$config_port/api/system/environment" \
    | grep -q '"agent_restart_required":true'

curl -fsS --max-time 15 -X POST \
    "http://127.0.0.1:$config_port/api/system/environment/apply" \
    | grep -q '"agent_restart_scheduled":true'

attempt=1
after_pid=""
while [ "$attempt" -le 45 ]; do
    after_pid="$(agent_pid)"
    if [ -n "$after_pid" ] && [ "$after_pid" != "$before_pid" ] \
        && curl -fsS --max-time 5 "http://127.0.0.1:$agent_port/api/tools" >/dev/null 2>&1; then
        break
    fi
    sleep 1
    attempt="$((attempt + 1))"
done
test -n "$after_pid"
test "$after_pid" != "$before_pid"

compose down
compose up -d
wait_for_agent
request 'GET config web /api/system/environment' \
    -fsS --max-time 10 "http://127.0.0.1:$config_port/api/system/environment" \
    | grep -q 'no_proxy=aico-docker-sandbox-smoke'

printf 'Docker sandbox smoke test passed (agent pid %s -> %s).\n' "$before_pid" "$after_pid"

# Exercise restart after the bridge session is ready. The service must preserve
# the claimed session, start exactly one Agent, and release the route when the
# container stops.
compose down -v
if [ -z "$bridge_port" ]; then
    bridge_port="$(python3 -c 'import socket; sock = socket.socket(); sock.bind(("", 0)); print(sock.getsockname()[1]); sock.close()')"
fi
bridge_log="$(mktemp -t aico-docker-bridge.XXXXXX)"
python3 -c '
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port = int(sys.argv[1])

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        return

    def reply(self):
        payload = json.dumps({"ok": True, "data": {"platform": "ios"}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        try:
            self.wfile.write(payload)
        except BrokenPipeError:
            pass

    def do_GET(self):
        print("GET", self.path, flush=True)
        self.reply()

    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(size).decode()
        task_id = self.headers.get("benchmark-task-id", "")
        print("POST", self.path, task_id, body, flush=True)
        if self.path == "/api/setup":
            time.sleep(5)
        self.reply()

ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
' "$bridge_port" >"$bridge_log" 2>&1 &
bridge_pid="$!"
sandbox_bridge_endpoint="http://host.docker.internal:$bridge_port"
sandbox_device_type="iOS"

compose up -d
wait_for_agent

attempt=1
while [ "$attempt" -le 30 ]; do
    if grep -q 'POST /api/setup docker-sandbox-smoke' "$bridge_log"; then
        break
    fi
    sleep 0.2
    attempt="$((attempt + 1))"
done
grep -q 'POST /api/setup docker-sandbox-smoke' "$bridge_log"
setup_count="$(grep -c 'POST /api/setup docker-sandbox-smoke' "$bridge_log" || true)"
test "$setup_count" -eq 1

request 'PUT config web /api/system/environment (restart race)' \
    -fsS --max-time 15 -X PUT \
    -H 'Content-Type: application/json' \
    --data '{"system_env":"no_proxy=aico-docker-sandbox-race\n"}' \
    "http://127.0.0.1:$config_port/api/system/environment" >/dev/null
curl -fsS --max-time 15 -X POST \
    "http://127.0.0.1:$config_port/api/system/environment/apply" \
    >/dev/null
wait_for_agent

compose exec -T aico sh -c '
count=0
for process in /proc/[0-9]*; do
    executable="$(readlink "$process/exe" 2>/dev/null || true)"
    if [ "$executable" = /usr/lib/aico/agent ]; then
        command_line="$(tr "\000" " " < "$process/cmdline" 2>/dev/null || true)"
        case "$command_line" in
            */usr/lib/aico/agent\ config-web\ *|*/usr/lib/aico/agent\ wifi-proxy\ *) continue ;;
        esac
        count="$((count + 1))"
    fi
done
test "$count" -eq 1
'

release_count="$(grep -c 'POST /api/release docker-sandbox-smoke {}' "$bridge_log" || true)"
test "$release_count" -eq 0
setup_count="$(grep -c 'POST /api/setup docker-sandbox-smoke' "$bridge_log" || true)"
test "$setup_count" -eq 1
compose down
attempt=1
while [ "$attempt" -le 20 ]; do
    if grep -q 'POST /api/release docker-sandbox-smoke {}' "$bridge_log"; then
        break
    fi
    sleep 0.2
    attempt="$((attempt + 1))"
done
setup_count="$(grep -c 'POST /api/setup docker-sandbox-smoke' "$bridge_log" || true)"
release_count="$(grep -c 'POST /api/release docker-sandbox-smoke {}' "$bridge_log" || true)"
test "$setup_count" -eq 1
test "$release_count" -eq 1

printf 'Docker sandbox bridge restart and release test passed.\n'
