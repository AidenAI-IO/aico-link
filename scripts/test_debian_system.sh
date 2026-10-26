#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly SYSTEM_DIR=${REPO_ROOT}/scripts/debian-system
readonly TEST_ROOT=$(mktemp -d)
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Debian system test failure: $*" >&2
    exit 1
}

active_docs=()
while IFS= read -r tracked_doc; do
    active_docs+=("${REPO_ROOT}/${tracked_doc}")
done < <(git -C "${REPO_ROOT}" ls-files -- \
    README.md docs/README.md 'docs/[0-9][0-9]-*')
if rg --no-ignore -n -i \
    '\./build\.sh (binaries|image)|/etc/init\.d/|overlay/(etc|oem|userdata)|pico-sdk/output/image|scripts/build/' \
    "${active_docs[@]}"; then
    fail "active documentation still publishes a legacy userspace workflow"
fi

bash -n \
    "${SYSTEM_DIR}/build.sh" \
    "${SYSTEM_DIR}/audit-bsp.sh" \
    "${SYSTEM_DIR}/audit-tf-dtb.sh" \
    "${SYSTEM_DIR}/container-build-rootfs.sh" \
    "${SYSTEM_DIR}/container-assemble-images.sh" \
    "${SYSTEM_DIR}/container-install-ota-config.sh" \
    "${SYSTEM_DIR}/container-audit-images.sh"

bsp_image_mock_bin=${TEST_ROOT}/bsp-image-mock-bin
bsp_image_mock_log=${TEST_ROOT}/bsp-image-docker.log
bsp_image_state=${TEST_ROOT}/bsp-image-present
mkdir -p "${bsp_image_mock_bin}"
cat >"${bsp_image_mock_bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${MOCK_DOCKER_LOG}"
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
    [ -e "${MOCK_DOCKER_IMAGE_STATE}" ] || exit 1
    printf 'sha256:mock-bsp-builder\n'
elif [ "${1:-}" = pull ]; then
    : >"${MOCK_DOCKER_IMAGE_STATE}"
fi
EOF
chmod +x "${bsp_image_mock_bin}/docker"
MOCK_DOCKER_LOG="${bsp_image_mock_log}" \
MOCK_DOCKER_IMAGE_STATE="${bsp_image_state}" \
PATH="${bsp_image_mock_bin}:${PATH}" \
    bash -c 'set -euo pipefail
source "$1"
ensure_bsp_build_image
ensure_bsp_build_image' _ "${SYSTEM_DIR}/build.sh"
[ "$(grep -Fc 'pull luckfoxtech/luckfox_pico:1.0' \
    "${bsp_image_mock_log}")" -eq 1 ] \
    || fail "a missing BSP builder image is not pulled exactly once"

vendored_fixture=${TEST_ROOT}/vendored-fixture
vendored_sdk=${vendored_fixture}/pico-sdk
mkdir -p "${vendored_sdk}/project/cfg/BoardConfig_IPC" \
    "${vendored_sdk}/sysdrv/source/kernel/arch/arm/configs" \
    "${vendored_sdk}/sysdrv/source/uboot/u-boot/configs"
touch "${vendored_sdk}/project/build.sh" \
    "${vendored_sdk}/sysdrv/source/uboot/u-boot/configs/aico-rv1106-rockusb.config"
git -C "${vendored_fixture}" init -q
git -C "${vendored_fixture}" add pico-sdk
git -C "${vendored_fixture}" -c user.name=Test -c user.email=test@example.invalid \
    commit -qm 'test: vendored SDK fixture'
DEBIAN_SYSTEM_SDK_DIR="${vendored_sdk}" \
DEBIAN_SYSTEM_OUTPUT_DIR="${TEST_ROOT}/vendored-output" \
    bash -c 'source "$1"; prepare_sdk' _ "${SYSTEM_DIR}/build.sh"
[ ! -e "${vendored_sdk}/.git" ] || fail "vendored SDK unexpectedly has separate Git metadata"
[ "$(cat "${TEST_ROOT}/vendored-output/source-sdk-commit.txt")" = \
    "$(git -C "${vendored_fixture}" rev-parse HEAD)" ] \
    || fail "vendored SDK provenance does not identify its owning firmware commit"
[ -L "${vendored_sdk}/.BoardConfig.mk" ] \
    || fail "vendored SDK board configuration was not prepared"

python3 - "${SYSTEM_DIR}" <<'PYTHON'
import ast, pathlib, sys
for path in pathlib.Path(sys.argv[1]).glob('*.py'):
    ast.parse(path.read_text(), filename=str(path))
PYTHON

kernel_config_fixture=${TEST_ROOT}/kernel.config
for disabled_value in '# CONFIG_VIDEO_TC358743_CEC is not set' \
    'CONFIG_VIDEO_TC358743_CEC=n' ''; do
    printf '%s\n' "${disabled_value}" >"${kernel_config_fixture}"
    bash -c 'source "$1"; kernel_setting_is_disabled "$2" CONFIG_VIDEO_TC358743_CEC' \
        _ "${SYSTEM_DIR}/build.sh" "${kernel_config_fixture}" \
        || fail "kernel validation rejects a disabled or dependency-omitted setting"
done
for enabled_value in y m; do
    printf 'CONFIG_VIDEO_TC358743_CEC=%s\n' "${enabled_value}" \
        >"${kernel_config_fixture}"
    if bash -c \
        'source "$1"; kernel_setting_is_disabled "$2" CONFIG_VIDEO_TC358743_CEC' \
        _ "${SYSTEM_DIR}/build.sh" "${kernel_config_fixture}"; then
        fail "kernel validation accepts CONFIG_VIDEO_TC358743_CEC=${enabled_value}"
    fi
done

for script in \
    build.sh audit-bsp.sh canonicalize-bsp.py container-build-rootfs.sh container-assemble-images.sh \
    container-install-ota-config.sh container-audit-images.sh \
    canonicalize-ext4.py generate-spdx.py validate-ota-config.py; do
    [ -x "${SYSTEM_DIR}/${script}" ] || fail "${script} is not executable"
done

# Plain http on purpose: apt verifies InRelease signatures itself, and this
# mirror serves HTTP/1.1 clients such as apt and debootstrap at full speed
# (mirrors.aliyun.com throttles HTTP/1.1 to ~300 kB/s; only HTTP/2 is fast).
grep -qx 'URIs: http://mirrors.ustc.edu.cn/debian' "${SYSTEM_DIR}/debian.sources" \
    || fail "system builder does not fetch the Debian archive from mirrors.ustc.edu.cn"
grep -qx 'URIs: http://mirrors.ustc.edu.cn/debian-security' "${SYSTEM_DIR}/debian.sources" \
    || fail "system builder does not fetch debian-security from mirrors.ustc.edu.cn"
if grep -q '^Check-Valid-Until:' "${SYSTEM_DIR}/debian.sources"; then
    fail "a live mirror must keep the InRelease Valid-Until check"
fi
grep -q '^FROM debian:trixie-slim@sha256:' "${SYSTEM_DIR}/Dockerfile"
# debootstrap takes its mirror from debian.sources rather than a second copy.
grep -Fq 'trixie "${ROOTFS_DIR}" "${DEBIAN_MIRROR}"' \
    "${SYSTEM_DIR}/container-build-rootfs.sh" \
    || fail "debootstrap does not use the mirror declared in debian.sources"
for metadata_key in debian_mirror debian_release_date debian_release_version \
    debian_security_date debian_updates_date; do
    grep -Fq "\"${metadata_key}\":" "${SYSTEM_DIR}/container-build-rootfs.sh" \
        || fail "build metadata does not record ${metadata_key}"
done
if grep -q 'snapshot.debian.org' "${SYSTEM_DIR}/container-build-rootfs.sh" \
    "${SYSTEM_DIR}/debian.sources"; then
    fail "the rootfs build still references snapshot.debian.org"
fi
grep -Fq 'mount -t binfmt_misc binfmt_misc "${BINFMT_DIR}"' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq '/usr/lib/systemd/systemd-binfmt' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq '/usr/lib/binfmt.d/qemu-arm.conf' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'grep -qx enabled "${BINFMT_DIR}/qemu-arm"' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
if grep -Fq 'update-binfmts --enable qemu-arm >/dev/null 2>&1 || true' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"; then
    fail "rootfs builder silently ignores qemu-arm registration failures"
fi
grep -Eq '^[[:space:]]*debootstrap \\' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"

for package in \
    systemd-sysv udev dbus kmod libpam-systemd openssh-server sudo adb iproute2 iputils-arping \
    wpasupplicant bluez systemd-resolved systemd-timesyncd dnsmasq-base \
    e2fsprogs dosfstools exfatprogs util-linux-extra v4l-utils libdrm2 python3 python3-pip; do
    grep -qx "${package}" "${SYSTEM_DIR}/packages.list" \
        || fail "production package list is missing ${package}"
done
grep -Fq 'test -x "${ROOTFS_MOUNT}/usr/sbin/hwclock"' \
    "${SYSTEM_DIR}/container-audit-images.sh" \
    || fail "rootfs image audit does not enforce the hwclock runtime contract"
if grep -Eq '^(net-tools|dhcpcd|dhcpcd-base|isc-dhcp-client|flash-kernel|initramfs-tools)$' \
    "${SYSTEM_DIR}/packages.list"; then
    fail "banned package is present in production package list"
fi
grep -q 'Pin-Priority: -1' "${SYSTEM_DIR}/aico-production.pref"

# Exercise the image audit against an enabled PAM stack and broken variants.
# Merely having libpam-systemd in the package manifest is not sufficient.
source <(sed -n '/^audit_login_sessions() {$/,/^}$/p' \
    "${SYSTEM_DIR}/container-audit-images.sh")
ROOTFS_MOUNT=${TEST_ROOT}/login-rootfs
mkdir -p "${ROOTFS_MOUNT}/etc/ssh/sshd_config.d" \
    "${ROOTFS_MOUNT}/etc/pam.d" \
    "${ROOTFS_MOUNT}/usr/lib/aico" \
    "${ROOTFS_MOUNT}/usr/lib/arm-linux-gnueabihf/security"
cp "${REPO_ROOT}/overlay-debian/etc/ssh/sshd_config.d/20-aico.conf" \
    "${ROOTFS_MOUNT}/etc/ssh/sshd_config.d/20-aico.conf"
cp "${REPO_ROOT}/overlay-debian/etc/ssh/sshrc" "${ROOTFS_MOUNT}/etc/ssh/sshrc"
cp "${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-ssh-session-tty" \
    "${ROOTFS_MOUNT}/usr/lib/aico/aico-ssh-session-tty"
printf '@include common-session\n' >"${ROOTFS_MOUNT}/etc/pam.d/sshd"
printf 'session optional pam_systemd.so\n' >"${ROOTFS_MOUNT}/etc/pam.d/common-session"
printf 'module fixture\n' >"${ROOTFS_MOUNT}/usr/lib/arm-linux-gnueabihf/security/pam_systemd.so"
audit_login_sessions
for login_path in \
    etc/ssh/sshd_config.d/20-aico.conf etc/pam.d/sshd etc/pam.d/common-session \
    usr/lib/arm-linux-gnueabihf/security/pam_systemd.so \
    etc/ssh/sshrc usr/lib/aico/aico-ssh-session-tty; do
    cp -p "${ROOTFS_MOUNT}/${login_path}" "${TEST_ROOT}/login-original"
    sed 's/^/# /' "${TEST_ROOT}/login-original" >"${ROOTFS_MOUNT}/${login_path}"
    if [ "${login_path##*/}" = pam_systemd.so ] || \
        [ "${login_path##*/}" = aico-ssh-session-tty ]; then
        rm "${ROOTFS_MOUNT}/${login_path}"
    fi
    if (audit_login_sessions) >"${TEST_ROOT}/login-audit.log" 2>&1; then
        fail "login session audit accepted disabled/missing ${login_path}"
    fi
    cp -p "${TEST_ROOT}/login-original" "${ROOTFS_MOUNT}/${login_path}"
done
audit_login_sessions

grep -Fq '1792M(rootfs_a),1792M(rootfs_b),3G(userdata),300M(ota)' \
    "${SYSTEM_DIR}/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk"
grep -Fq 'RK_UBOOT_DEFCONFIG_FRAGMENT="rk-emmc.config rv1106-ab.config aico-rv1106-rockusb.config"' \
    "${SYSTEM_DIR}/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk"
grep -Fq 'RK_KERNEL_DEFCONFIG_FRAGMENT="aico-zram.config rv1106-bt.config rv1106-sdiowifi.config aico-rk628.config debian-system.config rv1106-pm.config"' \
    "${SYSTEM_DIR}/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk"
grep -Fq 'RK_ENABLE_WIFI_CHIP=AIC8800D80' \
    "${SYSTEM_DIR}/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk" \
    || fail "production board config does not select the AIC8800D80"
grep -Fq 'RK_CAMERA_SENSOR_IQFILES=""' \
    "${SYSTEM_DIR}/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk" \
    || fail "bridge-only production board unexpectedly packages camera IQ files"
for symbol in CONFIG_MEDIA_CONTROLLER CONFIG_VIDEO_V4L2_SUBDEV_API \
    CONFIG_VIDEO_RK628_CSI CONFIG_VIDEO_TC358743 \
    CONFIG_VIDEO_TC358743_CEC; do
    grep -Fq "${symbol}" "${SYSTEM_DIR}/build.sh" \
        || fail "system BSP verification does not enforce ${symbol}"
done
grep -Fq "RK_KERNEL_CMDLINE_EXTRA=net.ifnames\$'\\x3d'0" \
    "${SYSTEM_DIR}/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk"
if grep -Eq '^[[:space:]]*(restart-poweroff|compatible = "restart-poweroff")' \
    "${REPO_ROOT}/pico-sdk/sysdrv/source/kernel/arch/arm/boot/dts/rv1106g-luckfox-pico-zero.dts"; then
    fail "Pico Zero device tree still converts poweroff into a reboot"
fi

# The vendored SDK must be present: silently skipping the checks below
# would let an incomplete repository checkout pass this suite.
if [ ! -e "${REPO_ROOT}/pico-sdk/project/build.sh" ]; then
    fail "vendored pico-sdk is missing; restore the repository checkout"
fi

# The BSP source changes live in the vendored pico-sdk directory. Verify the
# checked-out sources still carry them so a stale or rewritten SDK
# checkout fails here instead of an hour into the BSP build.
sdk_dir=${REPO_ROOT}/pico-sdk
for sdk_test in \
    test_ab_boot_config.py \
    test_aico_custom_dts.py \
    test_hdmi_bridge_config.py \
    test_mk_ab_misc.py; do
    PYTHONDONTWRITEBYTECODE=1 \
        python3 "${sdk_dir}/project/scripts/${sdk_test}" \
        || fail "pinned SDK contract test failed: ${sdk_test}"
done
grep -Fq 'export RK_JOBS="${RK_JOBS:-$(getconf _NPROCESSORS_ONLN)}"' \
    "${sdk_dir}/project/build.sh" \
    || fail "pico-sdk no longer defaults RK_JOBS to all host CPUs"
grep -Fq 'root=PARTLABEL=$root_label' "${sdk_dir}/project/build.sh" \
    || fail "pico-sdk no longer builds slot-specific root PARTLABELs"
grep -Fq 'slot_cmdline="$slot_cmdline $RK_KERNEL_CMDLINE_EXTRA"' \
    "${sdk_dir}/project/build.sh" \
    || fail "pico-sdk no longer appends RK_KERNEL_CMDLINE_EXTRA to slot bootargs"
grep -Fq 'function build_ab_images()' "${sdk_dir}/project/build.sh" \
    || fail "pico-sdk lacks the abimages build action"
grep -Fq 'abimages) option=build_ab_images' "${sdk_dir}/project/build.sh" \
    || fail "pico-sdk does not dispatch the abimages action"
grep -Fq 'memset(&entry, 0, sizeof(entry));' \
    "${sdk_dir}/sysdrv/source/kernel/scripts/resource_tool.c" \
    || fail "pico-sdk resource_tool is no longer reproducible"
grep -Fq 'phy_update_bits(rphy->phy_base + 0x11c, GENMASK(4, 0), 0x1f);' \
    "${sdk_dir}/sysdrv/source/kernel/drivers/phy/rockchip/phy-rockchip-inno-usb2.c" \
    || fail "pico-sdk lost the RV1106 45ohm HS ODT trim"
grep -Fq 'cancel_work_sync(&gi->work);' \
    "${sdk_dir}/sysdrv/source/kernel/drivers/usb/gadget/configfs.c" \
    || fail "pico-sdk lost the configfs UAF fix"
grep -Fq 'android_device = NULL;' \
    "${sdk_dir}/sysdrv/source/kernel/drivers/usb/gadget/configfs.c" \
    || fail "pico-sdk lost the configfs UAF teardown fix"
grep -Fq 'USB device port, assuming attached' \
    "${sdk_dir}/sysdrv/source/uboot/u-boot/arch/arm/mach-rockchip/boot_rkimg.c" \
    || fail "pico-sdk lost the RockUSB VBUS assumption"
grep -Fq 'usb2phy_update_bits(USB2PHY_PRE_EMPHASIS' \
    "${sdk_dir}/sysdrv/source/uboot/u-boot/board/rockchip/evb_rv1106/evb_rv1106.c" \
    || fail "pico-sdk lost the RockUSB PHY tuning"
grep -Fq 'RV1106 USB2 PHY tuned:' \
    "${sdk_dir}/sysdrv/source/uboot/u-boot/board/rockchip/evb_rv1106/evb_rv1106.c" \
    || fail "pico-sdk lost the RockUSB PHY readback logging"
grep -Fq 'CONFIG_CMD_ROCKUSB=y' \
    "${sdk_dir}/sysdrv/source/uboot/u-boot/configs/aico-rv1106-rockusb.config" \
    || fail "pico-sdk lost the RV1106 RockUSB defconfig fragment"

grep -Fq 'rsync -aHAX --numeric-ids --chown=0:0' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
if grep -Eq 'chown[[:space:]]+(-[^[:space:]]+[[:space:]]+)*(-R|--recursive)' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"; then
    fail "rootfs builder recursively chowns Debian package files"
fi
[ -x "${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-usb-gadget" ] \
    || fail "Debian USB gadget helper is missing"
[ -x "${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-boot-timeline" ] \
    || fail "Debian boot timeline helper is missing"
[ -x "${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-ttyd-start" ] \
    || fail "Debian ttyd helper is missing"
[ -x "${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-ttyd-login" ] \
    || fail "Debian ttyd login helper is missing"
[ -x "${REPO_ROOT}/overlay-debian/usr/bin/aico-power" ] \
    || fail "Debian power-state command is missing or not executable"
grep -Fq 'aico-ttyd.service' \
    "${REPO_ROOT}/overlay-debian/etc/systemd/system/aico.target"
grep -Fq 'overlay-debian/" "${ROOTFS_DIR}/"' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'stage_rootfs_cli_tools.sh' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'rootfs-cli-tools-versions.txt' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'rootfs_cli_tools_manifest_sha256' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
if grep -Fq '${REPO_ROOT}/overlay/' "${SYSTEM_DIR}/container-build-rootfs.sh"; then
    fail "Debian rootfs builder depends on the Buildroot overlay"
fi
grep -Fq 'aico-boot-timeline.service' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'aico-machine-id.service' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'useradd --uid 1000 --gid aico --create-home' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'sudo,audio,video,dialout,plugdev,netdev aico' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
# SHA-512 crypt hash for the factory password luckfox, with fixed salt aico.
grep -Fq '$6$aico$.IiSAzc6XNkYvV3CRDyP031IcVvRKR7F4Y8cItMftzo06ke.hz399b9z/3B442avhFdXyC29l8tdHUHroBOsM.' \
    "${SYSTEM_DIR}/container-build-rootfs.sh" \
    || fail "factory aico account password hash does not match luckfox"
grep -qx 'PasswordAuthentication yes' \
    "${REPO_ROOT}/overlay-debian/etc/ssh/sshd_config.d/20-aico.conf"
grep -qx 'PermitRootLogin prohibit-password' \
    "${REPO_ROOT}/overlay-debian/etc/ssh/sshd_config.d/20-aico.conf"
grep -Fq 'systemctl --root="${ROOTFS_DIR}" preset-all' \
    "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'ssh.socket rsync.service' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'sbom.spdx.json' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'rootfs.tar.zst' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'hash_seed=${ROOTFS_UUID}' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq -- '-d "${ROOTFS_IMPORT_DIR}"' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'canonicalize-ext4.py' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'comparison=rsync-HAXc-numeric-ids' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'var/cache/apt/pkgcache.bin' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'var/cache/apt/srcpkgcache.bin' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'var/cache/ldconfig/aux-cache' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq 'safe.directory="${REPO_ROOT}"' \
    "${SYSTEM_DIR}/container-build-rootfs.sh" \
    || fail "rootfs metadata does not trust the repository bind mount"
grep -Fq 'source_commit=${PICO_SDK_COMMIT:' \
    "${SYSTEM_DIR}/container-build-rootfs.sh" \
    || fail "rootfs metadata does not read the host-resolved SDK commit"
grep -Fq 'readonly SDK_DIR=/sdk' \
    "${SYSTEM_DIR}/container-audit-images.sh" \
    || fail "image audit does not read the SDK from the /sdk mount"
grep -Fq 'PICO_SDK_COMMIT=${sdk_commit}' "${SYSTEM_DIR}/build.sh" \
    || fail "system containers do not pass the selected SDK commit"
if ! sed -n '/^run_rootfs_container()/,/^}/p' "${SYSTEM_DIR}/build.sh" \
    | grep -Fq -- '-v "${SDK_DIR}:/sdk:ro"'; then
    fail "system containers do not mount the selected SDK at /sdk"
fi
[ "$(grep -Fc -- '--path-format=absolute --git-common-dir' \
    "${SYSTEM_DIR}/build.sh")" -eq 1 ] \
    || fail "rootfs container does not mount exactly one Git provenance directory"
if grep -Fq 'luckfox-pico-sdk' "${SYSTEM_DIR}/build.sh"; then
    fail "the system stage still clones pico-sdk into the output directory"
fi
if grep -Fq 'SOURCE_SDK' "${SYSTEM_DIR}/build.sh"; then
    fail "the system stage still copies or validates a separate source pico-sdk"
fi
grep -Fq 'readonly SDK_DIR=${DEBIAN_SYSTEM_SDK_DIR:-${REPO_ROOT}/pico-sdk}' \
    "${SYSTEM_DIR}/build.sh" \
    || fail "the system stage does not build the vendored pico-sdk directory in place"
if grep -Fq 'sdk-patches' "${SYSTEM_DIR}/build.sh" \
    || grep -Fq 'sdk-patches' "${SYSTEM_DIR}/audit-bsp.sh"; then
    fail "the system stage still references the removed SDK patch series"
fi
if grep -Fq 'git -C "${SDK_DIR}" apply' "${SYSTEM_DIR}/build.sh"; then
    fail "the system stage still applies SDK patches at build time"
fi
[ ! -e "${SYSTEM_DIR}/sdk-patches" ] \
    || fail "the system-stage SDK patch directory must not exist"
grep -Fq 'aico-rv1106-rockusb.config' "${SYSTEM_DIR}/build.sh" \
    || fail "the system stage does not verify the vendored SDK carries the RockUSB config"
grep -Fq 'readonly SDK_DIR=/sdk' \
    "${SYSTEM_DIR}/container-assemble-images.sh" \
    || fail "image assembly does not read the SDK from the /sdk mount"
if ! sed -n '/^run_images()/,/^}/p' "${SYSTEM_DIR}/build.sh" \
    | grep -Fq -- '-v "${SDK_DIR}:/sdk:ro"'; then
    fail "image assembly does not mount the selected SDK at /sdk"
fi
if sed -n '/^run_bsp()/,/^}/p' "${SYSTEM_DIR}/build.sh" \
    | grep -Fq 'source_git_common_dir'; then
    fail "BSP container still depends on a host Git object directory"
fi
grep -Fq 'KBUILD_BUILD_USER=aico' "${SYSTEM_DIR}/build.sh"
grep -Fq './build.sh abimages' "${SYSTEM_DIR}/build.sh"
grep -Fq 'canonicalize-bsp.py' "${SYSTEM_DIR}/build.sh"
grep -Fq 'audit-bsp.sh' "${SYSTEM_DIR}/build.sh"
grep -Fq 'factory A/B metadata is invalid' "${SYSTEM_DIR}/audit-bsp.sh"
grep -Fq "readonly EXPECTED_MODEL='Aico Link SCH v1'" \
    "${SYSTEM_DIR}/audit-bsp.sh" \
    || fail "BSP audit does not require the Aico Link SCH v1 production model"
grep -Eq '^[[:space:]]*model = "Aico Link SCH v1";$' \
    "${sdk_dir}/sysdrv/source/kernel/arch/arm/boot/dts/rv1106g-aico-custom.dts" \
    || fail "production DTS does not identify the Aico Link SCH v1 board"
for serial_contract in \
    '0:/serial@ff4a0000:okay' \
    '1:/serial@ff4b0000:disabled' \
    '2:/serial@ff4c0000:disabled' \
    '3:/serial@ff4d0000:disabled' \
    '4:/serial@ff4e0000:okay' \
    '5:/serial@ff4f0000:okay'; do
    grep -Fq "'${serial_contract}'" "${SYSTEM_DIR}/audit-bsp.sh" \
        || fail "BSP audit is missing production UART contract ${serial_contract}"
done
grep -Fq 'fiq_serial_id=$(fdtget -t u "${fdt}" /fiq-debugger rockchip,serial-id)' \
    "${SYSTEM_DIR}/audit-bsp.sh" \
    || fail "BSP audit does not verify the active FIQ recovery console"
if grep -Fq "grep -qw 'rootwait'" "${SYSTEM_DIR}/audit-bsp.sh"; then
    fail "BSP audit requires rootwait even though the slot FIT builder does not emit it"
fi
for firmware in \
    aic_userconfig_8800d80.txt \
    fmacfw_8800d80_u02.bin \
    fw_adid_8800d80_u02.bin \
    fw_patch_8800d80_u02.bin \
    fw_patch_table_8800d80_u02.bin \
    lmacfw_rf_8800d80_u02.bin; do
    grep -Fq "${firmware}" "${SYSTEM_DIR}/audit-bsp.sh" \
        || fail "BSP audit does not require AIC8800D80 firmware ${firmware}"
    grep -Fq "${firmware}" "${SYSTEM_DIR}/container-audit-images.sh" \
        || fail "final image audit does not require AIC8800D80 firmware ${firmware}"
done
grep -Fq 'boot_${slot}.img contains multiple root arguments' \
    "${SYSTEM_DIR}/audit-bsp.sh"
grep -Fq 'bsp-artifacts.sha256' "${SYSTEM_DIR}/audit-bsp.sh"
grep -Fq -- '--check' "${SYSTEM_DIR}/audit-bsp.sh"

grep -Fq 'stage_platform' "${SYSTEM_DIR}/container-build-rootfs.sh"
test -x "${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-dynamic-keyboard" \
    || fail "production platform overlay is missing the dynamic keyboard helper"
for binary in abctl agent aico-environment audio_service ble_service cpu_vad \
    frame_service ota rknn_vad ttyd; do
    if grep -Eq "/apps[^\n]*${binary}|usr/bin[^\n]*${binary}" \
        "${SYSTEM_DIR}/container-assemble-images.sh"; then
        fail "business executable is still staged directly into platform: ${binary}"
    fi
done
if grep -Eq '^    (example_|hello$|trigger$|image_process$|audio_stream$)' \
    "${SYSTEM_DIR}/container-assemble-images.sh"; then
    fail "diagnostic executable leaked into the production platform allowlist"
fi
grep -Fq 'src/agent/config/skills/' "${REPO_ROOT}/scripts/debian-package/container-build.sh"
grep -Fq 'src/config_web/web/' "${REPO_ROOT}/scripts/debian-package/container-build.sh"
grep -Fq 'AGENT_CONFIG_PATH' "${SYSTEM_DIR}/build.sh"
grep -Fq '${AGENT_CONFIG_PATH}:/run/secrets/agent.toml:ro' \
    "${SYSTEM_DIR}/build.sh"
grep -Fq 'apps/bin/agent" config-check --format=json' "${SYSTEM_DIR}/build.sh"
grep -Fq 'install -m 0600 "${AGENT_CONFIG}" "${USERDATA_ROOT}/agent/agent.toml"' \
    "${SYSTEM_DIR}/container-assemble-images.sh"
test ! -e "${REPO_ROOT}/overlay-debian-oem"
[ -x "${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-dynamic-keyboard" ] \
    || fail "Debian platform dynamic keyboard helper is missing"
[ -s "${REPO_ROOT}/assets/business/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn" ] \
    || fail "Debian platform VAD model is missing"
[ -s "${REPO_ROOT}/overlay-debian/usr/share/aico/audio/config_aivqe.json" ] \
    || fail "Debian platform VQE configuration is missing"
for library in libaec_bf_process.so librkaudio_common.so; do
    [ -s "${REPO_ROOT}/overlay-debian/usr/lib/aico/platform/lib/${library}" ] \
        || fail "Debian platform VQE runtime library is missing: ${library}"
done
[ "$(sha256sum "${REPO_ROOT}/overlay-debian/usr/lib/aico/platform/lib/libaec_bf_process.so" | awk '{print $1}')" = \
    3427abaa4b2ab7917d079e6cba46a68a836069bcc7f6b9e94630353fcd8c1a9a ] \
    || fail "Debian platform VQE AEC runtime checksum changed"
[ "$(sha256sum "${REPO_ROOT}/overlay-debian/usr/lib/aico/platform/lib/librkaudio_common.so" | awk '{print $1}')" = \
    de8ff824dd1f2e5ec1074b84490d2836ed9dc61d59d6a90d9cdf19386097263c ] \
    || fail "Debian platform RKAUDIO common runtime checksum changed"
grep -Fq 'VQE runtime library checksum mismatch' \
    "${SYSTEM_DIR}/container-audit-images.sh"
[ -s "${REPO_ROOT}/overlay-debian/usr/share/aico/edid/hdmi_1080p30_cta.hex" ] \
    || fail "Debian platform EDID is missing"
if grep -Fq '${REPO_ROOT}/overlay/' "${SYSTEM_DIR}/container-assemble-images.sh"; then
    fail "Debian platform assembler depends on the Buildroot overlay"
fi
grep -Fq 'kernel_drv_ko/' "${SYSTEM_DIR}/container-build-rootfs.sh"
grep -Fq '/userdata/debian/ota/config.json' "${SYSTEM_DIR}/build.sh"
grep -Fq 'factory_partition_hashes' "${SYSTEM_DIR}/validate-ota-config.py"
grep -Fq 'debian/ota/config.json' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'Agent configuration does not match the external build input' \
    "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'rootfs CLI tool checksum mismatch' \
    "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'rootfs CLI checksum manifest does not match the catalog' \
    "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'rootfs CLI version metadata does not match the catalog' \
    "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq '${APPS_OUTPUT}/rootfs-cli-tools:/rootfs-cli-tools:ro' \
    "${SYSTEM_DIR}/build.sh"

grep -Fq 'root=PARTLABEL=${root_label}' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq "net.ifnames=0" "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'Buildroot SysV startup file leaked' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'boot timeline helper was not installed' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'aico-boot-timeline.service is not enabled' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'rootfs import attribute audit did not pass' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'rootfs ownership or mode is invalid' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'sudo executable ownership or mode is invalid' \
    "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'sudo group does not require password-authenticated administrator access' \
    "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'passwordless sudo policy is present' \
    "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'nondeterministic APT package cache leaked' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'nondeterministic APT source cache leaked' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'nondeterministic ldconfig cache leaked' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'unresolved runtime DT_NEEDED' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'generic OTA image is not empty' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'audit_platform_files()' "${SYSTEM_DIR}/container-audit-images.sh"
grep -Fq 'unmount_mounts' "${SYSTEM_DIR}/container-audit-images.sh"
if grep -Fq 'mount_image "${IMAGE_DIR}/userdata.img" "${USERDATA_MOUNT}"' \
    "${SYSTEM_DIR}/container-audit-images.sh" \
    && grep -Fq 'mount_image "${IMAGE_DIR}/ota.img" "${OTA_MOUNT}"' \
    "${SYSTEM_DIR}/container-audit-images.sh"; then
    test "$(grep -n 'mount_image "\${IMAGE_DIR}/userdata.img"' \
        "${SYSTEM_DIR}/container-audit-images.sh" | cut -d: -f1)" -lt \
        "$(grep -n 'mount_image "\${IMAGE_DIR}/ota.img"' \
            "${SYSTEM_DIR}/container-audit-images.sh" | cut -d: -f1)"
fi

cat >"${TEST_ROOT}/packages.tsv" <<'EOF'
package	version	architecture	source	maintainer
systemd:armhf	257.7-1	armhf	systemd	Debian systemd Maintainers <pkg-systemd-maintainers@lists.alioth.debian.org>
EOF
PYTHONDONTWRITEBYTECODE=1 "${SYSTEM_DIR}/generate-spdx.py" \
    "${TEST_ROOT}/packages.tsv" "${TEST_ROOT}/sbom.json"
python3 - "${TEST_ROOT}/sbom.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as stream:
    document = json.load(stream)
assert document["spdxVersion"] == "SPDX-2.3"
assert document["packages"][0]["name"] == "systemd:armhf"
assert document["packages"][0]["externalRefs"][0]["referenceType"] == "purl"
PY

help_output=${TEST_ROOT}/help-output
DEBIAN_SYSTEM_OUTPUT_DIR="${help_output}" \
    "${SYSTEM_DIR}/build.sh" --help >/dev/null
[ ! -e "${help_output}" ] || fail "--help created the output directory"
if "${SYSTEM_DIR}/build.sh" invalid-action >/dev/null 2>&1; then
    fail "invalid build action succeeded"
fi

mkdir -p "${TEST_ROOT}/ota-config-images"
for image in boot_a.img boot_b.img rootfs.img; do
    printf '%s\n' "${image}" >"${TEST_ROOT}/ota-config-images/${image}"
done
boot_a_hash=$(sha256sum "${TEST_ROOT}/ota-config-images/boot_a.img" | awk '{print $1}')
boot_b_hash=$(sha256sum "${TEST_ROOT}/ota-config-images/boot_b.img" | awk '{print $1}')
rootfs_hash=$(sha256sum "${TEST_ROOT}/ota-config-images/rootfs.img" | awk '{print $1}')
cat >"${TEST_ROOT}/ota-config.json" <<EOF
{
  "repo": "AidenAI-IO/aiden-firmware",
  "channel": "stable",
  "download_safety_margin_bytes": 16777216,
  "factory_version": "20260817-120000-abcdef0",
  "factory_build_time": "2026-08-17T12:00:00Z",
  "factory_partition_hashes": {
    "a": {"boot": "${boot_a_hash}", "rootfs": "${rootfs_hash}"},
    "b": {"boot": "${boot_b_hash}", "rootfs": "${rootfs_hash}"}
  }
}
EOF
"${SYSTEM_DIR}/validate-ota-config.py" \
    --config "${TEST_ROOT}/ota-config.json" \
    --boot-a "${TEST_ROOT}/ota-config-images/boot_a.img" \
    --boot-b "${TEST_ROOT}/ota-config-images/boot_b.img" \
    --rootfs "${TEST_ROOT}/ota-config-images/rootfs.img" \
    >"${TEST_ROOT}/ota-config-audit.txt"
grep -qx "factory_partition_hashes.b.rootfs=${rootfs_hash}" \
    "${TEST_ROOT}/ota-config-audit.txt"
sed 's/"rootfs": "[0-9a-f]*"/"rootfs": "bad"/' \
    "${TEST_ROOT}/ota-config.json" >"${TEST_ROOT}/bad-ota-config.json"
if "${SYSTEM_DIR}/validate-ota-config.py" \
    --config "${TEST_ROOT}/bad-ota-config.json" \
    --boot-a "${TEST_ROOT}/ota-config-images/boot_a.img" \
    --boot-b "${TEST_ROOT}/ota-config-images/boot_b.img" \
    --rootfs "${TEST_ROOT}/ota-config-images/rootfs.img" >/dev/null 2>&1; then
    fail "OTA config validator accepted a mismatched rootfs hash"
fi

mkdir -p "${TEST_ROOT}/mock-bin"
cat >"${TEST_ROOT}/mock-bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\0' "$@" >>"${MOCK_DOCKER_LOG}"
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
    printf 'sha256:mock-system-builder\n'
fi
EOF
chmod +x "${TEST_ROOT}/mock-bin/docker"
mock_output=${TEST_ROOT}/mock-output
mock_log=${TEST_ROOT}/docker-args
mock_apps=${TEST_ROOT}/mock-apps
mkdir -p "${mock_output}" "${mock_apps}/rootfs-cli-tools" "${mock_apps}/apps" "${mock_apps}/apps-audit"
printf '%s' mock-deb >"${mock_output}/aico-business_0.0.1-2_armhf.deb"
printf 'status=pass\n' >"${mock_apps}/apps-audit/summary.txt"
printf '%064d  fq\n' 0 >"${mock_apps}/rootfs-cli-tools/manifest.sha256"
printf 'fq v0.17.0 linux/arm/v7 preserve\n' \
    >"${mock_apps}/rootfs-cli-tools/versions.txt"
mkdir -p "${TEST_ROOT}/mock-sdk/output/out/sysdrv_out/kernel_drv_ko"
git -C "${TEST_ROOT}" init -q mock-sdk
git -C "${TEST_ROOT}/mock-sdk" -c user.name=Test -c user.email=test@example.invalid commit -q --allow-empty -m 'test: fixture'
OTA_PUBLIC_KEY_PATH="${REPO_ROOT}/keys/ota_pubkey.pem" \
DEBIAN_SYSTEM_SDK_DIR="${TEST_ROOT}/mock-sdk" \
MOCK_DOCKER_LOG="${mock_log}" \
PATH="${TEST_ROOT}/mock-bin:${PATH}" \
DEBIAN_SYSTEM_OUTPUT_DIR="${mock_output}" \
DEBIAN_APPS_OUTPUT_DIR="${mock_apps}" \
DEBIAN_SYSTEM_APT_CACHE_PROXY=http://cache.example:3128 \
    "${SYSTEM_DIR}/build.sh" rootfs
tr '\0' '\n' <"${mock_log}" >"${TEST_ROOT}/docker-args.txt"
grep -qx -- '--privileged' "${TEST_ROOT}/docker-args.txt"
grep -qx "${mock_output}:/out" "${TEST_ROOT}/docker-args.txt"
grep -qx "${REPO_ROOT}:/work:ro" "${TEST_ROOT}/docker-args.txt"
grep -qx "${mock_apps}/rootfs-cli-tools:/rootfs-cli-tools:ro" \
    "${TEST_ROOT}/docker-args.txt"
grep -qx 'scripts/debian-system/container-build-rootfs.sh' \
    "${TEST_ROOT}/docker-args.txt"
grep -qx 'DEBIAN_SYSTEM_APT_CACHE_PROXY=http://cache.example:3128' \
    "${TEST_ROOT}/docker-args.txt" \
    || fail "the optional APT cache candidate was not passed to the rootfs container"

echo "Debian system static checks passed"

"${REPO_ROOT}/scripts/test_debian_package.sh"

"${REPO_ROOT}/scripts/test_debian_no_oem.sh"
