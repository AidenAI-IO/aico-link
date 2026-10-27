#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly AGGREGATOR=${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-ota-health-aggregate
readonly TEST_ROOT=$(mktemp -d)
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Debian OTA health aggregator test failure: $*" >&2
    exit 1
}

mkdir -p "${TEST_ROOT}/bin"
printf '{}\n' >"${TEST_ROOT}/pending_boot.json"
printf 'SAFE_ENV=1\n' >"${TEST_ROOT}/system.env"
cat >"${TEST_ROOT}/aico_boot.conf" <<'EOF'
ENABLE_AGENT=0
ENABLE_AUDIO_SERVICE=0
ENABLE_BLE_SERVICE=0
ENABLE_CONFIG_WEB=1
ENABLE_FRAME_SERVICE=0
ENABLE_USB_DHCP=0
ENABLE_USB_HID=0
ENABLE_WIFIDRV=0
ENABLE_WLAN_GUARD=0
ENABLE_BLUETOOTH_HCI=0
EOF

cat >"${TEST_ROOT}/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "${*: -1}" >>"${SYSTEMCTL_LOG}"
if [ "${FAIL_SERVICE:-}" = "${*: -1}" ]; then
    exit 1
fi
EOF
cat >"${TEST_ROOT}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "${*: -1}" >>"${CURL_LOG}"
if [ "${CURL_FAIL_URL:-}" = "${*: -1}" ]; then
    exit 1
fi
exit "${CURL_STATUS:-0}"
EOF
cat >"${TEST_ROOT}/bin/ota" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" >"${OTA_LOG}"
EOF
chmod +x "${TEST_ROOT}/bin/systemctl" "${TEST_ROOT}/bin/curl" "${TEST_ROOT}/bin/ota"

run_aggregator() {
    : >"${TEST_ROOT}/systemctl.log"
    : >"${TEST_ROOT}/curl.log"
    SYSTEMCTL_LOG=${TEST_ROOT}/systemctl.log \
    CURL_LOG=${TEST_ROOT}/curl.log \
    OTA_LOG=${TEST_ROOT}/ota.log \
    OTA_BIN=${TEST_ROOT}/bin/ota \
    OTA_CONFIG=${TEST_ROOT}/debian-config.json \
    PENDING_PATH=${TEST_ROOT}/pending_boot.json \
    BOOT_CONFIG=${TEST_ROOT}/aico_boot.conf \
    ENVIRONMENT_PATH=${TEST_ROOT}/system.env \
    SYSTEMCTL_BIN=${TEST_ROOT}/bin/systemctl \
    CURL_BIN=${TEST_ROOT}/bin/curl \
    SLEEP_BIN=/bin/true \
    WAIT_SECONDS=0 \
        "${AGGREGATOR}"
}

run_aggregator
grep -qx -- '--config' "${TEST_ROOT}/ota.log"
grep -qx "${TEST_ROOT}/debian-config.json" "${TEST_ROOT}/ota.log"
grep -qx 'mark-health' "${TEST_ROOT}/ota.log"
for service in \
    userdata.mount userdata-ota.mount \
    aico-userdata-migrate.service aico-environment.service \
    systemd-networkd.service aico-config-web.service; do
    grep -qx "${service}" "${TEST_ROOT}/systemctl.log" \
        || fail "core service was not checked: ${service}"
done
grep -qx 'http://127.0.0.1/' "${TEST_ROOT}/curl.log" \
    || fail "configuration portal HTTP was not checked"

rm -f "${TEST_ROOT}/ota.log"
if FAIL_SERVICE=systemd-networkd.service run_aggregator >/dev/null 2>&1; then
    fail "aggregator accepted an inactive local network stack"
fi
[ ! -e "${TEST_ROOT}/ota.log" ] || fail "marker command ran after a failed core check"

printf 'ENABLE_AGENT=1\n' >>"${TEST_ROOT}/aico_boot.conf"
rm -f "${TEST_ROOT}/ota.log"
FAIL_SERVICE=aico-agent.service CURL_FAIL_URL=http://127.0.0.1:8080/ run_aggregator
grep -qx 'mark-health' "${TEST_ROOT}/ota.log" \
    || fail "Agent startup failure prevented the health marker"
grep -qx 'aico-config-web.service' "${TEST_ROOT}/systemctl.log" \
    || fail "Agent failure recovery did not check the configuration portal service"
grep -qx 'http://127.0.0.1/' "${TEST_ROOT}/curl.log" \
    || fail "Agent failure recovery did not check the configuration portal HTTP"

rm -f "${TEST_ROOT}/ota.log"
if FAIL_SERVICE=aico-config-web.service run_aggregator >/dev/null 2>&1; then
    fail "aggregator accepted an inactive configuration portal"
fi
[ ! -e "${TEST_ROOT}/ota.log" ] || fail "marker command ran after inactive Config Web"

rm -f "${TEST_ROOT}/ota.log"
if CURL_STATUS=1 run_aggregator >/dev/null 2>&1; then
    fail "aggregator accepted an unresponsive configuration portal"
fi
[ ! -e "${TEST_ROOT}/ota.log" ] || fail "marker command ran after failed Config Web probe"

# A feature switch must not bypass the recovery portal requirement during OTA.
cp "${TEST_ROOT}/aico_boot.conf" "${TEST_ROOT}/aico_boot.enabled.conf"
for config_web_switch in disabled unset; do
    sed '/^ENABLE_CONFIG_WEB=/d' "${TEST_ROOT}/aico_boot.enabled.conf" >"${TEST_ROOT}/aico_boot.conf"
    if [ "${config_web_switch}" = disabled ]; then
        printf 'ENABLE_CONFIG_WEB=0\n' >>"${TEST_ROOT}/aico_boot.conf"
    fi

    rm -f "${TEST_ROOT}/ota.log"
    if FAIL_SERVICE=aico-config-web.service run_aggregator >/dev/null 2>&1; then
        fail "aggregator accepted an inactive portal with its switch ${config_web_switch}"
    fi
    [ ! -e "${TEST_ROOT}/ota.log" ] || fail "marker command ran with an inactive portal and its switch ${config_web_switch}"

    if CURL_STATUS=1 run_aggregator >/dev/null 2>&1; then
        fail "aggregator accepted an unresponsive portal with its switch ${config_web_switch}"
    fi
    [ ! -e "${TEST_ROOT}/ota.log" ] || fail "marker command ran with an unresponsive portal and its switch ${config_web_switch}"
done

echo "Debian OTA health aggregator tests passed"
