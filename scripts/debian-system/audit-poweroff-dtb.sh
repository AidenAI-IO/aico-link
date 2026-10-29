#!/usr/bin/env bash
set -euo pipefail

dtb=${1:?usage: audit-poweroff-dtb.sh FILE.dtb}
fail() { echo "Poweroff DTB audit failure: $*" >&2; exit 1; }

# Inspect the compiled tree, including inherited nodes. Checking only the
# board's DTS text missed restart-poweroff inherited from rv1106-evb.dtsi.
audit_node() {
    local node=$1 children compatible child
    children=$(fdtget -l "${dtb}" "${node}") || fail "cannot inspect ${node}"
    compatible=$(fdtget -t s -d '' "${dtb}" "${node}" compatible) \
        || fail "cannot read ${node}/compatible"
    case " ${compatible} " in
        *' restart-poweroff '*)
            fail "restart-poweroff must be removed from ${node}; the MCU owns power removal"
            ;;
    esac
    for child in ${children}; do
        audit_node "${node%/}/${child}"
    done
}
audit_node /
echo 'Poweroff DTB audit passed (no restart-poweroff fallback).'
