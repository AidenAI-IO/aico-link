#!/usr/bin/env bash
# Invoked inside the pinned test image by the web manifest suite.
set -euo pipefail
source_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "${test_dir}"' EXIT
tar -C "${source_root}/src/config_web" --exclude=node_modules --exclude=dist --exclude=coverage -cf - . \
    | tar -C "${test_dir}" -xf -
cd "${test_dir}"
npm ci --ignore-scripts --no-audit --no-fund
npm run check
