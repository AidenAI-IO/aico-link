#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
image=${DEBIAN_PACKAGE_TEST_IMAGE:-aico-debian-config-test}
docker build -t "$image" -f "$root/scripts/debian-package/Dockerfile" "$root"
docker run --rm --network=none -v "$root:/work:ro" -w /work "$image" \
    python3 scripts/test_system_config_package.py
