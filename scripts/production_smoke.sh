#!/usr/bin/env bash
set -euo pipefail

# This script is intended to run inside docker/test, not on the host. It checks
# the production ARM build against the same CMake/toolchain inputs used by the
# Debian application image, then cross-builds every shipped Go binary.
readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly OUTPUT_ROOT="${REPO_ROOT}/${AICO_PRODUCTION_SMOKE_OUTPUT:-output/production-smoke}"
readonly BUILD_DIR="${OUTPUT_ROOT}/cmake"
readonly BIN_DIR="${OUTPUT_ROOT}/bin"
readonly APPS_OUTPUT="${REPO_ROOT}/output/debian-apps"
readonly OPENCV_DIR="${APPS_OUTPUT}/opencv-mobile/lib/cmake/opencv4"
readonly OPENCV_ARCHIVE="${APPS_OUTPUT}/cache/opencv-mobile-4.13.0.zip"
readonly OPENCV_URL="https://github.com/nihui/opencv-mobile/releases/download/v35/opencv-mobile-4.13.0.zip"
readonly OPENCV_SHA256=9304482980b3e4ff1050a8527cdb5777fadf8c5dd9c1a8620170d23e252fb150
readonly SDK_DIR="${REPO_ROOT}/pico-sdk"
readonly RKNN_ARCHIVE="${REPO_ROOT}/third_party/rknpu2/v2.3.2/lib/librknnmrt.a"
readonly TOOLCHAIN="${REPO_ROOT}/cmake/toolchains/armhf-debian.cmake"

fail() {
    echo "production smoke failure: $*" >&2
    exit 1
}

require_path() {
    [ -e "$1" ] || fail "required production input is missing: $1"
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command is missing: $1"
}

for command in cmake ninja go readelf curl sha256sum arm-linux-gnueabihf-gcc arm-linux-gnueabihf-g++; do
    require_command "$command"
done
require_path "$SDK_DIR/project/build.sh"
require_path "$RKNN_ARCHIVE"
require_path "$TOOLCHAIN"

rm -rf "$OUTPUT_ROOT"
mkdir -p "$BIN_DIR" "${APPS_OUTPUT}/cache"

# The test image owns the cross compiler and can prepare the pinned OpenCV
# package itself. This keeps the smoke test independent of host caches while
# retaining the exact archive checksum used by the Debian apps build.
if [ ! -f "$OPENCV_DIR/OpenCVConfig.cmake" ]; then
    if [ ! -f "$OPENCV_ARCHIVE" ]; then
        curl -fsSL --retry 3 --connect-timeout 20 -o "$OPENCV_ARCHIVE.tmp" "$OPENCV_URL"
        echo "${OPENCV_SHA256}  $OPENCV_ARCHIVE.tmp" | sha256sum -c -
        mv "$OPENCV_ARCHIVE.tmp" "$OPENCV_ARCHIVE"
    fi
    echo "${OPENCV_SHA256}  $OPENCV_ARCHIVE" | sha256sum -c -
    AICO_REPO_ROOT="$REPO_ROOT" \
        DEBIAN_APPS_OUTPUT_DIR="$APPS_OUTPUT" \
        RK_JOBS="${AICO_TEST_JOBS:-2}" \
        bash "$REPO_ROOT/scripts/debian-apps/container-build-opencv-mobile.sh"
fi
require_path "$OPENCV_DIR/OpenCVConfig.cmake"

cmake -S "$REPO_ROOT" -B "$BUILD_DIR" -G Ninja \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_TOOLCHAIN_FILE="$TOOLCHAIN" \
    -DAICO_TARGET_PLATFORM=rv1106-debian-glibc \
    -DAICO_DEBIAN_OPENCV_DIR="$OPENCV_DIR" \
    -DAICO_ENABLE_LINK_MAPS=ON
cmake --build "$BUILD_DIR" --parallel "${AICO_TEST_JOBS:-2}"

# The production CMake build must emit the complete executable set. Checking
# names here catches a target accidentally removed from the packaging graph.
expected_cpp=(
    hello trigger image_process example_wakeup example_audio_capture
    example_audio_play example_camera_capture example_usb_hid frame_service
    frame_service_cli audio_service audio_service_cli audio_stream rknn_vad
    cpu_vad aico-environment
)
for executable in "${expected_cpp[@]}"; do
    binary="$BUILD_DIR/bin/$executable"
    require_path "$binary"
    header=$(readelf -hW "$binary")
    grep -q 'Machine:.*ARM' <<<"$header" || fail "$executable is not an ARM binary"
    grep -q 'Version5 EABI' <<<"$header" || fail "$executable is not ARM EABI5"
    install -m 0755 "$binary" "$BIN_DIR/$executable"
done

frame_dynamic=$(readelf -dW "$BUILD_DIR/bin/frame_service")
grep -q 'RUNPATH.*\$ORIGIN/../lib' <<<"$frame_dynamic" \
    || fail 'frame_service has no expected relative RUNPATH'

pushd "$REPO_ROOT/src/agent" >/dev/null
readonly agent_bins=(agent ble_service ota abctl)
for executable in "${agent_bins[@]}"; do
    command_name=$executable
    if [ "$executable" = agent ]; then
        command_name=daemon
    fi
    CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
        go build -mod=readonly -trimpath -buildvcs=false \
        -o "$BIN_DIR/$executable" "./cmd/$command_name"
    header=$(readelf -hW "$BIN_DIR/$executable")
    grep -q 'Machine:.*ARM' <<<"$header" || fail "$executable is not an ARM binary"
    grep -q 'Version5 EABI' <<<"$header" || fail "$executable is not ARM EABI5"
done
popd >/dev/null

printf 'production ARM smoke passed: %s C++ binaries and %s Go binaries\n' \
    "${#expected_cpp[@]}" "${#agent_bins[@]}"
