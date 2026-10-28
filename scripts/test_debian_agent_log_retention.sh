#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly RETENTION_HELPER=${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-agent-log-retention
readonly RETENTION_PROGRAM=${REPO_ROOT}/overlay-debian/usr/lib/aico/libexec/agent_log_retention.py
readonly UNIT_DIR=${REPO_ROOT}/overlay-debian/etc/systemd/system
readonly TEST_ROOT=$(mktemp -d)
readonly PYTHON_BIN=$(command -v python3)
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Agent log retention test failure: $*" >&2
    exit 1
}

[ -x "${RETENTION_HELPER}" ] || fail "retention helper is missing or not executable"
[ -r "${RETENTION_PROGRAM}" ] || fail "retention program is missing"
grep -Fqx 'StandardOutput=append:/userdata/agent/log/agent.log' \
    "${UNIT_DIR}/aico-agent.service" \
    || fail "agent service no longer writes the expected persistent log"
grep -Fqx 'ExecStart=/usr/lib/aico/aico-agent-log-retention' \
    "${UNIT_DIR}/aico-agent-log-retention.service" \
    || fail "retention service does not execute the retention helper"
grep -Fqx 'OnUnitActiveSec=1min' \
    "${UNIT_DIR}/aico-agent-log-retention.timer" \
    || fail "retention timer does not run once per minute"
grep -Fqx 'Wants=aico-agent-log-retention.timer' \
    "${UNIT_DIR}/aico.target" \
    || fail "aico.target does not start the retention timer"

log_path=${TEST_ROOT}/agent.log
level_path=${TEST_ROOT}/storage_level
config_path=${TEST_ROOT}/agent.toml

run_retention() {
    max_bytes=$1
    retain_bytes=$2
    AICO_AGENT_LOG_PATH="${log_path}" \
    AICO_AGENT_STORAGE_LEVEL_PATH="${level_path}" \
    AICO_AGENT_CONFIG_PATH="${config_path}" \
    AICO_AGENT_LOG_MAX_BYTES="${max_bytes}" \
    AICO_AGENT_LOG_RETAIN_BYTES="${retain_bytes}" \
    AICO_PYTHON_BIN="${PYTHON_BIN}" \
        "${RETENTION_HELPER}"
}

dd if=/dev/zero of="${log_path}" bs=1048576 count=12 2>/dev/null
printf 'normal-tail' >>"${log_path}"
inode_before=$(stat -c %i "${log_path}")
exec 3>>"${log_path}"
run_retention 10485760 5242880
normal_size=$(wc -c <"${log_path}" | tr -d '[:space:]')
normal_block_size=$(stat -f -c %S "${log_path}")
[ "${normal_size}" -le 5242880 ] \
    || fail "normal retention did not reduce the log to the retained limit"
[ "${normal_size}" -gt $((5242880 - normal_block_size)) ] \
    || fail "normal retention discarded more than one filesystem block"
[ "$(tail -c 11 "${log_path}")" = 'normal-tail' ] \
    || fail "normal retention did not preserve the newest log bytes"
[ "$(stat -c %i "${log_path}")" = "${inode_before}" ] \
    || fail "normal retention replaced the log inode"
printf 'after-open-fd\n' >&3
exec 3>&-
[ "$(tail -c 14 "${log_path}")" = 'after-open-fd' ] \
    || fail "the existing append descriptor stopped writing after retention"

dd if=/dev/zero of="${log_path}" bs=1048576 count=5 2>/dev/null
printf 'below-limit' >>"${log_path}"
below_limit_size=$(wc -c <"${log_path}" | tr -d '[:space:]')
run_retention 10485760 5242880
[ "$(wc -c <"${log_path}" | tr -d '[:space:]')" = "${below_limit_size}" ] \
    || fail "a log at the retained size was modified"

printf 'sub-block-log' >"${log_path}"
sub_block_size=$(wc -c <"${log_path}" | tr -d '[:space:]')
run_retention 1 1
[ "$(wc -c <"${log_path}" | tr -d '[:space:]')" = "${sub_block_size}" ] \
    || fail "sub-block retention modified a file below the atomic block bound"

cat >"${config_path}" <<'EOF'
[storage_settings.storage.degraded_mode]
max_agent_log_mb = 1
EOF
printf 'critical\n' >"${level_path}"
dd if=/dev/zero of="${log_path}" bs=1048576 count=2 2>/dev/null
printf 'critical-tail' >>"${log_path}"
run_retention 10485760 5242880
[ "$(wc -c <"${log_path}" | tr -d '[:space:]')" -le 1048576 ] \
    || fail "critical retention did not apply max_agent_log_mb"
[ "$(tail -c 13 "${log_path}")" = 'critical-tail' ] \
    || fail "critical retention did not preserve the newest log bytes"

cat >"${config_path}" <<'EOF'
[storage_settings.storage.degraded_mode]
max_agent_log_mb = 20
EOF
dd if=/dev/zero of="${log_path}" bs=1048576 count=16 2>/dev/null
printf 'configured-limit-tail' >>"${log_path}"
configured_size=$(wc -c <"${log_path}" | tr -d '[:space:]')
run_retention 10485760 5242880
[ "$(wc -c <"${log_path}" | tr -d '[:space:]')" = "${configured_size}" ] \
    || fail "critical retention ignored a configured limit above the normal limit"

rm -f "${level_path}" "${config_path}"
dd if=/dev/zero of="${log_path}" bs=1048576 count=16 2>/dev/null
exec 4>>"${log_path}"
printf '\n' >&4
(
    for sequence in $(seq 1 20000); do
        printf 'writer-%05d\n' "${sequence}" >&4
    done
) &
writer_pid=$!
run_retention 10485760 5242880
wait "${writer_pid}"
exec 4>&-
[ "$(grep -a -c '^writer-' "${log_path}")" = 20000 ] \
    || fail "retention lost records from a concurrent append writer"
[ "$(tail -n 1 "${log_path}")" = 'writer-20000' ] \
    || fail "retention did not preserve the newest concurrent record"

target=${TEST_ROOT}/target.log
printf 'do-not-touch' >"${target}"
ln -s "${target}" "${log_path}.symlink"
AICO_AGENT_LOG_PATH="${log_path}.symlink" \
AICO_AGENT_LOG_MAX_BYTES=1 \
AICO_AGENT_LOG_RETAIN_BYTES=1 \
    AICO_PYTHON_BIN="${PYTHON_BIN}" \
    "${RETENTION_HELPER}" 2>/dev/null \
    && fail "retention helper accepted a symlink log path"
[ "$(cat "${target}")" = 'do-not-touch' ] \
    || fail "retention helper modified a symlink target"

echo "Agent log retention tests passed"
