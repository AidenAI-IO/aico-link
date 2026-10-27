#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly OVERLAY=${REPO_ROOT}/overlay-debian
readonly UNIT=${OVERLAY}/etc/systemd/system/aico-agent.service
readonly HELPER=${OVERLAY}/usr/lib/aico/aico-python-prepare
readonly PROFILE=${OVERLAY}/etc/profile.d/aico-python.sh
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Debian Python environment test failure: $*" >&2
    exit 1
}

[ -x "${HELPER}" ] || fail "directory preparation helper is not executable"
sh -n "${HELPER}"
[ -f "${PROFILE}" ] || fail "login profile is missing"
sh -n "${PROFILE}"
if grep -Eq '^[[:space:]]*(mkdir|install|chmod)[[:space:]]' "${PROFILE}"; then
    fail "login profile must not mutate the filesystem"
fi

for assignment in \
    PYTHONUSERBASE=/userdata/agent/python \
    PIP_USER=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_BREAK_SYSTEM_PACKAGES=1; do
    grep -qx "Environment=${assignment}" "${UNIT}" \
        || fail "Agent unit is missing ${assignment}"
done
grep -qx 'ExecStartPre=/usr/lib/aico/aico-python-prepare' "${UNIT}" \
    || fail "Agent unit does not run the directory preparation helper"

test_userdata=${TEST_ROOT}/data
test_helper=${TEST_ROOT}/aico-python-prepare
sed \
    -e "s#/userdata/agent/python#${test_userdata}/agent/python#g" \
    -e "s#/userdata/agent/log#${test_userdata}/agent/log#g" \
    -e "s#/userdata/agent#${test_userdata}/agent#g" \
    -e "s#/userdata/tmp#${test_userdata}/tmp#g" \
    "${HELPER}" >"${test_helper}"
chmod 0755 "${test_helper}"
"${test_helper}"

[ "$(stat -c %a "${test_userdata}/agent")" = 755 ] \
    || fail "Agent directory mode is not 0755"
[ "$(stat -c %a "${test_userdata}/agent/python")" = 755 ] \
    || fail "Python userbase mode is not 0755"
[ "$(stat -c %a "${test_userdata}/tmp")" = 1777 ] \
    || fail "Python temporary directory mode is not 1777"

rmdir "${test_userdata}/agent/python"
mkdir "${TEST_ROOT}/python-target"
ln -s "${TEST_ROOT}/python-target" "${test_userdata}/agent/python"
if "${test_helper}" >/dev/null 2>&1; then
    fail "directory preparation helper accepted a symlink userbase"
fi

profile_output=$(PYTHONUSERBASE=/wrong PIP_USER=0 PIP_NO_CACHE_DIR=0 \
    PIP_DISABLE_PIP_VERSION_CHECK=0 PIP_BREAK_SYSTEM_PACKAGES=0 sh -c \
    '. "$1"; printf "%s|%s|%s|%s|%s" "$PYTHONUSERBASE" "$PIP_USER" "$PIP_NO_CACHE_DIR" "$PIP_DISABLE_PIP_VERSION_CHECK" "$PIP_BREAK_SYSTEM_PACKAGES"' \
    sh "${PROFILE}")
[ "${profile_output}" = '/userdata/agent/python|1|1|1|1' ] \
    || fail "login profile did not enforce the managed Python environment"

echo "Debian Python environment tests passed"
