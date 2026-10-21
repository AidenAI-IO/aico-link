#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
readonly DEFAULT_OUTPUT_DIR=${REPO_ROOT}/output/debian-apps
if [ -n "${DEBIAN_APPS_OUTPUT_DIR:-}" ]; then
    if [[ "${DEBIAN_APPS_OUTPUT_DIR}" = /* ]]; then
        OUTPUT_DIR=${DEBIAN_APPS_OUTPUT_DIR}
    else
        OUTPUT_DIR=${REPO_ROOT}/${DEBIAN_APPS_OUTPUT_DIR}
    fi
else
    OUTPUT_DIR=${DEFAULT_OUTPUT_DIR}
fi
readonly OUTPUT_DIR
readonly BUILD_IMAGE=${DEBIAN_APPS_BUILD_IMAGE:-aiden-debian13-armhf-builder:apps}
readonly OPENCV_ARCHIVE=opencv-mobile-4.13.0.zip
readonly OPENCV_URL=https://github.com/nihui/opencv-mobile/releases/download/v35/${OPENCV_ARCHIVE}
readonly OPENCV_SHA256=9304482980b3e4ff1050a8527cdb5777fadf8c5dd9c1a8620170d23e252fb150
readonly JOBS=${RK_JOBS:-$(getconf _NPROCESSORS_ONLN)}
readonly GO_ROOT=${DEBIAN_APPS_GO_ROOT:-${REPO_ROOT}/.toolchains/go1.26.0.linux-amd64}
readonly GO_BUILD_CACHE=${DEBIAN_APPS_GO_BUILD_CACHE:-${REPO_ROOT}/.cache/debian-apps/go-build}
readonly GO_MODULE_CACHE=${DEBIAN_APPS_GO_MODULE_CACHE:-${REPO_ROOT}/.cache/debian-apps/go-mod}
readonly OPENCV_SOURCE_CACHE=${DEBIAN_APPS_OPENCV_CACHE:-${REPO_ROOT}/.cache/debian-apps/opencv}
readonly BUILD_EPOCH=${SOURCE_DATE_EPOCH:-1767360516}

usage() {
    cat <<'EOF'
Usage: scripts/debian-apps/build-apps.sh [all|builder|opencv|apps|cli-tools|audit]

Environment:
  DEBIAN_APPS_OUTPUT_DIR    Output directory (defaults to output/debian-apps).
  DEBIAN_APPS_BUILD_IMAGE   Docker image name for the Debian armhf toolchain.
  DEBIAN_APPS_GO_ROOT       Pinned Go 1.26.0 linux/amd64 toolchain.
  DEBIAN_APPS_GO_BUILD_CACHE/DEBIAN_APPS_GO_MODULE_CACHE
                            Persistent writable Go caches.
  DEBIAN_APPS_OPENCV_CACHE  Persistent OpenCV-Mobile source archive cache.
  SOURCE_DATE_EPOCH         Reproducible Go build timestamp.
  RK_JOBS                   Parallel build jobs (defaults to all host CPUs).
EOF
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "Required command not found: $1" >&2
        exit 1
    }
}

ensure_opencv_source() {
    local archive=${OPENCV_SOURCE_CACHE}/${OPENCV_ARCHIVE}
    local staging_dir=${OUTPUT_DIR}/cache
    local actual
    mkdir -p "${OPENCV_SOURCE_CACHE}" "${staging_dir}"
    if [ ! -f "${archive}" ]; then
        local temp=${archive}.tmp.$$
        curl -fL --retry 3 --connect-timeout 20 -o "${temp}" "${OPENCV_URL}"
        mv "${temp}" "${archive}"
    fi
    actual=$(sha256sum "${archive}" | awk '{print $1}')
    if [ "${actual}" != "${OPENCV_SHA256}" ]; then
        # The cache now outlives the build, so a bad archive would fail every
        # later run too. Drop it and let the next build fetch a fresh copy.
        rm -f "${archive}"
        echo "OpenCV-Mobile source checksum mismatch" >&2
        echo "expected: ${OPENCV_SHA256}" >&2
        echo "actual:   ${actual}" >&2
        echo "discarded: ${archive}" >&2
        exit 1
    fi
    # container-build-opencv-mobile.sh reads the archive from the bind-mounted
    # output directory, which clean_generated_outputs() empties before every
    # build. Stage a copy there instead of caching it there.
    cp -f "${archive}" "${staging_dir}/${OPENCV_ARCHIVE}"
}

run_builder() {
    docker build -t "${BUILD_IMAGE}" -f "${SCRIPT_DIR}/Dockerfile" "${REPO_ROOT}"
    docker image inspect "${BUILD_IMAGE}" --format '{{.Id}}' \
        >"${OUTPUT_DIR}/builder-image-id.txt"
}

# Run an application build script with the pinned Go toolchain and writable
# caches. Preserve the caller's GOPROXY when configured, including fallback
# separators, while leaving the container default in place when it is unset.
run_container_script() {
    local script=$1
    local image_id source_git_common_dir
    shift
    image_id=$(docker image inspect "${BUILD_IMAGE}" --format '{{.Id}}')
    source_git_common_dir=$(git -C "${REPO_ROOT}" rev-parse \
        --path-format=absolute --git-common-dir 2>/dev/null || true)
    if [ ! -x "${GO_ROOT}/bin/go" ] || [ ! -f "${GO_ROOT}/VERSION" ] ||
        ! grep -qx 'go1.26.0' "${GO_ROOT}/VERSION"; then
        echo "Pinned Go 1.26.0 toolchain is missing: ${GO_ROOT}" >&2
        exit 1
    fi
    case "${BUILD_EPOCH}" in
    '' | *[!0-9]*)
        echo "SOURCE_DATE_EPOCH must be an unsigned Unix timestamp: ${BUILD_EPOCH}" >&2
        exit 1
        ;;
    esac
    mkdir -p "${GO_BUILD_CACHE}" "${GO_MODULE_CACHE}"
    # Forward the module-fetch configuration only. The bare `-e NAME` form leaves
    # a variable undefined in the container when the host has not set it, so the
    # image defaults still apply. Without this the runner's GOPROXY stops at the
    # container boundary and `go install module@version` reaches for
    # proxy.golang.org, which the self-hosted builders cannot resolve. Build
    # inputs that would change output (GOFLAGS) and the container-local
    # GOCACHE/GOMODCACHE/GOPATH/GOTOOLCHAIN are deliberately not forwarded.
    # Git worktree metadata resolves paths through the host common directory
    # when it is available. Dockerized Delta worktrees can
    # expose only the worktree, so the metadata mount is optional there.
    # Preserve the checkout's absolute path as well as the /work build alias.
    local -a docker_args=(
        docker run --rm
        -u "$(id -u):$(id -g)"
        -e "DEBIAN_APPS_OUTPUT_DIR=/out"
        -e "DEBIAN_APPS_BUILD_IMAGE_ID=${image_id}"
        -e "RK_JOBS=${JOBS}"
        -e "SOURCE_DATE_EPOCH=${BUILD_EPOCH}"
        -e GOPROXY
        -e GONOPROXY
        -e GOPRIVATE
        -e GOSUMDB
        -e GONOSUMDB
        -v "${REPO_ROOT}:/work"
        -v "${REPO_ROOT}:${REPO_ROOT}:ro"
        -v "${OUTPUT_DIR}:/out"
        -v "${GO_ROOT}:/usr/local/go:ro"
        -v "${GO_BUILD_CACHE}:/go-build-cache"
        -v "${GO_MODULE_CACHE}:/go-mod-cache"
        -w /work
    )
    if [ -n "${source_git_common_dir}" ]; then
        docker_args+=(
            -v "${source_git_common_dir}:${source_git_common_dir}:ro"
        )
    fi
    "${docker_args[@]}" "${BUILD_IMAGE}" bash "${script}" "$@"
}

run_opencv() {
    ensure_opencv_source
    run_container_script scripts/debian-apps/container-build-opencv-mobile.sh
}

run_apps() {
    run_container_script scripts/debian-apps/container-build-apps.sh
}

run_cli_tools() {
    run_container_script scripts/debian-apps/container-build-rootfs-cli-tools.sh
}

run_audit() {
    run_container_script scripts/debian-apps/audit-apps.sh \
        /out/apps /out/apps-audit
}

main() {
    local action=${1:-all}
    case "${action}" in
    -h | --help | help)
        usage
        return
        ;;
    all | builder | opencv | apps | cli-tools | audit)
        ;;
    *)
        usage >&2
        exit 2
        ;;
    esac

    require_command docker
    require_command git
    require_command sha256sum
    mkdir -p "${OUTPUT_DIR}"

    case "${action}" in
    all)
        require_command curl
        run_builder
        run_opencv
        run_apps
        run_cli_tools
        run_audit
        ;;
    builder)
        run_builder
        ;;
    opencv)
        require_command curl
        run_opencv
        ;;
    apps)
        run_apps
        ;;
    cli-tools)
        run_cli_tools
        ;;
    audit)
        run_audit
        ;;
    esac
}

main "$@"
