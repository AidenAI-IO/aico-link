#!/usr/bin/env bash
set -euo pipefail

source_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output_dir=${1:-"${source_root}/src/config_web/dist"}
mkdir -p "${output_dir}"
output_dir=$(cd "${output_dir}" && pwd)
staging=$(mktemp -d "${output_dir}.build.XXXXXX")
image_id=$(mktemp)
cleanup() { rm -f "${image_id}"; rm -rf "${staging}"; }
trap cleanup EXIT
docker build --iidfile "${image_id}" -f "${source_root}/src/config_web/Dockerfile" \
    "${source_root}/src/config_web"
docker run --rm --user "$(id -u):$(id -g)" -v "${staging}:/out" "$(cat "${image_id}")" \
    sh -c 'cp -a --no-preserve=ownership /app/dist/. /out/'
# The output directory is dedicated to generated assets. Replace it only after
# a successful build, so a failed build cannot remove the last usable frontend.
rsync -a --delete "${staging}/" "${output_dir}/"
