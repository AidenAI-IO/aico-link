#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
destination=${1:?Usage: write-maintainer-scripts.sh DEBIAN_DIRECTORY}
mkdir -p "${destination}"
contract_check=$(mktemp)
config_hook=$(mktemp)
trap 'rm -f "${contract_check}" "${config_hook}"' EXIT
python3 "${script_dir}/../release/contract.py" preinst-check "${contract_check}"
python3 - "${script_dir}" "${config_hook}" <<'PY'
import pathlib, sys
directory = pathlib.Path(sys.argv[1])
sys.path.insert(0, str(directory))
from system_config import inventory
code = (directory / "config-transition.py").read_text()
code = code.replace("NEW_FILES = {}", "NEW_FILES = " + repr(inventory()), 1)
pathlib.Path(sys.argv[2]).write_text(code)
PY
for phase in preinst postinst prerm postrm; do
    {
        printf '#!/bin/sh\nset -eu\nphase=%s\n' "${phase}"
        if [ "${phase}" = preinst ]; then cat "${contract_check}"; fi
        printf 'runtime_config() {\npython3 - "$@" <<\x27AICO_RUNTIME_CONFIG\x27\n'
        cat "${config_hook}"
        printf '\nAICO_RUNTIME_CONFIG\n}\n'
        cat "${script_dir}/maintainer-script.sh"
    } > "${destination}/${phase}"
    chmod 0755 "${destination}/${phase}"
done
