#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly RESULTS_ROOT=${AICO_G0_RESULTS_DIR:-${SCRIPT_DIR}/results}
readonly RKNN_FRAMES=${AICO_G0_RKNN_FRAMES:-1000}
readonly AUDIO_SECONDS=${AICO_G0_AUDIO_SECONDS:-10}
readonly CAMERA_FRAMES=${AICO_G0_CAMERA_FRAMES:-10}
readonly STRESS_SECONDS=${AICO_G0_STRESS_SECONDS:-7200}
readonly STRESS_RKNN_FRAMES=${AICO_G0_STRESS_RKNN_FRAMES:-1000}
readonly STRESS_SAMPLE_SECONDS=${AICO_G0_STRESS_SAMPLE_SECONDS:-2}
readonly CMA_RECOVERY_TOLERANCE_KB=${AICO_G0_CMA_RECOVERY_TOLERANCE_KB:-4096}
readonly DEVICE_ROOT=${AICO_G0_DEVICE_ROOT:-/dev}
readonly MODEL=${SCRIPT_DIR}/model/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn
readonly WEIGHTS=${SCRIPT_DIR}/model/silero_vad_6_2_lstm_decoder_weights.bin

usage() {
    cat <<'EOF'
Usage: board-g0-remote.sh ACTION

Actions:
  loader         Verify the bundle, run hello, and resolve every ELF loader dependency.
  module-state   Record loaded modules and relevant device nodes without changing them.
  load-modules   Load the system media/RGA/RKNN module sequence; must run as root.
  rknn           Run RKNN self-test and a profiled fixed-frame benchmark.
  camera         Capture a short profiled frame sequence without writing frame payloads.
  audio-capture  Capture a short profiled PCM sample into the result directory.
  audio-play     Play the most recent captured PCM sample.
  stress         Run RKNN, continuous camera, and audio capture concurrently.
  snapshot       Record a system, memory, CMA, DMA-BUF, process, and device snapshot.

Environment:
  AICO_G0_RKNN_FRAMES    RKNN benchmark frame count (default: 1000).
  AICO_G0_CAMERA_FRAMES  Camera frame count (default: 10).
  AICO_G0_AUDIO_SECONDS  Audio capture duration (default: 10).
  AICO_G0_STRESS_SECONDS Concurrent stress duration (default: 7200 / 2 hours).
  AICO_G0_STRESS_RKNN_FRAMES
                          RKNN frames per stress iteration (default: 1000).
  AICO_G0_STRESS_SAMPLE_SECONDS
                          Resource sample interval (default: 2).
  AICO_G0_CMA_RECOVERY_TOLERANCE_KB
                          Allowed post-stress CMA delta (default: 4096).
  AICO_G0_RESULTS_DIR    Result directory root (default: bundle/results).
EOF
}

fail() {
    echo "apps board G0 failure: $*" >&2
    exit 1
}

require_uint() {
    local name=$1
    local value=$2
    case "${value}" in
        '' | *[!0-9]*) fail "${name} must be an unsigned integer" ;;
    esac
    [ "${value}" -gt 0 ] || fail "${name} must be greater than zero"
}

verify_bundle() {
    [ -f "${SCRIPT_DIR}/MANIFEST.sha256" ] || fail "MANIFEST.sha256 is missing"
    (
        cd "${SCRIPT_DIR}"
        sha256sum -c MANIFEST.sha256
    )
    [ "$(readlink "${SCRIPT_DIR}/lib/librga.so")" = librga.so.2 ] ||
        fail "librga.so symlink is invalid"
    [ "$(readlink "${SCRIPT_DIR}/lib/librga.so.2")" = librga.so.2.1.0 ] ||
        fail "librga.so.2 symlink is invalid"
}

new_result_dir() {
    local action=$1
    local stamp
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    RESULT_DIR=${RESULTS_ROOT}/${stamp}-${action}-$$
    readonly RESULT_DIR
    mkdir -p "${RESULTS_ROOT}"
    if [ "$(id -u)" -eq 0 ] && [ -n "${SUDO_UID:-}" ] && [ -n "${SUDO_GID:-}" ]; then
        case "${SUDO_UID}" in *[!0-9]*) fail "sudo supplied unsafe result UID" ;; esac
        case "${SUDO_GID}" in *[!0-9]*) fail "sudo supplied unsafe result GID" ;; esac
        chown "${SUDO_UID}:${SUDO_GID}" "${RESULTS_ROOT}"
    fi
    mkdir "${RESULT_DIR}"
    printf 'result_dir=%s\n' "${RESULT_DIR}"
}

snapshot() {
    local label=$1
    local output=${RESULT_DIR}/${label}.txt
    {
        printf 'label=%s\n' "${label}"
        printf 'timestamp='; date -u +%Y-%m-%dT%H:%M:%SZ
        uname -a
        printf '\n[os-release]\n'
        cat /etc/os-release 2>/dev/null || true
        printf '\n[uptime]\n'
        cat /proc/uptime
        printf '\n[meminfo]\n'
        cat /proc/meminfo
        printf '\n[memory-pressure]\n'
        cat /proc/pressure/memory 2>/dev/null || true
        printf '\n[swap]\n'
        cat /proc/swaps 2>/dev/null || true
        printf '\n[modules]\n'
        lsmod 2>/dev/null || true
        printf '\n[devices]\n'
        find "${DEVICE_ROOT}" -maxdepth 2 \( -name 'rknpu*' -o -name 'video*' -o \
            -name 'media*' -o -name 'v4l-subdev*' -o -name 'snd' -o \
            -path "${DEVICE_ROOT}/mpi/*" -o -path "${DEVICE_ROOT}/dma_heap/*" -o \
            -path "${DEVICE_ROOT}/rk_dma_heap/*" \) \
            -printf '%p %m %u:%g -> %l\n' 2>/dev/null | LC_ALL=C sort
        printf '\n[processes]\n'
        ps -eo pid,ppid,user,stat,rss,vsz,nlwp,comm,args --sort=-rss 2>/dev/null || true
        printf '\n[dma-buf]\n'
        if [ -r /sys/kernel/debug/dma_buf/bufinfo ]; then
            cat /sys/kernel/debug/dma_buf/bufinfo
        else
            printf 'unavailable\n'
        fi
        printf '\n[rknpu-load]\n'
        cat /sys/kernel/debug/rknpu/load 2>/dev/null || true
    } >"${output}"
}

sample_process() {
    local pid=$1
    local label=$2
    local output=${RESULT_DIR}/${label}-samples.tsv
    if [ ! -f "${output}" ]; then
        printf 'epoch\tpid\tvmrss_kb\tvmhwm_kb\tpss_kb\tmemavailable_kb\tcmaallocated_kb\tcmareleased_kb\tcmafree_kb\tpsi_some\tpsi_full\n' \
            >"${output}"
    fi
    local vmrss=0 vmhwm=0 pss=0 memavailable=0
    local cmaallocated=0 cmareleased=0 cmafree=0 psi_some= psi_full=
    if [ -r "/proc/${pid}/status" ]; then
        vmrss=$(awk '/^VmRSS:/ {print $2}' "/proc/${pid}/status" 2>/dev/null || true)
        vmhwm=$(awk '/^VmHWM:/ {print $2}' "/proc/${pid}/status" 2>/dev/null || true)
    fi
    if [ -r "/proc/${pid}/smaps_rollup" ]; then
        pss=$(awk '/^Pss:/ {print $2}' "/proc/${pid}/smaps_rollup" 2>/dev/null || true)
    fi
    memavailable=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
    cmaallocated=$(awk '/^CmaAllocated:/ {print $2}' /proc/meminfo)
    cmareleased=$(awk '/^CmaReleased:/ {print $2}' /proc/meminfo)
    cmafree=$(awk '/^CmaFree:/ {print $2}' /proc/meminfo)
    psi_some=$(awk '/^some / {print $2}' /proc/pressure/memory 2>/dev/null || true)
    psi_full=$(awk '/^full / {print $2}' /proc/pressure/memory 2>/dev/null || true)
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$(date +%s.%N)" "${pid}" "${vmrss:-0}" "${vmhwm:-0}" "${pss:-0}" \
        "${memavailable:-0}" "${cmaallocated:-0}" "${cmareleased:-0}" \
        "${cmafree:-0}" "${psi_some}" "${psi_full}" >>"${output}"
}

profile_command() {
    local label=$1
    shift
    local pid status
    printf '\n[%s command]\n' "${label}"
    printf '%q ' "$@"
    printf '\n'
    "$@" >"${RESULT_DIR}/${label}.log" 2>&1 &
    pid=$!
    while kill -0 "${pid}" 2>/dev/null; do
        sample_process "${pid}" "${label}"
        sleep 0.2
    done
    if wait "${pid}"; then
        status=0
    else
        status=$?
    fi
    cat "${RESULT_DIR}/${label}.log"
    printf '%s_exit_status=%s\n' "${label}" "${status}" |
        tee "${RESULT_DIR}/${label}.status"
    return "${status}"
}

run_loader() {
    local file
    new_result_dir loader
    exec > >(tee "${RESULT_DIR}/run.log") 2>&1
    verify_bundle
    "${SCRIPT_DIR}/bin/hello"
    : >"${RESULT_DIR}/ldd.txt"
    for file in "${SCRIPT_DIR}"/bin/*; do
        printf '\n[%s]\n' "${file##*/}" | tee -a "${RESULT_DIR}/ldd.txt"
        ldd "${file}" 2>&1 | tee -a "${RESULT_DIR}/ldd.txt"
    done
    if grep -q 'not found' "${RESULT_DIR}/ldd.txt"; then
        fail "one or more loader dependencies were not resolved"
    fi
    snapshot after-loader
}

run_module_state() {
    new_result_dir module-state
    verify_bundle >"${RESULT_DIR}/bundle-verify.txt"
    snapshot module-state
}

run_load_modules() {
    [ "$(id -u)" -eq 0 ] || fail "load-modules must run as root"
    new_result_dir load-modules
    exec > >(tee "${RESULT_DIR}/run.log") 2>&1
    verify_bundle
    snapshot before-load
    "${SCRIPT_DIR}/aico-media-modules"
    snapshot after-load
    [ -e "${DEVICE_ROOT}/rknpu" ] || fail "${DEVICE_ROOT}/rknpu was not created"
}

run_rknn() {
    require_uint AICO_G0_RKNN_FRAMES "${RKNN_FRAMES}"
    [ -e "${DEVICE_ROOT}/rknpu" ] ||
        fail "${DEVICE_ROOT}/rknpu is missing; load media modules first"
    new_result_dir rknn
    exec > >(tee "${RESULT_DIR}/run.log") 2>&1
    verify_bundle
    snapshot before-rknn
    export LD_LIBRARY_PATH=${SCRIPT_DIR}/lib
    profile_command rknn-self-test \
        "${SCRIPT_DIR}/bin/rknn_vad" --model "${MODEL}" --weights "${WEIGHTS}" --self-test
    profile_command rknn-benchmark \
        "${SCRIPT_DIR}/bin/rknn_vad" --model "${MODEL}" --weights "${WEIGHTS}" \
        --benchmark-stages --benchmark-frames "${RKNN_FRAMES}"
    snapshot after-rknn
}

run_camera() {
    require_uint AICO_G0_CAMERA_FRAMES "${CAMERA_FRAMES}"
    new_result_dir camera
    exec > >(tee "${RESULT_DIR}/run.log") 2>&1
    verify_bundle
    snapshot before-camera
    export LD_LIBRARY_PATH=${SCRIPT_DIR}/lib
    profile_command camera \
        "${SCRIPT_DIR}/bin/example_camera_capture" --count "${CAMERA_FRAMES}" --no-output
    snapshot after-camera
}

run_audio_capture() {
    require_uint AICO_G0_AUDIO_SECONDS "${AUDIO_SECONDS}"
    new_result_dir audio-capture
    exec > >(tee "${RESULT_DIR}/run.log") 2>&1
    verify_bundle
    snapshot before-audio
    export LD_LIBRARY_PATH=${SCRIPT_DIR}/lib
    (
        cd "${RESULT_DIR}"
        if profile_command audio-capture timeout --signal=INT --kill-after=2s \
            "${AUDIO_SECONDS}" "${SCRIPT_DIR}/bin/example_audio_capture"; then
            status=0
        else
            status=$?
        fi
        case "${status}" in 0 | 124 | 130) ;; *) exit "${status}" ;; esac
    )
    [ -s "${RESULT_DIR}/audio_capture_debug.pcm" ] || fail "audio capture produced no PCM data"
    snapshot after-audio
}

run_audio_play() {
    local pcm
    pcm=$(find "${RESULTS_ROOT}" -mindepth 2 -maxdepth 2 -type f \
        -name audio_capture_debug.pcm -printf '%T@\t%p\n' 2>/dev/null |
        sort -nr | head -n 1 | cut -f 2-)
    [ -n "${pcm}" ] || fail "no captured PCM result is available"
    new_result_dir audio-play
    exec > >(tee "${RESULT_DIR}/run.log") 2>&1
    verify_bundle
    export LD_LIBRARY_PATH=${SCRIPT_DIR}/lib
    profile_command audio-play "${SCRIPT_DIR}/bin/example_audio_play" "${pcm}"
    snapshot after-audio-play
}

capture_dmesg() {
    local output=$1
    if dmesg >"${output}" 2>&1; then
        return 0
    fi
    printf 'dmesg_unavailable=yes\n' >>"${output}"
    return 1
}

run_stress() {
    local end_epoch camera_job audio_job rknn_job now early_failure=0
    local deadline_reached=0
    local camera_status audio_status rknn_status before_cma after_cma
    local before_dmesg_lines=0 kernel_log_available=0 bad_log=0
    local actual_pid label pattern
    require_uint AICO_G0_STRESS_SECONDS "${STRESS_SECONDS}"
    require_uint AICO_G0_STRESS_RKNN_FRAMES "${STRESS_RKNN_FRAMES}"
    require_uint AICO_G0_STRESS_SAMPLE_SECONDS "${STRESS_SAMPLE_SECONDS}"
    require_uint AICO_G0_CMA_RECOVERY_TOLERANCE_KB "${CMA_RECOVERY_TOLERANCE_KB}"
    [ -e "${DEVICE_ROOT}/rknpu" ] ||
        fail "${DEVICE_ROOT}/rknpu is missing; load media modules first"
    [ -e "${DEVICE_ROOT}/video0" ] ||
        fail "${DEVICE_ROOT}/video0 is missing; load media modules first"
    [ -d "${DEVICE_ROOT}/snd" ] || fail "${DEVICE_ROOT}/snd is missing"

    new_result_dir stress
    exec > >(tee "${RESULT_DIR}/run.log") 2>&1
    verify_bundle
    snapshot before-stress
    before_cma=$(awk '/^CmaAllocated:/ {print $2}' /proc/meminfo)
    if capture_dmesg "${RESULT_DIR}/dmesg-before.txt"; then
        kernel_log_available=1
        before_dmesg_lines=$(wc -l <"${RESULT_DIR}/dmesg-before.txt")
    fi

    export LD_LIBRARY_PATH=${SCRIPT_DIR}/lib
    end_epoch=$(( $(date +%s) + STRESS_SECONDS ))
    timeout --signal=INT --kill-after=5s "${STRESS_SECONDS}" \
        "${SCRIPT_DIR}/bin/example_camera_capture" --count 0 --no-output \
        >/dev/null 2>"${RESULT_DIR}/camera.stderr.log" &
    camera_job=$!
    timeout --signal=INT --kill-after=5s "${STRESS_SECONDS}" \
        "${SCRIPT_DIR}/bin/audio_stream" \
        >/dev/null 2>"${RESULT_DIR}/audio.stderr.log" &
    audio_job=$!
    (
        iteration=0
        while [ "$(date +%s)" -lt "${end_epoch}" ]; do
            iteration=$((iteration + 1))
            printf '\n[iteration=%s timestamp=%s]\n' \
                "${iteration}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
            "${SCRIPT_DIR}/bin/rknn_vad" --model "${MODEL}" --weights "${WEIGHTS}" \
                --benchmark-stages --benchmark-frames "${STRESS_RKNN_FRAMES}"
        done
        printf 'iterations=%s\n' "${iteration}"
    ) >"${RESULT_DIR}/rknn.log" 2>&1 &
    rknn_job=$!
    printf 'camera_job=%s\naudio_job=%s\nrknn_job=%s\n' \
        "${camera_job}" "${audio_job}" "${rknn_job}"

    while [ "$(date +%s)" -lt "${end_epoch}" ]; do
        for label in camera audio rknn; do
            case "${label}" in
                camera) actual_pid=${camera_job}; pattern='example_camera_capture' ;;
                audio) actual_pid=${audio_job}; pattern='/bin/audio_stream' ;;
                rknn) actual_pid=${rknn_job}; pattern='/bin/rknn_vad' ;;
            esac
            case "${label}" in
                camera) kill -0 "${camera_job}" 2>/dev/null || early_failure=1 ;;
                audio) kill -0 "${audio_job}" 2>/dev/null || early_failure=1 ;;
                rknn) kill -0 "${rknn_job}" 2>/dev/null || early_failure=1 ;;
            esac
            if [ "${early_failure}" -ne 0 ]; then
                # The workers stop on the same deadline this loop watches, and
                # both read it with whole-second granularity, so a worker can
                # finish between this iteration's clock check and the liveness
                # check just above. That window is as wide as the sampling this
                # loop performs, so it opens up exactly when the host is loaded.
                # Once the deadline has passed, an exited worker has finished
                # normally and is not an early exit.
                if [ "$(date +%s)" -ge "${end_epoch}" ]; then
                    early_failure=0
                    deadline_reached=1
                    break
                fi
                printf 'early_process_exit=%s timestamp=%s\n' \
                    "${label}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" |
                    tee -a "${RESULT_DIR}/stress-errors.txt"
                break
            fi
            actual_pid=$(pgrep -n -f "${pattern}" 2>/dev/null || printf '%s' "${actual_pid}")
            sample_process "${actual_pid}" "stress-${label}"
        done
        [ "${early_failure}" -eq 0 ] || break
        [ "${deadline_reached}" -eq 0 ] || break
        sleep "${STRESS_SAMPLE_SECONDS}"
    done

    now=$(date +%s)
    if [ "${early_failure}" -ne 0 ] || [ "${now}" -lt "${end_epoch}" ]; then
        pkill -TERM -P "${camera_job}" 2>/dev/null || true
        pkill -TERM -P "${audio_job}" 2>/dev/null || true
        pkill -TERM -P "${rknn_job}" 2>/dev/null || true
        kill -TERM "${camera_job}" "${audio_job}" "${rknn_job}" 2>/dev/null || true
    fi
    if wait "${camera_job}"; then camera_status=0; else camera_status=$?; fi
    if wait "${audio_job}"; then audio_status=0; else audio_status=$?; fi
    if wait "${rknn_job}"; then rknn_status=0; else rknn_status=$?; fi
    printf 'camera_exit_status=%s\naudio_exit_status=%s\nrknn_exit_status=%s\n' \
        "${camera_status}" "${audio_status}" "${rknn_status}" |
        tee "${RESULT_DIR}/stress.status"

    snapshot after-stress
    sleep 5
    snapshot after-recovery
    after_cma=$(awk '/^CmaAllocated:/ {print $2}' /proc/meminfo)
    printf 'cma_allocated_before_kb=%s\ncma_allocated_after_recovery_kb=%s\n' \
        "${before_cma:-0}" "${after_cma:-0}" >"${RESULT_DIR}/recovery.txt"

    if [ "${kernel_log_available}" -eq 1 ] &&
        capture_dmesg "${RESULT_DIR}/dmesg-after.txt"; then
        sed -n "$((before_dmesg_lines + 1)),\$p" "${RESULT_DIR}/dmesg-after.txt" \
            >"${RESULT_DIR}/dmesg-new.txt"
    else
        : >"${RESULT_DIR}/dmesg-new.txt"
    fi

    cat "${RESULT_DIR}/dmesg-new.txt" "${RESULT_DIR}/camera.stderr.log" \
        "${RESULT_DIR}/audio.stderr.log" "${RESULT_DIR}/rknn.log" |
        grep -Eai 'rknpu.*(timeout|error|fail)|npu.*(timeout|error|fail)|out of memory|oom-killer|segmentation fault|kernel panic|BUG:|Oops:|Call Trace:|allocation failed' \
        >"${RESULT_DIR}/error-matches.txt" || true
    [ ! -s "${RESULT_DIR}/error-matches.txt" ] || bad_log=1

    case "${camera_status}" in 0 | 124 | 130) ;; *) early_failure=1 ;; esac
    case "${audio_status}" in 0 | 124 | 130) ;; *) early_failure=1 ;; esac
    [ "${rknn_status}" -eq 0 ] || early_failure=1
    if [ "${after_cma:-0}" -gt $(( ${before_cma:-0} + CMA_RECOVERY_TOLERANCE_KB )) ]; then
        printf 'CMA allocation did not recover within %s kB tolerance\n' \
            "${CMA_RECOVERY_TOLERANCE_KB}" | tee -a "${RESULT_DIR}/stress-errors.txt"
        early_failure=1
    fi
    if [ "${bad_log}" -ne 0 ]; then
        printf 'kernel or application error patterns were detected\n' |
            tee -a "${RESULT_DIR}/stress-errors.txt"
        early_failure=1
    fi
    [ "${early_failure}" -eq 0 ] || fail "concurrent stress gate failed"
}

run_snapshot() {
    new_result_dir snapshot
    verify_bundle >"${RESULT_DIR}/bundle-verify.txt"
    snapshot snapshot
}

main() {
    local action=${1:-help}
    case "${action}" in
        -h | --help | help) usage ;;
        loader) run_loader ;;
        module-state) run_module_state ;;
        load-modules) run_load_modules ;;
        rknn) run_rknn ;;
        camera) run_camera ;;
        audio-capture) run_audio_capture ;;
        audio-play) run_audio_play ;;
        stress) run_stress ;;
        snapshot) run_snapshot ;;
        *)
            usage >&2
            exit 2
            ;;
    esac
}

main "$@"
