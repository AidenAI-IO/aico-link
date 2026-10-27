#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
init_script="$repo_root/docker/dev/aico-wifi-proxy-service"

if [ ! -r "/proc/$$/cmdline" ]; then
    echo "wifi_proxy init watchdog test skipped: /proc is unavailable"
    exit 0
fi

fixture_dir="$(mktemp -d "${TMPDIR:-/tmp}/wifi-proxy-init.XXXXXX")"
watchdog_pid=
proxy_pid=
unrelated_pid=
false_positive_pid=
mismatched_pid=

run_init_at() {
    init_path="$1"
    action="$2"
    AGENT_BIN="$fixture_dir/agent" \
    ENV_RUN_BIN="$fixture_dir/env-run" \
    LISTEN_ADDRESS=127.0.0.1:18080 \
    PROXY_CONFIG="$fixture_dir/wifi-proxies.json" \
    PROXY_ENVIRONMENT="$fixture_dir/proxy-env" \
    WIFI_INTERFACE=test0 \
    AGENT_INIT_SCRIPT="$fixture_dir/agent-init" \
    LOG_PATH="$fixture_dir/wifi-proxy.log" \
    PID_FILE="$fixture_dir/proxy.pid" \
    WATCHDOG_PID_FILE="$fixture_dir/watchdog.pid" \
        "$init_path" "$action"
}

run_init() {
    run_init_at "$init_script" "$1"
}

cleanup() {
    run_init stop >/dev/null 2>&1 || true
    for pid in "$proxy_pid" "$watchdog_pid" "$unrelated_pid" "$false_positive_pid" "$mismatched_pid"; do
        [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    done
    rm -rf "$fixture_dir"
}
trap cleanup EXIT INT TERM

cat > "$fixture_dir/agent" <<'EOF'
#!/bin/sh
trap 'exit 0' INT TERM
while :; do
    sleep 1
done
EOF

cat > "$fixture_dir/env-run" <<'EOF'
#!/bin/sh
exec "$@"
EOF

cat > "$fixture_dir/agent-init" <<'EOF'
#!/bin/sh
exit 0
EOF

chmod +x "$fixture_dir/agent" "$fixture_dir/env-run" "$fixture_dir/agent-init"
ln -s "$init_script" "$fixture_dir/aico-wifi-proxy-service"

wait_for_pid_file() {
    path="$1"
    attempts=0
    while [ ! -s "$path" ] && [ "$attempts" -lt 100 ]; do
        sleep 0.05
        attempts=$((attempts + 1))
    done
    [ -s "$path" ] || {
        echo "FAIL: timed out waiting for $path" >&2
        exit 1
    }
}

wait_for_exit() {
    pid="$1"
    attempts=0
    while kill -0 "$pid" 2>/dev/null && [ "$attempts" -lt 100 ]; do
        sleep 0.05
        attempts=$((attempts + 1))
    done
    ! kill -0 "$pid" 2>/dev/null
}

# A stale proxy PID can point at a process whose free-form argument contains
# both identifying strings. It must not satisfy the exact argv-prefix check.
sh -c 'sleep 30; :' "$fixture_dir/agent wifi-proxy is starting" &
false_positive_pid="$!"
printf '%s\n' "$false_positive_pid" > "$fixture_dir/proxy.pid"
run_init start >/dev/null
wait_for_pid_file "$fixture_dir/watchdog.pid"
wait_for_pid_file "$fixture_dir/proxy.pid"
watchdog_pid="$(cat "$fixture_dir/watchdog.pid")"
proxy_pid="$(cat "$fixture_dir/proxy.pid")"
kill -0 "$watchdog_pid"
kill -0 "$proxy_pid"
kill -0 "$false_positive_pid" || {
    echo "FAIL: start killed a process that only contained matching substrings" >&2
    exit 1
}
kill "$false_positive_pid"
wait "$false_positive_pid" 2>/dev/null || true
false_positive_pid=
run_init stop >/dev/null
wait_for_exit "$watchdog_pid" || {
    echo "FAIL: watchdog remained alive after the prefix false-positive check" >&2
    exit 1
}
wait_for_exit "$proxy_pid" || {
    echo "FAIL: proxy remained alive after the prefix false-positive check" >&2
    exit 1
}
watchdog_pid=
proxy_pid=

# A process with the right command prefix but different proxy arguments is not
# the managed child and must not be terminated by start.
"$fixture_dir/agent" wifi-proxy --listen 127.0.0.1:19999 --config wrong --environment wrong --wifi-interface wrong &
mismatched_pid="$!"
printf '%s\n' "$mismatched_pid" > "$fixture_dir/proxy.pid"
run_init start >/dev/null
wait_for_pid_file "$fixture_dir/watchdog.pid"
wait_for_pid_file "$fixture_dir/proxy.pid"
watchdog_pid="$(cat "$fixture_dir/watchdog.pid")"
proxy_pid="$(cat "$fixture_dir/proxy.pid")"
kill -0 "$mismatched_pid" || {
    echo "FAIL: start killed a process with mismatched proxy arguments" >&2
    exit 1
}
kill "$mismatched_pid"
wait "$mismatched_pid" 2>/dev/null || true
mismatched_pid=

# Starting through a symlink must resolve to the same watchdog identity and
# leave the existing supervisor and child untouched.
run_init_at "$fixture_dir/aico-wifi-proxy-service" start >/dev/null
if [ "$(cat "$fixture_dir/watchdog.pid")" != "$watchdog_pid" ] ||
   [ "$(cat "$fixture_dir/proxy.pid")" != "$proxy_pid" ]; then
    echo "FAIL: symlink invocation started a duplicate watchdog" >&2
    exit 1
fi

# Simulate an unclean watchdog crash. The child proxy stays alive and its PID
# file remains, exactly as it would after the supervisor is killed abruptly.
kill -9 "$watchdog_pid"
wait_for_exit "$watchdog_pid" || {
    echo "FAIL: crashed watchdog remained alive" >&2
    exit 1
}
kill -0 "$proxy_pid" || {
    echo "FAIL: proxy did not survive the simulated watchdog crash" >&2
    exit 1
}

old_watchdog_pid="$watchdog_pid"
old_proxy_pid="$proxy_pid"
sleep 30 &
unrelated_pid="$!"
printf '%s\n' "$unrelated_pid" > "$fixture_dir/watchdog.pid"
run_init start >/dev/null
wait_for_pid_file "$fixture_dir/watchdog.pid"
wait_for_pid_file "$fixture_dir/proxy.pid"
watchdog_pid="$(cat "$fixture_dir/watchdog.pid")"
proxy_pid="$(cat "$fixture_dir/proxy.pid")"

if [ "$watchdog_pid" = "$old_watchdog_pid" ] || \
        [ "$watchdog_pid" = "$unrelated_pid" ] || \
        [ "$proxy_pid" = "$old_proxy_pid" ]; then
    echo "FAIL: restart reused the crashed watchdog or orphaned proxy" >&2
    exit 1
fi
wait_for_exit "$old_proxy_pid" || {
    echo "FAIL: orphaned proxy remained alive after restart" >&2
    exit 1
}
kill -0 "$watchdog_pid"
kill -0 "$proxy_pid"
kill -0 "$unrelated_pid" || {
    echo "FAIL: start killed the unrelated process from the stale watchdog PID file" >&2
    exit 1
}
kill "$unrelated_pid"
wait "$unrelated_pid" 2>/dev/null || true
unrelated_pid=

run_init stop >/dev/null
wait_for_exit "$watchdog_pid" || {
    echo "FAIL: watchdog remained alive after stop" >&2
    exit 1
}
wait_for_exit "$proxy_pid" || {
    echo "FAIL: proxy remained alive after stop" >&2
    exit 1
}
watchdog_pid=
proxy_pid=

echo "wifi_proxy init watchdog recovery: ok"
