#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT=/work
readonly OUTPUT_DIR=${DEBIAN_APPS_OUTPUT_DIR:-/out}

export PATH=/usr/local/go/bin:${PATH}
export GOTOOLCHAIN=local
export GOCACHE=/go-build-cache
export GOMODCACHE=/go-mod-cache
export GOPATH=/tmp/aico-rootfs-cli-go

exec "${REPO_ROOT}/scripts/build_rootfs_cli_tools.sh" \
    --catalog "${REPO_ROOT}/scripts/rootfs_cli_tools.catalog" \
    --output-dir "${OUTPUT_DIR}/rootfs-cli-tools" \
    --cache-dir "${OUTPUT_DIR}/cache/rootfs-cli-tools"
