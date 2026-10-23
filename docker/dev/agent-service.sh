#!/bin/sh
set -eu

agent_bin="${AICO_AGENT_BIN:-/usr/lib/aico/agent}"
agent_dir="${AICO_AGENT_DIR:-/userdata/agent}"
system_env="${AICO_SYSTEM_ENV:-/userdata/system/env}"
env_run_bin="${AICO_ENV_RUN_BIN:-/usr/local/bin/aico-env-run}"
run_dir="${AICO_AGENT_RUN_DIR:-/run/agent}"
supervisor_pid_file="$run_dir/supervisor.pid"
agent_pid_file="$run_dir/agent.pid"
lock_file="$run_dir/service.lock"
bridge_session_file="$run_dir/bridge-session"
log_file="$agent_dir/log/agent.log"

mkdir -p "$run_dir" "$agent_dir/log"

is_running() {
    pid="${1:-}"
    case "$pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    kill -0 "$pid" 2>/dev/null
}

read_pid() {
    file="$1"
    if [ -f "$file" ]; then
        cat "$file" 2>/dev/null || true
    fi
}

service_log() {
    level="$1"
    event="$2"
    shift 2
    message="$*"
    timestamp="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    line="$timestamp [$level][agent][supervisor] $event $message"
    printf '%s\n' "$line" >> "$log_file"
    printf '%s\n' "$line"
}

rotate_log_if_needed() {
    max_bytes="${AICO_AGENT_LOG_MAX_BYTES:-10485760}"
    retain_bytes="${AICO_AGENT_LOG_RETAIN_BYTES:-5242880}"
    case "$max_bytes" in
        ''|0|*[!0-9]*) max_bytes=10485760 ;;
    esac
    case "$retain_bytes" in
        ''|*[!0-9]*) retain_bytes=5242880 ;;
    esac
    if [ "$retain_bytes" -ge "$max_bytes" ]; then
        retain_bytes="$((max_bytes / 2))"
    fi

    current_bytes="$(wc -c < "$log_file" 2>/dev/null || printf '0')"
    if [ "$current_bytes" -le "$max_bytes" ]; then
        return 0
    fi

    rotation_file="$run_dir/agent.log.rotate.$$"
    tail -c "$retain_bytes" "$log_file" > "$rotation_file"
    cat "$rotation_file" > "$log_file"
    rm -f "$rotation_file"
    service_log INFO log_rotated \
        "previous_bytes=$current_bytes retained_bytes=$retain_bytes max_bytes=$max_bytes"
}

wait_for_agent_exit() {
    pid="$1"
    check_interval="${AICO_AGENT_LOG_CHECK_INTERVAL:-1}"
    case "$check_interval" in
        ''|0|*[!0-9]*) check_interval=1 ;;
    esac
    while is_running "$pid"; do
        sleep "$check_interval"
        rotate_log_if_needed
    done
    wait "$pid"
}

load_system_env() {
    if [ -f "$system_env" ]; then
        set -a
        # shellcheck disable=SC1090
        . "$system_env"
        set +a
    fi
}

bridge_enabled() {
    endpoint="$1"
    mode="$2"
    case "$mode" in
        1|true|TRUE|yes|YES|on|ON) [ -n "$endpoint" ] ;;
        0|false|FALSE|no|NO|off|OFF) return 1 ;;
        *) [ -n "$endpoint" ] ;;
    esac
}

valid_bridge_identifier() {
    value="${1:-}"
    case "$value" in
        ''|*[!A-Za-z0-9._:/-]*) return 1 ;;
        *) return 0 ;;
    esac
}

bridge_identifiers_valid() {
    task_id="$1"
    episode_id="$2"
    valid_bridge_identifier "$task_id" && valid_bridge_identifier "$episode_id"
}

load_bridge_session() {
    bridge_session_endpoint=""
    bridge_session_task_id=""
    bridge_session_episode_id=""
    bridge_session_state="ready"
    bridge_session_setup_token=""
    if [ ! -f "$bridge_session_file" ]; then
        return 1
    fi
    {
        IFS= read -r bridge_session_endpoint \
            && IFS= read -r bridge_session_task_id \
            && IFS= read -r bridge_session_episode_id \
            && {
                IFS= read -r bridge_session_state || bridge_session_state="ready"
                IFS= read -r bridge_session_setup_token \
                    || bridge_session_setup_token=""
            }
    } < "$bridge_session_file" || return 1
    case "$bridge_session_state" in
        pending|ready) ;;
        *) return 1 ;;
    esac
    [ -n "$bridge_session_endpoint" ] \
        && valid_bridge_identifier "$bridge_session_task_id" \
        && valid_bridge_identifier "$bridge_session_episode_id" \
        && { [ -z "$bridge_session_setup_token" ] \
            || valid_bridge_identifier "$bridge_session_setup_token"; }
}

bridge_session_matches() {
    endpoint="$1"
    task_id="$2"
    episode_id="$3"
    load_bridge_session \
        && [ "$bridge_session_endpoint" = "$endpoint" ] \
        && [ "$bridge_session_task_id" = "$task_id" ] \
        && [ "$bridge_session_episode_id" = "$episode_id" ]
}

save_bridge_session() {
    endpoint="$1"
    task_id="$2"
    episode_id="$3"
    state="${4:-ready}"
    setup_token="${5:-}"
    temporary_file="$bridge_session_file.$$"
    {
        printf '%s\n' "$endpoint"
        printf '%s\n' "$task_id"
        printf '%s\n' "$episode_id"
        printf '%s\n' "$state"
        printf '%s\n' "$setup_token"
    } > "$temporary_file"
    mv -f "$temporary_file" "$bridge_session_file"
}

new_bridge_setup_token() {
    python3 -c 'import uuid; print(uuid.uuid4().hex)'
}

bridge_session_remote_state() {
    endpoint="$1"
    task_id="$2"
    response_file="$run_dir/bridge-health.$$.json"
    health_status="$(curl -sS -o "$response_file" -w '%{http_code}' \
        --connect-timeout 1 --max-time 2 "$endpoint/health" 2>/dev/null || true)"
    if [ "${health_status#2}" = "$health_status" ]; then
        rm -f "$response_file"
        printf 'unknown\n'
        return 0
    fi

    remote_state="$(python3 -c '
import json
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as source:
        payload = json.load(source)
    data = payload.get("data", payload)
    bridge_type = data.get("bridge_type")
    if bridge_type == "mobilegym" and isinstance(data.get("active_routes"), dict):
        print("active" if sys.argv[2] in data["active_routes"] else "missing")
    elif bridge_type == "adb_android" and "active_task_id" in data:
        active_task_id = str(data.get("active_task_id") or "")
        if active_task_id == sys.argv[2]:
            print("active")
        elif active_task_id:
            print("unknown")
        else:
            print("missing")
    else:
        print("unknown")
except Exception:
    print("unknown")
' "$response_file" "$task_id" 2>/dev/null || printf 'unknown')"
    rm -f "$response_file"
    printf '%s\n' "$remote_state"
}

prepare_bridge_episode() {
    endpoint="$1"
    task_id="$2"
    episode_id="$3"
    setup_token="${4:-}"

    attempt=1
    health_status="000"
    while [ "$attempt" -le "${AICO_BRIDGE_WAIT_ATTEMPTS:-10}" ]; do
        health_status="$(curl -sS -o /dev/null -w '%{http_code}' \
            --connect-timeout 1 --max-time 2 "$endpoint/health" 2>/dev/null || true)"
        if [ "$health_status" != "000" ]; then
            break
        fi
        if [ "$attempt" -eq 1 ]; then
            service_log INFO bridge_waiting "endpoint=$endpoint"
        fi
        sleep 1
        attempt="$((attempt + 1))"
    done

    if [ "$health_status" = "000" ]; then
        service_log WARN bridge_unavailable "endpoint=$endpoint; starting agent without an active episode"
        return 1
    fi

    if [ -z "$setup_token" ]; then
        setup_token="$(new_bridge_setup_token)"
    fi
    save_bridge_session \
        "$endpoint" "$task_id" "$episode_id" pending "$setup_token"
    response_file="$run_dir/bridge-setup.$$.json"
    if setup_status="$(curl -sS -o "$response_file" -w '%{http_code}' \
        --connect-timeout 2 --max-time 30 \
        -X POST "$endpoint/api/setup" \
        -H 'Content-Type: application/json' \
        -H "benchmark-task-id: $task_id" \
        --data "{\"episode_id\":\"$episode_id\",\"setup_token\":\"$setup_token\"}" \
        2>/dev/null)"; then
        setup_curl_status=0
    else
        setup_curl_status="$?"
    fi
    response="$(head -c 512 "$response_file" 2>/dev/null || true)"
    rm -f "$response_file"

    case "$setup_status" in
        2??)
            save_bridge_session \
                "$endpoint" "$task_id" "$episode_id" ready "$setup_token"
            service_log INFO bridge_setup_ready "endpoint=$endpoint task_id=$task_id episode_id=$episode_id"
            return 0
            ;;
        404|405)
            save_bridge_session \
                "$endpoint" "$task_id" "$episode_id" ready "$setup_token"
            service_log INFO bridge_setup_unsupported "endpoint=$endpoint status=$setup_status; continuing with generic bridge"
            return 0
            ;;
        *)
            case "$setup_curl_status" in
                3|5|6|7) rm -f "$bridge_session_file" ;;
            esac
            service_log WARN bridge_setup_failed \
                "endpoint=$endpoint status=${setup_status:-000} curl_exit=$setup_curl_status response=$response; starting agent anyway"
            return 1
            ;;
    esac
}

release_bridge_identity() {
    endpoint="$1"
    task_id="$2"
    release_status="$(curl -sS -o /dev/null -w '%{http_code}' \
        --connect-timeout 2 --max-time 5 \
        -X POST "$endpoint/api/release" \
        -H 'Content-Type: application/json' \
        -H "benchmark-task-id: $task_id" \
        --data '{}' 2>/dev/null || true)"

    case "$release_status" in
        2??)
            service_log INFO bridge_released "endpoint=$endpoint task_id=$task_id"
            ;;
        404|405)
            service_log INFO bridge_release_unsupported "endpoint=$endpoint status=$release_status"
            ;;
        *)
            service_log WARN bridge_release_failed "endpoint=$endpoint status=${release_status:-000}"
            ;;
    esac
}

release_bridge_episode() {
    if ! load_bridge_session; then
        return 0
    fi
    release_bridge_identity "$bridge_session_endpoint" "$bridge_session_task_id"
    rm -f "$bridge_session_file"
}

ensure_bridge_episode() {
    ensure_endpoint="$1"
    ensure_task_id="$2"
    ensure_episode_id="$3"

    if bridge_session_matches \
        "$ensure_endpoint" "$ensure_task_id" "$ensure_episode_id"; then
        remote_state="$(bridge_session_remote_state \
            "$ensure_endpoint" "$ensure_task_id")"
        case "$bridge_session_state:$remote_state" in
            ready:active)
                return 0
                ;;
            pending:active|pending:missing)
                service_log INFO bridge_setup_resuming \
                    "endpoint=$ensure_endpoint task_id=$ensure_task_id episode_id=$ensure_episode_id"
                prepare_bridge_episode \
                    "$ensure_endpoint" "$ensure_task_id" "$ensure_episode_id" \
                    "$bridge_session_setup_token" || true
                return 0
                ;;
            ready:missing)
                service_log INFO bridge_session_missing \
                    "endpoint=$ensure_endpoint task_id=$ensure_task_id; setting up a new route"
                prepare_bridge_episode \
                    "$ensure_endpoint" "$ensure_task_id" "$ensure_episode_id" \
                    || true
                return 0
                ;;
            *)
                return 0
                ;;
        esac
    fi
    if load_bridge_session; then
        release_bridge_identity "$bridge_session_endpoint" "$bridge_session_task_id"
        rm -f "$bridge_session_file"
    fi
    prepare_bridge_episode \
        "$ensure_endpoint" "$ensure_task_id" "$ensure_episode_id" || true
}

run_agent() {
    load_system_env

    set -- "$agent_bin" -dir "$agent_dir" -addr 0.0.0.0:8080
    if [ -n "${AICO_DEVICE_TYPE:-}" ]; then
        set -- "$@" --device-type "$AICO_DEVICE_TYPE"
    fi
    bridge_endpoint="${AICO_ENVIRONMENT_BRIDGE_ENDPOINT:-${ENVIRONMENT_BRIDGE_ENDPOINT:-}}"
    bridge_endpoint="${bridge_endpoint%/}"
    bridge_mode="${AICO_ENVIRONMENT_BRIDGE_MODE:-auto}"
    bridge_task_id="${AICO_BENCHMARK_TASK_ID:-docker-sandbox}"
    bridge_episode_id="${AICO_BRIDGE_EPISODE_ID:-docker-sandbox}"
    if bridge_enabled "$bridge_endpoint" "$bridge_mode"; then
        if bridge_identifiers_valid "$bridge_task_id" "$bridge_episode_id"; then
            ensure_bridge_episode \
                "$bridge_endpoint" "$bridge_task_id" "$bridge_episode_id"
            if [ "${stopping:-0}" -ne 0 ]; then
                return 0
            fi
            set -- "$@" \
                --environment-bridge-mode \
                --environment-bridge-endpoint "$bridge_endpoint" \
                --benchmark-task-id "$bridge_task_id"
        else
            release_bridge_episode
            service_log ERROR bridge_disabled "task or episode id contains unsupported characters"
        fi
    else
        release_bridge_episode
    fi

    if [ "${stopping:-0}" -ne 0 ]; then
        return 0
    fi

    service_log INFO process_starting "command=$agent_bin -dir $agent_dir -addr 0.0.0.0:8080"
    "$env_run_bin" "$@" >> "$log_file" 2>&1 &
    agent_pid="$!"
    printf '%s\n' "$agent_pid" > "$agent_pid_file"
    wait_for_agent_exit "$agent_pid"
    status="$?"
    rotate_log_if_needed
    return "$status"
}

supervise() {
    child_pid=""
    stopping=0

    stop_child() {
        stopping=1
        child_pid="$(read_pid "$agent_pid_file")"
        if is_running "$child_pid"; then
            kill "$child_pid" 2>/dev/null || true
        fi
    }

    trap stop_child INT TERM
    printf '%s\n' "$$" > "$supervisor_pid_file"
    trap 'rm -f "$supervisor_pid_file" "$agent_pid_file"' EXIT

    while [ "$stopping" -eq 0 ]; do
        set +e
        run_agent
        status="$?"
        set -e
        rm -f "$agent_pid_file"
        if [ "$stopping" -ne 0 ]; then
            break
        fi
        service_log WARN process_exited "exit_code=$status; restarting in 2s"
        sleep 2
    done
}

start_locked() {
    supervisor_pid="$(read_pid "$supervisor_pid_file")"
    if is_running "$supervisor_pid"; then
        printf 'Agent supervisor is already running (pid %s)\n' "$supervisor_pid"
        return 0
    fi
    rm -f "$supervisor_pid_file" "$agent_pid_file"
    # The supervisor must not inherit the service-operation lock. Otherwise
    # every later Config Web restart waits forever on the original process.
    if [ -w /proc/1/fd/1 ] && [ -w /proc/1/fd/2 ]; then
        nohup setsid "$0" run </dev/null >/proc/1/fd/1 2>/proc/1/fd/2 9>&- &
    else
        nohup setsid "$0" run </dev/null >>"$log_file" 2>&1 9>&- &
    fi
    supervisor_pid="$!"

    attempt=1
    while [ "$attempt" -le 10 ]; do
        recorded_pid="$(read_pid "$supervisor_pid_file")"
        if [ "$recorded_pid" = "$supervisor_pid" ] \
            && is_running "$supervisor_pid"; then
            break
        fi
        if ! is_running "$supervisor_pid"; then
            break
        fi
        sleep 0.1
        attempt="$((attempt + 1))"
    done
    recorded_pid="$(read_pid "$supervisor_pid_file")"
    if [ "$recorded_pid" != "$supervisor_pid" ] \
        || ! is_running "$supervisor_pid"; then
        if is_running "$supervisor_pid"; then
            kill -TERM "-$supervisor_pid" 2>/dev/null \
                || kill -TERM "$supervisor_pid" 2>/dev/null \
                || true
        fi
        rm -f "$supervisor_pid_file" "$agent_pid_file"
        service_log ERROR supervisor_start_failed "pid=$supervisor_pid"
        return 1
    fi
    printf 'Started agent supervisor (pid %s)\n' "$supervisor_pid"
}

stop_locked() {
    supervisor_pid="$(read_pid "$supervisor_pid_file")"

    if is_running "$supervisor_pid"; then
        kill -TERM "-$supervisor_pid" 2>/dev/null || true
    fi

    stop_attempts="${AICO_AGENT_STOP_ATTEMPTS:-150}"
    case "$stop_attempts" in
        ''|0|*[!0-9]*) stop_attempts=150 ;;
    esac
    if [ "$stop_attempts" -gt 150 ]; then
        stop_attempts=150
    fi
    attempt=1
    while is_running "$supervisor_pid" && [ "$attempt" -le "$stop_attempts" ]; do
        sleep 0.1
        attempt="$((attempt + 1))"
    done
    case "$supervisor_pid" in
        ''|*[!0-9]*) ;;
        *) kill -KILL "-$supervisor_pid" 2>/dev/null || true ;;
    esac

    agent_pid="$(read_pid "$agent_pid_file")"
    if is_running "$agent_pid"; then
        kill -KILL "$agent_pid" 2>/dev/null || true
    fi
    rm -f "$supervisor_pid_file" "$agent_pid_file"
    printf 'Stopped agent supervisor\n'
}

with_lock() {
    action="$1"
    (
        flock -x 9
        "$action"
    ) 9>"$lock_file"
}

case "${1:-}" in
    run)
        supervise
        ;;
    start)
        with_lock start_locked
        ;;
    stop)
        (
            flock -x 9
            stop_locked
            release_bridge_episode
        ) 9>"$lock_file"
        ;;
    restart|reload)
        (
            flock -x 9
            stop_locked
            start_locked
        ) 9>"$lock_file"
        ;;
    status)
        supervisor_pid="$(read_pid "$supervisor_pid_file")"
        agent_pid="$(read_pid "$agent_pid_file")"
        printf 'log_path=%s\n' "$log_file"
        if is_running "$supervisor_pid"; then
            printf 'watchdog=running pid=%s\n' "$supervisor_pid"
        else
            printf 'watchdog=stopped\n'
        fi
        if is_running "$agent_pid"; then
            printf 'agent=running pid=%s addr=0.0.0.0:8080\n' "$agent_pid"
        else
            printf 'agent=stopped\n'
        fi
        ;;
    *)
        printf 'Usage: %s {start|stop|restart|status|run}\n' "$0" >&2
        exit 1
        ;;
esac
