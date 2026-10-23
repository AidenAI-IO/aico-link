#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly HELPER="${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-frame-control"
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Debian frame control test failure: $*" >&2
    exit 1
}

[ -x "${HELPER}" ] || fail "helper is not executable"
sh -n "${HELPER}"

cat >"${TEST_ROOT}/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"${AICO_SYSTEMCTL_TEST_LOG}"
case "${1:-}" in
    restart)
        [ "${2:-}" = aico-frame.service ]
        ;;
    is-active)
        [ "${2:-}" = --quiet ]
        [ "${3:-}" = aico-frame.service ]
        ;;
    show)
        [ "${2:-}" = aico-frame.service ]
        [ "${3:-}" = -p ]
        [ "${4:-}" = MainPID ]
        [ "${5:-}" = --value ]
        printf '%s\n' 812
        ;;
    *) exit 2 ;;
esac
EOF
chmod 0755 "${TEST_ROOT}/systemctl"

AICO_SYSTEMCTL_TEST_LOG="${TEST_ROOT}/systemctl.log" \
PATH="${TEST_ROOT}:${PATH}" "${HELPER}" restart
grep -qx 'restart aico-frame.service' "${TEST_ROOT}/systemctl.log" \
    || fail "restart did not target aico-frame.service"

output="$(AICO_SYSTEMCTL_TEST_LOG="${TEST_ROOT}/systemctl.log" \
    PATH="${TEST_ROOT}:${PATH}" "${HELPER}" status)"
grep -qx 'frame_service=running pid=812 supervisor=systemd' <<<"${output}" \
    || fail "active service status is missing"

echo "Debian frame control tests passed"
