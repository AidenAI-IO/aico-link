#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
    echo "Usage: $0 <apps-dir> <report-dir>" >&2
    exit 2
fi

readonly APPS_DIR=$1
readonly REPORT_DIR=$2
readonly REPORT=${REPORT_DIR}/elf-audit.tsv
readonly MPP_SYMBOLS=(
    mpp_buffer_get_with_tag
    mpp_buffer_put_with_caller
    mpp_buffer_get_ptr_with_caller
    mpp_frame_init
    mpp_frame_deinit
    mpp_frame_set_width
    mpp_frame_set_height
    mpp_frame_set_hor_stride
    mpp_frame_set_ver_stride
    mpp_frame_set_pts
    mpp_frame_set_eos
    mpp_frame_set_jpege_chan_id
    mpp_frame_set_buffer
    mpp_frame_set_fmt
    mpp_create_ext
    mpp_init
    mpp_destroy
    mpp_enc_cfg_init
    mpp_enc_cfg_deinit
    mpp_enc_cfg_set_s32
    mpp_enc_cfg_set_u32
)

readonly READELF=${READELF:-arm-linux-gnueabihf-readelf}
readonly GO_EXECUTABLES=(abctl agent ble_service ota)
readonly STATIC_EXECUTABLES=(ttyd)
readonly RKNN_RUNTIME_VERSION='librknnmrt version: 2.3.2 (429f97ae6b@2025-04-09T09:11:49)'
readonly RKNN_STATIC_SYMBOLS=(
    __ctype_b
    __ctype_tolower
    aiden_rknn_glibc_compat_init
    rknn_create_mem
    rknn_destroy
    rknn_destroy_mem
    rknn_init
    rknn_query
    rknn_run
    rknn_set_io_mem
)
failures=0

fail() {
    echo "ELF audit failure: $*" >&2
    failures=$((failures + 1))
}

mkdir -p "${REPORT_DIR}"
printf 'kind\tsha256\tbytes\tmachine\thard_float\tinterpreter\trunpath\tneeded\tpath\n' \
    >"${REPORT}"

while IFS= read -r -d '' elf; do
    relative=${elf#${APPS_DIR}/}
    header=$(${READELF} -hW "${elf}")
    dynamic=$(${READELF} -dW "${elf}" 2>/dev/null || true)
    program_headers=$(${READELF} -lW "${elf}" 2>/dev/null || true)
    machine=$(sed -n 's/^[[:space:]]*Machine:[[:space:]]*//p' <<<"${header}")
    flags=$(sed -n 's/^[[:space:]]*Flags:[[:space:]]*//p' <<<"${header}")
    interpreter=$(sed -n 's/.*Requesting program interpreter: \([^]]*\).*/\1/p' \
        <<<"${program_headers}")
    runpath=$(sed -n 's/.*(RUNPATH).*\[\([^]]*\)\].*/\1/p' <<<"${dynamic}")
    rpath=$(sed -n 's/.*(RPATH).*\[\([^]]*\)\].*/\1/p' <<<"${dynamic}")
    needed=$(sed -n 's/.*(NEEDED).*\[\([^]]*\)\].*/\1/p' <<<"${dynamic}" \
        | paste -sd, -)
    kind=shared
    if [[ "${relative}" == bin/* ]]; then
        kind=executable
    fi
    for go_executable in "${GO_EXECUTABLES[@]}"; do
        if [ "${relative}" = "bin/${go_executable}" ]; then
            kind=static-go
            break
        fi
    done
    for static_executable in "${STATIC_EXECUTABLES[@]}"; do
        if [ "${relative}" = "bin/${static_executable}" ]; then
            kind=static
            break
        fi
    done
    hard_float=no
    if grep -q 'hard-float ABI' <<<"${flags}"; then
        hard_float=yes
    fi

    [ "${machine}" = ARM ] || fail "${relative} is not ARM (${machine})"
    if [ "${kind}" = static-go ] || [ "${kind}" = static ]; then
        grep -q 'Version5 EABI' <<<"${flags}" \
            || fail "${relative} is not ARM EABI5 (${flags})"
        [ -z "${interpreter}" ] \
            || fail "${relative} static binary has interpreter '${interpreter}'"
        [ -z "${needed}" ] \
            || fail "${relative} static binary has dynamic dependencies '${needed}'"
        [ -z "${runpath}" ] \
            || fail "${relative} static binary has RUNPATH '${runpath}'"
    elif [ "${kind}" = executable ]; then
        [ "${hard_float}" = yes ] || fail "${relative} is not marked hard-float"
        [ "${interpreter}" = /lib/ld-linux-armhf.so.3 ] \
            || fail "${relative} has unexpected interpreter '${interpreter}'"
        [ "${runpath}" = '$ORIGIN/../lib' ] \
            || fail "${relative} has unexpected RUNPATH '${runpath}'"
    else
        [ "${hard_float}" = yes ] || fail "${relative} is not marked hard-float"
        if [ -n "${interpreter}" ]; then
            fail "${relative} shared library unexpectedly has an interpreter"
        fi
    fi
    [ -z "${rpath}" ] || fail "${relative} contains legacy RPATH '${rpath}'"
    if grep -Eq '(^|,)(libc\.so\.0|ld-uClibc[^,]*)(,|$)' <<<"${needed}"; then
        fail "${relative} has a uClibc dependency: ${needed}"
    fi
    if grep -aEq 'libc\.so\.0|ld-uClibc|uClibc' "${elf}"; then
        fail "${relative} contains a uClibc marker"
    fi
    if grep -Eq '(^|,)libopencv_[^,]*\.so' <<<"${needed}"; then
        fail "${relative} dynamically depends on OpenCV: ${needed}"
    fi

    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "${kind}" \
        "$(sha256sum "${elf}" | awk '{print $1}')" \
        "$(stat -c %s "${elf}")" \
        "${machine}" \
        "${hard_float}" \
        "${interpreter}" \
        "${runpath}" \
        "${needed}" \
        "${relative}" >>"${REPORT}"
done < <(find "${APPS_DIR}/bin" "${APPS_DIR}/lib" -type f -print0 | sort -z)

for static_executable in "${STATIC_EXECUTABLES[@]}"; do
    [ -x "${APPS_DIR}/bin/${static_executable}" ] \
        || fail "missing static executable: bin/${static_executable}"
done

for go_executable in "${GO_EXECUTABLES[@]}"; do
    [ -x "${APPS_DIR}/bin/${go_executable}" ] \
        || fail "missing static Go executable: bin/${go_executable}"
done

for rknn_bin in rknn_vad smart_turn_helper; do
    rknn_needed=$(${READELF} -dW "${APPS_DIR}/bin/${rknn_bin}" \
        | sed -n 's/.*(NEEDED).*\[\([^]]*\)\].*/\1/p')
    if grep -Eq '^librknn(rt|mrt)\.so$' <<<"${rknn_needed}"; then
        fail "${rknn_bin} has a dynamic RKNN runtime dependency: ${rknn_needed}"
    fi
    rknn_symbols=$(${READELF} --syms -W "${APPS_DIR}/bin/${rknn_bin}")
    for symbol in "${RKNN_STATIC_SYMBOLS[@]}"; do
        awk -v symbol="${symbol}" \
            '$7 != "UND" && $8 == symbol { found = 1 } END { exit !found }' \
            <<<"${rknn_symbols}" || fail "${rknn_bin} does not define static symbol ${symbol}"
    done
    grep -aFq "${RKNN_RUNTIME_VERSION}" "${APPS_DIR}/bin/${rknn_bin}" \
        || fail "${rknn_bin} does not embed the expected RKNN mini runtime ${RKNN_RUNTIME_VERSION}"
done

frame_dynamic_symbols=$(${READELF} --dyn-syms -W "${APPS_DIR}/bin/frame_service")
for symbol in "${MPP_SYMBOLS[@]}"; do
    grep -Eq "[[:space:]]${symbol}$" <<<"${frame_dynamic_symbols}" \
        || fail "frame_service does not export ${symbol} for OpenCV-Mobile"
done

if [ ! -L "${APPS_DIR}/lib/librga.so" ]; then
    fail "missing librga.so symlink"
elif [ "$(readlink "${APPS_DIR}/lib/librga.so")" != librga.so.2 ]; then
    fail "librga.so does not point to librga.so.2"
fi
if [ ! -L "${APPS_DIR}/lib/librga.so.2" ]; then
    fail "missing librga.so.2 symlink"
elif [ "$(readlink "${APPS_DIR}/lib/librga.so.2")" != librga.so.2.1.0 ]; then
    fail "librga.so.2 does not point to librga.so.2.1.0"
fi
[ -f "${APPS_DIR}/lib/librga.so.2.1.0" ] \
    || fail "missing librga.so.2.1.0 target"

if [ "${failures}" -ne 0 ]; then
    echo "ELF audit failed with ${failures} issue(s)" >&2
    exit 1
fi

printf 'status=pass\nelf_count=%s\n' "$(( $(wc -l <"${REPORT}") - 1 ))" \
    >"${REPORT_DIR}/summary.txt"
