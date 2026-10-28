#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
    echo "Debian-only policy failure: $*" >&2
    exit 1
}

for path in "${REPO_ROOT}/debian_build.sh" \
    "${REPO_ROOT}/scripts/debian-apps/build-apps.sh" \
    "${REPO_ROOT}/scripts/debian-system/build.sh"; do
    [ -f "${path}" ] || fail "missing production path: ${path}"
done
grep -Fq 'scripts/debian-system/BoardConfig-TF-Debian13-RV1106_Aico_Link-IPC.mk' \
    "${REPO_ROOT}/scripts/ota_partition_layout.sh" \
    || fail "OTA layout does not default to the Debian board configuration"
if grep -Fq 'pico-sdk/.BoardConfig.mk' \
    "${REPO_ROOT}/scripts/ota_partition_layout.sh"; then
    fail "OTA layout still defaults to the SDK Buildroot board configuration"
fi

if rg --no-ignore -n \
    '\$\{REPO_ROOT\}/overlay/|rv1106-buildroot-uclibc|scripts/build/' \
    "${REPO_ROOT}/debian_build.sh" \
    "${REPO_ROOT}/scripts/debian-apps" \
    "${REPO_ROOT}/scripts/debian-system"; then
    fail "the Debian build depends on a retired repository userspace path"
fi

grep -Fq 'set(AICO_TARGET_PLATFORM "rv1106-debian-glibc"' \
    "${REPO_ROOT}/CMakeLists.txt" \
    || fail "CMake does not default to Debian/glibc"
if sed -n '/set_property(CACHE AICO_TARGET_PLATFORM PROPERTY STRINGS/,/)/p' \
    "${REPO_ROOT}/CMakeLists.txt" | rg -i 'buildroot|uclibc'; then
    fail "CMake still advertises a retired userspace platform"
fi

mapfile -d '' active_docs < <(
    find "${REPO_ROOT}/README.md" "${REPO_ROOT}/docs/README.md" \
        "${REPO_ROOT}/docs"/[0-9][0-9]-* \
        -type f -name '*.md' -print0
)
if rg --no-ignore -n -i \
    '\./build\.sh (binaries|image)|build_image\.sh|/etc/init\.d/|overlay/(etc|oem|userdata)|pico-sdk/output/image|scripts/build/|/dev/block/by-name' \
    "${active_docs[@]}"; then
    fail "active documentation publishes a retired userspace workflow"
fi

if [ -e "${REPO_ROOT}/overlay" ] || [ -e "${REPO_ROOT}/build.sh" ] || \
    [ -e "${REPO_ROOT}/scripts/build" ] || \
    [ -e "${REPO_ROOT}/cmake/platforms/rv1106-buildroot-uclibc.cmake" ]; then
    fail "retired Buildroot overlay or build entry points are still present"
fi

if rg --no-ignore -n \
    '(^|[[:space:]])gh[[:space:]]+release|create_github_release\.sh|\.github/workflows/' \
    "${REPO_ROOT}/debian_build.sh" \
    "${REPO_ROOT}/scripts/debian-apps" \
    "${REPO_ROOT}/scripts/debian-system"; then
    fail "the local Debian production build invokes GitHub publication automation"
fi

echo "Debian-only production policy passed"
