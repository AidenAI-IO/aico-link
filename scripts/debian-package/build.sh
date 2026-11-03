#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/version.sh"
APPS_OUTPUT=${DEBIAN_APPS_OUTPUT_DIR:-${REPO_ROOT}/output/debian-apps}
OUTPUT_DIR=${DEBIAN_PACKAGE_OUTPUT_DIR:-${REPO_ROOT}/output/debian-package}
# Docker bind mounts require absolute host paths, including custom output dirs.
[[ "${APPS_OUTPUT}" = /* ]] || APPS_OUTPUT=${REPO_ROOT}/${APPS_OUTPUT}
[[ "${OUTPUT_DIR}" = /* ]] || OUTPUT_DIR=${REPO_ROOT}/${OUTPUT_DIR}
readonly APPS_OUTPUT OUTPUT_DIR
readonly IMAGE="${DEBIAN_PACKAGE_BUILD_IMAGE:-aico-debian13-armhf-builder:system}"

test -d "${APPS_OUTPUT}/apps" || {
    echo "Missing applications: ${APPS_OUTPUT}/apps" >&2
    exit 1
}
grep -qx 'status=pass' "${APPS_OUTPUT}/apps-audit/summary.txt" || {
    echo "Application audit has not passed" >&2
    exit 1
}
mkdir -p "${OUTPUT_DIR}"
docker image inspect "${IMAGE}" >/dev/null
docker run --rm \
    -e "AICO_BUSINESS_VERSION=${AICO_BUSINESS_VERSION}" \
    -e "AICO_BUSINESS_REVISION=${AICO_BUSINESS_REVISION}" \
    -e "AICO_PLATFORM_CONTRACT=${AICO_PLATFORM_CONTRACT:-}" \
    -e "AICO_RELEASE_CHANNEL=${AICO_RELEASE_CHANNEL:-}" \
    -e "AICO_PLATFORM_BASE=${AICO_PLATFORM_BASE:-}" \
    -e "AICO_SYSTEM_FINGERPRINT=${AICO_SYSTEM_FINGERPRINT:-}" \
    -e "HOST_UID=$(id -u)" -e "HOST_GID=$(id -g)" \
    -v "${REPO_ROOT}:/work:ro" \
    -v "${APPS_OUTPUT}/apps:/apps:ro" \
    -v "${OUTPUT_DIR}:/out" \
    -w /work "${IMAGE}" bash scripts/debian-package/container-build.sh

cp "${OUTPUT_DIR}/aico-business_${AICO_BUSINESS_VERSION}-${AICO_BUSINESS_REVISION}_armhf.deb" \
    "${OUTPUT_DIR}/aico-business.deb"
