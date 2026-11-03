#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly HELPER=${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-machine-id-provision
readonly TEST_ROOT=$(mktemp -d)
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Debian machine-ID provision test failure: $*" >&2
    exit 1
}

mkdir -p "${TEST_ROOT}/bin"
cat >"${TEST_ROOT}/bin/ota" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"${OTA_CALLS}"
printf '%s\n' "${OTA_RESULT}"
EOF
cat >"${TEST_ROOT}/bin/systemctl" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"${SYSTEMCTL_CALLS}"
EOF
chmod +x "${TEST_ROOT}/bin/ota" "${TEST_ROOT}/bin/systemctl"

OTA_CALLS=${TEST_ROOT}/ota.calls \
SYSTEMCTL_CALLS=${TEST_ROOT}/systemctl.calls \
OTA_RESULT='{"reboot_required":false}' \
OTA_BIN=${TEST_ROOT}/bin/ota \
SYSTEMCTL_BIN=${TEST_ROOT}/bin/systemctl \
OTA_CONFIG=${TEST_ROOT}/debian-ota.json \
    "${HELPER}" >"${TEST_ROOT}/normal.out"
grep -qx -- "--config ${TEST_ROOT}/debian-ota.json provision-identity" \
    "${TEST_ROOT}/ota.calls"
[ ! -e "${TEST_ROOT}/systemctl.calls" ] \
    || fail "normal provisioning requested a reboot"
grep -qx '{"reboot_required":false}' "${TEST_ROOT}/normal.out"

: >"${TEST_ROOT}/ota.calls"
set +e
OTA_CALLS=${TEST_ROOT}/ota.calls \
SYSTEMCTL_CALLS=${TEST_ROOT}/systemctl.calls \
OTA_RESULT='{"reboot_required":true}' \
OTA_BIN=${TEST_ROOT}/bin/ota \
SYSTEMCTL_BIN=${TEST_ROOT}/bin/systemctl \
OTA_CONFIG=${TEST_ROOT}/debian-ota.json \
    "${HELPER}" >"${TEST_ROOT}/reboot.out"
status=$?
set -e
[ "${status}" -eq 75 ] || fail "reboot-required exit status is ${status}, want 75"
grep -qx 'reboot --no-block' "${TEST_ROOT}/systemctl.calls"
grep -qx '{"reboot_required":true}' "${TEST_ROOT}/reboot.out"

echo "Debian machine-ID provision helper tests passed"
