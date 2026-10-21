#!/usr/bin/env bash
set -euo pipefail

TEST_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_REPO_ROOT
BUILD_SCRIPT=${TEST_REPO_ROOT}/debian_build.sh
readonly BUILD_SCRIPT
TEST_ROOT=$(mktemp -d)
readonly TEST_ROOT
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Debian build entrypoint test failure: $*" >&2
    exit 1
}

bash -n "${BUILD_SCRIPT}"

# shellcheck disable=SC1090
source "${BUILD_SCRIPT}"

ensure_pico_sdk

[ "$(choose_job_count 24 32)" = 24 ] \
    || fail "24-job default was not preserved on a larger host"
[ "$(choose_job_count 24 8)" = 8 ] \
    || fail "job count was not capped to the available CPUs"
[ "$(choose_job_count 12 24)" = 12 ] \
    || fail "an explicit lower RK_JOBS value was not preserved"
[ "$(choose_job_count 48 24)" = 24 ] \
    || fail "an explicit oversized RK_JOBS value was not capped"

help_output=$("${BUILD_SCRIPT}" --help)
grep -Fq 'output/debian/image/update.img' <<<"${help_output}" \
    || fail "help does not describe the final update.img path"
for artifact in \
    boot_a.img.tar.gz boot_b.img.tar.gz manifest.json \
    rootfs.img.tar.gz update.img.tar.gz; do
    grep -Fq "${artifact}" <<<"${help_output}" \
        || fail "help does not describe local release artifact ${artifact}"
done
grep -Fq 'default: 24' <<<"${help_output}" \
    || fail "help does not describe the default parallelism"
grep -Fq 'key/id_25519.pem' <<<"${help_output}" \
    || fail "help does not describe the private PEM input"
grep -Fq 'key/id_25519.pub.pem' <<<"${help_output}" \
    || fail "help does not describe the public PEM input"
grep -Fq 'OTA_BASE_URL' <<<"${help_output}" \
    || fail "help does not describe direct-download manifest support"

grep -Fq 'scripts/debian-apps/build-apps.sh" all' "${BUILD_SCRIPT}" \
    || fail "apps build is missing"
for action in builder rootfs bsp images config audit; do
    grep -Fq "scripts/debian-system/build.sh\" ${action}" "${BUILD_SCRIPT}" \
        || fail "system ${action} action is missing"
done
grep -Fq 'scripts/generate_ota_manifest.sh' "${BUILD_SCRIPT}" \
    || fail "local signed manifest generation is missing"
grep -Fq 'scripts/generate_ota_device_config.sh' "${BUILD_SCRIPT}" \
    || fail "factory OTA config generation is missing"
grep -Fq 'aiden_ota_manifest_max_download_bytes' "${BUILD_SCRIPT}" \
    || fail "signed manifests do not enforce the OTA partition download limit"
grep -Fq 'scripts/debian-system/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk' \
    "${BUILD_SCRIPT}" \
    || fail "OTA partition limits do not derive from the Debian board config"
grep -Fq -- '--base-url "${ota_base_url}"' "${BUILD_SCRIPT}" \
    || fail "OTA_BASE_URL is not forwarded to manifest generation"
grep -Fq 'find /target -mindepth 1 -delete' "${BUILD_SCRIPT}" \
    || fail "container-assisted cleanup for foreign-owned outputs is missing"
grep -Fq 'refusing to clean a symlinked output directory' "${BUILD_SCRIPT}" \
    || fail "output cleanup does not reject symlink targets"
grep -Eq '^[[:space:]]+jq \\$' "${TEST_REPO_ROOT}/scripts/debian-system/Dockerfile" \
    || fail "system builder does not provide jq for local manifest generation"
grep -Eq '^[[:space:]]+openssl \\$' "${TEST_REPO_ROOT}/scripts/debian-system/Dockerfile" \
    || fail "system builder does not provide openssl for local manifest signing"
grep -Fq 'scripts/compress_release_images.sh' "${BUILD_SCRIPT}" \
    || fail "release image compression is missing"
grep -Fq 'install_local_release_artifacts' "${BUILD_SCRIPT}" \
    || fail "local release artifact installation is missing"
compression_line=$(grep -n 'Compressing OTA partition assets for the signed manifest' \
    "${BUILD_SCRIPT}" | cut -d: -f1)
manifest_line=$(grep -n 'Generating a locally signed OTA manifest' \
    "${BUILD_SCRIPT}" | cut -d: -f1)
[ -n "${compression_line}" ] && [ -n "${manifest_line}" ] \
    && [ "${compression_line}" -lt "${manifest_line}" ] \
    || fail "partition archives must be created before signing manifest.json"

image_dir=${TEST_ROOT}/images
output_dir=${TEST_ROOT}/output
mkdir -p "${image_dir}"
for image in boot_a.img boot_b.img rootfs.img update.img; do
    printf '%s\n' "${image}" >"${image_dir}/${image}"
done
compress_release_assets \
    "${image_dir}" "${MANIFEST_IMAGE_ASSETS[*]}" 0
openssl genpkey -algorithm Ed25519 -out "${TEST_ROOT}/signing-key.pem" \
    >/dev/null 2>&1
"${TEST_REPO_ROOT}/scripts/generate_ota_manifest.sh" \
    --version local-test \
    --channel local \
    --build-time 2026-08-20T00:00:00Z \
    --sign-key "${TEST_ROOT}/signing-key.pem" \
    --image-dir "${image_dir}" \
    --output "${image_dir}/manifest.json"
install_local_release_artifacts "${image_dir}" "${output_dir}" 0

cmp -s "${image_dir}/update.img" "${output_dir}/update.img" \
    || fail "raw update.img was not preserved"
cmp -s "${image_dir}/manifest.json" "${output_dir}/manifest.json" \
    || fail "manifest.json was not installed"
jq -e '
    [.parts[] | .asset?, .asset_a?, .asset_b? | select(. != null)]
    | all(.name | endswith(".img.tar.gz"))
    and all(.image_sha256 | test("^[0-9a-f]{64}$"))
' "${output_dir}/manifest.json" >/dev/null \
    || fail "manifest.json does not describe compressed image assets"
for image in boot_a.img boot_b.img rootfs.img update.img; do
    archive=${output_dir}/${image}.tar.gz
    [ -s "${archive}" ] || fail "local release archive is missing: ${archive}"
    [ "$(tar -tzf "${archive}")" = "${image}" ] \
        || fail "${archive} does not contain exactly ${image}"
    tar -xOzf "${archive}" "${image}" >"${TEST_ROOT}/${image}.extracted"
    cmp -s "${image_dir}/${image}" "${TEST_ROOT}/${image}.extracted" \
        || fail "${archive} does not preserve ${image}"
done
(
    cd "${output_dir}"
    sha256sum --check update.img.sha256 >/dev/null
) || fail "update.img.sha256 does not match the installed image"

# The trust anchor burned into the image is separable from the signing key, so
# an image can accept releases this host cannot sign. Cover the four outcomes.
grep -Fq 'OTA_PUBLIC_KEY_PATH="${ota_trust_public_key}"' "${BUILD_SCRIPT}" \
    || fail "the images stage does not receive the OTA trust anchor"

# The committed public key is the default anchor, so an image built here
# accepts released updates without anyone handling the signer's private key.
[ -f "${TEST_REPO_ROOT}/keys/ota_pubkey.pem" ] \
    || fail "keys/ota_pubkey.pem is missing"
"${TEST_REPO_ROOT}/scripts/validate_ota_pubkey.sh" \
    "${TEST_REPO_ROOT}/keys/ota_pubkey.pem" \
    || fail "keys/ota_pubkey.pem is not an Ed25519 public key"
# The Buildroot image step refuses a key annotated this way, so the committed
# anchor must never carry those markers.
if grep -Eiq '^[[:space:]]*(#|-----).*(dev|test|placeholder)' \
    "${TEST_REPO_ROOT}/keys/ota_pubkey.pem"; then
    fail "keys/ota_pubkey.pem is marked dev/test/placeholder"
fi
grep -Fq 'readonly DEFAULT_OTA_TRUST_PUBLIC_KEY=${REPO_ROOT}/keys/ota_pubkey.pem' \
    "${BUILD_SCRIPT}" || fail "the default OTA trust anchor is not the committed key"
grep -Fq 'elif [ -f "${DEFAULT_OTA_TRUST_PUBLIC_KEY}" ]; then' "${BUILD_SCRIPT}" \
    || fail "the build does not fall back to the committed OTA trust anchor"
# CI signs with the secret, so it must pin the anchor to that key rather than
# inherit the committed one, which a rotation would leave stale.
grep -Fq 'echo "OTA_TRUST_PUBLIC_KEY_PATH=$public_key" >> "$GITHUB_ENV"' \
    "${TEST_REPO_ROOT}/.github/workflows/build.yml" \
    || fail "the Debian workflow does not pin the OTA trust anchor to its signing key"

key_root=${TEST_ROOT}/keys
mkdir -p "${key_root}"
openssl genpkey -algorithm ed25519 -out "${key_root}/signing.pem" >/dev/null 2>&1
openssl pkey -in "${key_root}/signing.pem" -pubout \
    -out "${key_root}/signing.pub.pem" >/dev/null 2>&1
openssl genpkey -algorithm ed25519 -out "${key_root}/other.pem" >/dev/null 2>&1
openssl pkey -in "${key_root}/other.pem" -pubout \
    -out "${key_root}/other.pub.pem" >/dev/null 2>&1
openssl genpkey -algorithm rsa -pkeyopt rsa_keygen_bits:2048 \
    -out "${key_root}/rsa.pem" >/dev/null 2>&1
openssl pkey -in "${key_root}/rsa.pem" -pubout \
    -out "${key_root}/rsa.pub.pem" >/dev/null 2>&1

trust_work=${TEST_ROOT}/trust-work
mkdir -p "${trust_work}"
matching_output=$(
    validate_trust_public_key "${key_root}/signing.pub.pem" \
        "${key_root}/signing.pub.pem" "${trust_work}" 2>&1
) || fail "a trust anchor equal to the signing key was rejected"
# Assert the warning is absent rather than that nothing was written at all:
# this suite is also run under xtrace, whose trace shares this stderr.
case "${matching_output}" in
    *"OTA trust anchor differs"*)
        fail "a trust anchor equal to the signing key warned: ${matching_output}"
        ;;
esac

differing_output=$(
    validate_trust_public_key "${key_root}/other.pub.pem" \
        "${key_root}/signing.pub.pem" "${trust_work}" 2>&1
) || fail "a trust anchor differing from the signing key was rejected"
case "${differing_output}" in
    *"OTA trust anchor differs"*) ;;
    *) fail "a differing trust anchor did not warn: ${differing_output}" ;;
esac

if (validate_trust_public_key "${key_root}/rsa.pub.pem" \
    "${key_root}/signing.pub.pem" "${trust_work}") >/dev/null 2>&1; then
    fail "an RSA trust anchor was accepted"
fi
if (validate_trust_public_key "${key_root}/missing.pub.pem" \
    "${key_root}/signing.pub.pem" "${trust_work}") >/dev/null 2>&1; then
    fail "a missing trust anchor was accepted"
fi

echo "Debian build entrypoint checks passed"
