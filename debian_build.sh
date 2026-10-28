#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT=${SCRIPT_DIR}
readonly REPO_ROOT
readonly APPS_OUTPUT=${REPO_ROOT}/output/debian-apps
readonly SYSTEM_OUTPUT=${REPO_ROOT}/output/debian-system
readonly FINAL_OUTPUT=${REPO_ROOT}/output/debian/image
readonly DEFAULT_AGENT_CONFIG=${REPO_ROOT}/src/agent/config/agent.toml
readonly DEFAULT_OTA_PRIVATE_KEY=${REPO_ROOT}/key/id_25519.pem
readonly DEFAULT_OTA_PUBLIC_KEY=${REPO_ROOT}/key/id_25519.pub.pem
readonly DEFAULT_OTA_TRUST_PUBLIC_KEY=${REPO_ROOT}/keys/ota_pubkey.pem
readonly DEFAULT_GO_ROOT=${REPO_ROOT}/.toolchains/go1.26.0.linux-amd64
readonly GO_VERSION=1.26.0
readonly GO_DIST=linux-amd64
readonly GO_TARBALL=go${GO_VERSION}.${GO_DIST}.tar.gz
readonly GO_TARBALL_SHA256=aac1b08a0fb0c4e0a7c1555beb7b59180b05dfc5a3d62e40e9de90cd42f88235
readonly DEFAULT_SOURCE_DATE_EPOCH=1767360516
readonly CLEANUP_IMAGE=debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258
readonly -a MANIFEST_IMAGE_ASSETS=(boot_a.img boot_b.img rootfs.img)
readonly -a RELEASE_IMAGE_ASSETS=(boot_a.img boot_b.img rootfs.img update.img)
readonly -a LOCAL_RELEASE_OUTPUT_ASSETS=(
    boot_a.img.tar.gz
    boot_b.img.tar.gz
    manifest.json
    rootfs.img.tar.gz
    update.img.tar.gz
)
TEMPORARY_DIR=

usage() {
    cat <<'EOF'
Usage: ./debian_build.sh

Build the complete Debian armhf factory firmware from clean apps/system
outputs, create a locally signed OTA manifest/config, audit the image set, and
write the local firmware/OTA artifacts to output/debian/image:

  update.img
  tf-card.img
  boot_a.img.tar.gz
  boot_b.img.tar.gz
  manifest.json
  rootfs.img.tar.gz
  update.img.tar.gz

Write output/debian/image/tf-card.img to the whole TF card with a raw image
writer. output/debian/image/update.img is a Rockchip package, not a raw card
image.

Environment overrides:
  RK_JOBS                   Requested parallel jobs (default: 24, capped at
                            the number of online CPUs).
  AGENT_CONFIG_PATH         agent.toml installed into userdata.img. Defaults to
                            the committed src/agent/config/agent.toml template,
                            which carries no provider credentials; point it at
                            an external file for a production image.
  OTA_PRIVATE_KEY_PATH      Ed25519 private PEM (default: key/id_25519.pem).
  OTA_PUBLIC_KEY_PATH       Ed25519 public PEM (default: key/id_25519.pub.pem).
  OTA_TRUST_PUBLIC_KEY_PATH Ed25519 public PEM burned into the image as the
                            OTA trust anchor. Defaults to keys/ota_pubkey.pem,
                            the published signer, so a local image accepts
                            released updates; falls back to OTA_PUBLIC_KEY_PATH
                            when that file is absent. Set it explicitly to
                            build an image that trusts a different signer.
  DEBIAN_APPS_GO_ROOT       Go 1.26.0 linux/amd64 toolchain (default:
                            .toolchains/go1.26.0.linux-amd64).
  DEBIAN_APPS_GO_BUILD_CACHE, DEBIAN_APPS_GO_MODULE_CACHE,
  DEBIAN_APPS_OPENCV_CACHE
                            Persistent apps-stage caches (default:
                            .cache/debian-apps/{go-build,go-mod,opencv}).
                            They live outside output/, so this build does not
                            clear them. Point them outside the repository to
                            survive a CI checkout that cleans ignored files.
  OTA_REPO                  Local factory config repository label (default:
                            AidenAI-IO/aico-link).
  OTA_CHANNEL               Local manifest channel (default: local).
  OTA_BASE_URL              Optional HTTP(S) base URL embedded in manifest
                            assets for non-GitHub distribution.
  OTA_BUILD_VERSION         Local manifest version (default: timestamp + Git SHA).
  OTA_BUILD_TIME            RFC3339 manifest time (default: current UTC time).
  SOURCE_DATE_EPOCH         Reproducible archive timestamp (default: 1767360516).

The build removes only these generated directories before starting:
  output/debian-apps
  output/debian-system
  output/debian

It does not publish a GitHub Release and does not copy an external agent.toml
into the source tree.
EOF
}

die() {
    printf 'debian_build.sh: %s\n' "$*" >&2
    exit 1
}

log() {
    printf '\n[%s] %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"
}

cleanup() {
    [ -z "${TEMPORARY_DIR}" ] || rm -rf "${TEMPORARY_DIR}"
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command is missing: $1"
}

positive_integer() {
    case "$1" in
        '' | *[!0-9]* | 0) return 1 ;;
        *) return 0 ;;
    esac
}

choose_job_count() {
    local requested=$1
    local available=$2
    positive_integer "${requested}" || die "RK_JOBS must be a positive integer: ${requested}"
    positive_integer "${available}" || die "invalid online CPU count: ${available}"
    if [ "${requested}" -gt "${available}" ]; then
        printf '%s\n' "${available}"
    else
        printf '%s\n' "${requested}"
    fi
}

online_cpu_count() {
    local count
    count=$(getconf _NPROCESSORS_ONLN 2>/dev/null || true)
    if ! positive_integer "${count}" && command -v nproc >/dev/null 2>&1; then
        count=$(nproc)
    fi
    positive_integer "${count}" || die "unable to determine the number of online CPUs"
    printf '%s\n' "${count}"
}

sha256_file() {
    sha256sum "$1" | awk '{print $1}'
}

ensure_go_toolchain() {
    local go_root=$1
    local version_file=${go_root}/VERSION
    local go_binary=${go_root}/bin/go
    local toolchain_cache=${REPO_ROOT}/.toolchains
    local tarball_path=${toolchain_cache}/${GO_TARBALL}
    local downloaded=${tarball_path}.tmp.$$
    local extract_dir=${toolchain_cache}/.go${GO_VERSION}.${GO_DIST}.$$

    if [ -x "${go_binary}" ] && [ -f "${version_file}" ] &&
        grep -qx "go${GO_VERSION}" "${version_file}"; then
        [ "$("${go_binary}" env GOVERSION)" = "go${GO_VERSION}" ] \
            || die "Go VERSION file and executable disagree in ${go_root}"
        [ "$("${go_binary}" env GOHOSTOS)" = linux ] \
            || die "Go toolchain must target a linux build host: ${go_root}"
        [ "$("${go_binary}" env GOHOSTARCH)" = amd64 ] \
            || die "Go toolchain must target an amd64 build host: ${go_root}"
        return
    fi

    if [ "${go_root}" != "${DEFAULT_GO_ROOT}" ]; then
        die "custom DEBIAN_APPS_GO_ROOT is not a valid Go 1.26.0 linux/amd64 toolchain: ${go_root}"
    fi

    mkdir -p "${toolchain_cache}"
    if [ ! -f "${tarball_path}" ]; then
        log "Downloading pinned Go ${GO_VERSION} toolchain"
        rm -f "${downloaded}"
        curl -fL --retry 3 --connect-timeout 20 \
            -o "${downloaded}" "https://go.dev/dl/${GO_TARBALL}"
        [ "$(sha256_file "${downloaded}")" = "${GO_TARBALL_SHA256}" ] \
            || die "downloaded Go toolchain checksum mismatch"
        mv "${downloaded}" "${tarball_path}"
    fi
    [ "$(sha256_file "${tarball_path}")" = "${GO_TARBALL_SHA256}" ] \
        || die "cached Go toolchain checksum mismatch: ${tarball_path}"

    rm -rf "${extract_dir}" "${go_root}"
    mkdir -p "${extract_dir}"
    tar -C "${extract_dir}" -xzf "${tarball_path}"
    mv "${extract_dir}/go" "${go_root}"
    rmdir "${extract_dir}"

    [ "$("${go_root}/bin/go" env GOVERSION)" = "go${GO_VERSION}" ] \
        || die "installed Go toolchain has the wrong version"
}

validate_key_pair() {
    local private_key=$1
    local public_key=$2
    local work_dir=$3
    local private_public_der=${work_dir}/private-public.der
    local public_der=${work_dir}/public.der

    [ -f "${private_key}" ] || die "OTA private PEM is missing: ${private_key}"
    [ -f "${public_key}" ] || die "OTA public PEM is missing: ${public_key}"
    openssl pkey -in "${private_key}" -pubout -outform DER \
        -out "${private_public_der}" >/dev/null 2>&1 \
        || die "invalid Ed25519 private PEM: ${private_key}"
    openssl pkey -pubin -in "${public_key}" -outform DER \
        -out "${public_der}" >/dev/null 2>&1 \
        || die "invalid Ed25519 public PEM: ${public_key}"
    cmp -s "${private_public_der}" "${public_der}" \
        || die "OTA public and private PEM files do not form a key pair"
    "${REPO_ROOT}/scripts/validate_ota_pubkey.sh" "${public_key}"
}

# The key burned into /usr/share/keyrings/aico-ota.pem decides whose manifests the
# device will accept; the private key decides who signs this build's own
# manifest. They are the same key by default, which is what a self-contained
# build wants. Keeping them separable lets a locally built image trust a
# release signer whose private key stays out of reach, because verifying a
# manifest needs the public half only.
validate_trust_public_key() {
    local trust_key=$1
    local signing_public_key=$2
    local work_dir=$3
    local trust_der=${work_dir}/trust-public.der
    local signing_der=${work_dir}/signing-public.der

    [ -f "${trust_key}" ] || die "OTA trust anchor PEM is missing: ${trust_key}"
    openssl pkey -pubin -in "${trust_key}" -outform DER \
        -out "${trust_der}" >/dev/null 2>&1 \
        || die "invalid Ed25519 public PEM: ${trust_key}"
    # Not the last statement in this function, so errexit alone would not
    # carry the rejection out of a caller that runs this in a condition.
    "${REPO_ROOT}/scripts/validate_ota_pubkey.sh" "${trust_key}" \
        || die "OTA trust anchor is not an Ed25519 public key: ${trust_key}"
    openssl pkey -pubin -in "${signing_public_key}" -outform DER \
        -out "${signing_der}" >/dev/null 2>&1 \
        || die "invalid Ed25519 public PEM: ${signing_public_key}"
    if cmp -s "${trust_der}" "${signing_der}"; then
        return 0
    fi

    printf '\n[%s] OTA trust anchor differs from this build signing key\n' \
        "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >&2
    printf '  image trusts:       %s\n' "${trust_key}" >&2
    printf '  manifest signed by: %s\n' "${signing_public_key}" >&2
    printf '  The image accepts only manifests signed by the trusted key, and\n' >&2
    printf '  so rejects the manifest.json this build writes. Nothing on the\n' >&2
    printf '  device reads that manifest, so this is the supported way to build\n' >&2
    printf '  an image that updates from a release this host cannot sign.\n' >&2
}

ensure_pico_sdk() {
    # The vendored SDK is the BSP build tree and is built in place. The system
    # stage records the owning firmware commit as its source provenance.
    [ -e "${REPO_ROOT}/pico-sdk/project/build.sh" ] \
        || die "vendored pico-sdk has no project/build.sh; restore the repository checkout"
}

clean_generated_outputs() {
    local target resolved_target
    log "Removing previous Debian apps/system/final outputs"
    mkdir -p "${REPO_ROOT}/output"
    for target in "${APPS_OUTPUT}" "${SYSTEM_OUTPUT}" "${REPO_ROOT}/output/debian"; do
        [ ! -L "${target}" ] || die "refusing to clean a symlinked output directory: ${target}"
        mkdir -p "${target}"
        resolved_target=$(readlink -f "${target}")
        case "${resolved_target}" in
            "${REPO_ROOT}/output/"*) ;;
            *) die "refusing to clean an output outside the repository: ${resolved_target}" ;;
        esac
        docker run --rm \
            -e "HOST_UID=$(id -u)" \
            -e "HOST_GID=$(id -g)" \
            -v "${resolved_target}:/target" \
            "${CLEANUP_IMAGE}" \
            sh -eu -c '
                find /target -mindepth 1 -delete
                chown "${HOST_UID}:${HOST_GID}" /target
                chmod 0755 /target
            '
    done
}

compress_release_assets() {
    local image_dir=$1
    local assets=$2
    local archive_epoch=$3

    [ -d "${image_dir}" ] || die "release image directory is missing: ${image_dir}"
    case "${archive_epoch}" in
        '' | *[!0-9]*) die "SOURCE_DATE_EPOCH must be an unsigned Unix timestamp: ${archive_epoch}" ;;
    esac
    SOURCE_DATE_EPOCH="${archive_epoch}" \
        "${REPO_ROOT}/scripts/compress_release_images.sh" \
        --image-dir "${image_dir}" \
        --assets "${assets}" \
        --output /dev/null
}

install_local_release_artifacts() {
    local image_dir=$1
    local output_dir=$2
    local archive_epoch=$3
    local artifact

    compress_release_assets \
        "${image_dir}" "${RELEASE_IMAGE_ASSETS[*]}" "${archive_epoch}"
    mkdir -p "${output_dir}"
    [ -s "${image_dir}/update.img" ] || die "the system stage did not produce update.img"
    install -m 0644 "${image_dir}/update.img" "${output_dir}/update.img"
    [ -s "${image_dir}/tf-card.img" ] || die "the system stage did not produce tf-card.img"
    install -m 0644 "${image_dir}/tf-card.img" "${output_dir}/tf-card.img"
    for artifact in "${LOCAL_RELEASE_OUTPUT_ASSETS[@]}"; do
        [ -s "${image_dir}/${artifact}" ] \
            || die "local release artifact is missing: ${image_dir}/${artifact}"
        install -m 0644 "${image_dir}/${artifact}" "${output_dir}/${artifact}"
    done
    (
        cd "${output_dir}"
        sha256sum update.img >update.img.sha256
        sha256sum tf-card.img >tf-card.img.sha256
    )
}

main() {
    case "${1:-}" in
        -h | --help | help)
            usage
            return
            ;;
        '') ;;
        *)
            usage >&2
            exit 2
            ;;
    esac

    local command
    for command in awk cmp curl date docker getconf git grep id install openssl \
        python3 readlink sha256sum tar; do
        require_command "${command}"
    done
    docker info >/dev/null 2>&1 \
        || die "Docker is unavailable; ensure the daemon is running and this user can access it"

    local available_jobs selected_jobs
    available_jobs=$(online_cpu_count)
    selected_jobs=$(choose_job_count "${RK_JOBS:-24}" "${available_jobs}")
    export RK_JOBS=${selected_jobs}
    log "Using RK_JOBS=${RK_JOBS} (${available_jobs} online CPUs)"

    local agent_config=${AGENT_CONFIG_PATH:-${DEFAULT_AGENT_CONFIG}}
    local ota_private_key=${OTA_PRIVATE_KEY_PATH:-${DEFAULT_OTA_PRIVATE_KEY}}
    local ota_public_key=${OTA_PUBLIC_KEY_PATH:-${DEFAULT_OTA_PUBLIC_KEY}}
    local ota_trust_public_key=${OTA_TRUST_PUBLIC_KEY_PATH:-}
    local go_root=${DEBIAN_APPS_GO_ROOT:-${DEFAULT_GO_ROOT}}
    local ota_repo=${OTA_REPO:-AidenAI-IO/aico-link}
    local ota_channel=${OTA_CHANNEL:-local}
    local ota_base_url=${OTA_BASE_URL:-}
    local ota_build_time=${OTA_BUILD_TIME:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}
    local system_build_image=${DEBIAN_SYSTEM_BUILD_IMAGE:-aico-debian13-armhf-builder:system}
    local release_archive_epoch=${SOURCE_DATE_EPOCH:-${DEFAULT_SOURCE_DATE_EPOCH}}
    local git_revision ota_build_version
    git_revision=$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD)
    ota_build_version=${OTA_BUILD_VERSION:-local-$(date -u '+%Y%m%d-%H%M%S')-${git_revision}}

    if [ -n "${ota_base_url}" ]; then
        case "${ota_base_url}" in
            http://* | https://*) ;;
            *) die "OTA_BASE_URL must start with http:// or https://" ;;
        esac
        [[ ! "${ota_base_url}" =~ [[:space:]] ]] \
            || die "OTA_BASE_URL must not contain whitespace"
    fi

    [ -f "${agent_config}" ] \
        || die "external Agent configuration is missing: ${agent_config}"
    [ -f "${ota_private_key}" ] || die "OTA private PEM is missing: ${ota_private_key}"
    [ -f "${ota_public_key}" ] || die "OTA public PEM is missing: ${ota_public_key}"
    [ -d "${go_root}" ] || ensure_go_toolchain "${go_root}"
    agent_config=$(readlink -f "${agent_config}")
    ota_private_key=$(readlink -f "${ota_private_key}")
    ota_public_key=$(readlink -f "${ota_public_key}")
    # An explicit anchor wins. Otherwise trust the published signer committed
    # to the repository, so an image built here accepts released updates
    # without anyone handling that signer's private key. Only when the file is
    # absent does the build fall back to trusting itself.
    if [ -n "${ota_trust_public_key}" ]; then
        [ -f "${ota_trust_public_key}" ] \
            || die "OTA trust anchor PEM is missing: ${ota_trust_public_key}"
        ota_trust_public_key=$(readlink -f "${ota_trust_public_key}")
    elif [ -f "${DEFAULT_OTA_TRUST_PUBLIC_KEY}" ]; then
        ota_trust_public_key=$(readlink -f "${DEFAULT_OTA_TRUST_PUBLIC_KEY}")
    else
        ota_trust_public_key=${ota_public_key}
    fi
    go_root=$(readlink -f "${go_root}")

    TEMPORARY_DIR=$(mktemp -d "${TMPDIR:-/tmp}/aico-debian-build.XXXXXX")
    trap cleanup EXIT
    validate_key_pair "${ota_private_key}" "${ota_public_key}" "${TEMPORARY_DIR}"
    validate_trust_public_key \
        "${ota_trust_public_key}" "${ota_public_key}" "${TEMPORARY_DIR}"
    ensure_go_toolchain "${go_root}"
    ensure_pico_sdk
    clean_generated_outputs

    log "Building and auditing Debian applications"
    DEBIAN_APPS_OUTPUT_DIR="${APPS_OUTPUT}" \
        DEBIAN_APPS_GO_ROOT="${go_root}" \
        "${REPO_ROOT}/scripts/debian-apps/build-apps.sh" all
    grep -qx 'status=pass' "${APPS_OUTPUT}/apps-audit/summary.txt" \
        || die "Debian application audit did not pass"

    log "Building Debian system rootfs builder"
    DEBIAN_SYSTEM_OUTPUT_DIR="${SYSTEM_OUTPUT}" \
        DEBIAN_APPS_OUTPUT_DIR="${APPS_OUTPUT}" \
        "${REPO_ROOT}/scripts/debian-system/build.sh" builder

    log "Building Debian system BSP and A/B boot images"
    DEBIAN_SYSTEM_OUTPUT_DIR="${SYSTEM_OUTPUT}" \
        DEBIAN_APPS_OUTPUT_DIR="${APPS_OUTPUT}" \
        "${REPO_ROOT}/scripts/debian-system/build.sh" bsp

    log "Building Debian system rootfs"
    OTA_PUBLIC_KEY_PATH="${ota_trust_public_key}" \
        DEBIAN_SYSTEM_OUTPUT_DIR="${SYSTEM_OUTPUT}" \
        DEBIAN_APPS_OUTPUT_DIR="${APPS_OUTPUT}" \
        "${REPO_ROOT}/scripts/debian-system/build.sh" rootfs

    log "Assembling Debian system factory images"
    OTA_PUBLIC_KEY_PATH="${ota_trust_public_key}" \
        AGENT_CONFIG_PATH="${agent_config}" \
        DEBIAN_SYSTEM_OUTPUT_DIR="${SYSTEM_OUTPUT}" \
        DEBIAN_APPS_OUTPUT_DIR="${APPS_OUTPUT}" \
        "${REPO_ROOT}/scripts/debian-system/build.sh" images

    log "Compressing OTA partition assets for the signed manifest"
    compress_release_assets \
        "${SYSTEM_OUTPUT}/image" \
        "${MANIFEST_IMAGE_ASSETS[*]}" \
        "${release_archive_epoch}"

    local device_config=${SYSTEM_OUTPUT}/debian-ota-config.json
    local max_download_bytes
    local ota_board_config=${REPO_ROOT}/scripts/debian-system/BoardConfig-TF-Debian13-RV1106_Aico_Link-IPC.mk
    local -a manifest_location_args=()
    export AICO_OTA_BOARD_CONFIG_PATH=${ota_board_config}
    # shellcheck source=/dev/null
    source "${REPO_ROOT}/scripts/ota_partition_layout.sh"
    max_download_bytes=$(aico_ota_manifest_max_download_bytes) \
        || die "failed to resolve the OTA partition download limit"
    if [ -n "${ota_base_url}" ]; then
        manifest_location_args=(--base-url "${ota_base_url}")
    fi
    log "Generating a locally signed OTA manifest"
    docker run --rm \
        -u "$(id -u):$(id -g)" \
        -v "${REPO_ROOT}:/work:ro" \
        -v "${SYSTEM_OUTPUT}:/out" \
        -v "${ota_private_key}:/run/secrets/ota_private.pem:ro" \
        -w /work \
        "${system_build_image}" \
        bash scripts/generate_ota_manifest.sh \
        --version "${ota_build_version}" \
        --channel "${ota_channel}" \
        --build-time "${ota_build_time}" \
        --sign-key /run/secrets/ota_private.pem \
        --image-dir /out/image \
        ${manifest_location_args[@]+"${manifest_location_args[@]}"} \
        --max-download-bytes "${max_download_bytes}" \
        --output /out/image/manifest.json

    log "Generating the matching factory OTA configuration"
    docker run --rm \
        -u "$(id -u):$(id -g)" \
        -v "${REPO_ROOT}:/work:ro" \
        -v "${SYSTEM_OUTPUT}:/out" \
        -w /work \
        "${system_build_image}" \
        bash scripts/generate_ota_device_config.sh \
        --manifest /out/image/manifest.json \
        --repo "${ota_repo}" \
        --channel "${ota_channel}" \
        --output /out/debian-ota-config.json

    log "Installing the OTA configuration and repacking update.img"
    OTA_DEVICE_CONFIG_PATH="${device_config}" \
        DEBIAN_SYSTEM_OUTPUT_DIR="${SYSTEM_OUTPUT}" \
        DEBIAN_APPS_OUTPUT_DIR="${APPS_OUTPUT}" \
        "${REPO_ROOT}/scripts/debian-system/build.sh" config

    log "Running the final Debian image audit"
    DEBIAN_SYSTEM_OUTPUT_DIR="${SYSTEM_OUTPUT}" \
        DEBIAN_APPS_OUTPUT_DIR="${APPS_OUTPUT}" \
        "${REPO_ROOT}/scripts/debian-system/build.sh" audit
    grep -qx 'Audit passed' "${SYSTEM_OUTPUT}/audit-report.txt" \
        || die "final Debian image audit did not pass"

    log "Installing local firmware and OTA release artifacts"
    install_local_release_artifacts \
        "${SYSTEM_OUTPUT}/image" "${FINAL_OUTPUT}" "${release_archive_epoch}"

    log "Debian firmware build completed"
    printf 'TF card image: %s\n' "${FINAL_OUTPUT}/tf-card.img"
    printf 'SHA-256: %s\n' "$(sha256_file "${FINAL_OUTPUT}/tf-card.img")"
    printf 'Release artifacts:\n'
    printf '  %s\n' "${LOCAL_RELEASE_OUTPUT_ASSETS[@]}"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
