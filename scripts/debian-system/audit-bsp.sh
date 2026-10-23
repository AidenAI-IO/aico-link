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
readonly SDK_DIR=${DEBIAN_SYSTEM_SDK_DIR:-${REPO_ROOT}/pico-sdk}
readonly IMAGE_DIR=${SDK_DIR}/output/image
readonly MODULE_DIR=${SDK_DIR}/output/out/sysdrv_out/kernel_drv_ko
readonly DUMPIMAGE=${SDK_DIR}/sysdrv/source/uboot/u-boot/tools/dumpimage
readonly KERNEL_IMAGE=${SDK_DIR}/sysdrv/source/objs_kernel/arch/arm/boot/zImage
readonly KERNEL_CONFIG=${SDK_DIR}/sysdrv/source/objs_kernel/.config
readonly BSP_DTB=${SDK_DIR}/output/out/sysdrv_out/board_uclibc_rv1106/rv1106g-aico-custom.dtb
readonly EXPECTED_MODEL='Aico Link SCH v1'
readonly -a EXPECTED_SERIAL_CONTRACTS=(
    '0:/serial@ff4a0000:okay'
    '1:/serial@ff4b0000:disabled'
    '2:/serial@ff4c0000:disabled'
    '3:/serial@ff4d0000:disabled'
    '4:/serial@ff4e0000:okay'
    '5:/serial@ff4f0000:okay'
)
readonly PARTITION_LAYOUT='32K(env),512K@32K(idblock),256K(uboot),4M(misc),32M(boot_a),32M(boot_b),1792M(rootfs_a),1792M(rootfs_b),3G(userdata),300M(ota)'
readonly MISC_METADATA_HEX=00414230010000000f00010000000000000000000000000000000000671e21a4
readonly BUILD_EPOCH=${SOURCE_DATE_EPOCH:-1767360516}

WORK_DIR=
cleanup() {
    [ -z "${WORK_DIR}" ] || rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

fail() {
    echo "Debian system BSP audit failure: $*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command is missing: $1"
}

require_file() {
    test -s "$1" || fail "required BSP artifact is missing: $1"
}

audit_partition_sizes() {
    declare -A limits=(
        [env.img]=$((32 * 1024))
        [idblock.img]=$((512 * 1024))
        [uboot.img]=$((256 * 1024))
        [misc.img]=$((4 * 1024 * 1024))
        [boot_a.img]=$((32 * 1024 * 1024))
        [boot_b.img]=$((32 * 1024 * 1024))
    )
    local image size limit
    printf 'image\tsize_bytes\tlimit_bytes\theadroom_bytes\n' \
        >"${OUTPUT_DIR}/bsp-partition-size-audit.tsv"
    for image in env.img idblock.img uboot.img misc.img boot_a.img boot_b.img; do
        require_file "${IMAGE_DIR}/${image}"
        size=$(stat -c %s "${IMAGE_DIR}/${image}")
        limit=${limits[${image}]}
        [ "${size}" -le "${limit}" ] \
            || fail "${image} exceeds its partition size"
        printf '%s\t%s\t%s\t%s\n' "${image}" "${size}" "${limit}" \
            "$((limit - size))" >>"${OUTPUT_DIR}/bsp-partition-size-audit.tsv"
    done
    require_file "${IMAGE_DIR}/download.bin"
}

audit_misc() {
    local metadata_hex nonzero_count
    metadata_hex=$(od -An -v -tx1 -j 2048 -N 32 "${IMAGE_DIR}/misc.img" \
        | tr -d '[:space:]')
    [ "${metadata_hex}" = "${MISC_METADATA_HEX}" ] \
        || fail "factory A/B metadata is invalid"
    nonzero_count=$(od -An -v -tu1 "${IMAGE_DIR}/misc.img" \
        | awk '{ for (i = 1; i <= NF; i++) if ($i != 0) count++ } END { print count + 0 }')
    [ "${nonzero_count}" -eq 10 ] \
        || fail "misc.img contains data outside the factory A/B metadata"
    {
        printf 'offset=2048\n'
        printf 'size=32\n'
        printf 'metadata_hex=%s\n' "${metadata_hex}"
        printf 'slot_a=priority:15,tries_remaining:0,successful_boot:1\n'
        printf 'slot_b=priority:0,tries_remaining:0,successful_boot:0\n'
    } >"${OUTPUT_DIR}/bsp-misc-audit.txt"
}

audit_boot() {
    local slot suffix root_label boot fdt kernel resource bootargs model
    local serial_contract serial_index expected_path expected_status serial_path serial_status
    local fiq_status fiq_serial_id
    : >"${OUTPUT_DIR}/bsp-boot-fit-audit.txt"
    for slot in a b; do
        suffix=_${slot}
        root_label=rootfs_${slot}
        boot=${IMAGE_DIR}/boot_${slot}.img
        fdt=${WORK_DIR}/boot_${slot}.dtb
        kernel=${WORK_DIR}/boot_${slot}.kernel
        resource=${WORK_DIR}/boot_${slot}.resource
        "${DUMPIMAGE}" -l "${boot}" >>"${OUTPUT_DIR}/bsp-boot-fit-audit.txt"
        "${DUMPIMAGE}" -i "${boot}" -T flat_dt -p 0 -o "${fdt}" unused \
            >>"${OUTPUT_DIR}/bsp-boot-fit-audit.txt"
        "${DUMPIMAGE}" -i "${boot}" -T flat_dt -p 1 -o "${kernel}" unused \
            >>"${OUTPUT_DIR}/bsp-boot-fit-audit.txt"
        "${DUMPIMAGE}" -i "${boot}" -T flat_dt -p 2 -o "${resource}" unused \
            >>"${OUTPUT_DIR}/bsp-boot-fit-audit.txt"

        cmp "${kernel}" "${KERNEL_IMAGE}"
        bootargs=$(fdtget -t s "${fdt}" /chosen bootargs)
        model=$(fdtget -t s "${fdt}" / model)
        bash "${SCRIPT_DIR}/audit-tf-dtb.sh" "${fdt}" \
            >>"${OUTPUT_DIR}/bsp-boot-fit-audit.txt"
        fiq_status=$(fdtget -t s "${fdt}" /fiq-debugger status)
        fiq_serial_id=$(fdtget -t u "${fdt}" /fiq-debugger rockchip,serial-id)
        [ "${model}" = "${EXPECTED_MODEL}" ] \
            || fail "boot_${slot}.img has the wrong model: ${model}"
        [ "${fiq_status}" = okay ] \
            || fail "boot_${slot}.img disables the FIQ recovery console"
        [ "${fiq_serial_id}" = 2 ] \
            || fail "boot_${slot}.img routes the FIQ recovery console to the wrong UART"
        for serial_contract in "${EXPECTED_SERIAL_CONTRACTS[@]}"; do
            IFS=: read -r serial_index expected_path expected_status \
                <<<"${serial_contract}"
            serial_path=$(fdtget -t s "${fdt}" "/aliases" "serial${serial_index}")
            serial_status=$(fdtget -t s "${fdt}" "${serial_path}" status)
            [ "${serial_path}" = "${expected_path}" ] \
                || fail "boot_${slot}.img has the wrong serial${serial_index} alias"
            [ "${serial_status}" = "${expected_status}" ] \
                || fail "boot_${slot}.img has the wrong serial${serial_index} status"
        done
        grep -qw "blkdevparts=mmcblk0:${PARTITION_LAYOUT}" <<<"${bootargs}" \
            || fail "boot_${slot}.img has the wrong partition command line"
        grep -qw "root=PARTLABEL=${root_label}" <<<"${bootargs}" \
            || fail "boot_${slot}.img has the wrong root PARTLABEL"
        grep -qw "aico.slot_suffix=${suffix}" <<<"${bootargs}" \
            || fail "boot_${slot}.img has the wrong Aico Link slot suffix"
        grep -qw 'rootfstype=ext4' <<<"${bootargs}" \
            || fail "boot_${slot}.img is missing rootfstype=ext4"
        grep -qw 'net.ifnames=0' <<<"${bootargs}" \
            || fail "boot_${slot}.img is missing net.ifnames=0"
        grep -qw 'rk_dma_heap_cma=100M' <<<"${bootargs}" \
            || fail "boot_${slot}.img is missing the production CMA setting"
        [ "$(tr ' ' '\n' <<<"${bootargs}" | grep -c '^root=')" -eq 1 ] \
            || fail "boot_${slot}.img contains multiple root arguments"
        [ "$(tr ' ' '\n' <<<"${bootargs}" | grep -c '^aico\.slot_suffix=')" -eq 1 ] \
            || fail "boot_${slot}.img contains multiple slot suffixes"
        {
            printf 'slot=%s\n' "${slot}"
            printf 'bootargs=%s\n' "${bootargs}"
            printf 'model=%s\n' "${model}"
            printf 'fiq_debugger_status=%s\n' "${fiq_status}"
            printf 'fiq_debugger_serial_id=%s\n' "${fiq_serial_id}"
            for serial_contract in "${EXPECTED_SERIAL_CONTRACTS[@]}"; do
                IFS=: read -r serial_index expected_path expected_status \
                    <<<"${serial_contract}"
                printf 'serial%s=%s\n' "${serial_index}" "${expected_path}"
                printf 'serial%s_status=%s\n' "${serial_index}" "${expected_status}"
            done
            printf 'fdt_sha256=%s\n' "$(sha256sum "${fdt}" | awk '{print $1}')"
            printf 'kernel_sha256=%s\n' "$(sha256sum "${kernel}" | awk '{print $1}')"
            printf 'resource_sha256=%s\n' "$(sha256sum "${resource}" | awk '{print $1}')"
        } >>"${OUTPUT_DIR}/bsp-boot-fit-audit.txt"
    done
    cmp "${WORK_DIR}/boot_a.kernel" "${WORK_DIR}/boot_b.kernel"
}

audit_modules() {
    local module firmware firmware_count module_count
    for module in \
        aic8800_bsp.ko aic8800_btlpm.ko aic8800_fdrv.ko \
        cfg80211.ko mac80211.ko mpp_vcodec.ko rga3.ko rknpu.ko rockit.ko \
        video_rkcif.ko video_rkisp.ko; do
        require_file "${MODULE_DIR}/${module}"
    done
    for module in mpp_vcodec.ko rockit.ko; do
        grep -aq 'aico_page_abi=kernel-helpers-v1' "${MODULE_DIR}/${module}" \
            || fail "media module lacks Debian kernel page compatibility: ${module}"
    done
    for firmware in \
        aic_userconfig_8800d80.txt \
        fmacfw_8800d80_h_u02.bin \
        fmacfw_8800d80_u02.bin \
        fw_adid_8800d80_u02.bin \
        fw_patch_8800d80_u02.bin \
        fw_patch_8800d80_u02_ext0.bin \
        fw_patch_table_8800d80_u02.bin \
        lmacfw_rf_8800d80_u02.bin; do
        require_file "${MODULE_DIR}/aic8800dc_fw/${firmware}"
    done
    module_count=$(find "${MODULE_DIR}" -maxdepth 1 -type f -name '*.ko' | wc -l)
    firmware_count=$(find "${MODULE_DIR}/aic8800dc_fw" -type f | wc -l)
    [ "${module_count}" -ge 11 ] || fail "too few kernel modules were produced"
    [ "${firmware_count}" -ge 1 ] || fail "AIC8800 firmware is missing"
    printf '%s\n' "${module_count}" >"${WORK_DIR}/module-count"
    printf '%s\n' "${firmware_count}" >"${WORK_DIR}/firmware-count"
}

write_hashes() {
    (
        cd "${SDK_DIR}"
        sha256sum \
            output/image/env.img \
            output/image/idblock.img \
            output/image/uboot.img \
            output/image/misc.img \
            output/image/boot_a.img \
            output/image/boot_b.img \
            output/image/download.bin \
            output/out/sysdrv_out/board_uclibc_rv1106/rv1106g-aico-custom.dtb \
            sysdrv/source/objs_kernel/arch/arm/boot/zImage \
            sysdrv/source/objs_kernel/.config
        find output/out/sysdrv_out/kernel_drv_ko -type f -print0 \
            | LC_ALL=C sort -z | xargs -0 sha256sum
    ) >"${OUTPUT_DIR}/bsp-artifacts.sha256"
    (
        cd "${REPO_ROOT}"
        sha256sum \
            scripts/debian-system/build.sh \
            scripts/debian-system/audit-bsp.sh \
            scripts/debian-system/audit-tf-dtb.sh \
            scripts/debian-system/canonicalize-bsp.py \
            scripts/debian-system/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk \
            scripts/debian-system/debian-system.config
    ) >"${OUTPUT_DIR}/bsp-inputs.sha256"
}

write_summary() {
    local artifact_manifest_sha input_manifest_sha
    artifact_manifest_sha=$(sha256sum "${OUTPUT_DIR}/bsp-artifacts.sha256" | awk '{print $1}')
    input_manifest_sha=$(sha256sum "${OUTPUT_DIR}/bsp-inputs.sha256" | awk '{print $1}')
    {
        printf 'status=pass\n'
        printf 'source_sdk_commit=%s\n' "$(cat "${OUTPUT_DIR}/source-sdk-commit.txt")"
        printf 'bsp_builder_image=%s\n' "$(cat "${OUTPUT_DIR}/bsp-builder-image-id.txt")"
        printf 'partition_layout=%s\n' "${PARTITION_LAYOUT}"
        printf 'kernel_config_sha256=%s\n' "$(sha256sum "${KERNEL_CONFIG}" | awk '{print $1}')"
        printf 'boot_a_sha256=%s\n' "$(sha256sum "${IMAGE_DIR}/boot_a.img" | awk '{print $1}')"
        printf 'boot_b_sha256=%s\n' "$(sha256sum "${IMAGE_DIR}/boot_b.img" | awk '{print $1}')"
        printf 'artifact_manifest_sha256=%s\n' "${artifact_manifest_sha}"
        printf 'input_manifest_sha256=%s\n' "${input_manifest_sha}"
        printf 'kernel_module_count=%s\n' "$(cat "${WORK_DIR}/module-count")"
        printf 'aic8800_firmware_file_count=%s\n' "$(cat "${WORK_DIR}/firmware-count")"
    } >"${OUTPUT_DIR}/bsp-audit-summary.txt"
}

main() {
    for command in awk cmp fdtget find od sha256sum sort stat tr xargs; do
        require_command "${command}"
    done
    require_file "${DUMPIMAGE}"
    test -x "${DUMPIMAGE}" || fail "SDK dumpimage tool is not executable"
    require_file "${KERNEL_IMAGE}"
    require_file "${KERNEL_CONFIG}"
    require_file "${BSP_DTB}"
    require_file "${OUTPUT_DIR}/source-sdk-commit.txt"
    require_file "${OUTPUT_DIR}/bsp-builder-image-id.txt"
    require_file "${IMAGE_DIR}/.env.txt"
    "${SCRIPT_DIR}/canonicalize-bsp.py" \
        --check \
        --source-date-epoch "${BUILD_EPOCH}" \
        --fit "${IMAGE_DIR}/uboot.img" \
        --fit "${IMAGE_DIR}/boot_a.img" \
        --fit "${IMAGE_DIR}/boot_b.img" \
        --crc-table-source "${SDK_DIR}/sysdrv/source/uboot/u-boot/tools/rockchip/boot_merger.c" \
        --loader "${IMAGE_DIR}/download.bin"
    grep -qx "blkdevparts=mmcblk0:${PARTITION_LAYOUT}" "${IMAGE_DIR}/.env.txt" \
        || fail "BSP partition environment changed"
    cmp "${KERNEL_CONFIG}" "${OUTPUT_DIR}/kernel.config"
    cmp "${IMAGE_DIR}/.env.txt" "${OUTPUT_DIR}/bsp-env.txt"

    WORK_DIR=$(mktemp -d "${OUTPUT_DIR}/.bsp-audit.XXXXXX")
    audit_partition_sizes
    audit_misc
    audit_boot
    audit_modules
    write_hashes
    write_summary
}

main "$@"
