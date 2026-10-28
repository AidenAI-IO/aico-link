#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
uboot=${repo_root}/pico-sdk/sysdrv/source/uboot/u-boot
work_dir=$(mktemp -d)
trap 'rm -rf "${work_dir}"' EXIT

gcc -E -nostdinc -undef -D__DTS__ -x assembler-with-cpp \
    -I "${uboot}/include" -I "${uboot}/arch/arm/dts" \
    "${uboot}/arch/arm/dts/rv1106-aico-tf.dts" \
    -o "${work_dir}/board.dts"
dtc -q -I dts -O dtb -o "${work_dir}/board.dtb" "${work_dir}/board.dts"
dtb=${work_dir}/board.dtb
python3 "${repo_root}/tests/test_tf_uboot_card_detect.py" "${dtb}"
[ "$(fdtget -t s "${dtb}" /mmc@ffa90000 status)" = disabled ]
[ "$(fdtget -t s "${dtb}" /mmc@ffaa0000 status)" = okay ]
[ "$(fdtget -t u "${dtb}" /mmc@ffaa0000 bus-width)" = 4 ]
[ "$(fdtget -t u "${dtb}" /mmc@ffaa0000 max-frequency)" = 50000000 ]
[[ "$(fdtget -t u "${dtb}" /mmc@ffaa0000 pwr-en-gpios)" =~ [[:space:]]1[[:space:]]0$ ]]
# The vendor driver inverts the raw detect level; the electrical signal is low
# when inserted. The executable regression above checks both GPIO levels.
[[ "$(fdtget -t u "${dtb}" /mmc@ffaa0000 cd-gpios)" =~ [[:space:]]1[[:space:]]0$ ]]
grep -Fq 'ret = regmap_write(priv->regmap_base, 0x202f4, 0x00010000)' \
    "${uboot}/drivers/pinctrl/rockchip/pinctrl-rv1106.c"
echo 'TF U-Boot DTB and early pinctrl contract passed.'
