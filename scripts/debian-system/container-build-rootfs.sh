#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT=/work
readonly SCRIPT_DIR=${REPO_ROOT}/scripts/debian-system
readonly OUTPUT_DIR=/out
readonly WORK_DIR=${OUTPUT_DIR}/rootfs-work
readonly ROOTFS_DIR=${WORK_DIR}/rootfs
readonly ROOTFS_IMPORT_DIR=${WORK_DIR}/rootfs-import
readonly MOUNT_DIR=${WORK_DIR}/mnt
readonly ROOTFS_IMAGE=${OUTPUT_DIR}/rootfs.ext4
readonly ROOTFS_ARCHIVE=${OUTPUT_DIR}/rootfs.tar.zst
readonly ROOTFS_CLI_TOOLS_DIR=/rootfs-cli-tools
# debian.sources is the single definition of the archive mirrors; debootstrap
# and the metadata below take their URIs from it. It is a live mirror, not a
# pinned archive snapshot, so the archive state each build consumed is
# recorded in build-metadata.json instead.
readonly DEBIAN_MIRROR=$(sed -n 's/^URIs: //p' "${SCRIPT_DIR}/debian.sources" | sed -n 1p)
readonly DEBIAN_SECURITY_MIRROR=$(sed -n 's/^URIs: //p' "${SCRIPT_DIR}/debian.sources" | sed -n 2p)
readonly BUILD_EPOCH=${SOURCE_DATE_EPOCH:-1767360516}
readonly SDK_DIR=/sdk
readonly OTA_PUBLIC_KEY=/run/secrets/ota_pubkey.pem
readonly ROOTFS_UUID=1d29a2d4-5488-4bea-a648-bf133c4b53d3
readonly BINFMT_DIR=/proc/sys/fs/binfmt_misc

mounts=()

cleanup() {
    local index
    for ((index = ${#mounts[@]} - 1; index >= 0; index--)); do
        mountpoint -q "${mounts[index]}" && umount "${mounts[index]}" || true
    done
}
trap cleanup EXIT

mount_chroot_filesystems() {
    mkdir -p "${ROOTFS_DIR}/proc" "${ROOTFS_DIR}/sys" \
        "${ROOTFS_DIR}/dev/pts" "${ROOTFS_DIR}/run"
    mount -t proc proc "${ROOTFS_DIR}/proc"
    mounts+=("${ROOTFS_DIR}/proc")
    mount -t sysfs sysfs "${ROOTFS_DIR}/sys"
    mounts+=("${ROOTFS_DIR}/sys")
    mount --bind /dev "${ROOTFS_DIR}/dev"
    mounts+=("${ROOTFS_DIR}/dev")
    mount --bind /dev/pts "${ROOTFS_DIR}/dev/pts"
    mounts+=("${ROOTFS_DIR}/dev/pts")
    mount -t tmpfs tmpfs "${ROOTFS_DIR}/run"
    mounts+=("${ROOTFS_DIR}/run")
}

unmount_all() {
    cleanup
    mounts=()
}

arm_binfmt_is_enabled() {
    [ -r "${BINFMT_DIR}/qemu-arm" ] \
        && grep -qx enabled "${BINFMT_DIR}/qemu-arm"
}

ensure_arm_binfmt() {
    if ! mountpoint -q "${BINFMT_DIR}"; then
        if ! mount -t binfmt_misc binfmt_misc "${BINFMT_DIR}"; then
            echo "Unable to mount binfmt_misc; the rootfs builder must run in a privileged container" >&2
            exit 1
        fi
    fi

    if ! arm_binfmt_is_enabled; then
        if [ -x /usr/lib/systemd/systemd-binfmt ] \
            && [ -f /usr/lib/binfmt.d/qemu-arm.conf ]; then
            if ! /usr/lib/systemd/systemd-binfmt \
                /usr/lib/binfmt.d/qemu-arm.conf; then
                echo "Failed to register qemu-arm with systemd-binfmt" >&2
                exit 1
            fi
        elif command -v update-binfmts >/dev/null 2>&1; then
            if ! update-binfmts --enable qemu-arm; then
                echo "Failed to register qemu-arm with update-binfmts" >&2
                exit 1
            fi
        else
            echo "No supported qemu-arm binfmt registration tool is installed" >&2
            exit 1
        fi
    fi

    if ! arm_binfmt_is_enabled; then
        echo "qemu-arm binfmt registration is not enabled" >&2
        exit 1
    fi
}

check_mirror_uris() {
    local uri
    for uri in "${DEBIAN_MIRROR}" "${DEBIAN_SECURITY_MIRROR}"; do
        case "${uri}" in
        http://?*/?* | https://?*/?*) ;;
        *)
            echo "debian.sources must list an archive URI and a security URI: '${uri}'" >&2
            exit 1
            ;;
        esac
    done
}

# apt names a list file after its URI with the scheme dropped and '/' as '_'.
apt_list_file() {
    printf '%s\n' "$1" | sed -e 's#^[a-z]*://##' -e 's#/#_#g'
}

# A live mirror moves under the build, so keep the Version and Date of every
# InRelease this build actually consumed. trixie-updates and trixie-security
# carry only constant Version fields (13-updates, 13), so their Date is what
# identifies their state. They are read right after the chroot apt-get update
# because configure_rootfs() removes the apt lists.
inrelease_field() {
    local release_uri=$1 field=$2 inrelease value
    inrelease=${ROOTFS_DIR}/var/lib/apt/lists/$(apt_list_file "${release_uri}/InRelease")
    value=$(grep -m1 "^${field}: " "${inrelease}" 2>/dev/null | cut -d' ' -f2-)
    if [ -z "${value}" ]; then
        echo "InRelease has no ${field} field: ${inrelease}" >&2
        exit 1
    fi
    printf '%s\n' "${value}"
}

record_archive_state() {
    DEBIAN_RELEASE_VERSION=$(inrelease_field "${DEBIAN_MIRROR}/dists/trixie" Version)
    DEBIAN_RELEASE_DATE=$(inrelease_field "${DEBIAN_MIRROR}/dists/trixie" Date)
    DEBIAN_UPDATES_DATE=$(inrelease_field "${DEBIAN_MIRROR}/dists/trixie-updates" Date)
    DEBIAN_SECURITY_DATE=$(inrelease_field \
        "${DEBIAN_SECURITY_MIRROR}/dists/trixie-security" Date)
}

bootstrap_rootfs() {
    rm -rf "${WORK_DIR}"
    mkdir -p "${ROOTFS_DIR}" "${MOUNT_DIR}"
    ensure_arm_binfmt
    debootstrap \
        --arch=armhf \
        --variant=minbase \
        --keyring=/usr/share/keyrings/debian-archive-keyring.gpg \
        trixie "${ROOTFS_DIR}" "${DEBIAN_MIRROR}"

    rm -f "${ROOTFS_DIR}/etc/apt/sources.list"
    install -d -m 0755 "${ROOTFS_DIR}/etc/apt/sources.list.d" \
        "${ROOTFS_DIR}/etc/apt/preferences.d"
    install -m 0644 "${SCRIPT_DIR}/debian.sources" \
        "${ROOTFS_DIR}/etc/apt/sources.list.d/debian.sources"
    install -m 0644 "${SCRIPT_DIR}/aico-production.pref" \
        "${ROOTFS_DIR}/etc/apt/preferences.d/aico-production"
    install -m 0644 "${SCRIPT_DIR}/packages.list" \
        "${ROOTFS_DIR}/tmp/debian-system-packages.list"
    cat >"${ROOTFS_DIR}/usr/sbin/policy-rc.d" <<'EOF'
#!/bin/sh
exit 101
EOF
    chmod 0755 "${ROOTFS_DIR}/usr/sbin/policy-rc.d"

    mount_chroot_filesystems
    chroot "${ROOTFS_DIR}" /usr/bin/env \
        DEBIAN_FRONTEND=noninteractive SYSTEMD_OFFLINE=1 \
        apt-get -o Acquire::http::Timeout=30 -o Acquire::Retries=3 update
    record_archive_state
    chroot "${ROOTFS_DIR}" /bin/sh -ec \
        'DEBIAN_FRONTEND=noninteractive SYSTEMD_OFFLINE=1 apt-get -o Acquire::http::Timeout=30 -o Acquire::Retries=3 install -y --no-install-recommends $(grep -v "^[[:space:]]*#" /tmp/debian-system-packages.list | xargs)'
    if [ -s /aico-business.deb ]; then
        python3 "${REPO_ROOT}/scripts/release/contract.py" platform \
            "${ROOTFS_DIR}/usr/lib/aico/platform/contract.json"
        install -m 0644 /aico-business.deb "${ROOTFS_DIR}/tmp/aico-business.deb"
        chroot "${ROOTFS_DIR}" dpkg -i /tmp/aico-business.deb
        rm -f "${ROOTFS_DIR}/tmp/aico-business.deb"
    else
        echo 'Missing /aico-business.deb; business package is required' >&2
        exit 1
    fi
    chroot "${ROOTFS_DIR}" /usr/bin/env \
        DEBIAN_FRONTEND=noninteractive SYSTEMD_OFFLINE=1 \
        dpkg --configure -a
    for group in sudo audio video dialout plugdev netdev; do
        chroot "${ROOTFS_DIR}" getent group "${group}" >/dev/null \
            || chroot "${ROOTFS_DIR}" groupadd --system "${group}"
    done
    chroot "${ROOTFS_DIR}" getent group aico >/dev/null \
        || chroot "${ROOTFS_DIR}" groupadd --gid 1000 aico
    chroot "${ROOTFS_DIR}" getent passwd aico >/dev/null \
        || chroot "${ROOTFS_DIR}" useradd --uid 1000 --gid aico --create-home \
            --shell /bin/bash --groups sudo,audio,video,dialout,plugdev,netdev aico
    # A fixed SHA-512 crypt hash keeps the factory rootfs reproducible. Root
    # remains key-only even though ordinary-user password SSH is enabled.
    chroot "${ROOTFS_DIR}" usermod -p \
        '$6$aico$.IiSAzc6XNkYvV3CRDyP031IcVvRKR7F4Y8cItMftzo06ke.hz399b9z/3B442avhFdXyC29l8tdHUHroBOsM.' \
        aico

    # Keep the root account enabled for public-key SSH while making password
    # verification impossible. sshd separately prohibits root passwords.
    chroot "${ROOTFS_DIR}" usermod -p x root
    rm -f "${ROOTFS_DIR}/usr/sbin/policy-rc.d" \
        "${ROOTFS_DIR}/tmp/debian-system-packages.list"
    unmount_all
}

validate_platform_inputs() {
    grep -qx 'status=pass' /apps-audit/summary.txt || {
        echo "application audit has not passed" >&2
        exit 1
    }
    "${REPO_ROOT}/scripts/validate_ota_pubkey.sh" "${OTA_PUBLIC_KEY}"
    test -d "${SDK_DIR}/output/out/sysdrv_out/kernel_drv_ko" || {
        echo "Missing SDK kernel modules; run the bsp action before rootfs" >&2
        exit 1
    }
    test -s /apps/lib/librga.so.2.1.0
    for image in boot_a.img boot_b.img env.img; do
        test -s "${SDK_DIR}/output/image/${image}"
    done
}

stage_platform() {
    local platform=${ROOTFS_DIR}/usr/lib/aico/platform
    python3 "${REPO_ROOT}/scripts/release/contract.py" platform "${platform}/contract.json"
    install -d -m 0755 "${platform}/lib" "${platform}/modules" \
        "${ROOTFS_DIR}/usr/share/keyrings"
    rsync -aH --chown=0:0 /apps/lib/ "${platform}/lib/"
    rsync -aH --chown=0:0 \
        "${SDK_DIR}/output/out/sysdrv_out/kernel_drv_ko/" "${platform}/modules/"
    install -m 0644 "${OTA_PUBLIC_KEY}" "${ROOTFS_DIR}/usr/share/keyrings/aico-ota.pem"
    # Local/unmanaged images have no release channel and cannot select an APT suite.
    if [ -n "${AICO_RELEASE_CHANNEL:-}" ]; then
        python3 "${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-apt-source" \
            --root "${ROOTFS_DIR}" \
            --public-key "${REPO_ROOT}/overlay-debian/usr/share/keyrings/aico-archive-keyring.asc"
    fi
    find "${platform}" -type d -exec chmod 0755 {} +
    find "${platform}" -type f -exec chmod 0644 {} +
    # Runtime loaders use explicit insmod paths; vendor loader scripts are not
    # installed because they contain paths for the vendor filesystem layout.
    find "${platform}/modules" -type f -name '*.sh' -delete
    if find "${platform}" \( -name '*.a' -o -name '*.la' -o -name '*.o' \
        -o -name '*.map' -o -name '*.pc' -o -name CMakeFiles \
        -o -name pkgconfig -o -name include \) -print -quit | grep -q .; then
        echo "Development artifact leaked into production platform staging" >&2
        exit 1
    fi
    (
        cd "${ROOTFS_DIR}"
        find usr/lib/aico/platform usr/share/aico/edid usr/share/aico/audio/config_aivqe.json \
            usr/share/keyrings/aico-ota.pem -type f -print0 \
            | LC_ALL=C sort -z | xargs -0 sha256sum
    ) >"${OUTPUT_DIR}/platform-files.sha256"
    (
        cd "${SDK_DIR}/output/image"
        sha256sum boot_a.img boot_b.img env.img
    ) >"${OUTPUT_DIR}/rootfs-bsp-inputs.sha256"
    sha256sum "${OTA_PUBLIC_KEY}" | awk '{print $1}' >"${OUTPUT_DIR}/ota-public-key.sha256"
}

configure_rootfs() {
    # Apply only the Debian-native overlay with root ownership. This scoped
    # chown does not touch Debian package files or their numeric UID/GID data.
    python3 "${REPO_ROOT}/scripts/debian-package/system_config.py" exclude "${OUTPUT_DIR}/config-package.exclude"
    rsync -aHAX --numeric-ids --chown=0:0 --exclude-from="${OUTPUT_DIR}/config-package.exclude" \
        "${REPO_ROOT}/overlay-debian/" "${ROOTFS_DIR}/"
    chmod 0755 "${ROOTFS_DIR}"
    while IFS= read -r -d '' path; do
        chmod 0755 "${ROOTFS_DIR}/${path}"
    done < <(find "${REPO_ROOT}/overlay-debian" -mindepth 1 -type d -printf '%P\0')
    while IFS= read -r -d '' path; do
        if grep -Fxq "/${path}" "${OUTPUT_DIR}/config-package.exclude"; then continue; fi
        if [ -x "${REPO_ROOT}/overlay-debian/${path}" ]; then
            chmod 0755 "${ROOTFS_DIR}/${path}"
        else
            chmod 0644 "${ROOTFS_DIR}/${path}"
        fi
    done < <(find "${REPO_ROOT}/overlay-debian" -type f -printf '%P\0')
    "${REPO_ROOT}/scripts/stage_rootfs_cli_tools.sh" \
        --catalog "${REPO_ROOT}/scripts/rootfs_cli_tools.catalog" \
        --source-dir "${ROOTFS_CLI_TOOLS_DIR}" \
        --dest-overlay "${ROOTFS_DIR}"
    install -m 0644 "${ROOTFS_CLI_TOOLS_DIR}/manifest.sha256" \
        "${OUTPUT_DIR}/rootfs-cli-tools.sha256"
    install -m 0644 "${ROOTFS_CLI_TOOLS_DIR}/versions.txt" \
        "${OUTPUT_DIR}/rootfs-cli-tools-versions.txt"
    install -d -m 0755 \
        "${ROOTFS_DIR}/userdata" \
        "${ROOTFS_DIR}/userdata/ota" \
        "${ROOTFS_DIR}/var/lib/aico"
    printf 'aico\n' >"${ROOTFS_DIR}/etc/hostname"
    cat >"${ROOTFS_DIR}/etc/hosts" <<'EOF'
127.0.0.1 localhost
127.0.1.1 aico
::1 localhost ip6-localhost ip6-loopback
EOF
    ln -sfn /run/systemd/resolve/stub-resolv.conf "${ROOTFS_DIR}/etc/resolv.conf"
    : >"${ROOTFS_DIR}/etc/machine-id"
    install -d -m 0755 "${ROOTFS_DIR}/var/lib/dbus"
    ln -sfn /etc/machine-id "${ROOTFS_DIR}/var/lib/dbus/machine-id"
    rm -f "${ROOTFS_DIR}"/etc/ssh/ssh_host_*
    rm -f "${ROOTFS_DIR}/var/lib/systemd/random-seed"

    systemctl --root="${ROOTFS_DIR}" preset-all
    systemctl --root="${ROOTFS_DIR}" enable \
        systemd-networkd.service systemd-resolved.service systemd-timesyncd.service \
        wpa_supplicant@wlan0.service ssh.service serial-getty@ttyFIQ0.service \
        aico-slot-resolve.service aico-rootfs-grow.service \
        userdata.mount userdata-ota.mount \
        aico-userdata-migrate.service aico-machine-id.service \
        aico-root-home.service aico-user-home.service \
        aico-ssh-identity.service aico-platform-ldconfig.service \
        aico-environment.service aico-zram.service aico-swap.service \
        aico-media-modules.service aico-wifi-driver.service \
        aico-bluetooth-state.service aico-bluetooth-attach.service \
        bluetooth.service aico-rtc.service aico-boot-timeline.service aico.target
    local unit
    for unit in \
        dnsmasq.service wpa_supplicant.service \
        ssh.socket rsync.service systemd-networkd-wait-online.service \
        e2scrub_all.timer e2scrub_reap.service \
        apt-daily.service apt-daily.timer \
        apt-daily-upgrade.service apt-daily-upgrade.timer; do
        systemctl --root="${ROOTFS_DIR}" disable "${unit}" || true
    done
    systemctl --root="${ROOTFS_DIR}" mask \
        dnsmasq.service wpa_supplicant.service \
        ssh.socket rsync.service \
        systemd-networkd-wait-online.service \
        apt-daily.service apt-daily.timer \
        apt-daily-upgrade.service apt-daily-upgrade.timer \
        e2scrub_all.timer e2scrub_reap.service || true

    stage_platform
    mount_chroot_filesystems
    chroot "${ROOTFS_DIR}" /sbin/ldconfig
    chroot "${ROOTFS_DIR}" dpkg-query -W \
        -f='${binary:Package}\t${Version}\t${Architecture}\t${source:Package}\t${Maintainer}\n' \
        | LC_ALL=C sort >"${OUTPUT_DIR}/packages.txt"
    chroot "${ROOTFS_DIR}" dpkg --audit >"${OUTPUT_DIR}/dpkg-audit.txt"
    unmount_all
    if [ -s "${OUTPUT_DIR}/dpkg-audit.txt" ]; then
        echo "dpkg reports an incomplete or inconsistent production rootfs" >&2
        exit 1
    fi
    # Check package-owned configs before spending time creating and packing images.
    python3 "${REPO_ROOT}/scripts/debian-package/system_config.py" audit "${ROOTFS_DIR}"
    sed -i '1i package\tversion\tarchitecture\tsource\tmaintainer' \
        "${OUTPUT_DIR}/packages.txt"
    (
        cd "${OUTPUT_DIR}"
        sha256sum packages.txt >packages.sha256
    )
    python3 "${SCRIPT_DIR}/generate-spdx.py" \
        "${OUTPUT_DIR}/packages.txt" "${OUTPUT_DIR}/sbom.spdx.json"
    cp "${SCRIPT_DIR}/debian.sources" "${OUTPUT_DIR}/debian.sources"

    rm -rf "${ROOTFS_DIR}/var/lib/apt/lists/"* \
        "${ROOTFS_DIR}/var/cache/apt/archives/"*.deb \
        "${ROOTFS_DIR}/var/log/"* \
        "${ROOTFS_DIR}/tmp/"* \
        "${ROOTFS_DIR}/var/tmp/"*
    rm -f "${ROOTFS_DIR}/var/cache/apt/pkgcache.bin" \
        "${ROOTFS_DIR}/var/cache/apt/srcpkgcache.bin" \
        "${ROOTFS_DIR}/var/cache/ldconfig/aux-cache"
    rm -f "${ROOTFS_DIR}/usr/bin/qemu-arm-static"
    find "${ROOTFS_DIR}/dev" -xdev \( -type b -o -type c \) -delete

    # Normalize mutable build timestamps after package installation and before
    # producing manifests or archives. Symlink targets are not followed.
    find "${ROOTFS_DIR}" -xdev -exec touch -h -d "@${BUILD_EPOCH}" {} +
    (
        cd "${ROOTFS_DIR}"
        find . -xdev -printf '%U\t%G\t%m\t%T@\t%p\t%l\n' | LC_ALL=C sort
    ) >"${OUTPUT_DIR}/filesystem-manifest.txt"
    getcap -r "${ROOTFS_DIR}" 2>/dev/null \
        | sed "s#^${ROOTFS_DIR}##" >"${OUTPUT_DIR}/capabilities.txt" || true
    getfattr -R -d -m - "${ROOTFS_DIR}" 2>/dev/null \
        | sed "s#${ROOTFS_DIR}##g" >"${OUTPUT_DIR}/xattrs.txt" || true
    find "${ROOTFS_DIR}" -xdev -type f \( -perm -4000 -o -perm -2000 \) \
        -printf '%m\t%U\t%G\t%p\n' \
        | sed "s#${ROOTFS_DIR}##" | LC_ALL=C sort \
        >"${OUTPUT_DIR}/setid-files.txt"

    rm -f "${ROOTFS_ARCHIVE}"
    tar --sort=name --format=posix --numeric-owner --acls --xattrs \
        --xattrs-include='*' --mtime="@${BUILD_EPOCH}" --clamp-mtime \
        --pax-option=delete=atime,delete=ctime \
        -I 'zstd -19 -T0' -cf "${ROOTFS_ARCHIVE}" -C "${ROOTFS_DIR}" .
}

create_ext4_image() {
    local feature_opts
    feature_opts='^64bit,^huge_file,^metadata_csum,^metadata_csum_seed,^dir_index,^quota'
    if mkfs.ext4 -q -n -O '^orphan_file' /dev/null >/dev/null 2>&1; then
        feature_opts="${feature_opts},^orphan_file"
    else
        # Debian 13 supports orphan_file; retain the explicit compatibility
        # constraint even if a future host tool changes its probe behavior.
        feature_opts="${feature_opts},^orphan_file"
    fi

    rm -f "${ROOTFS_IMAGE}"
    rm -rf "${ROOTFS_IMPORT_DIR}"
    mkdir -p "${ROOTFS_IMPORT_DIR}"
    tar --zstd --acls --xattrs --xattrs-include='*' --numeric-owner \
        -xf "${ROOTFS_ARCHIVE}" -C "${ROOTFS_IMPORT_DIR}"
    truncate -s 1792M "${ROOTFS_IMAGE}"
    mkfs.ext4 -F -L rootfs -U "${ROOTFS_UUID}" -m 1 \
        -E "lazy_itable_init=0,lazy_journal_init=0,hash_seed=${ROOTFS_UUID},root_owner=0:0" \
        -O "${feature_opts}" -d "${ROOTFS_IMPORT_DIR}" "${ROOTFS_IMAGE}"
    e2fsck -fy "${ROOTFS_IMAGE}"
    resize2fs -M "${ROOTFS_IMAGE}"
    e2fsck -fy "${ROOTFS_IMAGE}"
    python3 "${SCRIPT_DIR}/canonicalize-ext4.py" \
        "${ROOTFS_IMAGE}" "${BUILD_EPOCH}"
    e2fsck -fn "${ROOTFS_IMAGE}"

    mount -o loop,ro,noload "${ROOTFS_IMAGE}" "${MOUNT_DIR}"
    mounts+=("${MOUNT_DIR}")
    rsync -naiHAXc --numeric-ids --delete --exclude=/lost+found \
        "${ROOTFS_DIR}/" "${MOUNT_DIR}/" >"${OUTPUT_DIR}/rootfs-import-diff.txt"
    if [ -s "${OUTPUT_DIR}/rootfs-import-diff.txt" ]; then
        cat "${OUTPUT_DIR}/rootfs-import-diff.txt" >&2
        echo "mke2fs rootfs import did not preserve the source tree" >&2
        exit 1
    fi
    umount "${MOUNT_DIR}"
    mounts=()
    {
        echo 'status=pass'
        echo 'importer=mke2fs-d'
        # sed rather than `head -n 1`: head closes the pipe after the first
        # line, and mkfs.ext4 -V writes a second one (the EXT2FS library
        # version). Whether it is still writing when the reader goes away is a
        # scheduling race, and losing it under pipefail aborts the whole build
        # with SIGPIPE. sed reads to EOF, so the writer never sees EPIPE.
        mkfs.ext4 -V 2>&1 | sed -n '1p'
        echo 'comparison=rsync-HAXc-numeric-ids'
        echo 'excluded=/lost+found'
    } >"${OUTPUT_DIR}/rootfs-import-audit.txt"
    rm -f "${OUTPUT_DIR}/rootfs-import-diff.txt"
    dumpe2fs -h "${ROOTFS_IMAGE}" >"${OUTPUT_DIR}/rootfs-dumpe2fs.txt" 2>&1
    (
        cd "${OUTPUT_DIR}"
        sha256sum rootfs.ext4 rootfs.tar.zst >rootfs-artifacts.sha256
    )
}

write_metadata() {
    local rootfs_sha packages_sha cli_manifest_sha cli_versions_sha
    local source_commit app_commit
    rootfs_sha=$(sha256sum "${ROOTFS_IMAGE}" | awk '{print $1}')
    packages_sha=$(sha256sum "${OUTPUT_DIR}/packages.txt" | awk '{print $1}')
    cli_manifest_sha=$(sha256sum "${OUTPUT_DIR}/rootfs-cli-tools.sha256" | awk '{print $1}')
    cli_versions_sha=$(sha256sum "${OUTPUT_DIR}/rootfs-cli-tools-versions.txt" | awk '{print $1}')
    # The selected SDK can live outside the repository, so its commit is
    # resolved on the host and passed in. The repository itself is mounted
    # read-only from a non-root host user while this privileged builder runs
    # as root, so trust only the exact bind mount path needed for provenance.
    source_commit=${PICO_SDK_COMMIT:?build.sh must pass PICO_SDK_COMMIT}
    app_commit=$(git -c safe.directory="${REPO_ROOT}" \
        -C "${REPO_ROOT}" rev-parse HEAD)
    cat >"${OUTPUT_DIR}/build-metadata.json" <<EOF
{
  "architecture": "armhf",
  "build_image_id": "${DEBIAN_SYSTEM_BUILD_IMAGE_ID:-unknown}",
  "debian_mirror": "${DEBIAN_MIRROR}",
  "debian_release_date": "${DEBIAN_RELEASE_DATE:?record_archive_state must run first}",
  "debian_release_version": "${DEBIAN_RELEASE_VERSION}",
  "debian_security_date": "${DEBIAN_SECURITY_DATE}",
  "debian_updates_date": "${DEBIAN_UPDATES_DATE}",
  "hardware_demo_commit": "${app_commit}",
  "packages_sha256": "${packages_sha}",
  "pico_sdk_commit": "${source_commit}",
  "rootfs_cli_tools_manifest_sha256": "${cli_manifest_sha}",
  "rootfs_cli_tools_versions_sha256": "${cli_versions_sha}",
  "rootfs_sha256": "${rootfs_sha}",
  "source_date_epoch": ${BUILD_EPOCH},
  "suite": "trixie"
}
EOF
}

finalize() {
    local path
    for path in \
        rootfs.ext4 rootfs.tar.zst packages.txt packages.sha256 \
        debian.sources build-metadata.json filesystem-manifest.txt \
        capabilities.txt xattrs.txt setid-files.txt sbom.spdx.json \
        dpkg-audit.txt rootfs-dumpe2fs.txt rootfs-import-audit.txt \
        rootfs-artifacts.sha256 rootfs-cli-tools.sha256 \
        rootfs-cli-tools-versions.txt platform-files.sha256 ota-public-key.sha256 rootfs-bsp-inputs.sha256; do
        chown "${HOST_UID:-0}:${HOST_GID:-0}" "${OUTPUT_DIR}/${path}"
    done
    rm -rf "${WORK_DIR}"
}

check_mirror_uris
source "${SCRIPT_DIR}/configure-apt-cache.sh"
configure_apt_cache_proxy "${DEBIAN_MIRROR}"
validate_platform_inputs
bootstrap_rootfs
configure_rootfs
create_ext4_image
write_metadata
finalize
