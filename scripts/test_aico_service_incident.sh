#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly SCRIPT=${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-service-incident
readonly TEST_ROOT=$(mktemp -d)
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "aico-service-incident test failure: $*" >&2
    exit 1
}

[ -x "${SCRIPT}" ] || fail "script missing or not executable"

mkdir -p "${TEST_ROOT}/bin" "${TEST_ROOT}/proc" "${TEST_ROOT}/userdata/agent/log"
cat >"${TEST_ROOT}/bin/systemctl" <<'EOF'
#!/bin/sh
echo "ActiveState=failed"
echo "SubState=dead"
echo "Result=exit-code"
echo "ExecMainStatus=1"
echo "ExecMainCode=exited"
echo "NRestarts=2"
echo "MainPID=0"
EOF
chmod +x "${TEST_ROOT}/bin/systemctl"

cat >"${TEST_ROOT}/proc/loadavg" <<'EOF'
1.25 0.80 0.40 1/100 1
EOF
cat >"${TEST_ROOT}/proc/meminfo" <<'EOF'
MemTotal:       262144 kB
MemAvailable:    80000 kB
EOF
cat >"${TEST_ROOT}/proc/stat" <<'EOF'
cpu  100 0 0 900 0 0 0 0
EOF
mkdir -p "${TEST_ROOT}/var/log/frame_service"
printf 'frame boom\n' >"${TEST_ROOT}/var/log/frame_service/frame_service.log"

export PATH="${TEST_ROOT}/bin:${PATH}"
export SYSTEMCTL_BIN="${TEST_ROOT}/bin/systemctl"
export AICO_DIAGNOSTICS_DIR="${TEST_ROOT}/diagnostics"
export AICO_DIAGNOSTICS_DEDUP_SECONDS=60
export AICO_DIAGNOSTICS_MAX_BYTES=16777216
# The script reads real /proc; still validates event write + dedup + log tail copy
# when log paths exist under the fake root by overriding via bind-style env is not
# supported, so create the real relative paths the script expects when possible.
mkdir -p /tmp 2>/dev/null || true

# Run against the script with a temporary copy that points log path into TEST_ROOT.
WORKDIR=${TEST_ROOT}/work
mkdir -p "${WORKDIR}"
cp "${SCRIPT}" "${WORKDIR}/aico-service-incident"
chmod +x "${WORKDIR}/aico-service-incident"
# Rewrite the frame log path for the hermetic test copy.
sed -i.bak 's|/var/log/frame_service/frame_service.log|'"${TEST_ROOT}"'/var/log/frame_service/frame_service.log|' \
    "${WORKDIR}/aico-service-incident"

"${WORKDIR}/aico-service-incident" aico-frame
[ -s "${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" ] || fail "events file missing"
grep -q '"unit":"aico-frame.service"' "${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" || fail "unit missing"
grep -q '"board":{' "${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" || fail "board snapshot missing"
grep -q 'frame boom' "${AICO_DIAGNOSTICS_DIR}/last/aico-frame.service" || fail "log tail missing"

before=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
"${WORKDIR}/aico-service-incident" aico-frame
after=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
[ "${before}" = "${after}" ] || fail "dedup failed: ${before} -> ${after}"

# A future stamp (clock rollback) must not suppress recording.
future=$(( $(date +%s) + 3600 ))
echo "${future}" >"${AICO_DIAGNOSTICS_DIR}/stamps/aico-frame"
future_before=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
"${WORKDIR}/aico-service-incident" aico-frame
future_after=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
[ "${future_after}" -gt "${future_before}" ] || fail "future stamp suppressed recording"

# Healthy unit snapshots must not be recorded.
cat >"${TEST_ROOT}/bin/systemctl" <<'EOF'
#!/bin/sh
echo "ActiveState=active"
echo "SubState=running"
echo "Result=success"
echo "ExecMainStatus=0"
echo "ExecMainCode=exited"
echo "NRestarts=0"
echo "MainPID=1"
EOF
chmod +x "${TEST_ROOT}/bin/systemctl"
rm -f "${AICO_DIAGNOSTICS_DIR}/stamps/aico-frame"
healthy_before=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
"${WORKDIR}/aico-service-incident" aico-frame
healthy_after=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
[ "${healthy_before}" = "${healthy_after}" ] || fail "healthy unit was recorded"
[ ! -f "${AICO_DIAGNOSTICS_DIR}/stamps/aico-frame" ] || fail "healthy probe must not write dedup stamp"

# A skipped healthy probe must not suppress a following real failure.
cat >"${TEST_ROOT}/bin/systemctl" <<'EOF'
#!/bin/sh
echo "ActiveState=failed"
echo "SubState=dead"
echo "Result=signal"
echo "ExecMainStatus=9"
echo "ExecMainCode=killed"
echo "NRestarts=1"
echo "MainPID=0"
EOF
chmod +x "${TEST_ROOT}/bin/systemctl"
signal_before=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
"${WORKDIR}/aico-service-incident" aico-frame
signal_after=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
[ "${signal_after}" -gt "${signal_before}" ] || fail "failure after healthy probe was suppressed"
grep -q '"result":"signal"' "${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" || fail "signal result missing"

# OnFailure monitor env must win over a already-restarted systemctl show.
cat >"${TEST_ROOT}/bin/systemctl" <<'EOF'
#!/bin/sh
echo "ActiveState=active"
echo "SubState=running"
echo "Result=success"
echo "ExecMainStatus=0"
echo "ExecMainCode=exited"
echo "NRestarts=4"
echo "MainPID=1"
EOF
chmod +x "${TEST_ROOT}/bin/systemctl"
rm -f "${AICO_DIAGNOSTICS_DIR}/stamps/aico-frame"
monitor_before=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
MONITOR_SERVICE_RESULT=signal MONITOR_EXIT_CODE=killed MONITOR_EXIT_STATUS=9 \
    "${WORKDIR}/aico-service-incident" aico-frame
monitor_after=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
[ "${monitor_after}" -gt "${monitor_before}" ] || fail "monitor env failure was not recorded"
tail -n 1 "${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | grep -q '"result":"signal"' || fail "monitor env result missing"
tail -n 1 "${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | grep -q '"exec_main_status":9' || fail "numeric monitor status missing"

# systemd reports signal exits as names (KILL), not numeric MONITOR_EXIT_STATUS.
rm -f "${AICO_DIAGNOSTICS_DIR}/stamps/aico-frame"
kill_before=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
MONITOR_SERVICE_RESULT=signal MONITOR_EXIT_CODE=killed MONITOR_EXIT_STATUS=KILL \
    "${WORKDIR}/aico-service-incident" aico-frame
kill_after=$(wc -l <"${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | tr -d ' ')
[ "${kill_after}" -gt "${kill_before}" ] || fail "KILL monitor status was not recorded"
tail -n 1 "${AICO_DIAGNOSTICS_DIR}/service-events.jsonl" | grep -q '"exec_main_status":"KILL"' \
    || fail "signal name status not preserved as JSON string"

echo "aico-service-incident tests passed"
