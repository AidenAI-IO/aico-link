#!/usr/bin/env bash
#
# Wrapper to launch VPhone iOS benchmark services without sourcing vphone.env
# in every terminal. This script sources vphone.env itself, then starts the
# requested service with the values from that file.
#
# Usage:
#   ./start.sh bridge [extra start_bridge args...]
#   ./start.sh agent  [extra start-agent-daemon args...]
#   ./start.sh run    [--task-id X] [--no-judge] [runner run args...]
#   ./start.sh webui  [extra webui args...]
#   ./start.sh env                # print the loaded config (no secrets)
#
# `bridge` auto-detects the VM's current guest IP from the macOS DHCP leases, so
# you don't edit vphone.env after a VM reboot; it falls back to
# VPHONE_GUEST_SSH_HOST if none is reachable.
# `run` auto-discovers the running agent daemon's port and control token, so you
# never paste --agent-url / --benchmark-token-file. Start a daemon first with
# `./start.sh agent`. Judging is OFF by default; pass --judge-model <model> to
# score, and --judge-key <key> to supply the judge API key inline (no
# export needed).
#
# The env file is picked up in this order:
#   1. $VPHONE_ENV_FILE if already exported
#   2. ./vphone.env next to this script (default)
#   3. --env-file <path> as the FIRST argument after the subcommand
#
# vphone.env is machine-specific and gitignored. On first use, create it from
# the tracked template and edit it for your machine:
#   cp vphone.env.example vphone.env
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_ENV_FILE="$SCRIPT_DIR/vphone.env"

usage() {
  sed -n '7,21p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit "${1:-2}"
}

[ $# -ge 1 ] || usage 2
SUBCMD="$1"; shift

# Allow an explicit "--env-file <path>" override as the first extra arg.
ENV_FILE="${VPHONE_ENV_FILE:-$DEFAULT_ENV_FILE}"
if [ "${1:-}" = "--env-file" ] && [ $# -ge 2 ]; then
  ENV_FILE="$2"; shift 2
fi

if [ ! -r "$ENV_FILE" ]; then
  echo "error: env file not readable: $ENV_FILE" >&2
  # vphone.env is gitignored; on a fresh checkout only the template exists.
  for example in "${ENV_FILE}.example" "$DEFAULT_ENV_FILE.example"; do
    if [ -r "$example" ]; then
      echo "hint: this file is machine-specific and not committed. Create it from the template:" >&2
      echo "        cp \"$example\" \"$ENV_FILE\"" >&2
      echo "      then edit $ENV_FILE for your machine (paths, VM host, socket)." >&2
      break
    fi
  done
  exit 2
fi

# Load the config so callers never have to `set -a; source vphone.env`.
set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a
export VPHONE_ENV_FILE="$ENV_FILE"

# Make homebrew tools (uv, docker, tmux) resolvable under a bare ssh shell too.
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

# Config validation. Paths are judged by whether they actually resolve on this
# machine, never by how they look: any real path is accepted even if it happens
# to resemble the template. The /path/to/... shape of vphone.env.example is used
# only to explain an already-broken value ("you copied the template but never
# edited it"), so it can never reject a working config on its own.
unfilled=0
saw_placeholder=0

note_if_placeholder() {
  case "${1:-}" in
    /path/to/*) saw_placeholder=1 ;;
  esac
}

config_error() {
  local name="$1" value="${2:-}" problem="$3"
  echo "error: $name $problem${value:+: $value}" >&2
  case "$value" in
    /path/to/*)
      echo "hint: that is a vphone.env.example placeholder, so $ENV_FILE looks like an" >&2
      echo "      unedited copy of the template. Fill in this machine's real values." >&2
      ;;
    *)
      echo "hint: fix $name in $ENV_FILE" >&2
      ;;
  esac
  echo "      \`./start.sh env\` lists every value and flags the broken ones." >&2
  exit 2
}

require_set() {
  local name="$1" value="${2:-}"
  [ -n "$value" ] || config_error "$name" "" "is not set in $ENV_FILE"
}

require_dir() {
  local name="$1" value="${2:-}"
  require_set "$name" "$value"
  [ -d "$value" ] || config_error "$name" "$value" "points to a directory that does not exist"
}

require_file() {
  local name="$1" value="${2:-}"
  require_set "$name" "$value"
  [ -r "$value" ] || config_error "$name" "$value" "points to a file that is not readable"
}

# Only the service subcommands need to run from the benchmark root. `env` and
# `help` must stay usable while the config is still broken -- they are how you
# diagnose it -- so the cd happens per-subcommand rather than at load time.
enter_benchmark_root() {
  local root="${VPHONE_BENCHMARK_ROOT:-$SCRIPT_DIR/..}"
  require_dir VPHONE_BENCHMARK_ROOT "$root"
  cd "$root"
  # Publish the resolved root. When vphone.env omits the variable we fall back to
  # $SCRIPT_DIR/.., and later bare references (and child processes) must see that
  # same directory instead of aborting with `unbound variable` under `set -u`.
  export VPHONE_BENCHMARK_ROOT="$PWD"
}

# Print one loaded value for `./start.sh env`, flagging entries that are unset or
# do not resolve, so the diagnostic command is most useful when config is wrong.
# kind: any | dir | file | sock. A missing socket is reported but not counted --
# it is absent whenever the VM is simply stopped, which is not a config error.
print_env_value() {
  local name="$1" value="${2:-}" kind="${3:-any}" note="${4:-}"
  local problem=""
  note_if_placeholder "$value"
  if [ -z "$value" ]; then
    problem="not set"
  else
    case "$kind" in
      dir)  [ -d "$value" ] || problem="directory does not exist" ;;
      file) [ -r "$value" ] || problem="file not readable" ;;
      sock) [ -S "$value" ] || problem="socket not present (VM stopped?)" ;;
    esac
  fi
  if [ -z "$problem" ]; then
    printf '%s=%s%s\n' "$name" "$value" "$note"
    return 0
  fi
  if [ "$kind" != "sock" ] || [ -z "$value" ]; then
    unfilled=$((unfilled + 1))
  fi
  printf '%s=%s%s   <- %s\n' "$name" "$value" "$note" "$problem"
}

# Refuse to start if the target TCP port is already taken, so we fail with a
# clear message instead of a Python "Address already in use" traceback.
require_free_port() {
  local port="$1" label="$2"
  local pid
  pid="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true)"
  if [ -n "$pid" ]; then
    echo "error: $label port $port is already in use by PID $pid:" >&2
    ps -o pid,lstart,command -p "$pid" 2>/dev/null | tail -n +1 >&2 || true
    echo "hint: it may already be running. Stop it first, or use a different port." >&2
    exit 1
  fi
}

# Print the guest VM IP that currently answers on the SSH port, or nothing.
# The iOS VM gets a fresh DHCP address (192.168.64.x) on each boot; macOS records
# it in /var/db/dhcpd_leases. We take the iPhone leases newest-first and return
# the first one whose SSH port actually accepts a connection, so stale leases
# from previous boots are ignored.
detect_guest_ip() {
  local leases=/var/db/dhcpd_leases
  local port="${VPHONE_GUEST_SSH_PORT:-22222}"
  [ -r "$leases" ] || return 0
  local ip
  for ip in $(awk '
      /^{/        {name=""; ip=""; lease=""}
      /name=/       {n=$0; sub(/.*name=/,"",n); name=n}
      /ip_address=/ {i=$0; sub(/.*ip_address=/,"",i); ip=i}
      /lease=/      {l=$0; sub(/.*lease=/,"",l); lease=l}
      /^}/          {if (name=="iPhone" && ip!="") print lease, ip}
    ' "$leases" | sort -r | awk '{print $2}'); do
    if nc -z -G 2 "$ip" "$port" 2>/dev/null; then
      printf '%s\n' "$ip"
      return 0
    fi
  done
  return 0
}

case "$SUBCMD" in
  bridge)
    enter_benchmark_root
    # start_bridge.py re-validates the socket (and reports a stopped VM better
    # than we can here), so only require that a path was configured at all.
    require_set VPHONE_SOCKET "${VPHONE_SOCKET:-}"
    require_free_port "${VPHONE_BRIDGE_PORT:-8899}" "bridge"
    # Auto-detect the VM's current guest IP so you don't edit vphone.env after a
    # reboot. If detection fails, fall back to VPHONE_GUEST_SSH_HOST from the env.
    guest_ip_args=()
    detected_ip="$(detect_guest_ip)"
    if [ -n "$detected_ip" ]; then
      if [ "$detected_ip" != "${VPHONE_GUEST_SSH_HOST:-}" ]; then
        echo "detected VM guest IP $detected_ip (env has ${VPHONE_GUEST_SSH_HOST:-<unset>}); using detected"
      else
        echo "detected VM guest IP $detected_ip (matches env)"
      fi
      guest_ip_args=(--guest-ssh-host "$detected_ip")
    else
      echo "no reachable VM guest IP detected; using env value ${VPHONE_GUEST_SSH_HOST:-<unset>}"
    fi
    echo "starting VPhone bridge (env=$ENV_FILE)"
    exec uv run python -m vphone.scripts.start_bridge --env-file "$ENV_FILE" \
      ${guest_ip_args[@]+"${guest_ip_args[@]}"} "$@"
    ;;
  agent)
    enter_benchmark_root
    require_set VPHONE_BRIDGE_ENDPOINT "${VPHONE_BRIDGE_ENDPOINT:-}"
    require_set VPHONE_BENCHMARK_TASK_ID "${VPHONE_BENCHMARK_TASK_ID:-}"
    require_file VPHONE_AGENT_CONFIG "${VPHONE_AGENT_CONFIG:-}"
    echo "starting agent daemon (bridge=$VPHONE_BRIDGE_ENDPOINT, task_id=$VPHONE_BENCHMARK_TASK_ID)"
    exec uv run python -m runner start-agent-daemon \
      --name vphone-ios \
      --environment-bridge-endpoint "$VPHONE_BRIDGE_ENDPOINT" \
      --benchmark-task-id "$VPHONE_BENCHMARK_TASK_ID" \
      --agent-config "$VPHONE_AGENT_CONFIG" \
      "$@"
    ;;
  run)
    enter_benchmark_root
    require_set VPHONE_BRIDGE_ENDPOINT "${VPHONE_BRIDGE_ENDPOINT:-}"
    require_set VPHONE_BENCHMARK_TASK_ID "${VPHONE_BENCHMARK_TASK_ID:-}"
    # Discover the currently running agent daemon's host port and token, so you
    # never have to paste --agent-url / --benchmark-token-file by hand. The port
    # is auto-assigned per start-agent-daemon run and changes each time.
    ports="$(docker ps --filter name=aico-benchmark-agent --format '{{.Ports}}' | head -1)"
    host_port="$(printf '%s' "$ports" | sed -nE 's/.*127\.0\.0\.1:([0-9]+)->8080.*/\1/p')"
    if [ -z "$host_port" ]; then
      echo "error: no running agent daemon found (container name aico-benchmark-agent*)." >&2
      echo "hint: start one first with: ./start.sh agent" >&2
      exit 1
    fi
    agent_url="http://127.0.0.1:$host_port"
    token_file="${VPHONE_BENCHMARK_ROOT}/runs/cli-services/agent-vphone-ios/config/control_token"
    if [ ! -r "$token_file" ]; then
      echo "error: control token not found: $token_file" >&2
      echo "hint: (re)start the daemon with: ./start.sh agent" >&2
      exit 1
    fi
    # Pull our own --judge-key <key> out of the args (runner does not know it) and
    # feed it to the judge, so you can pass the key inline
    # instead of exporting it. Everything else is forwarded to `runner run`.
    passthru=()
    while [ $# -gt 0 ]; do
      case "$1" in
        --judge-key)
          [ $# -ge 2 ] || { echo "error: --judge-key needs a value" >&2; exit 1; }
          export AICO_BENCHMARK_JUDGE_API_KEY="$2"
          shift 2
          ;;
        --judge-key=*)
          export AICO_BENCHMARK_JUDGE_API_KEY="${1#--judge-key=}"
          shift
          ;;
        *)
          passthru+=("$1")
          shift
          ;;
      esac
    done
    # Default suite unless the caller passes their own --suite.
    suite_args=()
    case " ${passthru[*]:-} " in
      *" --suite "*|*" --suite="*) : ;;
      *) suite_args=(--suite suites/vphone_ios_basic.json) ;;
    esac
    # Judging is OFF by default: it needs an API key, and forgetting it
    # turns every task into JUDGE_ERROR. Opt in with --judge-model / --judge; then
    # require the key up front instead of
    # failing after every task has run.
    judge_args=()
    case " ${passthru[*]:-} " in
      *" --judge-model "*|*" --judge-model="*|*" --judge "*|*" --judge="*)
        if [ -z "${AICO_BENCHMARK_JUDGE_API_KEY:-}" ]; then
          echo "error: judging requested but no judge key provided." >&2
          echo "hint: add --judge-key <judge API key>," >&2
          echo "      or drop --judge-model to run without scoring." >&2
          exit 1
        fi
        ;;
      *" --no-judge "*) : ;;               # caller already opted out
      *) judge_args=(--no-judge) ;;        # default: no scoring
    esac
    echo "running against daemon $agent_url (task_id=$VPHONE_BENCHMARK_TASK_ID)"
    # Expand possibly-empty arrays safely under `set -u` on bash 3.2 (macOS).
    exec uv run python -m runner run \
      ${suite_args[@]+"${suite_args[@]}"} \
      ${judge_args[@]+"${judge_args[@]}"} \
      --agent-url "$agent_url" \
      --benchmark-token-file "$token_file" \
      --environment-url "$VPHONE_BRIDGE_ENDPOINT" \
      --benchmark-task-id "$VPHONE_BENCHMARK_TASK_ID" \
      ${passthru[@]+"${passthru[@]}"}
    ;;
  webui)
    enter_benchmark_root
    require_file VPHONE_AGENT_CONFIG "${VPHONE_AGENT_CONFIG:-}"
    require_free_port "8765" "webui"
    echo "starting benchmark WebUI on :8765 (agent-config=$VPHONE_AGENT_CONFIG)"
    exec uv run python -m runner webui \
      --host 127.0.0.1 \
      --port 8765 \
      --agent-config "$VPHONE_AGENT_CONFIG" \
      "$@"
    ;;
  env)
    echo "env_file=$ENV_FILE"
    print_env_value VPHONE_BENCHMARK_ROOT "${VPHONE_BENCHMARK_ROOT:-}" dir
    print_env_value VPHONE_SOCKET "${VPHONE_SOCKET:-}" sock
    print_env_value VPHONE_BRIDGE_ENDPOINT "${VPHONE_BRIDGE_ENDPOINT:-}"
    print_env_value VPHONE_BENCHMARK_TASK_ID "${VPHONE_BENCHMARK_TASK_ID:-}"
    print_env_value VPHONE_GUEST_SSH_HOST "${VPHONE_GUEST_SSH_HOST:-}"
    print_env_value VPHONE_GUEST_SSH_PORT "${VPHONE_GUEST_SSH_PORT:-}"
    print_env_value VPHONE_AGENT_CONFIG "${VPHONE_AGENT_CONFIG:-}" file "  (contents not printed)"
    if [ "$unfilled" -gt 0 ]; then
      echo "" >&2
      echo "error: $unfilled value(s) above are unset or do not resolve on this machine." >&2
      if [ "$saw_placeholder" -eq 1 ]; then
        echo "hint: $ENV_FILE still holds vphone.env.example placeholders; fill in real values." >&2
      else
        echo "hint: fix the flagged values in $ENV_FILE." >&2
      fi
      exit 1
    fi
    ;;
  -h|--help|help)
    usage 0
    ;;
  *)
    echo "error: unknown subcommand: $SUBCMD" >&2
    usage 2
    ;;
esac
