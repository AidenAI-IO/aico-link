#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly OVERLAY=${REPO_ROOT}/overlay-debian
readonly UNIT_DIR=${OVERLAY}/etc/systemd/system
readonly TMPFILES=${OVERLAY}/etc/tmpfiles.d/aiden.conf
readonly TEST_ROOT=$(mktemp -d)
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Debian systemd overlay test failure: $*" >&2
    exit 1
}

[ -d "${UNIT_DIR}" ] || fail "overlay-debian systemd unit directory is missing"

grep -qx 'JobRunningTimeoutSec=5s' \
    "${UNIT_DIR}/dev-rtc0.device.d/20-aiden-optional.conf"
grep -qx 'JobRunningTimeoutSec=30s' \
    "${UNIT_DIR}/sys-subsystem-net-devices-wlan0.device.d/20-aiden-optional.conf"
grep -qx 'RuntimeDirectory=sshd' \
    "${UNIT_DIR}/aiden-ssh-identity.service"
grep -qx 'RuntimeDirectoryMode=0755' \
    "${UNIT_DIR}/aiden-ssh-identity.service"
grep -qx 'TimeoutStartSec=180s' \
    "${UNIT_DIR}/aiden-ssh-identity.service"
grep -qx 'for type in ed25519 ecdsa rsa; do' \
    "${OVERLAY}/usr/lib/aiden/aiden-ssh-identity"
grep -q 'runtime_dir=/run/sshd' \
    "${OVERLAY}/usr/lib/aiden/aiden-ssh-identity"
grep -q 'mkdir -p .*"${runtime_dir}"' \
    "${OVERLAY}/usr/lib/aiden/aiden-ssh-identity"
grep -q 'chmod 0755 "${runtime_dir}"' \
    "${OVERLAY}/usr/lib/aiden/aiden-ssh-identity"
grep -qx 'ConditionPathExists=/dev/rtc0' \
    "${UNIT_DIR}/aiden-rtc.service"
if grep -Eq '^(Wants|Requires|After)=.*dev-rtc0\.device' \
    "${UNIT_DIR}/aiden-rtc.service"; then
    fail "optional RTC service still waits for dev-rtc0.device"
fi

while IFS= read -r script; do
    [ "${script##*/}" = aiden-log.sh ] || [ -x "${script}" ] || fail "helper is not executable: ${script#${REPO_ROOT}/}"
    IFS= read -r shebang <"${script}"
    case "${shebang}" in
        *python3*) python3 -c 'import pathlib, sys; p = pathlib.Path(sys.argv[1]); compile(p.read_bytes(), str(p), "exec")' "${script}" ;;
        *) sh -n "${script}" ;;
    esac
done < <(find "${OVERLAY}/usr/lib/aiden" -maxdepth 1 -type f | LC_ALL=C sort)

# Debian units that consume the sanitized environment. This list replaces the
# retired Buildroot init-script map now that overlay/etc/init.d is gone.
environment_consumers='
aiden-ble.service
aiden-wifi-proxy.service
aiden-frame.service
aiden-adb-host.service
aiden-agent.service
aiden-audio.service
aiden-ota-health.service
aiden-hybrid-update.service
aiden-config-web.service
aiden-ttyd.service
'
environment_file=/run/aiden/system.env
for unit in ${environment_consumers}; do
    unit_path=${UNIT_DIR}/${unit}
    [ -f "${unit_path}" ] || fail "environment consumer unit is missing: ${unit}"
    grep -q 'aiden-environment.service' "${unit_path}" \
        || fail "${unit} does not depend on aiden-environment.service"
    grep -qx "EnvironmentFile=${environment_file}" "${unit_path}" \
        || fail "${unit} does not consume ${environment_file}"
done

if rg -n 'aiden-env-run|/etc/init\.d/' "${UNIT_DIR}"; then
    fail "Debian units must not use the Buildroot environment wrapper or SysV scripts"
fi
if rg -n '\b(ifconfig|dhcpcd|dhclient|udhcpc|killall)\b' \
    "${OVERLAY}/usr" "${UNIT_DIR}"; then
    fail "Debian runtime helpers contain a retired network control command"
fi
if rg -n '(^|[[:space:]])\.[[:space:]]+/userdata|source[[:space:]]+/userdata' \
    "${OVERLAY}/usr" "${UNIT_DIR}"; then
    fail "Debian runtime directly sources untrusted userdata"
fi
grep -q '/run/aiden/system.env' "${OVERLAY}/etc/profile.d/aiden-env.sh"
grep -q '/run/wifi_proxy/proxy-env' "${OVERLAY}/etc/profile.d/aiden-env.sh"
sh -n "${OVERLAY}/etc/profile.d/aiden-env.sh"

managed_env_run=${OVERLAY}/usr/lib/aiden/aiden-managed-env-run
proxy_environment=${TEST_ROOT}/proxy-env
printf '%s\n' \
    'HTTP_PROXY="socks5h://127.0.0.1:18080"' \
    'HTTPS_PROXY="socks5h://127.0.0.1:18080"' \
    'ALL_PROXY="socks5h://127.0.0.1:18080"' \
    'NO_PROXY="localhost,127.0.0.1,::1"' \
    >"${proxy_environment}"
managed_output=$(env -i AIDEN_WIFI_PROXY_ENVIRONMENT="${proxy_environment}" \
    HTTP_PROXY=http://upstream.example:8080 "${managed_env_run}" /usr/bin/env)
printf '%s\n' "${managed_output}" \
    | grep -qx 'HTTP_PROXY=socks5h://127.0.0.1:18080'
for proxy_key in http_proxy https_proxy all_proxy; do
    printf '%s\n' "${managed_output}" | grep -qx "${proxy_key}=socks5h://127.0.0.1:18080"
done
printf '%s\n' "${managed_output}" | grep -qx 'no_proxy=localhost,127.0.0.1,::1'
login_output=$(env -i PATH="$PATH" \
    AIDEN_SYSTEM_ENVIRONMENT="${TEST_ROOT}/missing-system-env" \
    AIDEN_WIFI_PROXY_ENVIRONMENT="${proxy_environment}" \
    http_proxy=http://stale.example:8080 \
    sh -c '. "$1"; /usr/bin/env' sh "${OVERLAY}/etc/profile.d/aiden-env.sh")
for proxy_key in HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; do
    printf '%s\n' "${login_output}" | grep -qx "${proxy_key}=socks5h://127.0.0.1:18080"
done
for proxy_key in NO_PROXY no_proxy; do
    printf '%s\n' "${login_output}" | grep -qx "${proxy_key}=localhost,127.0.0.1,::1"
done
login_disabled_output=$(env -i PATH="$PATH" AIDEN_WIFI_PROXY_ENABLED=0 \
    AIDEN_SYSTEM_ENVIRONMENT="${TEST_ROOT}/missing-system-env" \
    AIDEN_WIFI_PROXY_ENVIRONMENT="${proxy_environment}" \
    http_proxy=http://explicit.example:8080 \
    sh -c '. "$1"; /usr/bin/env' sh "${OVERLAY}/etc/profile.d/aiden-env.sh")
printf '%s\n' "${login_disabled_output}" | grep -qx 'http_proxy=http://explicit.example:8080'
if printf '%s\n' "${login_disabled_output}" | grep -q '^HTTP_PROXY='; then
    fail "disabled login proxy must preserve the explicit environment"
fi
if command -v visudo >/dev/null 2>&1; then
    visudo -cf "${OVERLAY}/etc/sudoers.d/20-aiden-proxy"
fi
bypass_output=$(env -i AIDEN_WIFI_PROXY_ENABLED=0 \
    AIDEN_WIFI_PROXY_ENVIRONMENT="${proxy_environment}" \
    HTTP_PROXY=http://upstream.example:8080 "${managed_env_run}" /usr/bin/env)
printf '%s\n' "${bypass_output}" \
    | grep -qx 'HTTP_PROXY=http://upstream.example:8080'

test ! -e "${UNIT_DIR}/oem.mount"
grep -qx 'What=/dev/mmcblk0p9' "${UNIT_DIR}/userdata.mount"
grep -qx 'What=/dev/mmcblk0p10' "${UNIT_DIR}/userdata-ota.mount"
for unit in userdata.mount userdata-ota.mount; do
    grep -q '^Requires=.*aiden-rootfs-grow.service' "${UNIT_DIR}/${unit}" \
        || fail "${unit} can mount after failed filesystem preparation"
done
grep -q 'aiden.slot_suffix' "${OVERLAY}/usr/lib/aiden/aiden-slot-resolve"
grep -q 'Root slot.*disagrees' "${OVERLAY}/usr/lib/aiden/aiden-slot-resolve"
grep -q 'rootfs${AIDEN_SLOT_SUFFIX}' "${OVERLAY}/usr/lib/aiden/aiden-rootfs-grow"
grep -q '^mount -o remount,rw /$' "${OVERLAY}/usr/lib/aiden/aiden-rootfs-grow"
grep -q 'if \[ ! -e "${marker}" \]; then' \
    "${OVERLAY}/usr/lib/aiden/aiden-rootfs-grow"
if grep -q '^ConditionPathExists=!/var/lib/aiden/rootfs-grown$' \
    "${UNIT_DIR}/aiden-rootfs-grow.service"; then
    fail "rootfs-grow must remount the root filesystem on every boot"
fi
grep -q 'systemd-timesyncd.service' \
    "${UNIT_DIR}/aiden-rootfs-grow.service"
grep -qx 'RemainAfterExit=yes' "${UNIT_DIR}/aiden-rootfs-grow.service"
grep -qx 'RefuseManualStop=yes' "${UNIT_DIR}/aiden-rootfs-grow.service" \
    || fail "manual growth-service restarts can stop live data mounts"
grep -q 'mkdir -p /userdata/agent/log' \
    "${OVERLAY}/usr/lib/aiden/aiden-userdata-migrate"

grep -qx 'DHCP=yes' "${OVERLAY}/etc/systemd/network/20-wlan0.network"
grep -qx 'Address=192.168.42.1/24' "${OVERLAY}/etc/systemd/network/30-usb0.network"
grep -qx 'ConfigureWithoutCarrier=yes' "${OVERLAY}/etc/systemd/network/30-usb0.network"
grep -qx 'RequiredForOnline=no' "${OVERLAY}/etc/systemd/network/30-usb0.network"
if rg -n 'networkctl reconfigure (usb0|"?\$\{?interface\}?")' \
    "${OVERLAY}/usr/lib/aiden/aiden-usb-gadget" \
    "${OVERLAY}/usr/lib/aiden/aiden-usb-ecm-watchdog" \
    "${OVERLAY}/usr/lib/aiden/aiden-wait-interface-ip" \
    "${OVERLAY}/usr/lib/aiden/aiden-dynamic-keyboard"; then
    fail "USB helpers must not ask networkd to replace an address they just configured"
fi
grep -q '/userdata/debian/wifi/wpa_supplicant-wlan0.conf' \
    "${UNIT_DIR}/wpa_supplicant@wlan0.service.d/20-aiden.conf"
grep -Fqx 'EnvironmentFile=-/run/aiden/wpa_supplicant-config.env' \
    "${UNIT_DIR}/wpa_supplicant@wlan0.service.d/20-aiden.conf"
grep -Fqx 'Environment=AIDEN_WPA_SUPPLICANT_CONFIG=/userdata/debian/wifi/wpa_supplicant-wlan0.conf' \
    "${UNIT_DIR}/wpa_supplicant@wlan0.service.d/20-aiden.conf"
grep -Fqx 'ExecStart=/usr/sbin/wpa_supplicant -c ${AIDEN_WPA_SUPPLICANT_CONFIG} -i %I' \
    "${UNIT_DIR}/wpa_supplicant@wlan0.service.d/20-aiden.conf"
grep -q -- '/usr/lib/aiden/agent config-web' \
    "${UNIT_DIR}/aiden-config-web.service"
grep -q -- '--wifi-config-environment=/run/aiden/wpa_supplicant-config.env' \
    "${UNIT_DIR}/aiden-config-web.service"
grep -Fq 'in_device = ($0 ~ /^[[:space:]]*\[basic_settings\.device\][[:space:]]*$/' \
    "${OVERLAY}/usr/lib/aiden/aiden-usb-gadget" \
    || fail "Debian USB gadget must read device_type from the grouped config path"
grep -Fq "Unsupported top-level [device] table" \
    "${OVERLAY}/usr/lib/aiden/aiden-usb-gadget" \
    || fail "Debian USB gadget must reject the retired top-level device table"
grep -Eq '^[[:space:]]*reject_legacy_device_config \|\| return 1$' \
    "${OVERLAY}/usr/lib/aiden/aiden-usb-gadget" \
    || fail "Debian USB gadget must reject invalid config before setup"
legacy_agent_config=${TEST_ROOT}/legacy-agent.toml
printf '[device]\ndevice_type = "Android"\n' >"${legacy_agent_config}"
set +e
legacy_gadget_output=$(AGENT_TOML="${legacy_agent_config}" \
    "${OVERLAY}/usr/lib/aiden/aiden-usb-gadget" start 2>&1)
legacy_gadget_status=$?
set -e
[ "${legacy_gadget_status}" -ne 0 ] \
    || fail "Debian USB gadget accepted the retired top-level device table"
printf '%s\n' "${legacy_gadget_output}" \
    | grep -Fq "Unsupported top-level [device] table" \
    || fail "Debian USB gadget did not report the retired device table"
if printf '%s\n' "${legacy_gadget_output}" | grep -Fq "Setting up Aiden USB composite gadget"; then
    fail "Debian USB gadget began setup before rejecting invalid config"
fi
if grep -Eq '^Requires=.*aiden-wifi-proxy\.service' \
    "${UNIT_DIR}/aiden-config-web.service"; then
    fail "Config Web must remain available when the Wi-Fi proxy fails"
fi
grep -Eq '^Wants=.*aiden-wifi-proxy\.service' \
    "${UNIT_DIR}/aiden-config-web.service"
if grep -Eq '(^Wants=|^After=).*aiden-agent\.service' \
    "${UNIT_DIR}/aiden-config-web.service"; then
    fail "Config Web must not wait for the Agent to start"
fi
if grep -Eq '^Requires=.*aiden-agent\.service' \
    "${UNIT_DIR}/aiden-config-web.service"; then
    fail "Config Web must not require the Agent"
fi
if grep -Eq '(^Wants=|^After=).*aiden-agent\.service' \
    "${UNIT_DIR}/aiden-ota-health-marker.service"; then
    fail "OTA health marker must not wait for the Agent to start"
fi
if grep -Eq '^Requires=.*aiden-agent\.service' \
    "${UNIT_DIR}/aiden-ota-health-marker.service"; then
    fail "OTA health marker must not require the Agent"
fi
while IFS= read -r log_path; do
    log_directory=${log_path%/*}
    grep -Fqx "d ${log_directory} 0755 root root -" "${TMPFILES}" \
        || fail "tmpfiles does not create ${log_directory}"
done < <(sed -n 's|^StandardOutput=append:\(/var/log/[^[:space:]]*\)$|\1|p' \
    "${UNIT_DIR}"/*.service | LC_ALL=C sort -u)
grep -q -- '--wifi-interface=wlan0' \
    "${UNIT_DIR}/aiden-config-web.service"
grep -q -- '--wifi-backend=systemd-networkd' \
    "${UNIT_DIR}/aiden-config-web.service"
if grep -q -- '--wifi-iface' "${UNIT_DIR}/aiden-config-web.service"; then
    fail "Config Web still uses the retired --wifi-iface flag"
fi
grep -qx 'ConditionPathExists=/usr/lib/aiden/agent' \
    "${UNIT_DIR}/aiden-config-web.service"
grep -q '/usr/lib/aiden/agent wifi-proxy' \
    "${UNIT_DIR}/aiden-wifi-proxy.service"
grep -q 'aiden-wifi-proxy.service' "${UNIT_DIR}/aiden-agent.service"
grep -q 'aiden-wifi-proxy-agent-restart.path' "${UNIT_DIR}/aiden.target"
proxy_watch_path=${UNIT_DIR}/aiden-wifi-proxy-agent-restart.path
for directive in DefaultDependencies=no After=aiden-wifi-proxy.service Before=shutdown.target Conflicts=shutdown.target; do
    grep -Fqx "${directive}" "${proxy_watch_path}" \
        || fail "Wi-Fi proxy path unit is missing safe ordering: ${directive}"
done
grep -Eq '^After=.*aiden-wifi-proxy\.service' \
    "${UNIT_DIR}/aiden-wifi-proxy-agent-restart.service" \
    || fail "Wi-Fi proxy restart service must wait for the proxy"
grep -q 'AIDEN_WIFI_PROXY_INIT_SCRIPT=/usr/lib/aiden/aiden-wifi-proxy-control' \
    "${UNIT_DIR}/aiden-config-web.service"
grep -q 'aiden-managed-env-run /usr/lib/aiden/agent' \
    "${UNIT_DIR}/aiden-agent.service"
grep -q 'AIDEN_USB_COMPOSITE_REFRESH_COMMAND=/usr/lib/aiden/aiden-usb-ecm-watchdog' \
    "${UNIT_DIR}/aiden-agent.service"
grep -q 'watchdog=running pid=\$pid supervisor=systemd' \
    "${OVERLAY}/usr/lib/aiden/aiden-agent-control"
grep -q 'systemctl show aiden-agent.service -p MainPID --value' \
    "${OVERLAY}/usr/lib/aiden/aiden-agent-control"
grep -q 'aiden-boot-timeline-init.service' "${UNIT_DIR}/aiden-agent.service"
grep -q 'AIDEN_FRAME_SERVICE_INIT_SCRIPT=/usr/lib/aiden/aiden-frame-control' \
    "${UNIT_DIR}/aiden-config-web.service"
grep -q 'aiden-userdata-migrate.service' "${UNIT_DIR}/aiden-frame.service"
grep -qx 'StartLimitIntervalSec=0' "${UNIT_DIR}/aiden-frame.service"
grep -qx 'Restart=on-failure' "${UNIT_DIR}/aiden-frame.service"
grep -qx 'RestartSec=2s' "${UNIT_DIR}/aiden-frame.service"
grep -qx 'Group=aiden' "${UNIT_DIR}/aiden-frame.service"
grep -qx 'SupplementaryGroups=audio video' "${UNIT_DIR}/aiden-frame.service"
grep -qx 'Group=aiden' "${UNIT_DIR}/aiden-audio.service"
grep -qx 'SupplementaryGroups=audio video' "${UNIT_DIR}/aiden-audio.service"
grep -Fqx 'Requires=aiden-environment.service aiden-media-modules.service aiden-audio-mixer.service' \
    "${UNIT_DIR}/aiden-audio.service"
grep -Fqx 'After=aiden-environment.service aiden-media-modules.service aiden-audio-mixer.service sound.target' \
    "${UNIT_DIR}/aiden-audio.service"
grep -Fqx 'Requires=aiden-media-modules.service' \
    "${UNIT_DIR}/aiden-audio-mixer.service"
grep -Fqx 'After=aiden-media-modules.service sound.target' \
    "${UNIT_DIR}/aiden-audio-mixer.service"
grep -Fqx 'Before=aiden-audio.service' \
    "${UNIT_DIR}/aiden-audio-mixer.service"
grep -Fqx 'PartOf=aiden-audio.service' \
    "${UNIT_DIR}/aiden-audio-mixer.service"
grep -Fqx 'ExecStart=/usr/lib/aiden/aiden-audio-mixer' \
    "${UNIT_DIR}/aiden-audio-mixer.service"
bash "${REPO_ROOT}/scripts/test_debian_audio_mixer.sh"
grep -qx 'ExecStart=/usr/lib/aiden/aiden-boot-timeline init-systemd' \
    "${UNIT_DIR}/aiden-boot-timeline-init.service"
grep -qx 'ExecStart=/usr/lib/aiden/aiden-boot-timeline finalize-systemd' \
    "${UNIT_DIR}/aiden-boot-timeline.service"
grep -q 'networkctl reconfigure' "${OVERLAY}/usr/lib/aiden/aiden-wlan-guard"
if grep -qE 'dhcpcd|dhclient' "${OVERLAY}/usr/lib/aiden/aiden-wlan-guard"; then
    fail "Wi-Fi guard takes DHCP ownership from networkd"
fi
grep -Fqx 'insert_if_present aic8800_fdrv.ko he_on="${he_on}" custregd=1' \
    "${OVERLAY}/usr/lib/aiden/aiden-wifi-driver"
grep -Fqx 'he_on=${AIDEN_WIFI_HE:-0}' "${OVERLAY}/usr/lib/aiden/aiden-wifi-driver"
grep -Fq 'stage=begin he_on=${he_on}' "${OVERLAY}/usr/lib/aiden/aiden-wifi-driver"
grep -Fq 'result=failed reason=wlan0-missing' "${OVERLAY}/usr/lib/aiden/aiden-wifi-driver"
grep -Fq 'mark "begin he_on=${he_on}"' "${OVERLAY}/usr/lib/aiden/aiden-wifi-driver"
grep -Fq 'invalid-he_on' "${OVERLAY}/usr/lib/aiden/aiden-wifi-driver"
grep -Fq 'StandardOutput=append:/var/log/wifi_driver/wifi_driver.log' \
    "${UNIT_DIR}/aiden-wifi-driver.service"
grep -Fq 'StandardError=append:/var/log/wifi_driver/wifi_driver.log' \
    "${UNIT_DIR}/aiden-wifi-driver.service"
grep -Fqx 'ExecStartPre=/usr/lib/aiden/aiden-wifi-log-retention' \
    "${UNIT_DIR}/aiden-wifi-driver.service"
grep -Fqx 'ExecStartPre=/usr/lib/aiden/aiden-wifi-log-retention' \
    "${UNIT_DIR}/wpa_supplicant@wlan0.service.d/20-aiden.conf"
grep -Fq 'StandardOutput=append:/var/log/wpa_supplicant/wlan0.log' \
    "${UNIT_DIR}/wpa_supplicant@wlan0.service.d/20-aiden.conf"
grep -Fq 'StandardError=append:/var/log/wpa_supplicant/wlan0.log' \
    "${UNIT_DIR}/wpa_supplicant@wlan0.service.d/20-aiden.conf"
for log_directory in /var/log/wifi_driver /var/log/wpa_supplicant /var/log/wlan_guard; do
    grep -Fqx "d ${log_directory} 0755 root root -" "${TMPFILES}" \
        || fail "tmpfiles does not create ${log_directory}"
done
grep -Fq 'link-not-ready wpa_state=' \
    "${OVERLAY}/usr/lib/aiden/aiden-wlan-guard"
grep -Fq 'recovery-end attempt=' \
    "${OVERLAY}/usr/lib/aiden/aiden-wlan-guard"
grep -Fq 'mark "gateway-failure count=${failures}"' \
    "${OVERLAY}/usr/lib/aiden/aiden-wlan-guard"
grep -Fq 'cat "${log_file}.tmp.$$" >"${log_file}"' \
    "${OVERLAY}/usr/lib/aiden/aiden-wlan-guard"
# Exercise the same direct execution used by ExecStartPre without touching logs.
AIDEN_WIFI_LOG_MAX_LINES=invalid \
    "${OVERLAY}/usr/lib/aiden/aiden-wifi-log-retention"
grep -Fq '/usr/lib/aiden/aiden-wifi-log-retention 2>/dev/null || true' \
    "${OVERLAY}/usr/lib/aiden/aiden-wlan-guard"
grep -Fq 'log_file=${WLAN_GUARD_LOG_FILE:-/var/log/wlan_guard/wlan_guard.log}' \
    "${OVERLAY}/usr/lib/aiden/aiden-wlan-guard"
grep -Fq 'StandardOutput=append:/var/log/wlan_guard/wlan_guard.log' \
    "${UNIT_DIR}/aiden-wlan-guard.service"
grep -Fq 'StandardError=append:/var/log/wlan_guard/wlan_guard.log' \
    "${UNIT_DIR}/aiden-wlan-guard.service"
grep -Fqx 'AIDEN_WIFI_HE=0' "${OVERLAY}/etc/aiden_boot.conf"
grep -Fqx 'ENABLE_WIFIDRV=1' "${OVERLAY}/etc/aiden_boot.conf"
grep -Fqx 'ENABLE_WLAN_GUARD=0' "${OVERLAY}/etc/aiden_boot.conf"
grep -Fqx 'ENABLE_BLUETOOTH_HCI=0' "${OVERLAY}/etc/aiden_boot.conf"
grep -Fqx 'ENABLE_BLE_SERVICE=0' "${OVERLAY}/etc/aiden_boot.conf"
grep -Fqx 'After=aiden-wifi-driver.service dev-ttyS0.device' \
    "${UNIT_DIR}/aiden-bluetooth-attach.service" \
    || fail "Bluetooth attach service does not wait for the Aiden UART0 device"
grep -Fqx 'ConditionPathExists=/dev/ttyS0' \
    "${UNIT_DIR}/aiden-bluetooth-attach.service" \
    || fail "Bluetooth attach service does not target the Aiden UART0 device"
grep -Fqx 'ExecStart=/usr/bin/hciattach -n -s 1500000 /dev/ttyS0 any 1500000 flow nosleep' \
    "${UNIT_DIR}/aiden-bluetooth-attach.service" \
    || fail "Bluetooth attach command does not use the Aiden UART0 transport"
grep -Fq '[ -c /dev/ttyS0 ]' \
    "${OVERLAY}/usr/lib/aiden/aiden-bluetooth-prepare" \
    || fail "Bluetooth preparation does not validate the Aiden UART0 device"
if grep -Rq '/dev/ttyS1' \
    "${UNIT_DIR}/aiden-bluetooth-attach.service" \
    "${OVERLAY}/usr/lib/aiden/aiden-bluetooth-prepare"; then
    fail "Bluetooth runtime still targets the legacy Pico UART1 transport"
fi
grep -Fqx 'Wants=wpa_supplicant@wlan0.service' "${UNIT_DIR}/aiden-wlan-guard.service"
if grep -Eq '^Requires=.*wpa_supplicant@wlan0' "${UNIT_DIR}/aiden-wlan-guard.service"; then
    fail "Wi-Fi guard must survive restarting supplicant during recovery"
fi

grep -Fqx 'SUBSYSTEM=="misc", KERNEL=="rknpu", GROUP="video", MODE="0660"' \
    "${OVERLAY}/etc/udev/rules.d/70-aiden-rknpu.rules"
grep -Fqx 'SUBSYSTEM=="mpi", GROUP="video", MODE="0660"' \
    "${OVERLAY}/etc/udev/rules.d/70-aiden-rknpu.rules"
grep -Fqx 'SUBSYSTEM=="rk_dma_heap", KERNEL=="rk-dma-heap-cma", GROUP="video", MODE="0660", SYMLINK+="dma_heap/system"' \
    "${OVERLAY}/etc/udev/rules.d/70-aiden-rknpu.rules"
grep -Fq 'set_video_access /dev/rknpu' \
    "${OVERLAY}/usr/lib/aiden/aiden-media-modules"
grep -Fq 'for node in /dev/mpi/*' \
    "${OVERLAY}/usr/lib/aiden/aiden-media-modules"
grep -Fq 'rk_heap_node=/dev/rk_dma_heap/rk-dma-heap-cma' \
    "${OVERLAY}/usr/lib/aiden/aiden-media-modules"
grep -Fq 'ln -s ../rk_dma_heap/rk-dma-heap-cma /dev/dma_heap/system' \
    "${OVERLAY}/usr/lib/aiden/aiden-media-modules"

watchdog=${OVERLAY}/usr/lib/aiden/aiden-usb-ecm-watchdog
watchdog_root=${TEST_ROOT}/usb-watchdog
mkdir -p "${watchdog_root}"
printf '%s\n' ffb00000.usb >"${watchdog_root}/UDC"
printf '%s\n' configured >"${watchdog_root}/state"
AIDEN_USB_UDC_FILE="${watchdog_root}/UDC" \
AIDEN_USB_UDC_STATE_FILE="${watchdog_root}/state" \
AIDEN_USB_NET_CLASS_PATH="${watchdog_root}/no-usb0" \
AIDEN_USB_LOCK_DIR="${watchdog_root}/lock" \
AIDEN_USB_GRACE_FILE="${watchdog_root}/grace" \
AIDEN_USB_REFRESH_STATE_FILE="${watchdog_root}/refresh.state" \
    "${watchdog}" refresh
grep -qx 'last_refresh_reason=manual refresh' "${watchdog_root}/refresh.state"
grep -qx 'last_refresh_result=ok' "${watchdog_root}/refresh.state"
grep -qx 'ffb00000.usb' "${watchdog_root}/UDC"

# A composite reset drops the HID session together with ECM, so an unanswered
# ARP burst alone must never be enough to tear a working session down.
grep -Fq 'command -v arping >/dev/null 2>&1 || return 2' "${watchdog}" \
    || fail "a missing arping must not be able to justify a composite reset"
grep -Fq 'carrier_up()' "${watchdog}" \
    || fail "watchdog must treat a carrier-less usb0 as idle rather than stalled"
grep -Fq 'read_rx_packets()' "${watchdog}" \
    || fail "watchdog must sample usb0 RX counters as a positive liveness signal"
if grep -Eq '^threshold=\$\{USB_ECM_FAIL_THRESHOLD:-[1-5]\}$' "${watchdog}"; then
    fail "ECM stall confirmation must span more than a few probe intervals"
fi

# Drive the real watch loop against a fake sysfs to prove the liveness guard:
# ARP goes unanswered throughout, so only the RX counter decides the outcome.
run_ecm_watchdog() {
    local root=$1 seconds=$2
    mkdir -p "${root}/bin" "${root}/net/statistics"
    printf '%s\n' ffb00000.usb >"${root}/UDC"
    printf '%s\n' configured >"${root}/state"
    printf '%s\n' 1 >"${root}/net/carrier"
    printf '%s\n' '0 aa:bb:cc:dd:ee:ff 192.168.42.99 host *' >"${root}/leases"
    printf '#!/bin/sh\nexit 1\n' >"${root}/bin/arping"
    # The host logger may be unable to reach /dev/log in a container. Logging
    # is not under test here, so keep that environmental failure out of the
    # watchdog liveness fixture.
    printf '#!/bin/sh\nexit 0\n' >"${root}/bin/logger"
    chmod 0755 "${root}/bin/arping" "${root}/bin/logger"
    PATH="${root}/bin:${PATH}" \
    AIDEN_USB_UDC_FILE="${root}/UDC" \
    AIDEN_USB_UDC_STATE_FILE="${root}/state" \
    AIDEN_USB_NET_CLASS_PATH="${root}/net" \
    AIDEN_USB_CARRIER_FILE="${root}/net/carrier" \
    AIDEN_USB_RX_PACKETS_FILE="${root}/net/statistics/rx_packets" \
    AIDEN_USB_LEASE_FILE="${root}/leases" \
    AIDEN_USB_LOCK_DIR="${root}/lock" \
    AIDEN_USB_GRACE_FILE="${root}/grace" \
    AIDEN_USB_REFRESH_STATE_FILE="${root}/refresh.state" \
    USB_ECM_PROBE_INTERVAL=1 USB_ECM_FAIL_THRESHOLD=2 USB_ECM_COOLDOWN=1 \
        "${watchdog}" watch >/dev/null 2>&1 &
    watchdog_pid=$!
    sleep "${seconds}"
    if ! kill -0 "${watchdog_pid}" 2>/dev/null; then
        wait "${watchdog_pid}" 2>/dev/null || true
        fail "ECM watchdog exited before the liveness interval"
    fi
    kill "${watchdog_pid}" 2>/dev/null || true
    wait "${watchdog_pid}" 2>/dev/null || true
}

live_root=${TEST_ROOT}/usb-watchdog-live
mkdir -p "${live_root}/net/statistics"
printf '%s\n' 100 >"${live_root}/net/statistics/rx_packets"
(
    for i in $(seq 1 12); do
        printf '%s\n' $((100 + i)) >"${live_root}/net/statistics/rx_packets"
        sleep 1
    done
) &
rx_writer=$!
run_ecm_watchdog "${live_root}" 5
kill "${rx_writer}" 2>/dev/null || true
wait "${rx_writer}" 2>/dev/null || true
grep -qx 'ffb00000.usb' "${live_root}/UDC" \
    || fail "a session still receiving traffic must stay bound to the UDC"
[ ! -f "${live_root}/refresh.state" ] \
    || fail "a session still receiving traffic must never be reset over silent ARP"

# Negative control: with RX frozen the same unanswered probes are a real stall
# and recovery must still fire, so the guard above is not simply disabling it.
stalled_root=${TEST_ROOT}/usb-watchdog-stalled
mkdir -p "${stalled_root}/net/statistics"
printf '%s\n' 100 >"${stalled_root}/net/statistics/rx_packets"
run_ecm_watchdog "${stalled_root}" 6
[ -f "${stalled_root}/refresh.state" ] \
    || fail "a genuinely stalled ECM session must still be recovered"
grep -qx 'last_refresh_reason=ECM stall' "${stalled_root}/refresh.state" \
    || fail "a stalled ECM session must be recovered with the ECM stall reason"

grep -q '/usr/lib/aiden/aiden-environment' "${UNIT_DIR}/aiden-environment.service"
grep -q '/run/aiden/environment.invalid' "${UNIT_DIR}/aiden-environment.service"
grep -q 'schema_version' "${OVERLAY}/usr/lib/aiden/aiden-userdata-migrate"
grep -q 'backup_root=${state_dir}/backups' \
    "${OVERLAY}/usr/lib/aiden/aiden-userdata-migrate"
grep -q 'mark-health' "${OVERLAY}/usr/lib/aiden/aiden-ota-health-aggregate"
grep -q 'provision-identity' "${OVERLAY}/usr/lib/aiden/aiden-machine-id-provision"
grep -q 'Requires=.*aiden-machine-id.service' \
    "${UNIT_DIR}/aiden-environment.service"
grep -q 'Requires=aiden-ota-health-marker.service' \
    "${UNIT_DIR}/aiden-ota-health.service"
grep -q '/userdata/debian/ota/config.json' \
    "${UNIT_DIR}/aiden-ota-health.service"
grep -qx 'TimeoutStartSec=10min' \
    "${UNIT_DIR}/aiden-ota-health.service"
if rg -n 'WriteHealthMarkerIfPending' "${REPO_ROOT}/src/agent/cmd/daemon"; then
    fail "Agent daemon still writes an early OTA health marker"
fi

grep -Fq 'Wants=aiden-ttyd.service' "${UNIT_DIR}/aiden.target"
grep -Fq 'ExecStart=/usr/lib/aiden/aiden-ttyd-start' \
    "${UNIT_DIR}/aiden-ttyd.service"
grep -qx 'User=aiden' "${UNIT_DIR}/aiden-ttyd.service"
grep -qx 'Group=aiden' "${UNIT_DIR}/aiden-ttyd.service"
grep -Fxq "IFS= read -r login_user" \
    "${OVERLAY}/usr/lib/aiden/aiden-ttyd-login"
grep -Fxq 'SU_BIN=${AIDEN_TTYD_SU_BIN:-/bin/su}' \
    "${OVERLAY}/usr/lib/aiden/aiden-ttyd-login"
grep -Fxq 'if [ -z "${login_user}" ] || ! getent passwd "${login_user}" >/dev/null; then' \
    "${OVERLAY}/usr/lib/aiden/aiden-ttyd-login"
grep -Fxq 'exec "${SU_BIN}" --login -- "${login_user}"' \
    "${OVERLAY}/usr/lib/aiden/aiden-ttyd-login"
getent_mock=${TEST_ROOT}/getent
getent_args=${TEST_ROOT}/getent-args
cat >"${getent_mock}" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"${GETENT_ARGS_OUTPUT}"
exit "${GETENT_RESULT:-2}"
EOF
chmod +x "${getent_mock}"
if login_output=$(printf '%s\n' 'missing$' | \
    PATH="${TEST_ROOT}:${PATH}" GETENT_ARGS_OUTPUT="${getent_args}" \
    "${OVERLAY}/usr/lib/aiden/aiden-ttyd-login" 2>&1); then
    fail "unknown ttyd login account unexpectedly succeeded"
else
    login_status=$?
fi
[ "${login_status}" -eq 1 ] || fail "unknown ttyd login account returned ${login_status}"
[ "$(printf '%s' "${login_output}" | tr -d '\r')" = 'login: Invalid login name' ] \
    || fail "unexpected unknown-account output: ${login_output}"
sed -n '1p' "${getent_args}" | grep -Fxq 'passwd'
sed -n '2p' "${getent_args}" | grep -Fxq 'missing$'
su_mock=${TEST_ROOT}/su
su_args=${TEST_ROOT}/su-args
cat >"${su_mock}" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"${SU_ARGS_OUTPUT}"
EOF
chmod +x "${su_mock}"
printf '%s\n' 'build$' | \
    GETENT_RESULT=0 AIDEN_TTYD_SU_BIN="${su_mock}" \
    SU_ARGS_OUTPUT="${su_args}" PATH="${TEST_ROOT}:${PATH}" \
    "${OVERLAY}/usr/lib/aiden/aiden-ttyd-login" >/dev/null
grep -Fxq -- '--login' "${su_args}"
grep -Fxq -- '--' "${su_args}"
grep -Fxq -- 'build$' "${su_args}"
grep -Fq 'ENABLE_TTYD=1' "${OVERLAY}/etc/aiden_boot.conf"
ttyd_mock=${TEST_ROOT}/ttyd-mock
ttyd_args=${TEST_ROOT}/ttyd-args
cat >"${ttyd_mock}" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"${TTYD_ARGS_OUTPUT}"
EOF
chmod +x "${ttyd_mock}"
TTYD_BIN="${ttyd_mock}" TTYD_ARGS_OUTPUT="${ttyd_args}" \
WETTY_PORT=4000 WETTY_FONT_SIZE=20 \
    "${OVERLAY}/usr/lib/aiden/aiden-ttyd-start"
grep -Fxq -- '--base-path' "${ttyd_args}"
grep -Fxq -- '/webtty/' "${ttyd_args}"
grep -Fxq -- '4000' "${ttyd_args}"
grep -Fxq -- 'fontSize=20' "${ttyd_args}"
grep -Fxq -- '/usr/lib/aiden/aiden-ttyd-login' "${ttyd_args}"
grep -qx 'disable dnsmasq.service' \
    "${OVERLAY}/etc/systemd/system-preset/90-aiden.preset"
grep -qx 'disable wpa_supplicant.service' \
    "${OVERLAY}/etc/systemd/system-preset/90-aiden.preset"
grep -qx 'enable aiden-boot-timeline.service' \
    "${OVERLAY}/etc/systemd/system-preset/90-aiden.preset"

timeline_helper=${OVERLAY}/usr/lib/aiden/aiden-boot-timeline
timeline_root=${TEST_ROOT}/boot-timeline
mkdir -p "${timeline_root}/archive"
printf '%s\n' '12.34 8.00' >"${timeline_root}/uptime"
cat >"${timeline_root}/systemctl" <<'EOF'
#!/bin/sh
[ "${1:-}" = show ] || exit 2
case "${2:-}" in
    aiden-agent.service)
        cat <<'PROPS'
InactiveExitTimestampMonotonic=1000000
ActiveEnterTimestampMonotonic=3500000
ExecMainStartTimestampMonotonic=1100000
ExecMainExitTimestampMonotonic=0
ExecMainCode=exited
ExecMainStatus=0
ConditionResult=yes
AssertResult=yes
ActiveState=active
SubState=running
Result=success
PROPS
        ;;
    failed.service)
        cat <<'PROPS'
InactiveExitTimestampMonotonic=0
ActiveEnterTimestampMonotonic=0
ExecMainStartTimestampMonotonic=2000000
ExecMainExitTimestampMonotonic=2800000
ExecMainCode=exited
ExecMainStatus=1
ConditionResult=yes
AssertResult=yes
ActiveState=failed
SubState=failed
Result=exit-code
PROPS
        ;;
    *) exit 1 ;;
esac
EOF
chmod +x "${timeline_root}/systemctl"
timeline_env=(
    "BOOT_TIMELINE_LOG=${timeline_root}/timeline.log"
    "BOOT_TIMELINE_ARCHIVE_DIR=${timeline_root}/archive"
    "BOOT_TIMELINE_ARCHIVE_STATE=${timeline_root}/archive.state"
    "BOOT_TIMELINE_LOCK_FILE=${timeline_root}/timeline.lock"
    "BOOT_TIMELINE_UPTIME_PATH=${timeline_root}/uptime"
    "BOOT_TIMELINE_SYSTEMCTL_BIN=${timeline_root}/systemctl"
    "BOOT_TIMELINE_SYSTEMD_UNITS=aiden-agent.service failed.service"
)
env "${timeline_env[@]}" "${timeline_helper}" init-systemd
env "${timeline_env[@]}" "${timeline_helper}" mark agent:listening
env "${timeline_env[@]}" "${timeline_helper}" finalize-systemd
grep -q 'systemd:begin$' "${timeline_root}/timeline.log"
grep -q '3.50 2.50 unit:active aiden-agent.service sub=running result=success code=exited status=0 condition=yes assert=yes$' \
    "${timeline_root}/timeline.log"
grep -q '2.80 0.80 unit:failed failed.service sub=failed result=exit-code code=exited status=1 condition=yes assert=yes$' \
    "${timeline_root}/timeline.log"
grep -q 'mark agent:listening$' "${timeline_root}/timeline.log"
grep -q 'mark systemd:aiden-target$' "${timeline_root}/timeline.log"
archive_path=$(cat "${timeline_root}/archive.state")
[ -f "${archive_path}" ] || fail "boot timeline archive state is dangling"
cmp "${timeline_root}/timeline.log" "${archive_path}"
env "${timeline_env[@]}" "${timeline_helper}" capture-systemd
[ "$(grep -c ' unit:' "${timeline_root}/timeline.log")" -eq 2 ] \
    || fail "systemd timeline capture is not idempotent"
awk '{ if ($1 + 0 < previous) exit 1; previous = $1 + 0 }' \
    "${timeline_root}/timeline.log" \
    || fail "systemd timeline records are not monotonic"

verify_output=${TEST_ROOT}/systemd-verify.txt
systemd-analyze verify \
    "${UNIT_DIR}"/*.service "${UNIT_DIR}"/*.timer "${UNIT_DIR}"/*.mount "${UNIT_DIR}/aiden.target" \
    >"${verify_output}" 2>&1 || true
if grep -E "${UNIT_DIR}/.*(Unknown key name|Failed to parse|Missing '=')" \
    "${verify_output}"; then
    fail "systemd-analyze found an invalid overlay directive"
fi

"${REPO_ROOT}/scripts/test_debian_ota_health_aggregate.sh"
python3 "${REPO_ROOT}/scripts/test_hybrid_update.py"
"${REPO_ROOT}/scripts/test_debian_machine_id_provision.sh"
"${REPO_ROOT}/scripts/test_debian_agent_control.sh"
"${REPO_ROOT}/scripts/test_debian_agent_log_retention.sh"
"${REPO_ROOT}/scripts/test_debian_frame_control.sh"
"${REPO_ROOT}/scripts/test_debian_python_environment.sh"
python3 "${REPO_ROOT}/scripts/test_wlan_guard.py"
sh -n "${OVERLAY}/etc/ssh/sshrc"
python3 "${REPO_ROOT}/scripts/test_debian_ssh_sessions.py"

echo "Debian systemd overlay tests passed"
