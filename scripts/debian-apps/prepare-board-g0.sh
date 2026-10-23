#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
readonly APPS_OUTPUT=${DEBIAN_APPS_OUTPUT_DIR:-${REPO_ROOT}/output/debian-apps}
readonly APPS_DIR=${APPS_OUTPUT}/apps
readonly AUDIT_DIR=${APPS_OUTPUT}/apps-audit
readonly DEFAULT_BUNDLE_DIR=${APPS_OUTPUT}/board-g0
readonly BUNDLE_DIR=${DEBIAN_APPS_G0_OUTPUT_DIR:-${DEFAULT_BUNDLE_DIR}}
readonly BUILD_EPOCH=${SOURCE_DATE_EPOCH:-1767360516}
readonly BUNDLE_NAME=debian-apps-g0
readonly ARCHIVE=${BUNDLE_DIR}/${BUNDLE_NAME}.tar.gz
readonly MODEL_DIR=${REPO_ROOT}/assets/business/models
readonly MODULE_HELPER=${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-media-modules
readonly MODEL_RKNN=silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn
readonly MODEL_WEIGHTS=silero_vad_6_2_lstm_decoder_weights.bin
readonly MODEL_RKNN_SHA256=81515d04c665ac8c1370d376c07987cf7f368a72ab9e3083dd68ad26c7ae1509
readonly MODEL_WEIGHTS_SHA256=f4c25e3669172b75b928e4c4a855657fd747f24fd2d8af599e0852e713fbc183
readonly REMOTE_RUNNER=${SCRIPT_DIR}/board-g0-remote.sh

readonly -a APP_PAYLOAD=(
    bin/aico-environment
    bin/audio_service
    bin/audio_service_cli
    bin/audio_stream
    bin/agent
    bin/cpu_vad
    bin/example_audio_capture
    bin/example_audio_play
    bin/example_camera_capture
    bin/example_usb_hid
    bin/example_wakeup
    bin/frame_service
    bin/frame_service_cli
    bin/hello
    bin/image_process
    bin/rknn_vad
    bin/ttyd
    bin/trigger
    lib/librga.so.2.1.0
)

usage() {
    cat <<'EOF'
Usage: scripts/debian-apps/prepare-board-g0.sh [bundle|verify]

Create or verify the non-destructive apps-stage physical-board G0 payload. This
script never contacts a board. The bundle contains the audited Debian/glibc
C/C++ executables, RGA, the statically linked official RKNN mini runtime, the
two pinned VAD model files, and the system media-module loader.

Environment:
  DEBIAN_APPS_OUTPUT_DIR     apps-stage build output.
  DEBIAN_APPS_G0_OUTPUT_DIR  Bundle output directory.
  SOURCE_DATE_EPOCH            Reproducible archive timestamp.
EOF
}

fail() {
    echo "apps board G0 bundle failure: $*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

file_sha256() {
    sha256sum "$1" | awk '{print $1}'
}

verify_model() {
    local path=$1
    local expected=$2
    local actual
    [ -f "${path}" ] || fail "missing model file: ${path}"
    actual=$(file_sha256 "${path}")
    [ "${actual}" = "${expected}" ] ||
        fail "model checksum mismatch for ${path}: expected ${expected}, got ${actual}"
}

verify_apps() {
    local rel expected actual matches
    [ -f "${AUDIT_DIR}/summary.txt" ] || fail "missing apps audit summary"
    [ -f "${AUDIT_DIR}/elf-audit.tsv" ] || fail "missing apps ELF audit"
    grep -qx 'status=pass' "${AUDIT_DIR}/summary.txt" || fail "apps ELF audit did not pass"

    for rel in "${APP_PAYLOAD[@]}"; do
        [ -f "${APPS_DIR}/${rel}" ] || fail "missing audited payload file: ${rel}"
        matches=$(awk -F '\t' -v path="${rel}" 'NR > 1 && $9 == path {count++} END {print count+0}' \
            "${AUDIT_DIR}/elf-audit.tsv")
        [ "${matches}" = 1 ] || fail "ELF audit must contain exactly one row for ${rel}"
        expected=$(awk -F '\t' -v path="${rel}" 'NR > 1 && $9 == path {print $2}' \
            "${AUDIT_DIR}/elf-audit.tsv")
        actual=$(file_sha256 "${APPS_DIR}/${rel}")
        [ "${actual}" = "${expected}" ] ||
            fail "payload checksum no longer matches ELF audit for ${rel}"
    done

    [ "$(readlink "${APPS_DIR}/lib/librga.so")" = librga.so.2 ] ||
        fail "librga.so must point to librga.so.2"
    [ "$(readlink "${APPS_DIR}/lib/librga.so.2")" = librga.so.2.1.0 ] ||
        fail "librga.so.2 must point to librga.so.2.1.0"
}

write_bundle_metadata() {
    local root=$1
    local apps_audit_sha module_helper_sha remote_runner_sha
    apps_audit_sha=$(file_sha256 "${AUDIT_DIR}/elf-audit.tsv")
    module_helper_sha=$(file_sha256 "${MODULE_HELPER}")
    remote_runner_sha=$(file_sha256 "${REMOTE_RUNNER}")
    {
        printf 'format=1\n'
        printf 'source_date_epoch=%s\n' "${BUILD_EPOCH}"
        printf 'hardware_demo_commit=%s\n' "$(git -C "${REPO_ROOT}" rev-parse HEAD)"
        printf 'pico_sdk_commit=%s\n' "$(git -C "${REPO_ROOT}" rev-parse HEAD)"
        printf 'apps_elf_audit_sha256=%s\n' "${apps_audit_sha}"
        printf 'media_module_helper_sha256=%s\n' "${module_helper_sha}"
        printf 'board_g0_remote_runner_sha256=%s\n' "${remote_runner_sha}"
        printf 'rknn_model_sha256=%s\n' "${MODEL_RKNN_SHA256}"
        printf 'decoder_weights_sha256=%s\n' "${MODEL_WEIGHTS_SHA256}"
    } >"${root}/bundle-metadata.txt"
}

write_payload_manifest() {
    local root=$1
    {
        printf 'lib/librga.so -> librga.so.2\n'
        printf 'lib/librga.so.2 -> librga.so.2.1.0\n'
    } >"${root}/SYMLINKS.txt"
    (
        cd "${root}"
        find . -type f ! -name MANIFEST.sha256 -print0 |
            LC_ALL=C sort -z |
            xargs -0 sha256sum >MANIFEST.sha256
    )
}

run_bundle() {
    local temp_root payload_root rel
    case "${BUILD_EPOCH}" in
        '' | *[!0-9]*) fail "SOURCE_DATE_EPOCH must be an unsigned Unix timestamp" ;;
    esac
    verify_apps
    verify_model "${MODEL_DIR}/${MODEL_RKNN}" "${MODEL_RKNN_SHA256}"
    verify_model "${MODEL_DIR}/${MODEL_WEIGHTS}" "${MODEL_WEIGHTS_SHA256}"
    [ -x "${MODULE_HELPER}" ] || fail "missing executable media-module helper"
    [ -x "${REMOTE_RUNNER}" ] || fail "missing executable board G0 remote runner"

    mkdir -p "${BUNDLE_DIR}"
    temp_root=$(mktemp -d "${BUNDLE_DIR}/.${BUNDLE_NAME}.XXXXXX")
    trap 'rm -rf "${temp_root}"' RETURN
    payload_root=${temp_root}/${BUNDLE_NAME}
    mkdir -p "${payload_root}/bin" "${payload_root}/lib" \
        "${payload_root}/model" "${payload_root}/audit"

    for rel in "${APP_PAYLOAD[@]}"; do
        cp -a "${APPS_DIR}/${rel}" "${payload_root}/${rel}"
    done
    cp -a "${APPS_DIR}/lib/librga.so" "${payload_root}/lib/librga.so"
    cp -a "${APPS_DIR}/lib/librga.so.2" "${payload_root}/lib/librga.so.2"
    cp -a "${MODEL_DIR}/${MODEL_RKNN}" "${payload_root}/model/"
    cp -a "${MODEL_DIR}/${MODEL_WEIGHTS}" "${payload_root}/model/"
    cp -a "${MODULE_HELPER}" "${payload_root}/aico-media-modules"
    cp -a "${REMOTE_RUNNER}" "${payload_root}/board-g0-remote.sh"
    cp -a "${AUDIT_DIR}/summary.txt" "${payload_root}/audit/"
    cp -a "${AUDIT_DIR}/elf-audit.tsv" "${payload_root}/audit/"
    write_bundle_metadata "${payload_root}"
    write_payload_manifest "${payload_root}"

    chmod 0755 "${payload_root}/aico-media-modules" \
        "${payload_root}/board-g0-remote.sh" "${payload_root}/bin/"*
    find "${payload_root}" -exec touch -h -d "@${BUILD_EPOCH}" {} +
    tar --sort=name --owner=0 --group=0 --numeric-owner \
        --mtime="@${BUILD_EPOCH}" --pax-option=delete=atime,delete=ctime \
        --use-compress-program='gzip -n' -cf "${ARCHIVE}.tmp" \
        -C "${temp_root}" "${BUNDLE_NAME}"
    mv "${ARCHIVE}.tmp" "${ARCHIVE}"
    (
        cd "${BUNDLE_DIR}"
        sha256sum "$(basename "${ARCHIVE}")" >"$(basename "${ARCHIVE}.sha256")"
    )
    trap - RETURN
    rm -rf "${temp_root}"
    printf 'Created %s\n' "${ARCHIVE}"
    cat "${ARCHIVE}.sha256"
}

run_verify() {
    local temp_root payload_root
    [ -f "${ARCHIVE}" ] || fail "bundle archive is missing: ${ARCHIVE}"
    [ -f "${ARCHIVE}.sha256" ] || fail "bundle archive checksum is missing"
    (cd "${BUNDLE_DIR}" && sha256sum -c "$(basename "${ARCHIVE}.sha256")")
    temp_root=$(mktemp -d)
    trap 'rm -rf "${temp_root}"' RETURN
    tar -xzf "${ARCHIVE}" -C "${temp_root}"
    payload_root=${temp_root}/${BUNDLE_NAME}
    (cd "${payload_root}" && sha256sum -c MANIFEST.sha256)
    [ "$(readlink "${payload_root}/lib/librga.so")" = librga.so.2 ] ||
        fail "verified archive has the wrong librga.so link"
    [ "$(readlink "${payload_root}/lib/librga.so.2")" = librga.so.2.1.0 ] ||
        fail "verified archive has the wrong librga.so.2 link"
    trap - RETURN
    rm -rf "${temp_root}"
}

main() {
    local action=${1:-bundle}
    case "${action}" in
        -h | --help | help)
            usage
            return
            ;;
        bundle | verify) ;;
        *)
            usage >&2
            exit 2
            ;;
    esac
    for command in awk cp find git grep gzip readlink sha256sum sort tar touch xargs; do
        require_command "${command}"
    done
    case "${action}" in
        bundle) run_bundle ;;
        verify) run_verify ;;
    esac
}

main "$@"
