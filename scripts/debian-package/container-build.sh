#!/usr/bin/env bash
set -euo pipefail
readonly REPO_ROOT=/work APPS_DIR=/apps OUTPUT_DIR=/out
source "${REPO_ROOT}/scripts/debian-package/version.sh"
readonly PKG_ROOT=${OUTPUT_DIR}/package-root
readonly VERSION=${AIDEN_BUSINESS_VERSION}
readonly REVISION=${AIDEN_BUSINESS_REVISION}
readonly PACKAGE_VERSION=${VERSION}-${REVISION}
readonly PACKAGE_NAME=aiden-business
readonly BINARIES=(
  agent audio_service audio_service_cli ble_service cpu_vad frame_service
  frame_service_cli rknn_vad smart_turn_helper ota abctl aiden-environment ttyd
)
rm -rf "${PKG_ROOT}" "${OUTPUT_DIR}/${PACKAGE_NAME}_"*.deb
install -d -m 0755 "${PKG_ROOT}/DEBIAN" "${PKG_ROOT}/usr/lib/aiden" "${PKG_ROOT}/usr/share/aiden/config-web" "${PKG_ROOT}/usr/share/aiden/skills" "${PKG_ROOT}/usr/share/aiden/audio" "${PKG_ROOT}/usr/lib/aiden/models" "${PKG_ROOT}/usr/share/doc/${PACKAGE_NAME}"
for binary in "${BINARIES[@]}"; do
  test -x "${APPS_DIR}/bin/${binary}" || { echo "missing application binary: ${binary}" >&2; exit 1; }
  install -m 0755 "${APPS_DIR}/bin/${binary}" "${PKG_ROOT}/usr/lib/aiden/${binary}"
done
rsync -aH --chown=0:0 "${REPO_ROOT}/src/config_web/web/" "${PKG_ROOT}/usr/share/aiden/config-web/"
rsync -aH --chown=0:0 "${REPO_ROOT}/src/agent/config/skills/" "${PKG_ROOT}/usr/share/aiden/skills/"
install -m 0644 "${REPO_ROOT}/src/agent/internal/agent/quick_actions.json" "${PKG_ROOT}/usr/share/aiden/quick_actions.json"
rsync -aH --chown=0:0 "${REPO_ROOT}/assets/business/audio/" "${PKG_ROOT}/usr/share/aiden/audio/"
rsync -aH --chown=0:0 "${REPO_ROOT}/assets/business/models/" "${PKG_ROOT}/usr/lib/aiden/models/"
# Vendor libraries are supplied by the platform rootfs through ld.so.conf.d.
cat >"${PKG_ROOT}/DEBIAN/control" <<CTL
Package: ${PACKAGE_NAME}
Version: ${PACKAGE_VERSION}
Section: misc
Priority: optional
Architecture: armhf
Maintainer: Aiden AI <firmware@aiden.ai>
Pre-Depends: python3
Depends: libc6, systemd, sudo
Description: Aiden business runtime
 Agent, hardware services, configuration web assets, bundled skills and runtime models.
CTL
bash "${REPO_ROOT}/scripts/debian-package/write-maintainer-scripts.sh" "${PKG_ROOT}/DEBIAN"
python3 "${REPO_ROOT}/scripts/release/contract.py" package \
    "${PKG_ROOT}/usr/share/doc/${PACKAGE_NAME}/release-manifest.json"
find "${PKG_ROOT}" -type d -exec chmod 0755 {} +
find "${PKG_ROOT}" -type f ! -path '*/DEBIAN/*' -exec chmod 0644 {} +
for binary in "${BINARIES[@]}"; do chmod 0755 "${PKG_ROOT}/usr/lib/aiden/${binary}"; done
python3 "${REPO_ROOT}/scripts/debian-package/system_config.py" stage "${PKG_ROOT}"
dpkg-deb --build --root-owner-group "${PKG_ROOT}" "${OUTPUT_DIR}/${PACKAGE_NAME}_${PACKAGE_VERSION}_armhf.deb"
dpkg-deb --info "${OUTPUT_DIR}/${PACKAGE_NAME}_${PACKAGE_VERSION}_armhf.deb" >"${OUTPUT_DIR}/${PACKAGE_NAME}.deb.info"
dpkg-deb --contents "${OUTPUT_DIR}/${PACKAGE_NAME}_${PACKAGE_VERSION}_armhf.deb" >"${OUTPUT_DIR}/${PACKAGE_NAME}.deb.contents"
chown "${HOST_UID:-0}:${HOST_GID:-0}" \
    "${OUTPUT_DIR}/${PACKAGE_NAME}_${PACKAGE_VERSION}_armhf.deb" \
    "${OUTPUT_DIR}/${PACKAGE_NAME}.deb.info" "${OUTPUT_DIR}/${PACKAGE_NAME}.deb.contents"
