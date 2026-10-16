#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
kernel=${repo_root}/pico-sdk/sysdrv/source/kernel
work_dir=$(mktemp -d)
trap 'rm -rf "${work_dir}"' EXIT
gcc -E -nostdinc -undef -D__DTS__ -x assembler-with-cpp \
    -I "${kernel}/include" -I "${kernel}/arch/arm/boot/dts" \
    "${kernel}/arch/arm/boot/dts/rv1106g-aiden-custom.dts" \
    -o "${work_dir}/board.dts"
dtc -q -I dts -O dtb -o "${work_dir}/board.dtb" "${work_dir}/board.dts"
audit=${repo_root}/scripts/debian-system/audit-tf-dtb.sh
bash "${audit}" "${work_dir}/board.dtb"
for property in bus-width max-frequency pinctrl-2 vqmmc-supply; do
    cp "${work_dir}/board.dtb" "${work_dir}/bad.dtb"
    fdtput -d "${work_dir}/bad.dtb" /mmc@ffaa0000 "${property}"
    if bash "${audit}" "${work_dir}/bad.dtb" >"${work_dir}/failure.log" 2>&1; then
        echo "TF audit accepted missing ${property}" >&2
        exit 1
    fi
done
for width in 4 8; do
    cp "${work_dir}/board.dtb" "${work_dir}/bad.dtb"
    fdtput -t u "${work_dir}/bad.dtb" /mmc@ffaa0000 bus-width "${width}"
    if bash "${audit}" "${work_dir}/bad.dtb" >"${work_dir}/failure.log" 2>&1; then
        echo "TF audit accepted ${width}-bit mode" >&2
        exit 1
    fi
done
cp "${work_dir}/board.dtb" "${work_dir}/bad.dtb"
fdtput -t u "${work_dir}/bad.dtb" /mmc@ffaa0000 max-frequency 50000000
if bash "${audit}" "${work_dir}/bad.dtb" >"${work_dir}/failure.log" 2>&1; then
    echo 'TF audit accepted a 50 MHz limit' >&2
    exit 1
fi
for entry in '/mmc@ffaa0000 rockchip,tf-diagnostics' '/vcc3v3-sd-regulator regulator-always-on'; do
    read -r node property <<<"${entry}"
    cp "${work_dir}/board.dtb" "${work_dir}/bad.dtb"
    fdtput "${work_dir}/bad.dtb" "${node}" "${property}"
    if bash "${audit}" "${work_dir}/bad.dtb" >"${work_dir}/failure.log" 2>&1; then
        echo "TF audit accepted ${node}/${property}" >&2
        exit 1
    fi
done
grep -qx exfatprogs "${repo_root}/scripts/debian-system/packages.list"
echo 'TF DTB regression tests passed.'
