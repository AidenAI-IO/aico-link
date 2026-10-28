#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
readonly DEFAULT_OUTPUT_DIR=${REPO_ROOT}/output/debian-system
if [ -n "${DEBIAN_SYSTEM_OUTPUT_DIR:-}" ]; then
    if [[ "${DEBIAN_SYSTEM_OUTPUT_DIR}" = /* ]]; then
        OUTPUT_DIR=${DEBIAN_SYSTEM_OUTPUT_DIR}
    else
        OUTPUT_DIR=${REPO_ROOT}/${DEBIAN_SYSTEM_OUTPUT_DIR}
    fi
else
    OUTPUT_DIR=${DEFAULT_OUTPUT_DIR}
fi
readonly OUTPUT_DIR
# The BSP is built in place from the vendored pico-sdk directory. The A/B,
# RockUSB and reproducibility changes are tracked in this repository, so the
# system stage stages compatible media modules without editing BSP sources.
readonly SDK_DIR=${DEBIAN_SYSTEM_SDK_DIR:-${REPO_ROOT}/pico-sdk}
readonly IMAGE_DIR=${OUTPUT_DIR}/image
readonly APPS_OUTPUT=${DEBIAN_APPS_OUTPUT_DIR:-${REPO_ROOT}/output/debian-apps}
readonly ROOTFS_BUILD_IMAGE=${DEBIAN_SYSTEM_BUILD_IMAGE:-aico-debian13-armhf-builder:system}
readonly BSP_BUILD_IMAGE=${DEBIAN_SYSTEM_BSP_BUILD_IMAGE:-luckfoxtech/luckfox_pico:1.0}
readonly JOBS=${RK_JOBS:-$(getconf _NPROCESSORS_ONLN)}
readonly BUILD_EPOCH=${SOURCE_DATE_EPOCH:-1767360516}

usage() {
    cat <<'EOF'
Usage: scripts/debian-system/build.sh [all|builder|rootfs|bsp|images|config|audit]

Environment:
  DEBIAN_SYSTEM_OUTPUT_DIR       Output directory (default: output/debian-system).
  DEBIAN_SYSTEM_SDK_DIR          Luckfox BSP SDK build tree (default: repository
                                 pico-sdk directory; built in place).
  DEBIAN_APPS_OUTPUT_DIR         Audited application output.
  DEBIAN_SYSTEM_BUILD_IMAGE      Rootfs/image builder image name.
  DEBIAN_SYSTEM_APT_CACHE_PROXY  Optional HTTP cache URL reachable from the rootfs
                                 container; used only after signed metadata validation.
                                 Existing HTTP/all_proxy settings take precedence.
  DEBIAN_SYSTEM_BSP_BUILD_IMAGE  Luckfox BSP builder image name.
  OTA_PUBLIC_KEY_PATH            Production Ed25519 public key (required by rootfs/images).
  AGENT_CONFIG_PATH              External agent.toml installed into userdata.img
                                 (required by images; never copied into the repository).
  OTA_DEVICE_CONFIG_PATH         Config generated from the signed release manifest
                                 (required by config and all).
  SOURCE_DATE_EPOCH              Rootfs archive/build metadata timestamp.
  RK_JOBS                        BSP build parallelism.

The rootfs action requires BSP modules and audited apps. The images action creates
a generic rootfs.img containing the platform and business package. The SDK
packer maps each generic image to both A/B partitions, so the two slots start
with identical bytes.

The config action validates factory hashes against those images, installs the
Debian-only config at /userdata/debian/ota/config.json inside userdata.img,
and repacks update.img. Run it after generating the signed release manifest.
EOF
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "Required command not found: $1" >&2
        exit 1
    }
}

kernel_setting_is_disabled() {
    local kernel_config=$1
    local symbol=$2

    ! grep -Eq "^${symbol}=(y|m)$" "${kernel_config}"
}

validate_epoch() {
    case "${BUILD_EPOCH}" in
        '' | *[!0-9]*)
            echo "SOURCE_DATE_EPOCH must be an unsigned Unix timestamp: ${BUILD_EPOCH}" >&2
            exit 1
            ;;
    esac
}

prepare_sdk() {
    local source_commit

    if [ ! -e "${SDK_DIR}/project/build.sh" ]; then
        echo "Luckfox SDK has no project/build.sh: ${SDK_DIR}" >&2
        exit 1
    fi
    # The vendored SDK carries the Aico Link A/B and RockUSB changes.
    # Check a file it adds so a stale checkout fails here instead of an hour
    # into the BSP build.
    if [ ! -e "${SDK_DIR}/sysdrv/source/uboot/u-boot/configs/aico-rv1106-rockusb.config" ]; then
        echo "Luckfox SDK lacks the Aico Link RV1106 RockUSB config; restore the SDK sources" >&2
        exit 1
    fi

    mkdir -p "${OUTPUT_DIR}"

    # The vendored SDK carries the BSP source changes. The system stage only overlays
    # the production board configuration and kernel config that it owns.
    install -m 0755 \
        "${SCRIPT_DIR}/BoardConfig-TF-Debian13-RV1106_Aico_Link-IPC.mk" \
        "${SDK_DIR}/project/cfg/BoardConfig_IPC/BoardConfig-TF-Debian13-RV1106_Aico_Link-IPC.mk"
    install -m 0644 "${SCRIPT_DIR}/debian-system.config" \
        "${SDK_DIR}/sysdrv/source/kernel/arch/arm/configs/debian-system.config"
    ln -sfn \
        project/cfg/BoardConfig_IPC/BoardConfig-TF-Debian13-RV1106_Aico_Link-IPC.mk \
        "${SDK_DIR}/.BoardConfig.mk"

    # Keep the metadata field stable: the vendored SDK resolves to its owning
    # firmware commit; an external SDK checkout resolves to its own commit.
    source_commit=$(git -C "${SDK_DIR}" rev-parse HEAD)
    printf '%s\n' "${source_commit}" >"${OUTPUT_DIR}/source-sdk-commit.txt"
}

docker_proxy_args() {
    local name
    for name in http_proxy https_proxy all_proxy no_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY; do
        if [ -n "${!name:-}" ]; then
            printf '%s\0%s\0' -e "${name}=${!name}"
        fi
    done
}

run_builder() {
    mkdir -p "${OUTPUT_DIR}"
    docker build -t "${ROOTFS_BUILD_IMAGE}" -f "${SCRIPT_DIR}/Dockerfile" "${REPO_ROOT}"
    docker image inspect "${ROOTFS_BUILD_IMAGE}" --format '{{.Id}}' \
        >"${OUTPUT_DIR}/rootfs-builder-image-id.txt"
}

run_rootfs_container() {
    local script=$1
    shift
    local image_id source_git_common_dir sdk_commit
    local -a proxy_args=() platform_args=()
    test -s "${APPS_OUTPUT}/rootfs-cli-tools/manifest.sha256" || {
        echo "Missing application rootfs CLI tools: ${APPS_OUTPUT}/rootfs-cli-tools" >&2
        exit 1
    }
    test -s "${APPS_OUTPUT}/rootfs-cli-tools/versions.txt" || {
        echo "Missing application rootfs CLI version metadata" >&2
        exit 1
    }
    while IFS= read -r -d '' item; do proxy_args+=("${item}"); done < <(docker_proxy_args)
    image_id=$(docker image inspect "${ROOTFS_BUILD_IMAGE}" --format '{{.Id}}')
    source_git_common_dir=$(git -C "${REPO_ROOT}" rev-parse \
        --path-format=absolute --git-common-dir)
    # The selected SDK may live outside the repository, so resolve its commit
    # on the host and mount it at a fixed path for the privileged container.
    sdk_commit=$(git -C "${SDK_DIR}" rev-parse HEAD)
    if [ "${script}" = scripts/debian-system/container-build-rootfs.sh ]; then
        DEBIAN_APPS_OUTPUT_DIR="${APPS_OUTPUT}" \
            DEBIAN_PACKAGE_OUTPUT_DIR="${OUTPUT_DIR}" \
            DEBIAN_PACKAGE_BUILD_IMAGE="${ROOTFS_BUILD_IMAGE}" \
            "${REPO_ROOT}/scripts/debian-package/build.sh"
    fi

    if [ "${script}" = scripts/debian-system/container-build-rootfs.sh ]; then
        platform_args=(-v "${OTA_PUBLIC_KEY_PATH}:/run/secrets/ota_pubkey.pem:ro"
            -v "${OUTPUT_DIR}/aico-business.deb:/aico-business.deb:ro")
    fi
    docker run --rm --privileged \
        ${platform_args[@]+"${platform_args[@]}"} \
        -v "${APPS_OUTPUT}/apps:/apps:ro" \
        -v "${APPS_OUTPUT}/apps-audit:/apps-audit:ro" \
        ${proxy_args[@]+"${proxy_args[@]}"} \
        -e "HOST_UID=$(id -u)" \
        -e "HOST_GID=$(id -g)" \
        -e "SOURCE_DATE_EPOCH=${BUILD_EPOCH}" \
        -e "DEBIAN_SYSTEM_BUILD_IMAGE_ID=${image_id}" \
        -e "PICO_SDK_COMMIT=${sdk_commit}" \
        -e "AICO_PLATFORM_CONTRACT=${AICO_PLATFORM_CONTRACT:-}" \
        -e "AICO_RELEASE_CHANNEL=${AICO_RELEASE_CHANNEL:-}" \
        -e "AICO_PLATFORM_BASE=${AICO_PLATFORM_BASE:-}" \
        -e "AICO_SYSTEM_FINGERPRINT=${AICO_SYSTEM_FINGERPRINT:-}" \
        -e "DEBIAN_SYSTEM_APT_CACHE_PROXY=${DEBIAN_SYSTEM_APT_CACHE_PROXY:-}" \
        -v "${REPO_ROOT}:/work:ro" \
        -v "${REPO_ROOT}:${REPO_ROOT}:ro" \
        -v "${source_git_common_dir}:${source_git_common_dir}:ro" \
        -v "${SDK_DIR}:/sdk:ro" \
        -v "${OUTPUT_DIR}:/out" \
        -v "${APPS_OUTPUT}/rootfs-cli-tools:/rootfs-cli-tools:ro" \
        -w /work \
        "${ROOTFS_BUILD_IMAGE}" \
        bash "${script}" "$@"
}

run_rootfs() {
    docker image inspect "${ROOTFS_BUILD_IMAGE}" >/dev/null
    if [ -z "${OTA_PUBLIC_KEY_PATH:-}" ] || [ ! -f "${OTA_PUBLIC_KEY_PATH}" ]; then
        echo "OTA_PUBLIC_KEY_PATH must name a production Ed25519 public key" >&2
        exit 1
    fi
    test -d "${SDK_DIR}/output/out/sysdrv_out/kernel_drv_ko" || {
        echo "Missing BSP modules; run the bsp action before rootfs" >&2
        exit 1
    }
    grep -qx 'status=pass' "${APPS_OUTPUT}/apps-audit/summary.txt"
    run_rootfs_container scripts/debian-system/container-build-rootfs.sh
}

ensure_bsp_build_image() {
    if docker image inspect "${BSP_BUILD_IMAGE}" >/dev/null 2>&1; then
        return
    fi

    echo "BSP builder image is not available locally; pulling ${BSP_BUILD_IMAGE}"
    docker pull "${BSP_BUILD_IMAGE}"
}

run_bsp() {
    local build_timestamp
    ensure_bsp_build_image
    prepare_sdk
    build_timestamp=$(date -u -d "@${BUILD_EPOCH}" '+%Y-%m-%d %H:%M:%S UTC')
    docker image inspect "${BSP_BUILD_IMAGE}" --format '{{.Id}}' \
        >"${OUTPUT_DIR}/bsp-builder-image-id.txt"
    docker run --rm \
        -u "$(id -u):$(id -g)" \
        -e "RK_JOBS=${JOBS}" \
        -e "SOURCE_DATE_EPOCH=${BUILD_EPOCH}" \
        -e "KBUILD_BUILD_TIMESTAMP=${build_timestamp}" \
        -e KBUILD_BUILD_USER=aico \
        -e KBUILD_BUILD_HOST=system \
        -v "${SDK_DIR}:/sdk" \
        -v "${SCRIPT_DIR}:/aico-bsp-scripts:ro" \
        -v "${OUTPUT_DIR}:/aico-system-output" \
        -w /sdk \
        "${BSP_BUILD_IMAGE}" \
        bash -lc './build.sh uboot && ./build.sh driver &&
            python3 /aico-bsp-scripts/prepare-media-compat.py \
                --sdk /sdk --output /aico-system-output/media-compat \
                --kernel-build /sdk/sysdrv/source/objs_kernel \
                --cross-compile /sdk/tools/linux/toolchain/arm-rockchip830-linux-uclibcgnueabihf/bin/arm-rockchip830-linux-uclibcgnueabihf- \
                --jobs "${RK_JOBS}" &&
            install -m 0644 /aico-system-output/media-compat/mpp_vcodec.ko \
                /aico-system-output/media-compat/rockit.ko \
                /sdk/output/out/sysdrv_out/kernel_drv_ko/ &&
            ./build.sh env && ./build.sh abimages'

    "${SCRIPT_DIR}/canonicalize-bsp.py" \
        --source-date-epoch "${BUILD_EPOCH}" \
        --fit "${SDK_DIR}/output/image/uboot.img" \
        --fit "${SDK_DIR}/output/image/boot_a.img" \
        --fit "${SDK_DIR}/output/image/boot_b.img" \
        --crc-table-source "${SDK_DIR}/sysdrv/source/uboot/u-boot/tools/rockchip/boot_merger.c" \
        --loader "${SDK_DIR}/output/image/download.bin"

    local kernel_config=${SDK_DIR}/sysdrv/source/objs_kernel/.config
    local env_text=${SDK_DIR}/output/image/.env.txt
    local symbol
    for symbol in \
        CONFIG_DEVTMPFS CONFIG_DEVTMPFS_MOUNT CONFIG_TMPFS \
        CONFIG_TMPFS_XATTR CONFIG_TMPFS_POSIX_ACL CONFIG_EXT4_FS \
        CONFIG_CGROUPS CONFIG_MEMCG CONFIG_BLK_CGROUP CONFIG_CGROUP_SCHED \
        CONFIG_CGROUP_PIDS CONFIG_NAMESPACES CONFIG_UTS_NS CONFIG_IPC_NS \
        CONFIG_PID_NS CONFIG_NET_NS CONFIG_SECCOMP CONFIG_SECCOMP_FILTER \
        CONFIG_AUTOFS_FS CONFIG_INOTIFY_USER CONFIG_EPOLL CONFIG_SIGNALFD \
        CONFIG_TIMERFD CONFIG_FHANDLE CONFIG_ZSMALLOC CONFIG_ZRAM \
        CONFIG_RFKILL CONFIG_BT CONFIG_BT_BREDR CONFIG_BT_RFCOMM \
        CONFIG_BT_RFCOMM_TTY CONFIG_BT_LE CONFIG_BT_HCIUART \
        CONFIG_BT_HCIUART_H4 CONFIG_CRYPTO_ECDH CONFIG_CRYPTO_CMAC \
        CONFIG_MEDIA_CONTROLLER CONFIG_VIDEO_V4L2_SUBDEV_API \
        CONFIG_VIDEO_RK628_CSI; do
        grep -qx "${symbol}=y" "${kernel_config}" || {
            echo "Required production kernel setting is not enabled: ${symbol}" >&2
            exit 1
        }
    done
    for symbol in CONFIG_VIDEO_TC358743 CONFIG_VIDEO_TC358743_CEC; do
        kernel_setting_is_disabled "${kernel_config}" "${symbol}" || {
            echo "Production kernel setting must remain disabled: ${symbol}" >&2
            exit 1
        }
    done
    grep -qx \
        'blkdevparts=mmcblk1:32K(env),512K@32K(idblock),256K(uboot),4M(misc),32M(boot_a),32M(boot_b),1792M(rootfs_a),1792M(rootfs_b),300M(ota),-(userdata)' \
        "${env_text}"
    tr '\000' '\n' <"${SDK_DIR}/output/image/env.img" \
        | grep -x \
            'sd_parts=mmcblk1:32K(env),512K@32K(idblock),256K(uboot),4M(misc),32M(boot_a),32M(boot_b),1792M(rootfs_a),1792M(rootfs_b),300M(ota),-(userdata)' \
            >/dev/null
    for item in env.img idblock.img uboot.img misc.img boot_a.img boot_b.img download.bin; do
        test -s "${SDK_DIR}/output/image/${item}"
    done
    test -s "${SDK_DIR}/output/out/sysdrv_out/kernel_drv_ko/aic8800_bsp.ko"
    test -s "${SDK_DIR}/output/out/sysdrv_out/kernel_drv_ko/aic8800_fdrv.ko"
    cp "${kernel_config}" "${OUTPUT_DIR}/kernel.config"
    cp "${env_text}" "${OUTPUT_DIR}/bsp-env.txt"
    DEBIAN_SYSTEM_OUTPUT_DIR="${OUTPUT_DIR}" \
        DEBIAN_SYSTEM_SDK_DIR="${SDK_DIR}" \
        "${SCRIPT_DIR}/audit-bsp.sh"
}

run_images() {
    prepare_sdk
    docker image inspect "${ROOTFS_BUILD_IMAGE}" >/dev/null
    test -s "${OUTPUT_DIR}/rootfs.ext4" || {
        echo "Missing system rootfs; run the rootfs action first" >&2
        exit 1
    }
    test -d "${APPS_OUTPUT}/apps" || {
        echo "Missing applications: ${APPS_OUTPUT}/apps" >&2
        exit 1
    }
    grep -qx 'status=pass' "${APPS_OUTPUT}/apps-audit/summary.txt" || {
        echo "application audit has not passed" >&2
        exit 1
    }
    if [ -z "${OTA_PUBLIC_KEY_PATH:-}" ] || [ ! -f "${OTA_PUBLIC_KEY_PATH}" ]; then
        echo "OTA_PUBLIC_KEY_PATH must name a production Ed25519 public key" >&2
        exit 1
    fi
    if [ -z "${AGENT_CONFIG_PATH:-}" ] || [ ! -f "${AGENT_CONFIG_PATH}" ]; then
        echo "AGENT_CONFIG_PATH must name an external agent.toml" >&2
        exit 1
    fi
    "${APPS_OUTPUT}/apps/bin/agent" config-check --format=json \
        --config="${AGENT_CONFIG_PATH}" >"${OUTPUT_DIR}/agent-config-validation.json"

    local image_id
    image_id=$(docker image inspect "${ROOTFS_BUILD_IMAGE}" --format '{{.Id}}')
    docker run --rm --privileged \
        -e "HOST_UID=$(id -u)" \
        -e "HOST_GID=$(id -g)" \
        -e "SOURCE_DATE_EPOCH=${BUILD_EPOCH}" \
        -e "DEBIAN_SYSTEM_BUILD_IMAGE_ID=${image_id}" \
        -v "${REPO_ROOT}:/work:ro" \
        -v "${SDK_DIR}:/sdk:ro" \
        -v "${OUTPUT_DIR}:/out" \
        -v "${OTA_PUBLIC_KEY_PATH}:/run/secrets/ota_pubkey.pem:ro" \
        -v "${AGENT_CONFIG_PATH}:/run/secrets/agent.toml:ro" \
        -w /work \
        "${ROOTFS_BUILD_IMAGE}" \
        bash scripts/debian-system/container-assemble-images.sh

    run_sdk_packer
    test -s "${IMAGE_DIR}/update.img"
    # The full-card image must contain the personalized userdata image.
    local layout
    layout=$(source "${SCRIPT_DIR}/BoardConfig-TF-Debian13-RV1106_Aico_Link-IPC.mk"; printf '%s' "${RK_PARTITION_CMD_IN_ENV}")
    python3 "${SCRIPT_DIR}/assemble-tf-card.py" \
        --layout "${layout}" --env "${OUTPUT_DIR}/bsp-env.txt" \
        --image-dir "${IMAGE_DIR}" --output "${IMAGE_DIR}/tf-card.img"
}

run_sdk_packer() {
    # The image assembly container may use a rootless UID mapping for image/.
    # Run the SDK packer in that same container so it can create package-file
    # beside the inputs without relying on host ownership of the mount. The
    # packer only reads its tool directory, so the SDK tree is mounted read-only.
    docker run --rm \
        -v "${SDK_DIR}:/sdk:ro" \
        -v "${OUTPUT_DIR}:/out" \
        "${ROOTFS_BUILD_IMAGE}" \
        bash -lc '/sdk/tools/linux/Linux_Pack_Firmware/mk-update_pack.sh -id rv1106 -i /out/image'
}

run_config() {
    prepare_sdk
    docker image inspect "${ROOTFS_BUILD_IMAGE}" >/dev/null
    if [ -z "${OTA_DEVICE_CONFIG_PATH:-}" ] || [ ! -f "${OTA_DEVICE_CONFIG_PATH}" ]; then
        echo "OTA_DEVICE_CONFIG_PATH must name a config generated from the signed release manifest" >&2
        exit 1
    fi
    for image in boot_a.img boot_b.img rootfs.img userdata.img ota.img; do
        test -s "${IMAGE_DIR}/${image}" || {
            echo "Missing system image ${IMAGE_DIR}/${image}; run the images action first" >&2
            exit 1
        }
    done

    local image_id
    image_id=$(docker image inspect "${ROOTFS_BUILD_IMAGE}" --format '{{.Id}}')
    docker run --rm --privileged \
        -e "HOST_UID=$(id -u)" \
        -e "HOST_GID=$(id -g)" \
        -e "DEBIAN_SYSTEM_BUILD_IMAGE_ID=${image_id}" \
        -v "${REPO_ROOT}:/work:ro" \
        -v "${OUTPUT_DIR}:/out" \
        -v "${OTA_DEVICE_CONFIG_PATH}:/run/secrets/debian-ota-config.json:ro" \
        -w /work \
        "${ROOTFS_BUILD_IMAGE}" \
        bash scripts/debian-system/container-install-ota-config.sh

    run_sdk_packer
    test -s "${IMAGE_DIR}/update.img"
    local layout
    layout=$(source "${SCRIPT_DIR}/BoardConfig-TF-Debian13-RV1106_Aico_Link-IPC.mk"; printf '%s' "${RK_PARTITION_CMD_IN_ENV}")
    python3 "${SCRIPT_DIR}/assemble-tf-card.py" \
        --layout "${layout}" --env "${OUTPUT_DIR}/bsp-env.txt" \
        --image-dir "${IMAGE_DIR}" --output "${IMAGE_DIR}/tf-card.img"
}

run_audit() {
    prepare_sdk
    docker image inspect "${ROOTFS_BUILD_IMAGE}" >/dev/null
    run_rootfs_container scripts/debian-system/container-audit-images.sh
}

main() {
    local action=${1:-all}
    case "${action}" in
        -h | --help | help)
            usage
            return
            ;;
        all | builder | rootfs | bsp | images | config | audit) ;;
        *)
            usage >&2
            exit 2
            ;;
    esac

    require_command docker
    require_command git
    require_command python3
    require_command sha256sum
    validate_epoch
    if [ "${action}" != help ]; then mkdir -p "${OUTPUT_DIR}"; fi

    case "${action}" in
        all)
            run_builder
            run_bsp
            run_rootfs
            run_images
            run_config
            run_audit
            ;;
        builder) run_builder ;;
        rootfs) run_rootfs ;;
        bsp) run_bsp ;;
        images) run_images ;;
        config) run_config ;;
        audit) run_audit ;;
    esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
