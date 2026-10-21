#!/bin/sh
# Regenerate the Wi-Fi country allowlist from the AIC8800 driver's regulatory
# database in the vendored pico-sdk tree. Run from the repository root.
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
REGDB=${ROOT_DIR}/pico-sdk/sysdrv/drv_ko/wifi/aic8800dc/aic8800_fdrv/regdb.c
OUT=${ROOT_DIR}/src/agent/internal/wifiregion/countries.go

test -f "${REGDB}" || {
    echo "driver regulatory database is missing: ${REGDB}" >&2
    echo "restore the vendored pico-sdk sources from this repository" >&2
    exit 1
}

{
    cat <<'EOF'
package wifiregion

// Code generated from the AIC8800 driver regulatory database. DO NOT EDIT.
//
// Regenerate with:
//
//	scripts/gen_wifi_countries.sh
//
// This set is the upper bound of what the driver and firmware recognize. The
// list actually offered to users should be narrowed to the regions the product
// is certified for; narrow it here rather than in the user interface, because
// the server revalidates every submitted code against this table.
var supportedCountries = map[string]struct{}{
EOF
    grep -o '\.alpha2 = "[A-Z0-9][A-Z0-9]"' "${REGDB}" \
        | sed 's/.*"\(..\)"/\1/' \
        | sort -u \
        | awk '{printf "\t\"%s\": {},\n", $1}'
    echo '}'
} >"${OUT}"

echo "wrote ${OUT} ($(grep -c '{},' "${OUT}") entries)"
