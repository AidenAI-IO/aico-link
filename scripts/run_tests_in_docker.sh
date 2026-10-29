#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly IMAGE="${AICO_TEST_IMAGE:-aico-link-firmware-test:local}"
readonly PLATFORM=linux/amd64
readonly CACHE_DIR="${AICO_TEST_CACHE_DIR:-${REPO_ROOT}/.cache/docker-test}"
readonly SUMMARY_PATH="${AICO_TEST_SUMMARY:-${REPO_ROOT}/output/test-summary.json}"

usage() {
    cat <<'EOF'
Usage: scripts/run_tests_in_docker.sh [options]

Build the pinned test image and run tests/test-manifest.yaml inside it.

Manifest options are passed to tests/run_manifest.py:
  --profile quick|full  Quick local feedback or full CI gate (default: full)
  --suite NAME       Run only one manifest suite (repeatable)
  --class NAME       Run only suites in a manifest class
  --list             List suites without running them
  --production       Promote production-cross-smoke to a required suite
  --docker-socket    Expose the host Docker socket for an explicitly Docker-backed suite
  --host-network     Use the host network for the Docker sandbox smoke suite (requires --docker-socket)

Environment:
  AICO_TEST_IMAGE   Docker image tag (default: aico-link-firmware-test:local)
  AICO_TEST_JOBS    Parallel build jobs inside the container
  AICO_TEST_SUMMARY JSON summary output path
  AICO_TEST_SKIP_BUILD  Use an image built by CI's persistent BuildKit cache
EOF
}

manifest_args=()
require_production=0
use_docker_socket=0
use_host_network=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --production)
            require_production=1
            shift
            ;;
        --docker-socket)
            use_docker_socket=1
            shift
            ;;
        --host-network)
            use_host_network=1
            shift
            ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            manifest_args+=("$1")
            shift
            ;;
    esac
done

if [ "$use_host_network" -eq 1 ] && [ "$use_docker_socket" -ne 1 ]; then
    echo "--host-network requires --docker-socket" >&2
    exit 2
fi

command -v docker >/dev/null 2>&1 || {
    echo "Docker is required; host test tools are intentionally not used" >&2
    exit 1
}
docker info >/dev/null 2>&1 || {
    echo "Docker daemon is unavailable" >&2
    exit 1
}

mkdir -p "$CACHE_DIR" "$(dirname "$SUMMARY_PATH")"

if [ "${AICO_TEST_SKIP_BUILD:-0}" = "1" ]; then
    docker image inspect "$IMAGE" >/dev/null 2>&1 || {
        echo "AICO_TEST_SKIP_BUILD=1 but Docker image is missing: $IMAGE" >&2
        exit 1
    }
else
    docker build \
        --platform "$PLATFORM" \
        --file "$REPO_ROOT/docker/test/Dockerfile" \
        --tag "$IMAGE" \
        "$REPO_ROOT"
fi

if [ "$require_production" -eq 1 ]; then
    manifest_args+=(--require-suite production-cross-smoke)
fi

uid=$(id -u)
gid=$(id -g)
case "$SUMMARY_PATH" in
    "$REPO_ROOT"/*) ;;
    *)
        echo "AICO_TEST_SUMMARY must be inside the repository: $SUMMARY_PATH" >&2
        exit 1
        ;;
esac

docker_args=(
    run --rm
    --platform "$PLATFORM"
    --init
    --user "${uid}:${gid}"
    --env HOME=/tmp/aico-home
    --env AICO_TEST_JOBS="${AICO_TEST_JOBS:-2}"
    --env GOCACHE=/tmp/aico-cache/go-build
    --env GOMODCACHE=/tmp/aico-cache/go-mod
    --env GOPATH=/tmp/aico-cache/go-path
    --env UV_CACHE_DIR=/tmp/aico-cache/uv
    --env AICO_TEST_HOST_NETWORK="$use_host_network"
    --volume "$REPO_ROOT:$REPO_ROOT"
    --volume "$CACHE_DIR:/tmp/aico-cache"
    --workdir "$REPO_ROOT"
)
# Do not expose the host daemon to ordinary PR test commands. A suite that
# explicitly needs sibling containers must opt in, so the privilege boundary is
# visible at the call site.
if [ "$use_docker_socket" -eq 1 ]; then
    docker_socket_source=/var/run/docker.sock
    if [ -L "$docker_socket_source" ]; then
        docker_socket_source=$(readlink "$docker_socket_source")
    fi
    [ -S "$docker_socket_source" ] || {
        echo "--docker-socket was requested but the Docker socket is unavailable" >&2
        exit 1
    }
    docker_args+=(--volume "$docker_socket_source:/var/run/docker.sock")
    socket_gid=$(stat -c '%g' "$docker_socket_source" 2>/dev/null || stat -f '%g' "$docker_socket_source")
    # Docker Desktop presents bind-mounted sockets as root:root even if the
    # host socket belongs to another group. Keep files owned by the caller.
    docker_args+=(--group-add 0 --group-add "$socket_gid")
fi
if [ "$use_host_network" -eq 1 ]; then
    docker_args+=(--network host)
fi

runner_args=(
    "$IMAGE"
    python3 tests/run_manifest.py
    --summary "$SUMMARY_PATH"
)
if [ "${#manifest_args[@]}" -gt 0 ]; then
    runner_args+=("${manifest_args[@]}")
fi

docker "${docker_args[@]}" "${runner_args[@]}"
