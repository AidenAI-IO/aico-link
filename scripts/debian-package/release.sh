#!/usr/bin/env bash
set -euo pipefail

readonly PACKAGE_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PACKAGE_REPO_ROOT="$(cd "${PACKAGE_SCRIPT_DIR}/../.." && pwd)"
source "${PACKAGE_SCRIPT_DIR}/version.sh"
export DEBIAN_APPS_OUTPUT_DIR=${DEBIAN_APPS_OUTPUT_DIR:-${PACKAGE_REPO_ROOT}/output/debian-apps}
export DEBIAN_PACKAGE_OUTPUT_DIR=${DEBIAN_PACKAGE_OUTPUT_DIR:-${PACKAGE_REPO_ROOT}/output/debian-package}
[[ "${DEBIAN_APPS_OUTPUT_DIR}" = /* ]] || DEBIAN_APPS_OUTPUT_DIR=${PACKAGE_REPO_ROOT}/${DEBIAN_APPS_OUTPUT_DIR}
[[ "${DEBIAN_PACKAGE_OUTPUT_DIR}" = /* ]] || DEBIAN_PACKAGE_OUTPUT_DIR=${PACKAGE_REPO_ROOT}/${DEBIAN_PACKAGE_OUTPUT_DIR}

usage() {
    cat <<'EOF'
Usage: scripts/debian-package/release.sh [build|stage]

build    Build/audit applications, package them and stage release assets (Linux amd64 + Docker).
stage    Package already audited apps from the current clean commit and stage release assets.

Defaults: scripts/debian-package/version.sh (0.0.1-2).
Overrides: AICO_BUSINESS_VERSION, AICO_BUSINESS_REVISION,
DEBIAN_APPS_OUTPUT_DIR, DEBIAN_PACKAGE_OUTPUT_DIR, DEBIAN_PACKAGE_BUILD_IMAGE.
Build uses the same Go/OpenCV cache variables as debian_build.sh.
Channel publication: scripts/release/release.py (or the Aico Link Channel Release workflow).
No BSP, rootfs, firmware OTA signing key or Agent credentials are needed.
EOF
}

stage_package() {
    python3 "${PACKAGE_SCRIPT_DIR}/release.py" check-source "${PACKAGE_REPO_ROOT}" "${DEBIAN_APPS_OUTPUT_DIR}"
    export DEBIAN_PACKAGE_BUILD_IMAGE=${DEBIAN_PACKAGE_BUILD_IMAGE:-aico-debian13-armhf-builder:package}
    docker build -t "${DEBIAN_PACKAGE_BUILD_IMAGE}" -f "${PACKAGE_SCRIPT_DIR}/Dockerfile" "${PACKAGE_REPO_ROOT}"
    "${PACKAGE_SCRIPT_DIR}/build.sh"
    python3 "${PACKAGE_SCRIPT_DIR}/release.py" stage "${PACKAGE_REPO_ROOT}" "${DEBIAN_APPS_OUTPUT_DIR}" "${DEBIAN_PACKAGE_OUTPUT_DIR}"
}

build_package() {
    [ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] || {
        echo 'Run this build on Linux amd64 (for example ssh luhaodev)' >&2
        exit 1
    }
    # Reuse the pinned toolchain installer without invoking the firmware pipeline.
    bash -c 'source "$1/debian_build.sh"; require_command docker; ensure_go_toolchain "${DEBIAN_APPS_GO_ROOT:-$DEFAULT_GO_ROOT}"; ensure_pico_sdk' bash "${PACKAGE_REPO_ROOT}"
    for action in builder opencv apps audit; do
        "${PACKAGE_REPO_ROOT}/scripts/debian-apps/build-apps.sh" "${action}"
    done
    stage_package
}


case "${1:-build}" in
    build) build_package ;;
    stage) stage_package ;;
    publish) echo 'Use scripts/release/release.py publish for channel releases' >&2; exit 2 ;;
    -h|--help|help) usage ;;
    *) usage >&2; exit 2 ;;
esac
