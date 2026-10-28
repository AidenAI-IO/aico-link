#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT=/work
readonly OUTPUT_DIR=/out
# Mounted by run_images() so image assembly honors DEBIAN_SYSTEM_SDK_DIR.
readonly SDK_DIR=/sdk
readonly IMAGE_DIR=${OUTPUT_DIR}/image
readonly WORK_DIR=${OUTPUT_DIR}/image-work
readonly USERDATA_ROOT=${WORK_DIR}/userdata-root
readonly OTA_ROOT=${WORK_DIR}/ota-root
readonly ROOTFS_IMAGE=${OUTPUT_DIR}/rootfs.ext4
readonly OTA_PUBLIC_KEY=/run/secrets/ota_pubkey.pem
readonly AGENT_CONFIG=/run/secrets/agent.toml
readonly USERDATA_UUID=ee2962d6-bd9c-4096-b22b-71934584d36a
readonly OTA_UUID=950e39a6-5445-47df-a542-e80ed45b08ac


mounts=()
cleanup() {
    local index
    for ((index = ${#mounts[@]} - 1; index >= 0; index--)); do
        mountpoint -q "${mounts[index]}" && umount "${mounts[index]}" || true
    done
}
trap cleanup EXIT

prepare_images() {
    "${REPO_ROOT}/scripts/validate_ota_pubkey.sh" "${OTA_PUBLIC_KEY}"
    test "$(sha256sum "${OTA_PUBLIC_KEY}" | awk '{print $1}')" = \
        "$(cat "${OUTPUT_DIR}/ota-public-key.sha256")" || {
        echo "OTA public key changed since rootfs was built; rebuild rootfs" >&2
        exit 1
    }
    (
        cd "${SDK_DIR}/output/image"
        sha256sum -c "${OUTPUT_DIR}/rootfs-bsp-inputs.sha256"
    ) || {
        echo "BSP changed since rootfs was built; rebuild rootfs with the matching modules" >&2
        exit 1
    }
    rm -rf "${WORK_DIR}" "${IMAGE_DIR}"
    install -d -m 0755 "${WORK_DIR}" "${IMAGE_DIR}"
}

make_ext4_image() {
    local source_dir=$1
    local image=$2
    local size=$3
    local label=$4
    local uuid=$5
    local mount_dir=${WORK_DIR}/mnt-${label}
    local feature_opts='^64bit,^huge_file,^metadata_csum,^metadata_csum_seed,^dir_index,^orphan_file,^quota'
    mkdir -p "${mount_dir}"
    rm -f "${image}"
    truncate -s "${size}" "${image}"
    mkfs.ext4 -F -L "${label}" -U "${uuid}" -m 1 \
        -E lazy_itable_init=0,lazy_journal_init=0 \
        -O "${feature_opts}" "${image}"
    mount -o loop "${image}" "${mount_dir}"
    mounts+=("${mount_dir}")
    rsync -aHAX --numeric-ids "${source_dir}/" "${mount_dir}/"
    sync
    umount "${mount_dir}"
    mounts=()
    e2fsck -fy "${image}"
    resize2fs -M "${image}"
    e2fsck -fy "${image}"
}

stage_images() {
    test -s "${AGENT_CONFIG}" || {
        echo "Missing external Agent configuration: ${AGENT_CONFIG}" >&2
        exit 1
    }
    install -d -m 0755 \
        "${USERDATA_ROOT}/agent" "${USERDATA_ROOT}/ota" "${OTA_ROOT}" "${IMAGE_DIR}"
    install -m 0600 "${AGENT_CONFIG}" "${USERDATA_ROOT}/agent/agent.toml"
    sha256sum "${AGENT_CONFIG}" | awk '{print $1}' \
        >"${OUTPUT_DIR}/agent-config.sha256"
    # Finite factory seed only; the final -(userdata) partition fills the card
    # and aico-rootfs-grow expands this filesystem on the device.
    make_ext4_image "${USERDATA_ROOT}" "${IMAGE_DIR}/userdata.img" 3G userdata "${USERDATA_UUID}"
    make_ext4_image "${OTA_ROOT}" "${IMAGE_DIR}/ota.img" 300M ota "${OTA_UUID}"
    cp --reflink=auto "${ROOTFS_IMAGE}" "${IMAGE_DIR}/rootfs.img"
    for item in env.img idblock.img uboot.img misc.img boot_a.img boot_b.img download.bin; do
        cp "${SDK_DIR}/output/image/${item}" "${IMAGE_DIR}/${item}"
    done
    cp "${SDK_DIR}/output/image/.env.txt" "${OUTPUT_DIR}/bsp-env.txt"
    (
        cd "${IMAGE_DIR}"
        sha256sum boot_a.img boot_b.img rootfs.img userdata.img ota.img \
            >prepack-images.sha256
    )
}

finalize() {
    local path
    for path in "${IMAGE_DIR}"/* \
        "${OUTPUT_DIR}/agent-config.sha256"; do
        [ -e "${path}" ] || continue
        chown "${HOST_UID:-0}:${HOST_GID:-0}" "${path}"
    done
    # Keep the output directory ownership consistent for host-side inspection.
    chown "${HOST_UID:-0}:${HOST_GID:-0}" "${IMAGE_DIR}"
    rm -rf "${WORK_DIR}"
}

prepare_images
stage_images
finalize
