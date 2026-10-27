#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT=${AICO_REPO_ROOT:-${REPO_ROOT:-/work}}
readonly OUTPUT_DIR=${DEBIAN_APPS_OUTPUT_DIR:-/out}
readonly SOURCE_ARCHIVE=${OUTPUT_DIR}/cache/opencv-mobile-4.13.0.zip
readonly SOURCE_DIR=${OUTPUT_DIR}/opencv-mobile-source
readonly BUILD_DIR=${OUTPUT_DIR}/opencv-mobile-build
readonly INSTALL_DIR=${OUTPUT_DIR}/opencv-mobile
readonly PATCH_FILE=${REPO_ROOT}/scripts/debian-apps/opencv-mobile-rk-mpp-main-program.patch
readonly JOBS=${RK_JOBS:-$(getconf _NPROCESSORS_ONLN)}
readonly OPENCV_SOURCE_DATE_EPOCH=1767360516

if [ ! -f "${SOURCE_ARCHIVE}" ]; then
    echo "Missing OpenCV-Mobile source archive: ${SOURCE_ARCHIVE}" >&2
    exit 1
fi

rm -rf "${SOURCE_DIR}" "${BUILD_DIR}" "${INSTALL_DIR}"
mkdir -p "${SOURCE_DIR}" "${BUILD_DIR}" "${INSTALL_DIR}"

cd "${SOURCE_DIR}"
cmake -E tar xf "${SOURCE_ARCHIVE}"
readonly OPENCV_SOURCE_DIR=${SOURCE_DIR}/opencv-mobile-4.13.0
patch -d "${OPENCV_SOURCE_DIR}" -p1 <"${PATCH_FILE}"

mapfile -t opencv_options < <(sed -n '/^-/p' "${OPENCV_SOURCE_DIR}/options.txt")
export SOURCE_DATE_EPOCH=${OPENCV_SOURCE_DATE_EPOCH}
cmake -S "${OPENCV_SOURCE_DIR}" -B "${BUILD_DIR}" -G Ninja \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_TOOLCHAIN_FILE="${REPO_ROOT}/cmake/toolchains/armhf-debian.cmake" \
    -DCMAKE_INSTALL_PREFIX="${INSTALL_DIR}" \
    "${opencv_options[@]}" \
    -DBUILD_LIST=core,imgproc,highgui \
    -DOPENCV_DISABLE_THREAD_SUPPORT=ON \
    -DWITH_LAPACK=OFF \
    -DWITH_OPENMP=OFF \
    -DWITH_RK=ON
cmake --build "${BUILD_DIR}" --parallel "${JOBS}"
cmake --install "${BUILD_DIR}"

install -D -m 0644 "${OPENCV_SOURCE_DIR}/LICENSE" \
    "${INSTALL_DIR}/share/licenses/opencv4/LICENSE"
sha256sum "${INSTALL_DIR}"/lib/libopencv_*.a \
    >"${INSTALL_DIR}/opencv-static-libs.sha256"
sha256sum "${SOURCE_ARCHIVE}" >"${INSTALL_DIR}/source-archive.sha256"
printf '%s\n' \
    'release=v35' \
    'opencv_version=4.13.0' \
    'commit=99e69050a773cbcd8b01c1b410ba26df0439036f' \
    'commit_date=2026-01-02T13:28:36Z' \
    "source_date_epoch=${OPENCV_SOURCE_DATE_EPOCH}" \
    >"${INSTALL_DIR}/source.txt"
cp "${BUILD_DIR}/CMakeCache.txt" "${INSTALL_DIR}/CMakeCache.txt"
