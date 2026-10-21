#!/usr/bin/env bash
set -euo pipefail
readonly ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly PACKAGE_DIR=${ROOT_DIR}/scripts/debian-package
fail() { echo "Debian package test failure: $*" >&2; exit 1; }
test -x "${PACKAGE_DIR}/build.sh" || fail "build.sh is not executable"
test -x "${PACKAGE_DIR}/container-build.sh" || fail "container-build.sh is not executable"
for script in build.sh container-build.sh version.sh release.sh write-maintainer-scripts.sh; do
  bash -n "${PACKAGE_DIR}/${script}"
done
grep -Fq 'Package: ${PACKAGE_NAME}' "${PACKAGE_DIR}/container-build.sh" || fail "control metadata missing"
for binary in agent audio_service ble_service mcu_service cpu_vad frame_service rknn_vad ota abctl aiden-environment ttyd; do
  grep -Fq "${binary}" "${PACKAGE_DIR}/container-build.sh" || fail "binary is not packaged: ${binary}"
done
grep -Fq 'dpkg-deb --build --root-owner-group' "${PACKAGE_DIR}/container-build.sh" || fail "package is not built with dpkg-deb"
grep -Fq 'aiden-business.deb' "${ROOT_DIR}/scripts/debian-system/build.sh" || fail "system does not consume aiden-business"
grep -Fq 'dpkg -i /tmp/aiden-business.deb' "${ROOT_DIR}/scripts/debian-system/container-build-rootfs.sh" || fail "rootfs does not install package"

sh -n "${PACKAGE_DIR}/maintainer-script.sh"
