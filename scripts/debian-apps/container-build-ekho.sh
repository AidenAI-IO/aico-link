#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT=${AICO_REPO_ROOT:-/work}
readonly OUTPUT_DIR=${DEBIAN_APPS_OUTPUT_DIR:-/out}
readonly CACHE_DIR=${DEBIAN_APPS_EKHO_CACHE:-${REPO_ROOT}/.cache/debian-apps/ekho}
readonly BUILD_DIR=${OUTPUT_DIR}/ekho-build
readonly DIST_DIR=${OUTPUT_DIR}/apps
readonly PATCH_DIR=${REPO_ROOT}/scripts/debian-apps/ekho
readonly JOBS=${RK_JOBS:-$(getconf _NPROCESSORS_ONLN)}
source "${PATCH_DIR}/source.env"
readonly EKHO_VERSION EKHO_COMMIT EKHO_SOURCE_ARCHIVE EKHO_SOURCE_URL EKHO_SOURCE_SHA256
readonly ARCHIVE=${CACHE_DIR}/${EKHO_SOURCE_ARCHIVE}
readonly PATCH_FILE=${PATCH_DIR}/flite-slt.patch

mkdir -p "${CACHE_DIR}"
# Containers sharing this cache can have identical process IDs.
download=$(mktemp "${ARCHIVE}.tmp.XXXXXX")
trap 'rm -f "${download}"' EXIT
if [ ! -f "${ARCHIVE}" ]; then
    curl -fL --retry 3 --connect-timeout 20 --max-time 300 \
        -o "${download}" "${EKHO_SOURCE_URL}"
    mv "${download}" "${ARCHIVE}"
fi
if ! echo "${EKHO_SOURCE_SHA256}  ${ARCHIVE}" | sha256sum -c -; then
    rm -f "${ARCHIVE}"
    echo "Ekho source checksum mismatch; discarded the cached archive" >&2
    exit 1
fi

rm -rf "${BUILD_DIR}"
mkdir -p "${BUILD_DIR}"
tar -xzf "${ARCHIVE}" --strip-components=1 -C "${BUILD_DIR}"
patch --batch -d "${BUILD_DIR}" -p1 <"${PATCH_FILE}"
cd "${BUILD_DIR}"
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-1767360516}
autoreconf -fi
PKG_CONFIG_LIBDIR=/usr/lib/arm-linux-gnueabihf/pkgconfig:/usr/share/pkgconfig \
    ./configure --host=arm-linux-gnueabihf --build="$(sh ./config.guess)" \
    --prefix=/usr --bindir=/usr/lib/aico --datadir=/usr/share/aico \
    --without-pulseaudio --disable-dist-voice --enable-flite \
    CXXFLAGS='-O2 -g0 -I/usr/include/utf8cpp' CFLAGS='-O2 -g0'
# This generated symbol table has very large global constructors. Optimizing
# it is expensive and does not improve the synthesis engine's hot paths.
make src/libekho_a-symbol_array.o CXXFLAGS='-O0 -g0 -I/usr/include/utf8cpp'
# $$ survives Make expansion; the single quotes survive until the link shell
# so the loader, rather than the build shell, expands ORIGIN at runtime.
make -j "${JOBS}" ekho \
    LDFLAGS="-Wl,--enable-new-dtags -Wl,-rpath,'\$\$ORIGIN/../lib'"
arm-linux-gnueabihf-strip ekho
header=$(arm-linux-gnueabihf-readelf -hW ekho)
dynamic=$(arm-linux-gnueabihf-readelf -dW ekho)
grep -q 'Machine:.*ARM' <<<"${header}"
grep -q 'hard-float ABI' <<<"${header}"
grep -F '(RUNPATH)' <<<"${dynamic}" | grep -Fq '[$ORIGIN/../lib]'
grep -F '(NEEDED)' <<<"${dynamic}" | grep -Fq '[libflite_cmu_us_slt.so.1]'
if grep -Fq 'libespeak' <<<"${dynamic}"; then
    echo "Ekho unexpectedly links eSpeak instead of the Flite SLT backend" >&2
    exit 1
fi

readonly DATA_DIR=${DIST_DIR}/share/ekho-data
readonly DOC_DIR=${DIST_DIR}/share/doc/ekho
install -d -m 0755 "${DIST_DIR}/bin" "${DIST_DIR}/metadata"
rm -rf "${DATA_DIR}" "${DOC_DIR}"
install -d -m 0755 "${DATA_DIR}" "${DOC_DIR}/source"
install -m 0755 ekho "${DIST_DIR}/bin/ekho"
# Ship only Mandarin and the alphabet assets used by mixed-language speech.
# Prebuilt dictionary/index files avoid writes to /usr/share at runtime.
for file in pinyin.voice pinyin.index zh.dict zh_list zh_listx zh_patch; do
    install -m 0644 "ekho-data/${file}" "${DATA_DIR}/${file}"
done
cp -R ekho-data/pinyin ekho-data/alphabet "${DATA_DIR}/"
install -m 0644 COPYING "${DOC_DIR}/COPYING"
install -m 0644 ekho-data/pinyin/COPYING-mandarin-yali "${DOC_DIR}/COPYING-mandarin-yali"
# Include the exact upstream source and local modifications with the binary.
install -m 0644 "${ARCHIVE}" "${DOC_DIR}/source/${EKHO_SOURCE_ARCHIVE}"
install -m 0644 "${PATCH_FILE}" "${PATCH_DIR}/source.env" \
    "${REPO_ROOT}/scripts/debian-apps/container-build-ekho.sh" \
    "${REPO_ROOT}/scripts/debian-apps/Dockerfile" "${DOC_DIR}/source/"
printf 'version=%s\ncommit=%s\nurl=%s\nsource_sha256=%s\nenglish_engine=flite\nenglish_voice=slt\nenglish_gain=2.0\nflite_version=%s\ndata_path=/usr/share/aico/ekho-data\npatch_sha256=%s\n' \
    "${EKHO_VERSION}" "${EKHO_COMMIT}" "${EKHO_SOURCE_URL}" "${EKHO_SOURCE_SHA256}" \
    "$(dpkg-query -W -f='${Version}' libflite1:armhf)" \
    "$(sha256sum "${PATCH_FILE}" | awk '{print $1}')" >"${DOC_DIR}/build.txt"
cp "${DOC_DIR}/build.txt" "${DIST_DIR}/metadata/ekho.txt"
(
    cd "${DIST_DIR}"
    find bin/ekho share/ekho-data share/doc/ekho -type f -print0 \
        | LC_ALL=C sort -z | xargs -0 sha256sum
) >"${DIST_DIR}/metadata/ekho-files.sha256"
