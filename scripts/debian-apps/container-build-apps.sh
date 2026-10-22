#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT=/work
readonly OUTPUT_DIR=${DEBIAN_APPS_OUTPUT_DIR:-/out}
readonly OPENCV_DIR=${OUTPUT_DIR}/opencv-mobile/lib/cmake/opencv4
readonly BUILD_DIR=${OUTPUT_DIR}/apps-build
readonly DIST_DIR=${OUTPUT_DIR}/apps
readonly JOBS=${RK_JOBS:-$(getconf _NPROCESSORS_ONLN)}
readonly VENDOR_LIB_DIR=${REPO_ROOT}/pico-sdk/project/cfg/BoardConfig_IPC/overlay/overlay-luckfox-glibc-rockchip/usr/lib
readonly RKNN_ROOT=${REPO_ROOT}/third_party/rknpu2/v2.3.2
readonly RKNN_MICRO_ARCHIVE=${RKNN_ROOT}/lib/librknnmrt.a
readonly RKNN_MICRO_ARCHIVE_SHA256=2cc37ceb72648411970b74d70918d09a337c8463ff8ecbc627691c974c9d9362
readonly GO_VERSION=go1.26.0
readonly SOURCE_EPOCH=${SOURCE_DATE_EPOCH:-1767360516}
readonly TTYD_VERSION=1.7.3
readonly TTYD_ASSET=ttyd.armhf
readonly TTYD_SHA256=b0784080bd78f0a5916462672f461542c607f8ea7cee56b075e8cd04e1ffcc4d
readonly TTYD_URL=https://github.com/tsl0922/ttyd/releases/download/${TTYD_VERSION}/${TTYD_ASSET}

if [ ! -f "${OPENCV_DIR}/OpenCVConfig.cmake" ]; then
    echo "Missing Debian OpenCV-Mobile build: ${OPENCV_DIR}" >&2
    exit 1
fi

[ -f "${RKNN_MICRO_ARCHIVE}" ] || {
    echo "Missing Debian RKNN mini-runtime archive: ${RKNN_MICRO_ARCHIVE}" >&2
    exit 1
}
[ "$(stat -c %s "${RKNN_MICRO_ARCHIVE}")" = 315362 ] || {
    echo "Unexpected RKNN mini-runtime archive size" >&2
    exit 1
}
[ "$(sha256sum "${RKNN_MICRO_ARCHIVE}" | awk '{print $1}')" = "${RKNN_MICRO_ARCHIVE_SHA256}" ] || {
    echo "RKNN mini-runtime archive checksum mismatch" >&2
    exit 1
}
grep -aF \
    'librknnmrt version: 2.3.2 (429f97ae6b@2025-04-09T09:11:49)' \
    "${RKNN_MICRO_ARCHIVE}" >/dev/null || {
    echo "Unexpected RKNN mini-runtime archive version" >&2
    exit 1
}

rm -rf "${BUILD_DIR}" "${DIST_DIR}"
mkdir -p "${BUILD_DIR}" "${DIST_DIR}/bin" "${DIST_DIR}/lib" \
    "${DIST_DIR}/maps" "${DIST_DIR}/metadata"

ttyd_download=${BUILD_DIR}/${TTYD_ASSET}
ttyd_cache=/work/.cache/debian-apps/${TTYD_ASSET}
if [ -f "${ttyd_cache}" ] && echo "${TTYD_SHA256}  ${ttyd_cache}" | sha256sum -c - >/dev/null 2>&1; then
    cp -f "${ttyd_cache}" "${ttyd_download}"
else
    curl -fL --retry 3 --connect-timeout 20 -o "${ttyd_download}" "${TTYD_URL}"
fi
echo "${TTYD_SHA256}  ${ttyd_download}" | sha256sum -c -
install -m 0755 "${ttyd_download}" "${DIST_DIR}/bin/ttyd"
printf 'version=%s\nasset=%s\nsha256=%s\nurl=%s\n' \
    "${TTYD_VERSION}" "${TTYD_ASSET}" "${TTYD_SHA256}" "${TTYD_URL}" \
    >"${DIST_DIR}/metadata/ttyd.txt"

cmake -S "${REPO_ROOT}" -B "${BUILD_DIR}" -G Ninja \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_TOOLCHAIN_FILE="${REPO_ROOT}/cmake/toolchains/armhf-debian.cmake" \
    -DAIDEN_TARGET_PLATFORM=rv1106-debian-glibc \
    -DAIDEN_DEBIAN_OPENCV_DIR="${OPENCV_DIR}" \
    -DAIDEN_ENABLE_LINK_MAPS=ON
cmake --build "${BUILD_DIR}" --parallel "${JOBS}" --verbose \
    2>&1 | tee "${DIST_DIR}/metadata/build.log"

find "${BUILD_DIR}/bin" -maxdepth 1 -type f -print0 \
    | while IFS= read -r -d '' binary; do
        install -m 0755 "${binary}" "${DIST_DIR}/bin/$(basename "${binary}")"
    done
cp "${BUILD_DIR}"/maps/*.map "${DIST_DIR}/maps/"

if [ "$(/usr/local/go/bin/go env GOVERSION)" != "${GO_VERSION}" ]; then
    echo "Go ${GO_VERSION} is required; found $(/usr/local/go/bin/go env GOVERSION)" >&2
    exit 1
fi
case "${SOURCE_EPOCH}" in
'' | *[!0-9]*)
    echo "SOURCE_DATE_EPOCH must be an unsigned Unix timestamp: ${SOURCE_EPOCH}" >&2
    exit 1
    ;;
esac
export PATH=/usr/local/go/bin:${PATH}
export GOTOOLCHAIN=local
export GOCACHE=/go-build-cache
export GOMODCACHE=/go-mod-cache
export GOPATH=/tmp/aiden-go-path
readonly AGENT_COMMIT=$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD)
readonly AGENT_BUILD_TIME=$(date -u -d "@${SOURCE_EPOCH}" +%Y%m%d-%H%M%S)
readonly AGENT_BUILD_VERSION=${AGENT_BUILD_TIME}-${AGENT_COMMIT}
readonly AGENT_LDFLAGS="-s -w -buildid= -X aiden-agent/internal/agent.buildCommit=${AGENT_COMMIT} -X aiden-agent/internal/agent.buildVersion=${AGENT_BUILD_VERSION}"

pushd "${REPO_ROOT}/src/agent" >/dev/null
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags "${AGENT_LDFLAGS}" \
    -o "${DIST_DIR}/bin/agent" ./cmd/daemon
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags '-s -w -buildid=' \
    -o "${DIST_DIR}/bin/ble_service" ./cmd/ble_service
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags '-s -w -buildid=' \
    -o "${DIST_DIR}/bin/mcu_service" ./cmd/mcu_service
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags "${AGENT_LDFLAGS}" \
    -o "${DIST_DIR}/bin/ota" ./cmd/ota
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags "${AGENT_LDFLAGS}" \
    -o "${DIST_DIR}/bin/abctl" ./cmd/abctl
go version >"${DIST_DIR}/metadata/go-version.txt"
go list -mod=readonly -m all >"${DIST_DIR}/metadata/go-modules.txt"
popd >/dev/null

install -m 0644 "${VENDOR_LIB_DIR}/librga.so.2.1.0" \
    "${DIST_DIR}/lib/librga.so.2.1.0"
ln -s librga.so.2.1.0 "${DIST_DIR}/lib/librga.so.2"
ln -s librga.so.2 "${DIST_DIR}/lib/librga.so"

arm-linux-gnueabihf-gcc --version >"${DIST_DIR}/metadata/compiler.txt"
cmake --version >"${DIST_DIR}/metadata/cmake.txt"
printf '%s\n' "${DEBIAN_APPS_BUILD_IMAGE_ID:-unknown}" \
    >"${DIST_DIR}/metadata/builder-image-id.txt"
cp /etc/apt/sources.list.d/debian.sources \
    "${DIST_DIR}/metadata/builder-debian.sources"
dpkg-query -W -f='${binary:Package}\t${Version}\n' \
    | LC_ALL=C sort >"${DIST_DIR}/metadata/builder-packages.txt"
printf '%s\n' "$(git -C "${REPO_ROOT}" rev-parse HEAD)" \
    >"${DIST_DIR}/metadata/hardware-demo-commit.txt"
git -C "${REPO_ROOT}" status --short \
    >"${DIST_DIR}/metadata/hardware-demo-status.txt"
# The SDK is part of the firmware commit; retain the existing metadata names.
printf '%s\n' "$(git -C "${REPO_ROOT}" rev-parse HEAD)" \
    >"${DIST_DIR}/metadata/pico-sdk-commit.txt"
git -C "${REPO_ROOT}" status --short -- pico-sdk \
    >"${DIST_DIR}/metadata/pico-sdk-status.txt"
printf 'source_date_epoch=%s\nagent_build_version=%s\n' \
    "${SOURCE_EPOCH}" "${AGENT_BUILD_VERSION}" \
    >"${DIST_DIR}/metadata/go-build.txt"
cp "${BUILD_DIR}/CMakeCache.txt" "${DIST_DIR}/metadata/CMakeCache.txt"
sha256sum "${VENDOR_LIB_DIR}/librockit.a" \
    "${VENDOR_LIB_DIR}/librockchip_mpp.a" \
    "${VENDOR_LIB_DIR}/librga.so.2.1.0" \
    "${RKNN_MICRO_ARCHIVE}" \
    "${OUTPUT_DIR}"/opencv-mobile/lib/libopencv_*.a \
    >"${DIST_DIR}/metadata/dependency-sha256.txt"
