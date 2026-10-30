#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly APPS_DIR=${REPO_ROOT}/scripts/debian-apps
readonly TEST_ROOT=$(mktemp -d)
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Debian apps test failure: $*" >&2
    exit 1
}

apps_scripts=(
    "${APPS_DIR}/build-apps.sh"
    "${APPS_DIR}/container-build-opencv-mobile.sh"
    "${APPS_DIR}/container-build-apps.sh"
    "${APPS_DIR}/container-build-rootfs-cli-tools.sh"
    "${APPS_DIR}/audit-apps.sh"
    "${APPS_DIR}/prepare-board-g0.sh"
    "${APPS_DIR}/board-g0-remote.sh"
    "${APPS_DIR}/run-board-g0.sh"
)
for apps_script in "${apps_scripts[@]}"; do
    bash -n "${apps_script}"
done

grep -Fq 'aico@192.168.76.153' "${APPS_DIR}/run-board-g0.sh"
grep -Fq '/home/aico/debian-apps-g0' "${APPS_DIR}/run-board-g0.sh"
grep -Fq 'volatile sig_atomic_t quit' "${REPO_ROOT}/src/example_audio_capture.cpp"
grep -Fq 'signal(SIGTERM, signal_handler)' "${REPO_ROOT}/src/example_audio_capture.cpp"

grep -Fq 'chown "${SUDO_UID}:${SUDO_GID}" "${RESULTS_ROOT}"' \
    "${APPS_DIR}/board-g0-remote.sh"

# Plain http on purpose: apt verifies InRelease signatures itself, and this
# mirror serves HTTP/1.1 clients such as apt at full speed (mirrors.aliyun.com
# throttles HTTP/1.1 to ~300 kB/s; only its HTTP/2 path is fast).
grep -qx 'URIs: http://mirrors.ustc.edu.cn/debian' "${APPS_DIR}/debian.sources" \
    || fail "apps builder does not fetch the Debian archive from mirrors.ustc.edu.cn"
grep -qx 'URIs: http://mirrors.ustc.edu.cn/debian-security' "${APPS_DIR}/debian.sources" \
    || fail "apps builder does not fetch debian-security from mirrors.ustc.edu.cn"
if grep -q '^Check-Valid-Until:' "${APPS_DIR}/debian.sources"; then
    fail "a live mirror must keep the InRelease Valid-Until check"
fi
grep -q 'builder-packages.txt' "${APPS_DIR}/container-build-apps.sh"
grep -q 'GOOS=linux GOARCH=arm GOARM=7' \
    "${APPS_DIR}/container-build-apps.sh"
grep -Fq 'ttyd.armhf' "${APPS_DIR}/container-build-apps.sh"
grep -Fq 'b0784080bd78f0a5916462672f461542c607f8ea7cee56b075e8cd04e1ffcc4d' \
    "${APPS_DIR}/container-build-apps.sh"
grep -q -- '-buildid=' "${APPS_DIR}/container-build-apps.sh"
grep -Fq 'getenv("FRAME_SERVICE_JPEG_ENCODER")' \
    "${APPS_DIR}/opencv-mobile-rk-mpp-main-program.patch"
grep -Fq 'strcmp(encoder, "software") == 0' \
    "${APPS_DIR}/opencv-mobile-rk-mpp-main-program.patch"
grep -q 'source-archive.sha256' \
    "${APPS_DIR}/container-build-opencv-mobile.sh"
grep -q 'OPENCV_SOURCE_DATE_EPOCH=1767360516' \
    "${APPS_DIR}/container-build-opencv-mobile.sh"
grep -qx '[[:space:]]*curl \\' "${APPS_DIR}/Dockerfile"
grep -Fq -- '--output-dir "${OUTPUT_DIR}/rootfs-cli-tools"' \
    "${APPS_DIR}/container-build-rootfs-cli-tools.sh"
grep -Fq -- '--catalog "${REPO_ROOT}/scripts/rootfs_cli_tools.catalog"' \
    "${APPS_DIR}/container-build-rootfs-cli-tools.sh"
grep -Fq 'run_cli_tools' "${APPS_DIR}/build-apps.sh"
# `go install module@version` inside the container must use the host's module
# proxy. Without this the self-hosted builders fall back to proxy.golang.org and
# the rootfs CLI tool build dies on a connection reset.
for go_proxy_var in GOPROXY GONOPROXY GOPRIVATE GOSUMDB GONOSUMDB; do
    if ! grep -Eq -- "^[[:space:]]*-e ${go_proxy_var} \\\\$" "${APPS_DIR}/build-apps.sh" &&
       ! grep -Eq -- "^[[:space:]]*-e ${go_proxy_var}$" "${APPS_DIR}/build-apps.sh"; then
        fail "apps container must forward ${go_proxy_var} to reach the module proxy"
    fi
done
if grep -Eq -- '^[[:space:]]*-e GOFLAGS \\$' "${APPS_DIR}/build-apps.sh"; then
    fail "apps container must not inherit GOFLAGS, which would change build output"
fi
grep -Fq 'assets/business/models' "${APPS_DIR}/prepare-board-g0.sh"
grep -Fq 'bin/ttyd' "${APPS_DIR}/prepare-board-g0.sh"
if grep -Fq '${REPO_ROOT}/overlay/oem' "${APPS_DIR}/prepare-board-g0.sh"; then
    fail "Debian board bundle depends on the Buildroot OEM overlay"
fi
grep -Fq 'set(AICO_TARGET_PLATFORM "rv1106-debian-glibc"' \
    "${REPO_ROOT}/CMakeLists.txt"
if sed -n '/set_property(CACHE AICO_TARGET_PLATFORM PROPERTY STRINGS/,/)/p' \
    "${REPO_ROOT}/CMakeLists.txt" | grep -q buildroot; then
    fail "CMake still advertises Buildroot as a supported target platform"
fi

help_output=${TEST_ROOT}/help-output
DEBIAN_APPS_OUTPUT_DIR="${help_output}" \
    "${APPS_DIR}/build-apps.sh" --help >/dev/null
[ ! -e "${help_output}" ] || fail "--help created the output directory"

if "${APPS_DIR}/build-apps.sh" invalid-action >/dev/null 2>&1; then
    fail "invalid build action succeeded"
fi

mkdir -p "${TEST_ROOT}/mock-bin"
mkdir -p "${TEST_ROOT}/go-root/bin"
printf 'go1.26.0\n' >"${TEST_ROOT}/go-root/VERSION"
cat >"${TEST_ROOT}/go-root/bin/go" <<'EOF'
#!/usr/bin/env sh
exit 0
EOF
chmod +x "${TEST_ROOT}/go-root/bin/go"
cat >"${TEST_ROOT}/mock-bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\0' "$@" >>"${MOCK_DOCKER_LOG}"
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
    printf 'sha256:mock-builder-image\n'
fi
if [ "${1:-}" = run ]; then
    # Record the environment Docker would pass for both -e NAME=value and
    # -e NAME. Keep the raw argument log separate for mount/command checks.
    : >"${MOCK_DOCKER_LOG}.env"
    previous=
    for argument in "$@"; do
        if [ "${previous}" = -e ]; then
            case "${argument}" in
            *=*) printf '%s\0' "${argument}" >>"${MOCK_DOCKER_LOG}.env" ;;
            *)
                if [ "${!argument+x}" = x ]; then
                    printf '%s\0' "${argument}=${!argument}" >>"${MOCK_DOCKER_LOG}.env"
                fi
                ;;
            esac
            previous=
        else
            previous=${argument}
        fi
    done
fi
EOF
chmod +x "${TEST_ROOT}/mock-bin/docker"

mock_output=${TEST_ROOT}/mock-output
mock_log=${TEST_ROOT}/docker-args
MOCK_DOCKER_LOG="${mock_log}" \
PATH="${TEST_ROOT}/mock-bin:${PATH}" \
DEBIAN_APPS_OUTPUT_DIR="${mock_output}" \
DEBIAN_APPS_GO_ROOT="${TEST_ROOT}/go-root" \
DEBIAN_APPS_GO_BUILD_CACHE="${TEST_ROOT}/go-build-cache" \
DEBIAN_APPS_GO_MODULE_CACHE="${TEST_ROOT}/go-mod-cache" \
    env -u GOPROXY "${APPS_DIR}/build-apps.sh" apps
tr '\0' '\n' <"${mock_log}" >"${TEST_ROOT}/docker-args.txt"
grep -qx 'DEBIAN_APPS_BUILD_IMAGE_ID=sha256:mock-builder-image' \
    "${TEST_ROOT}/docker-args.txt"
grep -qx "${mock_output}:/out" "${TEST_ROOT}/docker-args.txt"
source_git_common_dir=$(git -C "${REPO_ROOT}" rev-parse \
    --path-format=absolute --git-common-dir 2>/dev/null || true)
if [ -n "${source_git_common_dir}" ]; then
    grep -qx "${source_git_common_dir}:${source_git_common_dir}:ro" \
        "${TEST_ROOT}/docker-args.txt"
fi
grep -qx "${TEST_ROOT}/go-root:/usr/local/go:ro" \
    "${TEST_ROOT}/docker-args.txt"
grep -qx "${TEST_ROOT}/go-build-cache:/go-build-cache" \
    "${TEST_ROOT}/docker-args.txt"
grep -qx "${TEST_ROOT}/go-mod-cache:/go-mod-cache" \
    "${TEST_ROOT}/docker-args.txt"
grep -qx 'scripts/debian-apps/container-build-apps.sh' \
    "${TEST_ROOT}/docker-args.txt"
# Bare -e GOPROXY flags must leave it undefined when the caller has not set
# it. Inspect the effective environment rather than just the argument names.
tr '\0' '\n' <"${mock_log}.env" >"${TEST_ROOT}/docker-env.txt"
if grep -q '^GOPROXY=' "${TEST_ROOT}/docker-env.txt"; then
    fail "build-apps.sh forwarded a GOPROXY that was not set"
fi

# With one set, the container must receive it verbatim: a cold module cache
# otherwise downloads straight from proxy.golang.org, which the self-hosted
# runners cannot reliably reach.
: >"${mock_log}"
MOCK_DOCKER_LOG="${mock_log}" \
PATH="${TEST_ROOT}/mock-bin:${PATH}" \
GOPROXY="https://goproxy.example/|https://proxy.golang.org|direct" \
DEBIAN_APPS_OUTPUT_DIR="${mock_output}" \
DEBIAN_APPS_GO_ROOT="${TEST_ROOT}/go-root" \
DEBIAN_APPS_GO_BUILD_CACHE="${TEST_ROOT}/go-build-cache" \
DEBIAN_APPS_GO_MODULE_CACHE="${TEST_ROOT}/go-mod-cache" \
    "${APPS_DIR}/build-apps.sh" apps
tr '\0' '\n' <"${mock_log}" >"${TEST_ROOT}/goproxy-docker-args.txt"
tr '\0' '\n' <"${mock_log}.env" >"${TEST_ROOT}/goproxy-docker-env.txt"
grep -qx 'GOPROXY=https://goproxy.example/|https://proxy.golang.org|direct' \
    "${TEST_ROOT}/goproxy-docker-env.txt" \
    || fail "build-apps.sh did not forward GOPROXY into the apps container"

: >"${mock_log}"
MOCK_DOCKER_LOG="${mock_log}" \
PATH="${TEST_ROOT}/mock-bin:${PATH}" \
DEBIAN_APPS_OUTPUT_DIR="${mock_output}" \
DEBIAN_APPS_GO_ROOT="${TEST_ROOT}/go-root" \
DEBIAN_APPS_GO_BUILD_CACHE="${TEST_ROOT}/go-build-cache" \
DEBIAN_APPS_GO_MODULE_CACHE="${TEST_ROOT}/go-mod-cache" \
    "${APPS_DIR}/build-apps.sh" cli-tools
tr '\0' '\n' <"${mock_log}" >"${TEST_ROOT}/cli-tools-docker-args.txt"
grep -qx 'scripts/debian-apps/container-build-rootfs-cli-tools.sh' \
    "${TEST_ROOT}/cli-tools-docker-args.txt"

bad_source_output=${TEST_ROOT}/bad-source-output
bad_source_cache=${TEST_ROOT}/bad-source-cache
mkdir -p "${bad_source_cache}"
printf 'not the pinned archive\n' \
    >"${bad_source_cache}/opencv-mobile-4.13.0.zip"
: >"${mock_log}"
if MOCK_DOCKER_LOG="${mock_log}" \
    PATH="${TEST_ROOT}/mock-bin:${PATH}" \
    DEBIAN_APPS_OUTPUT_DIR="${bad_source_output}" \
    DEBIAN_APPS_OPENCV_CACHE="${bad_source_cache}" \
        "${APPS_DIR}/build-apps.sh" opencv >/dev/null 2>&1; then
    fail "OpenCV checksum mismatch succeeded"
fi
[ ! -s "${mock_log}" ] \
    || fail "Docker ran after the OpenCV checksum mismatch"
[ ! -e "${bad_source_cache}/opencv-mobile-4.13.0.zip" ] \
    || fail "the rejected OpenCV archive stayed in the persistent cache"
[ ! -e "${bad_source_output}/cache/opencv-mobile-4.13.0.zip" ] \
    || fail "the rejected OpenCV archive was staged into the output directory"

# A cached archive must be staged into the output directory the container
# bind-mounts, and must not be downloaded again. sha256sum is mocked because
# the pinned checksum cannot be reproduced without the real 60MB archive.
opencv_output=${TEST_ROOT}/opencv-output
opencv_cache=${TEST_ROOT}/opencv-cache
mkdir -p "${opencv_cache}" "${TEST_ROOT}/opencv-bin"
printf 'pinned archive stand-in\n' >"${opencv_cache}/opencv-mobile-4.13.0.zip"
cat >"${TEST_ROOT}/opencv-bin/sha256sum" <<'EOF'
#!/usr/bin/env sh
printf '%s  %s\n' \
    9304482980b3e4ff1050a8527cdb5777fadf8c5dd9c1a8620170d23e252fb150 "$1"
EOF
cat >"${TEST_ROOT}/opencv-bin/curl" <<'EOF'
#!/usr/bin/env sh
echo "build-apps.sh re-downloaded an already cached OpenCV archive" >&2
exit 1
EOF
chmod +x "${TEST_ROOT}/opencv-bin/sha256sum" "${TEST_ROOT}/opencv-bin/curl"
: >"${mock_log}"
MOCK_DOCKER_LOG="${mock_log}" \
PATH="${TEST_ROOT}/opencv-bin:${TEST_ROOT}/mock-bin:${PATH}" \
DEBIAN_APPS_OUTPUT_DIR="${opencv_output}" \
DEBIAN_APPS_OPENCV_CACHE="${opencv_cache}" \
DEBIAN_APPS_GO_ROOT="${TEST_ROOT}/go-root" \
DEBIAN_APPS_GO_BUILD_CACHE="${TEST_ROOT}/go-build-cache" \
DEBIAN_APPS_GO_MODULE_CACHE="${TEST_ROOT}/go-mod-cache" \
    "${APPS_DIR}/build-apps.sh" opencv >/dev/null
cmp -s "${opencv_cache}/opencv-mobile-4.13.0.zip" \
    "${opencv_output}/cache/opencv-mobile-4.13.0.zip" \
    || fail "the cached OpenCV archive was not staged for the container"

apps_dir=${TEST_ROOT}/apps
mkdir -p "${apps_dir}/bin" "${apps_dir}/lib"
touch \
    "${apps_dir}/bin/abctl" \
    "${apps_dir}/bin/agent" \
    "${apps_dir}/bin/ble_service" \
    "${apps_dir}/bin/mcu_service" \
    "${apps_dir}/bin/frame_service" \
    "${apps_dir}/bin/ota" \
    "${apps_dir}/bin/rknn_vad" \
    "${apps_dir}/bin/smart_turn_helper" \
    "${apps_dir}/bin/ttyd" \
    "${apps_dir}/lib/librga.so.2.1.0"
chmod +x \
    "${apps_dir}/bin/abctl" \
    "${apps_dir}/bin/agent" \
    "${apps_dir}/bin/ble_service" \
    "${apps_dir}/bin/mcu_service" \
    "${apps_dir}/bin/ota"
chmod +x "${apps_dir}/bin/ttyd"
printf '%s\n' \
    'librknnmrt version: 2.3.2 (429f97ae6b@2025-04-09T09:11:49)' \
    >"${apps_dir}/bin/rknn_vad"
printf '%s\n' \
    'librknnmrt version: 2.3.2 (429f97ae6b@2025-04-09T09:11:49)' \
    >"${apps_dir}/bin/smart_turn_helper"
ln -s librga.so.2.1.0 "${apps_dir}/lib/librga.so.2"
ln -s librga.so.2 "${apps_dir}/lib/librga.so"

cat >"${TEST_ROOT}/mock-readelf" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mode=${1:-}
file=${!#}
is_go=no
case "${file}" in
*/bin/abctl | */bin/agent | */bin/ble_service | */bin/mcu_service | */bin/ota | */bin/ttyd) is_go=yes ;;
esac
case "${mode}" in
-hW)
    printf '  Machine:                           ARM\n'
    printf '  Flags:                             0x5000400, Version5 EABI, hard-float ABI\n'
    ;;
-lW)
    if [[ "${file}" == */bin/* ]] && [ "${is_go}" != yes ]; then
        printf '      [Requesting program interpreter: /lib/ld-linux-armhf.so.3]\n'
    fi
    ;;
-dW)
    if [ "${is_go}" = yes ]; then
        exit 0
    fi
    if [[ "${file}" == */bin/* ]]; then
        runpath='$ORIGIN/../lib'
        if [ "${MOCK_BAD_RUNPATH:-0}" = 1 ]; then
            runpath=/usr/lib/aico/platform/lib
        fi
        printf ' 0x0000001d (RUNPATH) Library runpath: [%s]\n' "${runpath}"
    fi
    if [[ "${file}" == */bin/rknn_vad || "${file}" == */bin/smart_turn_helper ]]; then
        printf ' 0x00000001 (NEEDED) Shared library: [libc.so.6]\n'
    elif [ "${MOCK_OPENCV_NEEDED:-0}" = 1 ] && [[ "${file}" == */bin/frame_service ]]; then
        printf ' 0x00000001 (NEEDED) Shared library: [libopencv_core.so.413]\n'
    elif [ "${MOCK_UCLIBC_NEEDED:-0}" = 1 ] && [[ "${file}" == */bin/frame_service ]]; then
        printf ' 0x00000001 (NEEDED) Shared library: [libc.so.0]\n'
    else
        printf ' 0x00000001 (NEEDED) Shared library: [libc.so.6]\n'
    fi
    ;;
--dyn-syms)
    symbols=(
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
    for symbol in "${symbols[@]}"; do
        if [ "${MOCK_MISSING_MPP_SYMBOL:-}" = "${symbol}" ]; then
            continue
        fi
        printf '     1: 00000000     0 FUNC    GLOBAL DEFAULT  UND %s\n' "${symbol}"
    done
    ;;
--syms)
    symbols=(
        __ctype_b
        __ctype_tolower
        aico_rknn_glibc_compat_init
        rknn_create_mem
        rknn_destroy
        rknn_destroy_mem
        rknn_init
        rknn_query
        rknn_run
        rknn_set_io_mem
    )
    for symbol in "${symbols[@]}"; do
        if [ "${MOCK_MISSING_RKNN_SYMBOL:-}" = "${symbol}" ]; then
            continue
        fi
        printf '     1: 00000000     0 FUNC    GLOBAL DEFAULT    1 %s\n' "${symbol}"
    done
    ;;
*)
    exit 2
    ;;
esac
EOF
chmod +x "${TEST_ROOT}/mock-readelf"

run_audit() {
    local report_dir=$1
    shift
    env READELF="${TEST_ROOT}/mock-readelf" "$@" \
        "${APPS_DIR}/audit-apps.sh" "${apps_dir}" "${report_dir}"
}

expect_audit_failure() {
    local name=$1
    shift
    if run_audit "${TEST_ROOT}/report-${name}" "$@" >/dev/null 2>&1; then
        fail "ELF audit accepted ${name}"
    fi
}

run_audit "${TEST_ROOT}/report-pass"
grep -qx 'status=pass' "${TEST_ROOT}/report-pass/summary.txt"
grep -qx 'elf_count=10' "${TEST_ROOT}/report-pass/summary.txt"

chmod -x "${apps_dir}/bin/mcu_service"
expect_audit_failure nonexecutable-mcu-service
chmod +x "${apps_dir}/bin/mcu_service"

expect_audit_failure bad-runpath MOCK_BAD_RUNPATH=1
expect_audit_failure opencv-needed MOCK_OPENCV_NEEDED=1
expect_audit_failure uclibc-needed MOCK_UCLIBC_NEEDED=1
expect_audit_failure missing-mpp-symbol MOCK_MISSING_MPP_SYMBOL=mpp_init
expect_audit_failure missing-rknn-symbol MOCK_MISSING_RKNN_SYMBOL=__ctype_tolower

ln -snf missing-rga.so "${apps_dir}/lib/librga.so.2"
expect_audit_failure dangling-rga
ln -snf librga.so.2.1.0 "${apps_dir}/lib/librga.so.2"

ln -snf librga.so.2.1.0 "${apps_dir}/lib/librga.so"
expect_audit_failure wrong-rga-chain

bundle_output=${TEST_ROOT}/bundle-output
bundle_apps=${bundle_output}/apps
bundle_audit=${bundle_output}/apps-audit
bundle_dir=${TEST_ROOT}/bundle
mkdir -p "${bundle_apps}/bin" "${bundle_apps}/lib" "${bundle_audit}"

bundle_payload=(
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
    bin/smart_turn_helper
    bin/ttyd
    bin/trigger
    lib/librga.so.2.1.0
)
printf 'kind\tsha256\tbytes\tmachine\thard_float\tinterpreter\trunpath\tneeded\tpath\n' \
    >"${bundle_audit}/elf-audit.tsv"
for rel in "${bundle_payload[@]}"; do
    mkdir -p "$(dirname "${bundle_apps}/${rel}")"
    printf 'test payload %s\n' "${rel}" >"${bundle_apps}/${rel}"
    hash=$(sha256sum "${bundle_apps}/${rel}" | awk '{print $1}')
    size=$(wc -c <"${bundle_apps}/${rel}")
    printf 'executable\t%s\t%s\tARM\tyes\t/lib/ld-linux-armhf.so.3\t$ORIGIN/../lib\tlibc.so.6\t%s\n' \
        "${hash}" "${size}" "${rel}" >>"${bundle_audit}/elf-audit.tsv"
done
chmod +x "${bundle_apps}/bin/"*
ln -s librga.so.2.1.0 "${bundle_apps}/lib/librga.so.2"
ln -s librga.so.2 "${bundle_apps}/lib/librga.so"
printf 'status=pass\nelf_count=20\n' >"${bundle_audit}/summary.txt"

help_bundle_dir=${TEST_ROOT}/bundle-help
DEBIAN_APPS_OUTPUT_DIR="${bundle_output}" \
DEBIAN_APPS_G0_OUTPUT_DIR="${help_bundle_dir}" \
    "${APPS_DIR}/prepare-board-g0.sh" --help >/dev/null
[ ! -e "${help_bundle_dir}" ] || fail "G0 bundle --help created its output directory"

DEBIAN_APPS_OUTPUT_DIR="${bundle_output}" \
DEBIAN_APPS_G0_OUTPUT_DIR="${bundle_dir}" \
    "${APPS_DIR}/prepare-board-g0.sh" bundle >/dev/null
[ -s "${bundle_dir}/debian-apps-g0.tar.gz" ] || fail "G0 bundle was not created"
DEBIAN_APPS_OUTPUT_DIR="${bundle_output}" \
DEBIAN_APPS_G0_OUTPUT_DIR="${bundle_dir}" \
    "${APPS_DIR}/prepare-board-g0.sh" verify >/dev/null

printf 'corrupted payload\n' >"${bundle_apps}/bin/hello"
if DEBIAN_APPS_OUTPUT_DIR="${bundle_output}" \
    DEBIAN_APPS_G0_OUTPUT_DIR="${bundle_dir}" \
        "${APPS_DIR}/prepare-board-g0.sh" bundle >/dev/null 2>&1; then
    fail "G0 bundle accepted an app that no longer matched the ELF audit"
fi

mkdir -p "${TEST_ROOT}/board-mock-bin"
: >"${TEST_ROOT}/board-command-log"
cat >"${TEST_ROOT}/board-mock-bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'ssh' >>"${BOARD_COMMAND_LOG}"
printf '\t%s' "$@" >>"${BOARD_COMMAND_LOG}"
printf '\n' >>"${BOARD_COMMAND_LOG}"
cat >/dev/null || true
EOF
cat >"${TEST_ROOT}/board-mock-bin/scp" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'scp' >>"${BOARD_COMMAND_LOG}"
printf '\t%s' "$@" >>"${BOARD_COMMAND_LOG}"
printf '\n' >>"${BOARD_COMMAND_LOG}"
EOF
chmod +x "${TEST_ROOT}/board-mock-bin/ssh" "${TEST_ROOT}/board-mock-bin/scp"

BOARD_COMMAND_LOG="${TEST_ROOT}/board-command-log" \
SSH_BIN="${TEST_ROOT}/board-mock-bin/ssh" \
SCP_BIN="${TEST_ROOT}/board-mock-bin/scp" \
    "${APPS_DIR}/run-board-g0.sh" preflight
grep -q '^ssh' "${TEST_ROOT}/board-command-log"

: >"${TEST_ROOT}/board-command-log"
if BOARD_COMMAND_LOG="${TEST_ROOT}/board-command-log" \
    SSH_BIN="${TEST_ROOT}/board-mock-bin/ssh" \
    SCP_BIN="${TEST_ROOT}/board-mock-bin/scp" \
    DEBIAN_APPS_OUTPUT_DIR="${bundle_output}" \
    DEBIAN_APPS_G0_OUTPUT_DIR="${bundle_dir}" \
        "${APPS_DIR}/run-board-g0.sh" deploy >/dev/null 2>&1; then
    fail "board deploy succeeded without the proprietary-transfer gate"
fi
[ ! -s "${TEST_ROOT}/board-command-log" ] ||
    fail "board command ran before the proprietary-transfer gate"

BOARD_COMMAND_LOG="${TEST_ROOT}/board-command-log" \
SSH_BIN="${TEST_ROOT}/board-mock-bin/ssh" \
SCP_BIN="${TEST_ROOT}/board-mock-bin/scp" \
DEBIAN_APPS_OUTPUT_DIR="${bundle_output}" \
DEBIAN_APPS_G0_OUTPUT_DIR="${bundle_dir}" \
AICO_G0_ALLOW_PROPRIETARY_TRANSFER=1 \
    "${APPS_DIR}/run-board-g0.sh" deploy >/dev/null
grep -q '^scp' "${TEST_ROOT}/board-command-log"
grep -q '^ssh' "${TEST_ROOT}/board-command-log"

: >"${TEST_ROOT}/board-command-log"
if BOARD_COMMAND_LOG="${TEST_ROOT}/board-command-log" \
    SSH_BIN="${TEST_ROOT}/board-mock-bin/ssh" \
        "${APPS_DIR}/run-board-g0.sh" load-modules >/dev/null 2>&1; then
    fail "module loading succeeded without its explicit gate"
fi
[ ! -s "${TEST_ROOT}/board-command-log" ] ||
    fail "board command ran before the module-loading gate"

: >"${TEST_ROOT}/board-command-log"
if BOARD_COMMAND_LOG="${TEST_ROOT}/board-command-log" \
    SSH_BIN="${TEST_ROOT}/board-mock-bin/ssh" \
        "${APPS_DIR}/run-board-g0.sh" audio-play >/dev/null 2>&1; then
    fail "audio playback succeeded without its explicit gate"
fi
[ ! -s "${TEST_ROOT}/board-command-log" ] ||
    fail "board command ran before the audio-playback gate"

: >"${TEST_ROOT}/board-command-log"
if BOARD_COMMAND_LOG="${TEST_ROOT}/board-command-log" \
    SSH_BIN="${TEST_ROOT}/board-mock-bin/ssh" \
        "${APPS_DIR}/run-board-g0.sh" stress >/dev/null 2>&1; then
    fail "stress succeeded without its explicit gate"
fi
[ ! -s "${TEST_ROOT}/board-command-log" ] ||
    fail "board command ran before the stress gate"

BOARD_COMMAND_LOG="${TEST_ROOT}/board-command-log" \
SSH_BIN="${TEST_ROOT}/board-mock-bin/ssh" \
AICO_G0_ALLOW_STRESS=1 \
AICO_G0_STRESS_SECONDS=7200 \
    "${APPS_DIR}/run-board-g0.sh" stress >/dev/null
grep -q 'AICO_G0_STRESS_SECONDS=7200' "${TEST_ROOT}/board-command-log"
grep -q $'\tstress$' "${TEST_ROOT}/board-command-log"

remote_bundle=${TEST_ROOT}/remote-bundle
mkdir -p "${remote_bundle}/bin" "${remote_bundle}/lib" \
    "${remote_bundle}/model" "${remote_bundle}/dev/snd"
cp "${APPS_DIR}/board-g0-remote.sh" "${remote_bundle}/board-g0-remote.sh"
chmod +x "${remote_bundle}/board-g0-remote.sh"
touch "${remote_bundle}/dev/rknpu" "${remote_bundle}/dev/video0"
touch "${remote_bundle}/lib/librga.so.2.1.0"
ln -s librga.so.2.1.0 "${remote_bundle}/lib/librga.so.2"
ln -s librga.so.2 "${remote_bundle}/lib/librga.so"
touch \
    "${remote_bundle}/model/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn" \
    "${remote_bundle}/model/silero_vad_6_2_lstm_decoder_weights.bin"
cat >"${remote_bundle}/bin/example_camera_capture" <<'EOF'
#!/usr/bin/env bash
trap 'exit 0' INT TERM
while true; do sleep 0.1; done
EOF
cat >"${remote_bundle}/bin/audio_stream" <<'EOF'
#!/usr/bin/env bash
trap 'exit 0' INT TERM
while true; do sleep 0.1; done
EOF
cat >"${remote_bundle}/bin/rknn_vad" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
sleep 0.05
printf 'mock_rknn_pass\n'
EOF
chmod +x "${remote_bundle}/bin/"*
(
    cd "${remote_bundle}"
    find . -type f ! -name MANIFEST.sha256 -print0 |
        LC_ALL=C sort -z |
        xargs -0 sha256sum >MANIFEST.sha256
)
stress_log=${TEST_ROOT}/stress-run.log
if ! AICO_G0_DEVICE_ROOT="${remote_bundle}/dev" \
    AICO_G0_RESULTS_DIR="${remote_bundle}/results" \
    AICO_G0_STRESS_SECONDS=2 \
    AICO_G0_STRESS_RKNN_FRAMES=1 \
    AICO_G0_STRESS_SAMPLE_SECONDS=1 \
        "${remote_bundle}/board-g0-remote.sh" stress >"${stress_log}" 2>&1; then
    # board-g0-remote.sh routes its own stdout and stderr through tee, so its
    # failure message arrives here rather than on this script's stderr.
    # Without printing it, a failure is just an exit status and nothing else.
    cat "${stress_log}" >&2
    fail "mock board stress run failed"
fi
stress_status=$(find "${remote_bundle}/results" -name stress.status -type f -print -quit)
[ -n "${stress_status}" ] || fail "mock board stress produced no status record"
stress_report=$(tr '\n' ' ' <"${stress_status}")
grep -Eq '^camera_exit_status=(0|124|130)$' "${stress_status}" ||
    fail "unexpected mock stress camera exit status: ${stress_report}"
grep -Eq '^audio_exit_status=(0|124|130)$' "${stress_status}" ||
    fail "unexpected mock stress audio exit status: ${stress_report}"
grep -qx 'rknn_exit_status=0' "${stress_status}" ||
    fail "unexpected mock stress rknn exit status: ${stress_report}"
stress_errors=$(dirname "${stress_status}")/stress-errors.txt
[ ! -e "${stress_errors}" ] ||
    fail "mock board stress reported an unexpected failure: $(tr '\n' ' ' <"${stress_errors}")"
