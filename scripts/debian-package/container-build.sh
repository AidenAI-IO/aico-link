#!/usr/bin/env bash
set -euo pipefail
readonly REPO_ROOT=/work APPS_DIR=/apps OUTPUT_DIR=/out
source "${REPO_ROOT}/scripts/debian-package/version.sh"
readonly PKG_ROOT=${OUTPUT_DIR}/package-root
readonly VERSION=${AICO_BUSINESS_VERSION}
readonly REVISION=${AICO_BUSINESS_REVISION}
readonly PACKAGE_VERSION=${VERSION}-${REVISION}
readonly PACKAGE_NAME=aico-business
readonly BINARIES=(
  agent audio_service audio_service_cli ble_service mcu_service cpu_vad frame_service
  frame_service_cli rknn_vad ota abctl aico-environment ttyd
)
rm -rf "${PKG_ROOT}" "${OUTPUT_DIR}/${PACKAGE_NAME}_"*.deb
install -d -m 0755 "${PKG_ROOT}/DEBIAN" "${PKG_ROOT}/usr/lib/aico" "${PKG_ROOT}/usr/share/aico/config-web" "${PKG_ROOT}/usr/share/aico/skills" "${PKG_ROOT}/usr/share/aico/audio" "${PKG_ROOT}/usr/lib/aico/models" "${PKG_ROOT}/usr/share/doc/${PACKAGE_NAME}"
for binary in "${BINARIES[@]}"; do
  test -x "${APPS_DIR}/bin/${binary}" || { echo "missing application binary: ${binary}" >&2; exit 1; }
  install -m 0755 "${APPS_DIR}/bin/${binary}" "${PKG_ROOT}/usr/lib/aico/${binary}"
done
rsync -aH --chown=0:0 "${REPO_ROOT}/src/config_web/web/" "${PKG_ROOT}/usr/share/aico/config-web/"
rsync -aH --chown=0:0 "${REPO_ROOT}/src/agent/config/skills/" "${PKG_ROOT}/usr/share/aico/skills/"
install -m 0644 "${REPO_ROOT}/src/agent/internal/agent/quick_actions.json" "${PKG_ROOT}/usr/share/aico/quick_actions.json"
rsync -aH --chown=0:0 "${REPO_ROOT}/assets/business/audio/" "${PKG_ROOT}/usr/share/aico/audio/"
(cd "${REPO_ROOT}/assets/business/models/dtln" && sha256sum --check SHA256SUMS)
rsync -aH --chown=0:0 "${REPO_ROOT}/assets/business/models/" "${PKG_ROOT}/usr/lib/aico/models/"
# Vendor libraries are supplied by the platform rootfs through ld.so.conf.d.
cat >"${PKG_ROOT}/DEBIAN/control" <<CTL
Package: ${PACKAGE_NAME}
Version: ${PACKAGE_VERSION}
Section: misc
Priority: optional
Architecture: armhf
Maintainer: Aico <firmware@aiden.ai>
Pre-Depends: python3
Depends: libc6, systemd, sudo
Homepage: https://aico.link
Description: Aico Link firmware and Aico AI Agent runtime
 Aico AI Agent, Aico Link hardware services, configuration web assets, bundled skills
 and runtime models.
CTL
bash "${REPO_ROOT}/scripts/debian-package/write-maintainer-scripts.sh" "${PKG_ROOT}/DEBIAN"
python3 "${REPO_ROOT}/scripts/release/contract.py" package \
    "${PKG_ROOT}/usr/share/doc/${PACKAGE_NAME}/release-manifest.json"
find "${PKG_ROOT}" -type d -exec chmod 0755 {} +
find "${PKG_ROOT}" -type f ! -path '*/DEBIAN/*' -exec chmod 0644 {} +
for binary in "${BINARIES[@]}"; do chmod 0755 "${PKG_ROOT}/usr/lib/aico/${binary}"; done
python3 "${REPO_ROOT}/scripts/debian-package/system_config.py" stage "${PKG_ROOT}"
dpkg-deb --build --root-owner-group "${PKG_ROOT}" "${OUTPUT_DIR}/${PACKAGE_NAME}_${PACKAGE_VERSION}_armhf.deb"
dpkg-deb --info "${OUTPUT_DIR}/${PACKAGE_NAME}_${PACKAGE_VERSION}_armhf.deb" >"${OUTPUT_DIR}/${PACKAGE_NAME}.deb.info"
dpkg-deb --contents "${OUTPUT_DIR}/${PACKAGE_NAME}_${PACKAGE_VERSION}_armhf.deb" >"${OUTPUT_DIR}/${PACKAGE_NAME}.deb.contents"
chown "${HOST_UID:-0}:${HOST_GID:-0}" \
    "${OUTPUT_DIR}/${PACKAGE_NAME}_${PACKAGE_VERSION}_armhf.deb" \
    "${OUTPUT_DIR}/${PACKAGE_NAME}.deb.info" "${OUTPUT_DIR}/${PACKAGE_NAME}.deb.contents"
