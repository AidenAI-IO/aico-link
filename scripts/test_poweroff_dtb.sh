#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
kernel=${repo_root}/pico-sdk/sysdrv/source/kernel
work_dir=$(mktemp -d)
trap 'rm -rf "${work_dir}"' EXIT
gcc -E -nostdinc -undef -D__DTS__ -x assembler-with-cpp \
    -I "${kernel}/include" -I "${kernel}/arch/arm/boot/dts" \
    "${kernel}/arch/arm/boot/dts/rv1106g-aico-custom.dts" \
    -o "${work_dir}/board.dts"
dtc -q -I dts -O dtb -o "${work_dir}/board.dtb" "${work_dir}/board.dts"
audit=${repo_root}/scripts/debian-system/audit-poweroff-dtb.sh
bash "${audit}" "${work_dir}/board.dtb"

# Reproduce the EVB include regression, a renamed node and a nested node.
# The driver binds by compatible case-insensitively, including list entries.
for node in /restart-poweroff /power-controller /pinctrl/power-controller; do
    for compatible in restart-poweroff Restart-Poweroff RESTART-POWEROFF; do
        cp "${work_dir}/board.dtb" "${work_dir}/bad.dtb"
        fdtput -c "${work_dir}/bad.dtb" "${node}"
        fdtput -t s "${work_dir}/bad.dtb" "${node}" compatible vendor,poweroff "${compatible}"
        if bash "${audit}" "${work_dir}/bad.dtb" >"${work_dir}/failure.log" 2>&1; then
            echo "Poweroff audit accepted ${compatible} at ${node}" >&2
            exit 1
        fi
        grep -Fq 'restart-poweroff must be removed' "${work_dir}/failure.log"
    done
done

printf 'invalid DTB\n' >"${work_dir}/invalid.dtb"
for file in invalid.dtb missing.dtb; do
    if bash "${audit}" "${work_dir}/${file}" >"${work_dir}/failure.log" 2>&1; then
        echo "Poweroff audit accepted ${file}" >&2
        exit 1
    fi
    grep -Fq 'cannot inspect /' "${work_dir}/failure.log"
done
echo 'Poweroff DTB regression tests passed.'
