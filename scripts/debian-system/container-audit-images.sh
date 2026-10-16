#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT=/work
readonly OUTPUT_DIR=/out
readonly IMAGE_DIR=${OUTPUT_DIR}/image
readonly SDK_DIR=/sdk
readonly WORK_DIR=${OUTPUT_DIR}/audit-work
readonly ROOTFS_MOUNT=${WORK_DIR}/rootfs
readonly USERDATA_MOUNT=${WORK_DIR}/userdata
readonly OTA_MOUNT=${WORK_DIR}/ota
readonly UNPACK_DIR=${WORK_DIR}/unpacked
readonly REPORT=${OUTPUT_DIR}/audit-report.txt
readonly ROOTFS_CLI_TOOLS_DIR=/rootfs-cli-tools

# shellcheck source=../rootfs_cli_tool_catalog.sh
source "${REPO_ROOT}/scripts/rootfs_cli_tool_catalog.sh"


readonly TTYD_SHA256=b0784080bd78f0a5916462672f461542c607f8ea7cee56b075e8cd04e1ffcc4d

# These are the glibc/armhf VQE binaries shipped by the SDK's USE_32BIT
# RKAUDIO build. Keep the exact digests here so an incompatible replacement
# cannot silently enter the production rootfs.
readonly VQE_AEC_SHA256=3427abaa4b2ab7917d079e6cba46a68a836069bcc7f6b9e94630353fcd8c1a9a
readonly VQE_COMMON_SHA256=de8ff824dd1f2e5ec1074b84490d2836ed9dc61d59d6a90d9cdf19386097263c

mounts=()
unmount_mounts() {
    local index
    for ((index = ${#mounts[@]} - 1; index >= 0; index--)); do
        mountpoint -q "${mounts[index]}" && umount "${mounts[index]}" || true
    done
    mounts=()
}

cleanup() {
    unmount_mounts
    rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

fail() {
    echo "Debian system image audit failure: $*" >&2
    exit 1
}

mount_image() {
    local image=$1
    local target=$2
    mkdir -p "${target}"
    mount -o loop,ro "${image}" "${target}"
    mounts+=("${target}")
}

audit_ext4() {
    local image=$1
    local name=$2
    local features forbidden
    e2fsck -fn "${image}" >>"${REPORT}" 2>&1
    features=$(dumpe2fs -h "${image}" 2>/dev/null \
        | sed -n 's/^Filesystem features:[[:space:]]*//p')
    for forbidden in 64bit huge_file metadata_csum metadata_csum_seed dir_index orphan_file quota; do
        if grep -qw "${forbidden}" <<<"${features}"; then
            fail "forbidden ${name} ext4 feature enabled: ${forbidden}"
        fi
    done
    printf '%s\t%s\n' "${name}" "${features}" >>"${OUTPUT_DIR}/ext4-features.tsv"
}

audit_packages() {
    local package
    for package in \
        systemd-sysv udev dbus kmod libpam-systemd openssh-server sudo adb \
        iproute2 iputils-arping wpasupplicant bluez systemd-resolved \
        systemd-timesyncd dnsmasq-base e2fsprogs dosfstools exfatprogs util-linux-extra v4l-utils libdrm2; do
        awk -F '\t' -v package="${package}" \
            'NR > 1 && ($1 == package || $1 == package ":armhf") {found=1} END {exit !found}' \
            "${OUTPUT_DIR}/packages.txt" \
            || fail "required rootfs package is missing: ${package}"
    done
    if awk -F '\t' '
        NR == 1 {next}
        $1 ~ /^(net-tools|dhcpcd|dhcpcd-base|isc-dhcp-client|flash-kernel|initramfs-tools)(:armhf)?$/ ||
        $1 ~ /^(linux-image-|u-boot)/ {print; found=1}
        END {exit !found}
    ' "${OUTPUT_DIR}/packages.txt"; then
        fail "banned production package is installed"
    fi
}

audit_business_package() {
    python3 "${REPO_ROOT}/scripts/debian-package/system_config.py" audit "${ROOTFS_MOUNT}"
    awk -F '\t' 'NR > 1 && $1 == "aiden-business" {found=1} END {exit !found}' \
        "${OUTPUT_DIR}/packages.txt" || fail "aiden-business is not installed"
    for path in \
        /usr/lib/aiden/agent /usr/lib/aiden/frame_service \
        /usr/lib/aiden/audio_service /usr/lib/aiden/ble_service \
        /usr/lib/aiden/frame_service_cli /usr/lib/aiden/audio_service_cli \
        /usr/lib/aiden/aiden-environment /usr/lib/aiden/ttyd \
        /usr/share/aiden/config-web/index.html \
        /usr/share/aiden/skills/aiden/SKILL.md; do
        test -e "${ROOTFS_MOUNT}${path}" || fail "aiden-business file is missing: ${path}"
    done
    dpkg-query --root="${ROOTFS_MOUNT}" -S /usr/lib/aiden/agent \
        | grep -q '^aiden-business:' || fail "agent is not owned by aiden-business"
}

audit_rootfs_cli_tools() {
    local actual_names expected_names actual_versions expected_versions
    local expected_sha actual_sha name extra

    cmp "${ROOTFS_CLI_TOOLS_DIR}/manifest.sha256" \
        "${OUTPUT_DIR}/rootfs-cli-tools.sha256" \
        || fail "rootfs CLI checksum metadata changed after rootfs assembly"
    cmp "${ROOTFS_CLI_TOOLS_DIR}/versions.txt" \
        "${OUTPUT_DIR}/rootfs-cli-tools-versions.txt" \
        || fail "rootfs CLI version metadata changed after rootfs assembly"

    expected_names=$(rootfs_cli_catalog_names \
        "${REPO_ROOT}/scripts/rootfs_cli_tools.catalog" | LC_ALL=C sort)
    actual_names=$(awk '
        NF == 2 && $1 ~ /^[0-9a-f]{64}$/ && $2 ~ /^[A-Za-z0-9][A-Za-z0-9._+-]*$/ {
            print $2
            next
        }
        { invalid = 1 }
        END { if (invalid) exit 1 }
    ' "${ROOTFS_CLI_TOOLS_DIR}/manifest.sha256" | LC_ALL=C sort) \
        || fail "rootfs CLI checksum manifest is invalid"
    [ "${actual_names}" = "${expected_names}" ] \
        || fail "rootfs CLI checksum manifest does not match the catalog"

    expected_versions=$(rootfs_cli_catalog_records \
        "${REPO_ROOT}/scripts/rootfs_cli_tools.catalog" \
        | awk -F '|' '{ print $1, $2, $5, $8 }')
    actual_versions=$(cat "${ROOTFS_CLI_TOOLS_DIR}/versions.txt")
    [ "${actual_versions}" = "${expected_versions}" ] \
        || fail "rootfs CLI version metadata does not match the catalog"

    while read -r expected_sha name extra; do
        [ -z "${extra:-}" ] || fail "rootfs CLI checksum manifest has extra fields"
        test -x "${ROOTFS_MOUNT}/usr/bin/${name}" \
            || fail "rootfs CLI tool is missing or not executable: ${name}"
        actual_sha=$(sha256sum "${ROOTFS_MOUNT}/usr/bin/${name}" | awk '{print $1}')
        [ "${actual_sha}" = "${expected_sha}" ] \
            || fail "rootfs CLI tool checksum mismatch: ${name}"
    done <"${ROOTFS_CLI_TOOLS_DIR}/manifest.sha256"
}

audit_login_sessions() {
    grep -qx 'UsePAM yes' \
        "${ROOTFS_MOUNT}/etc/ssh/sshd_config.d/20-aiden.conf" \
        || fail "SSH PAM session handling is disabled"
    grep -Eq '^@include[[:space:]]+common-session[[:space:]]*$' \
        "${ROOTFS_MOUNT}/etc/pam.d/sshd" \
        || fail "SSH does not include the common PAM session stack"
    grep -Eq '^session[[:space:]]+optional[[:space:]]+pam_systemd\.so([[:space:]]|$)' \
        "${ROOTFS_MOUNT}/etc/pam.d/common-session" \
        || fail "PAM does not register logind sessions"
    test -s "${ROOTFS_MOUNT}/usr/lib/arm-linux-gnueabihf/security/pam_systemd.so" \
        || fail "pam_systemd module is missing"
    test -x "${ROOTFS_MOUNT}/usr/lib/aiden/aiden-ssh-session-tty" \
        || fail "SSH terminal registration helper is missing"
    cmp "${ROOTFS_MOUNT}/etc/ssh/sshrc" "${REPO_ROOT}/overlay-debian/etc/ssh/sshrc" \
        || fail "SSH terminal registration hook is missing or changed"
}

audit_rootfs() {
    grep -qx 'LANG=C.UTF-8' "${ROOTFS_MOUNT}/etc/locale.conf" \
        || fail "default UTF-8 locale is missing"
    test "$(readlink "${ROOTFS_MOUNT}/etc/default/locale")" = ../locale.conf \
        || fail "Debian locale compatibility link is missing"
    python3 "${REPO_ROOT}/scripts/release/contract.py" platform "${OUTPUT_DIR}/expected-contract.json"
    cmp "${OUTPUT_DIR}/expected-contract.json" \
        "${ROOTFS_MOUNT}/usr/lib/aiden/platform/contract.json" \
        || fail "platform contract does not match the base image"
    grep -qx 'status=pass' "${OUTPUT_DIR}/rootfs-import-audit.txt" \
        || fail "rootfs import attribute audit did not pass"
    test "$(stat -c '%u:%g:%a' "${ROOTFS_MOUNT}")" = 0:0:755 \
        || fail "rootfs ownership or mode is invalid"
    test -x "${ROOTFS_MOUNT}/lib/systemd/systemd" || fail "systemd PID 1 is missing"
    test -x "${ROOTFS_MOUNT}/usr/sbin/sshd" || fail "sshd is missing"
    test -x "${ROOTFS_MOUNT}/usr/sbin/hwclock" || fail "hwclock is missing"
    audit_login_sessions
    grep -qx 'aiden:x:1000:1000::/home/aiden:/bin/bash' \
        "${ROOTFS_MOUNT}/etc/passwd" || fail "aiden login user is missing"
    grep -qx 'aiden:x:1000:' "${ROOTFS_MOUNT}/etc/group" \
        || fail "aiden primary group is missing"
    for group in sudo audio video dialout plugdev netdev; do
        grep -Eq "^${group}:[^:]*:[^:]*:([^,]*,)*aiden(,|$)" \
            "${ROOTFS_MOUNT}/etc/group" \
            >/dev/null || fail "aiden is missing required ${group} group"
    done
    test "$(stat -c '%u:%g:%a' "${ROOTFS_MOUNT}/home/aiden")" = 1000:1000:700 \
        || fail "aiden home ownership or mode is invalid"
    test "$(stat -c '%u:%g:%a' "${ROOTFS_MOUNT}/usr/bin/sudo")" = 0:0:4755 \
        || fail "sudo executable ownership or mode is invalid"
    test "$(stat -c '%u:%g:%a' "${ROOTFS_MOUNT}/etc/sudoers")" = 0:0:440 \
        || fail "sudoers ownership or mode is invalid"
    test "$(stat -c '%u:%g:%a' "${ROOTFS_MOUNT}/etc/sudoers.d/20-aiden-proxy")" = 0:0:440 \
        || fail "sudo proxy policy ownership or mode is invalid"
    grep -Eq '^%sudo[[:space:]]+ALL=\(ALL:ALL\)[[:space:]]+ALL$' \
        "${ROOTFS_MOUNT}/etc/sudoers" \
        || fail "sudo group does not require password-authenticated administrator access"
    if grep -REq '(^|[[:space:],])NOPASSWD:' \
        "${ROOTFS_MOUNT}/etc/sudoers" "${ROOTFS_MOUNT}/etc/sudoers.d"; then
        fail "passwordless sudo policy is present"
    fi
    grep -qx 'PasswordAuthentication yes' \
        "${ROOTFS_MOUNT}/etc/ssh/sshd_config.d/20-aiden.conf" \
        || fail "ordinary-user SSH password authentication is disabled"
    grep -qx 'PermitRootLogin prohibit-password' \
        "${ROOTFS_MOUNT}/etc/ssh/sshd_config.d/20-aiden.conf" \
        || fail "root SSH password authentication is not prohibited"
    test -x "${ROOTFS_MOUNT}/usr/bin/adb" || fail "Debian adb is missing"
    test -x "${ROOTFS_MOUNT}/usr/bin/python3" || fail "Debian Python is missing"
    test -x "${ROOTFS_MOUNT}/usr/bin/pip3" || fail "Debian pip is missing"
    test -x "${ROOTFS_MOUNT}/usr/bin/hciattach" || fail "hciattach is missing"
    test -x "${ROOTFS_MOUNT}/usr/sbin/dnsmasq" || fail "dnsmasq-base is missing"
    test -x "${ROOTFS_MOUNT}/usr/lib/aiden/aiden-usb-gadget" \
        || fail "USB gadget helper was not installed"
    test -x "${ROOTFS_MOUNT}/usr/lib/aiden/aiden-boot-timeline" \
        || fail "boot timeline helper was not installed"
    test -x "${ROOTFS_MOUNT}/usr/lib/aiden/aiden-machine-id-provision" \
        || fail "machine-ID provision helper was not installed"
    test -x "${ROOTFS_MOUNT}/usr/lib/aiden/aiden-wifi-log-retention" \
        || fail "Wi-Fi log retention helper is missing or not executable"
    cmp "${ROOTFS_MOUNT}/usr/lib/aiden/aiden-usb-gadget" \
        "${REPO_ROOT}/overlay-debian/usr/lib/aiden/aiden-usb-gadget"
    cmp "${ROOTFS_MOUNT}/usr/lib/aiden/aiden-boot-timeline" \
        "${REPO_ROOT}/overlay-debian/usr/lib/aiden/aiden-boot-timeline"
    test ! -s "${ROOTFS_MOUNT}/etc/machine-id" \
        || fail "generic rootfs contains a machine-id"
    test -L "${ROOTFS_MOUNT}/var/lib/dbus/machine-id" \
        || fail "D-Bus machine-id is not linked"
    test "$(readlink "${ROOTFS_MOUNT}/var/lib/dbus/machine-id")" = /etc/machine-id \
        || fail "D-Bus machine-id link has the wrong target"
    if find "${ROOTFS_MOUNT}/etc/ssh" -maxdepth 1 -name 'ssh_host_*' -print -quit \
        | grep -q .; then
        fail "build-time SSH host key leaked into the rootfs"
    fi
    test ! -e "${ROOTFS_MOUNT}/usr/bin/qemu-arm-static" \
        || fail "qemu-arm-static leaked into the target rootfs"
    test ! -e "${ROOTFS_MOUNT}/var/cache/apt/pkgcache.bin" \
        || fail "nondeterministic APT package cache leaked into the rootfs"
    test ! -e "${ROOTFS_MOUNT}/var/cache/apt/srcpkgcache.bin" \
        || fail "nondeterministic APT source cache leaked into the rootfs"
    test ! -e "${ROOTFS_MOUNT}/var/cache/ldconfig/aux-cache" \
        || fail "nondeterministic ldconfig cache leaked into the rootfs"
    test "$(stat -c '%u:%g:%a' "${ROOTFS_MOUNT}/tmp")" = 0:0:1777 \
        || fail "/tmp ownership or mode is invalid"
    test "$(stat -c '%u:%g:%a' "${ROOTFS_MOUNT}/var/tmp")" = 0:0:1777 \
        || fail "/var/tmp ownership or mode is invalid"
    test ! -e "${ROOTFS_MOUNT}/oem" && test ! -L "${ROOTFS_MOUNT}/oem" \
        || fail "retired /oem path is present in rootfs"
    if find "${ROOTFS_MOUNT}" -type l -lname '/oem*' -print -quit | grep -q .; then
        fail "rootfs contains a link to the retired OEM path"
    fi
    test "$(stat -c '%u:%g:%a' "${ROOTFS_MOUNT}/userdata")" = 0:0:755 \
        || fail "/userdata ownership or mode is invalid"
    if find "${ROOTFS_MOUNT}/etc/init.d" -maxdepth 1 \
        \( -name 'S[0-9][0-9]*' -o -name rcS \) -print -quit | grep -q .; then
        fail "Buildroot SysV startup file leaked into the Debian rootfs"
    fi
    test ! -e "${ROOTFS_MOUNT}/etc/opkg" || fail "opkg configuration leaked into Debian"
    test ! -e "${ROOTFS_MOUNT}/usr/bin/opkg" || fail "opkg leaked into Debian"
    if rg -n '\b(ifconfig|dhcpcd|dhclient|udhcpc)\b' \
        "${ROOTFS_MOUNT}/usr/lib/aiden" "${ROOTFS_MOUNT}/etc/systemd/system"; then
        fail "retired network manager command leaked into Debian runtime"
    fi

    for directory in adb audio_service ble_service frame_service ota ttyd wifi_proxy; do
        grep -Fqx "d /var/log/${directory} 0755 root root -" \
            "${ROOTFS_MOUNT}/etc/tmpfiles.d/aiden.conf" \
            || fail "tmpfiles does not create /var/log/${directory}"
    done

    proxy_watch_path=${ROOTFS_MOUNT}/etc/systemd/system/aiden-wifi-proxy-agent-restart.path
    for directive in DefaultDependencies=no After=aiden-wifi-proxy.service Before=shutdown.target Conflicts=shutdown.target; do
        grep -Fqx "${directive}" "${proxy_watch_path}" \
            || fail "Wi-Fi proxy path unit is missing safe ordering: ${directive}"
    done
    wifi_drop_in=${ROOTFS_MOUNT}/etc/systemd/system/wpa_supplicant@wlan0.service.d/20-aiden.conf
    grep -Fqx 'EnvironmentFile=-/run/aiden/wpa_supplicant-config.env' "${wifi_drop_in}" \
        || fail "wpa_supplicant does not consume the runtime candidate selector"
    grep -Fqx 'Environment=AIDEN_WPA_SUPPLICANT_CONFIG=/userdata/debian/wifi/wpa_supplicant-wlan0.conf' "${wifi_drop_in}" \
        || fail "wpa_supplicant does not define the persistent config fallback"
    grep -Fqx 'ExecStart=/usr/sbin/wpa_supplicant -c ${AIDEN_WPA_SUPPLICANT_CONFIG} -i %I' "${wifi_drop_in}" \
        || fail "wpa_supplicant does not expand the selected config path"
    grep -Eq '^After=.*aiden-wifi-proxy\.service' \
        "${ROOTFS_MOUNT}/etc/systemd/system/aiden-wifi-proxy-agent-restart.service" \
        || fail "Wi-Fi proxy restart service is missing proxy ordering"
    cmp "${ROOTFS_MOUNT}/etc/systemd/system/aiden-bluetooth-attach.service" \
        "${REPO_ROOT}/overlay-debian/etc/systemd/system/aiden-bluetooth-attach.service" \
        || fail "Bluetooth attach service differs from the Aiden UART0 source"
    cmp "${ROOTFS_MOUNT}/usr/lib/aiden/aiden-bluetooth-prepare" \
        "${REPO_ROOT}/overlay-debian/usr/lib/aiden/aiden-bluetooth-prepare" \
        || fail "Bluetooth preparation differs from the Aiden UART0 source"

    test -L "${ROOTFS_MOUNT}/etc/systemd/system/multi-user.target.wants/aiden.target" \
        || fail "aiden.target is not enabled"
    test -L "${ROOTFS_MOUNT}/etc/systemd/system/multi-user.target.wants/aiden-machine-id.service" \
        || fail "aiden-machine-id.service is not enabled"
    test -L "${ROOTFS_MOUNT}/etc/systemd/system/multi-user.target.wants/aiden-boot-timeline.service" \
        || fail "aiden-boot-timeline.service is not enabled"
    test "$(readlink "${ROOTFS_MOUNT}/etc/systemd/system/dnsmasq.service")" = /dev/null \
        || fail "global dnsmasq service is not masked"
    test "$(readlink "${ROOTFS_MOUNT}/etc/systemd/system/wpa_supplicant.service")" = /dev/null \
        || fail "global wpa_supplicant service is not masked"
    test "$(readlink "${ROOTFS_MOUNT}/etc/systemd/system/ssh.socket")" = /dev/null \
        || fail "ssh.socket could bypass persistent SSH identity ordering"
    test "$(readlink "${ROOTFS_MOUNT}/etc/systemd/system/rsync.service")" = /dev/null \
        || fail "rsync daemon is unexpectedly enabled"
    test -L "${ROOTFS_MOUNT}/etc/systemd/system/getty.target.wants/serial-getty@ttyFIQ0.service" \
        || fail "serial recovery console is not enabled"

    systemd-analyze --root="${ROOTFS_MOUNT}" verify \
        aiden.target aiden-machine-id.service aiden-agent.service aiden-config-web.service \
        aiden-wifi-proxy.service aiden-wifi-proxy-agent-restart.path \
        aiden-wifi-proxy-agent-restart.service \
        aiden-ttyd.service aiden-usb-gadget.service aiden-boot-timeline-init.service \
        aiden-boot-timeline.service userdata.mount userdata-ota.mount \
        >"${OUTPUT_DIR}/systemd-unit-audit.txt" 2>&1 || {
            cat "${OUTPUT_DIR}/systemd-unit-audit.txt" >&2
            fail "systemd unit verification failed"
        }
}

audit_platform_files() {
    local actual expected library expected_sha actual_sha
    local storage_tool
    for storage_tool in mkfs.vfat fsck.vfat mkfs.ext4 e2fsck mkfs.exfat fsck.exfat; do
        test -x "${ROOTFS_MOUNT}/usr/sbin/${storage_tool}" \
            || fail "storage tool is missing or not executable: ${storage_tool}"
    done
    test -x "${ROOTFS_MOUNT}/usr/lib/aiden/aiden-dynamic-keyboard" \
        || fail "dynamic keyboard helper is missing"
    (cd "${ROOTFS_MOUNT}" && sha256sum -c "${OUTPUT_DIR}/platform-files.sha256") \
        >>"${REPORT}" || fail "platform files changed after rootfs assembly"
    "${REPO_ROOT}/scripts/validate_ota_pubkey.sh" "${ROOTFS_MOUNT}/usr/share/keyrings/aiden-ota.pem"
    actual_sha=$(sha256sum "${ROOTFS_MOUNT}/usr/lib/aiden/ttyd" | awk '{print $1}')
    [ "${actual_sha}" = "${TTYD_SHA256}" ] \
        || fail "ttyd checksum mismatch"
    if find "${ROOTFS_MOUNT}/usr/lib/aiden/platform" \( -name '*.a' -o -name '*.la' -o -name '*.o' \
        -o -name '*.map' -o -name '*.pc' -o -name CMakeFiles \
        -o -name pkgconfig -o -name include \) -print -quit | grep -q .; then
        fail "build-only artifact leaked into platform"
    fi
    test -L "${ROOTFS_MOUNT}/usr/lib/aiden/platform/lib/librga.so" || fail "librga.so symlink is missing"
    test "$(readlink "${ROOTFS_MOUNT}/usr/lib/aiden/platform/lib/librga.so")" = librga.so.2 \
        || fail "librga.so symlink is invalid"
    test -L "${ROOTFS_MOUNT}/usr/lib/aiden/platform/lib/librga.so.2" || fail "librga.so.2 symlink is missing"
    test "$(readlink "${ROOTFS_MOUNT}/usr/lib/aiden/platform/lib/librga.so.2")" = librga.so.2.1.0 \
        || fail "librga.so.2 symlink is invalid"
    for library in libaec_bf_process.so librkaudio_common.so; do
        case "${library}" in
            libaec_bf_process.so) expected_sha=${VQE_AEC_SHA256} ;;
            librkaudio_common.so) expected_sha=${VQE_COMMON_SHA256} ;;
            *) fail "unexpected VQE library name: ${library}"; continue ;;
        esac
        test -s "${ROOTFS_MOUNT}/usr/lib/aiden/platform/lib/${library}" \
            || fail "VQE runtime library is missing: ${library}"
        cmp "${ROOTFS_MOUNT}/usr/lib/aiden/platform/lib/${library}" \
            "${REPO_ROOT}/overlay-debian/usr/lib/aiden/platform/lib/${library}" \
            || fail "VQE runtime library differs from the Debian platform source: ${library}"
        actual_sha=$(sha256sum "${ROOTFS_MOUNT}/usr/lib/aiden/platform/lib/${library}" | awk '{print $1}')
        [ "${actual_sha}" = "${expected_sha}" ] \
            || fail "VQE runtime library checksum mismatch: ${library}"
    done
    test ! -e "${ROOTFS_MOUNT}/usr/lib/aiden/platform/lib/librknnrt.so" \
        || fail "obsolete dynamic librknnrt.so leaked into platform"
    test -s "${ROOTFS_MOUNT}/usr/share/keyrings/aiden-ota.pem" || fail "OTA public key is missing"
    test -s "${ROOTFS_MOUNT}/usr/lib/aiden/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn" \
        || fail "VAD model is missing from aiden-business"
    test -s "${ROOTFS_MOUNT}/usr/share/aiden/edid/hdmi_1080p30_cta.hex" \
        || fail "EDID asset is missing"
    test -s "${ROOTFS_MOUNT}/usr/share/aiden/audio/voice-notifications/tts-unavailable.en-US.wav" \
        || fail "voice notification is missing from aiden-business"
    test -s "${ROOTFS_MOUNT}/usr/share/aiden/config-web/index.html" \
        || fail "config-web assets are missing from aiden-business"
    test -s "${ROOTFS_MOUNT}/usr/share/aiden/quick_actions.json" \
        || fail "quick actions are missing from aiden-business"
    test "$(find "${ROOTFS_MOUNT}/usr/share/aiden/skills" -mindepth 2 -maxdepth 2 \
        -name SKILL.md -type f | wc -l)" -ge 1 || fail "bundled skills are missing from aiden-business"
    for module in \
        libarc4.ko ctr.ko ccm.ko aes_generic.ko cfg80211.ko \
        aic8800_bsp.ko aic8800_fdrv.ko aic8800_btlpm.ko \
        rga3.ko mpp_vcodec.ko rknpu.ko rockit.ko; do
        test -s "${ROOTFS_MOUNT}/usr/lib/aiden/platform/modules/${module}" || fail "kernel module is missing: ${module}"
    done
    local firmware
    for firmware in \
        aic_userconfig_8800d80.txt \
        fmacfw_8800d80_h_u02.bin \
        fmacfw_8800d80_u02.bin \
        fw_adid_8800d80_u02.bin \
        fw_patch_8800d80_u02.bin \
        fw_patch_8800d80_u02_ext0.bin \
        fw_patch_table_8800d80_u02.bin \
        lmacfw_rf_8800d80_u02.bin; do
        test -s "${ROOTFS_MOUNT}/usr/lib/aiden/platform/modules/aic8800dc_fw/${firmware}" \
            || fail "AIC8800D80 firmware is missing: ${firmware}"
    done
}

audit_elf_closure() {
    declare -A libraries=()
    local library file dynamic dependency runpath machine relative
    while IFS= read -r -d '' library; do
        libraries["$(basename "${library}")"]=${library}
    done < <(find \
        "${ROOTFS_MOUNT}/lib" "${ROOTFS_MOUNT}/usr/lib" \
        \( -type f -o -type l \) -name '*.so*' -print0)

    printf 'path\tmachine\trunpath\tneeded\n' >"${OUTPUT_DIR}/elf-runtime-audit.tsv"
    while IFS= read -r -d '' file; do
        head -c 4 "${file}" | grep -q $'\177ELF' || continue
        relative="/${file#${ROOTFS_MOUNT}/}"
        machine=$(readelf -hW "${file}" 2>/dev/null \
            | sed -n 's/^[[:space:]]*Machine:[[:space:]]*//p')
        [ "${machine}" = ARM ] || fail "non-ARM ELF leaked into platform: ${relative}"
        dynamic=$(readelf -dW "${file}" 2>/dev/null || true)
        if grep -qE 'libc\.so\.0|ld-uClibc' <<<"${dynamic}" \
            || strings "${file}" | grep -qE '/ld-uClibc|libc\.so\.0'; then
            fail "uClibc dependency leaked into platform: ${relative}"
        fi
        runpath=$(sed -n 's/.*(\(RPATH\|RUNPATH\)).*[[]\([^]]*\)[]].*/\2/p' <<<"${dynamic}")
        case "${relative}:${runpath}" in
            /usr/bin/*:'$ORIGIN/../lib' | /usr/lib/aiden/*:'$ORIGIN/../lib' | *:) ;;
            *) fail "unapproved runtime RPATH/RUNPATH: ${relative}: ${runpath}" ;;
        esac
        while IFS= read -r dependency; do
            [ -n "${dependency}" ] || continue
            [ -n "${libraries[${dependency}]:-}" ] \
                || fail "unresolved runtime DT_NEEDED ${dependency}: ${relative}"
        done < <(sed -n 's/.*(NEEDED).*[[]\([^]]*\)[]].*/\1/p' <<<"${dynamic}")
        printf '%s\t%s\t%s\t%s\n' "${relative}" "${machine}" "${runpath}" \
            "$(sed -n 's/.*(NEEDED).*[[]\([^]]*\)[]].*/\1/p' <<<"${dynamic}" | paste -sd, -)" \
            >>"${OUTPUT_DIR}/elf-runtime-audit.tsv"
    done < <(find "${ROOTFS_MOUNT}/usr/lib/aiden" -xdev -type f -print0)
}

audit_boot() {
    local dumpimage=${SDK_DIR}/sysdrv/source/uboot/u-boot/tools/dumpimage
    local slot suffix root_label boot fdt bootargs
    test -x "${dumpimage}" || fail "SDK dumpimage tool is missing"
    : >"${OUTPUT_DIR}/boot-fit-audit.txt"
    for slot in a b; do
        suffix=_${slot}
        root_label=rootfs_${slot}
        boot=${IMAGE_DIR}/boot_${slot}.img
        fdt=${WORK_DIR}/boot_${slot}.dtb
        "${dumpimage}" -l "${boot}" >>"${OUTPUT_DIR}/boot-fit-audit.txt"
        "${dumpimage}" -i "${boot}" -T flat_dt -p 0 -o "${fdt}" unused \
            >>"${OUTPUT_DIR}/boot-fit-audit.txt"
        bootargs=$(fdtget -t s "${fdt}" /chosen bootargs)
        bash "${REPO_ROOT}/scripts/debian-system/audit-tf-dtb.sh" "${fdt}" \
            >>"${OUTPUT_DIR}/boot-fit-audit.txt"
        grep -qw "root=PARTLABEL=${root_label}" <<<"${bootargs}" \
            || fail "${boot} has the wrong root PARTLABEL"
        grep -qw "aiden.slot_suffix=${suffix}" <<<"${bootargs}" \
            || fail "${boot} has the wrong Aiden slot suffix"
        grep -qw 'rootfstype=ext4' <<<"${bootargs}" \
            || fail "${boot} is missing rootfstype=ext4"
        grep -qw 'net.ifnames=0' <<<"${bootargs}" \
            || fail "${boot} is missing net.ifnames=0"
        grep -qw 'rk_dma_heap_cma=100M' <<<"${bootargs}" \
            || fail "${boot} is missing the production CMA setting"
        printf 'slot=%s bootargs=%s\n' "${slot}" "${bootargs}" \
            >>"${OUTPUT_DIR}/boot-fit-audit.txt"
    done
}

audit_update_image() {
    rm -rf "${UNPACK_DIR}"
    mkdir -p "${UNPACK_DIR}"
    "${SDK_DIR}/tools/linux/Linux_Pack_Firmware/mk-update_unpack.sh" \
        -i "${IMAGE_DIR}/update.img" -o "${UNPACK_DIR}" >>"${REPORT}" 2>&1
    for item in env.img idblock.img uboot.img misc.img boot_a.img boot_b.img \
        rootfs.img userdata.img ota.img; do
        cmp "${IMAGE_DIR}/${item}" "${UNPACK_DIR}/Image/${item}"
    done
    test ! -e "${UNPACK_DIR}/Image/oem.img" || fail "update.img contains a retired OEM image"
    cmp "${IMAGE_DIR}/download.bin" "${UNPACK_DIR}/download.bin"
}

main() {
    rm -rf "${WORK_DIR}"
    mkdir -p "${WORK_DIR}"
    : >"${REPORT}"
    printf 'image\tfeatures\n' >"${OUTPUT_DIR}/ext4-features.tsv"

    declare -A limits=(
        [env.img]=$((32 * 1024))
        [idblock.img]=$((512 * 1024))
        [uboot.img]=$((256 * 1024))
        [misc.img]=$((4 * 1024 * 1024))
        [boot_a.img]=$((32 * 1024 * 1024))
        [boot_b.img]=$((32 * 1024 * 1024))
        [rootfs.img]=$((1792 * 1024 * 1024))
        [userdata.img]=$((3 * 1024 * 1024 * 1024))
        [ota.img]=$((300 * 1024 * 1024))
    )
    printf 'image\tsize_bytes\tlimit_bytes\theadroom_bytes\n' \
        >"${OUTPUT_DIR}/partition-size-audit.tsv"
    local image size limit
    for image in env.img idblock.img uboot.img misc.img boot_a.img boot_b.img \
        rootfs.img userdata.img ota.img; do
        test -s "${IMAGE_DIR}/${image}" || fail "image is missing: ${image}"
        size=$(stat -c %s "${IMAGE_DIR}/${image}")
        limit=${limits[${image}]}
        [ "${size}" -le "${limit}" ] || fail "${image} exceeds its partition"
        printf '%s\t%s\t%s\t%s\n' "${image}" "${size}" "${limit}" \
            "$((limit - size))" >>"${OUTPUT_DIR}/partition-size-audit.tsv"
    done
    grep -qx \
        'blkdevparts=mmcblk0:32K(env),512K@32K(idblock),256K(uboot),4M(misc),32M(boot_a),32M(boot_b),1792M(rootfs_a),1792M(rootfs_b),3G(userdata),300M(ota)' \
        "${OUTPUT_DIR}/bsp-env.txt" || fail "BSP partition layout changed"

    test ! -e "${IMAGE_DIR}/oem.img" || fail "retired OEM image is present"
    audit_ext4 "${IMAGE_DIR}/rootfs.img" rootfs
    audit_ext4 "${IMAGE_DIR}/userdata.img" userdata
    audit_ext4 "${IMAGE_DIR}/ota.img" ota
    audit_packages

    mount_image "${IMAGE_DIR}/rootfs.img" "${ROOTFS_MOUNT}"
    audit_rootfs
    audit_business_package
    audit_rootfs_cli_tools
    audit_platform_files
    audit_elf_closure
    unmount_mounts

    mount_image "${IMAGE_DIR}/userdata.img" "${USERDATA_MOUNT}"
    test -f "${USERDATA_MOUNT}/agent/agent.toml" \
        || fail "Agent configuration is missing from userdata"
    test "$(stat -c '%u:%g:%a' "${USERDATA_MOUNT}/agent/agent.toml")" = 0:0:600 \
        || fail "Agent configuration permissions are invalid"
    test -s "${OUTPUT_DIR}/agent-config.sha256" \
        || fail "Agent configuration hash record is missing"
    expected_agent_config_hash=$(tr -d '\r\n' <"${OUTPUT_DIR}/agent-config.sha256")
    case "${expected_agent_config_hash}" in
        '' | *[!0-9a-f]*) fail "Agent configuration hash record is invalid" ;;
    esac
    test "${#expected_agent_config_hash}" -eq 64 \
        || fail "Agent configuration hash record is invalid"
    test "$(sha256sum "${USERDATA_MOUNT}/agent/agent.toml" | awk '{print $1}')" \
        = "${expected_agent_config_hash}" \
        || fail "Agent configuration does not match the external build input"
    test -d "${USERDATA_MOUNT}/ota" || fail "userdata OTA mount point is missing"
    test -f "${USERDATA_MOUNT}/debian/ota/config.json" \
        || fail "Debian OTA device configuration is missing from userdata"
    test "$(stat -c '%u:%g:%a' "${USERDATA_MOUNT}/debian/ota/config.json")" = 0:0:600 \
        || fail "Debian OTA device configuration permissions are invalid"
    "${REPO_ROOT}/scripts/debian-system/validate-ota-config.py" \
        --config "${USERDATA_MOUNT}/debian/ota/config.json" \
        --boot-a "${IMAGE_DIR}/boot_a.img" \
        --boot-b "${IMAGE_DIR}/boot_b.img" \
        --rootfs "${IMAGE_DIR}/rootfs.img" \
        >"${OUTPUT_DIR}/ota-config-mounted-audit.txt"
    unmount_mounts

    mount_image "${IMAGE_DIR}/ota.img" "${OTA_MOUNT}"
    [ -z "$(find "${OTA_MOUNT}" -mindepth 1 -maxdepth 1 ! -name lost+found -print -quit)" ] \
        || fail "generic OTA image is not empty"
    unmount_mounts

    audit_boot
    audit_update_image
    (
        cd "${OUTPUT_DIR}"
        sha256sum image/*.{img,bin} packages.txt sbom.spdx.json >SHA256SUMS
    )
    echo "Audit passed" >>"${REPORT}"
    chown "${HOST_UID:-0}:${HOST_GID:-0}" \
        "${REPORT}" "${OUTPUT_DIR}/ext4-features.tsv" \
        "${OUTPUT_DIR}/partition-size-audit.tsv" \
        "${OUTPUT_DIR}/systemd-unit-audit.txt" \
        "${OUTPUT_DIR}/elf-runtime-audit.tsv" \
        "${OUTPUT_DIR}/boot-fit-audit.txt" \
        "${OUTPUT_DIR}/ota-config-mounted-audit.txt" \
        "${OUTPUT_DIR}/agent-config.sha256" "${OUTPUT_DIR}/SHA256SUMS"
}

main
