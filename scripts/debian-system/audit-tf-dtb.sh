#!/usr/bin/env bash
set -euo pipefail

dtb=${1:?usage: audit-tf-dtb.sh FILE.dtb}
fail() { echo "TF DTB audit failure: $*" >&2; exit 1; }
expect() {
    local actual
    actual=$(fdtget -t "$1" "${dtb}" "$2" "$3") || fail "missing $2/$3"
    [ "${actual}" = "$4" ] || fail "$2/$3: expected '$4', got '${actual}'"
}
phandle() { fdtget -t u "${dtb}" "$1" phandle; }
absent() {
    if fdtget -p "${dtb}" "$1" | grep -qx "$2"; then
        fail "unexpected $1/$2"
    fi
}

host=/mmc@ffaa0000
rail=/vcc3v3-sd-regulator
pins=/pinctrl
expect s /aliases mmc0 /mmc@ffa90000
expect s /aliases mmc1 "${host}"
expect s /aliases mmc2 /mmc@ff9a0000
expect u /mmc@ffa90000 bus-width 8
expect u /mmc@ff9a0000 bus-width 4
expect s "${host}" status okay
expect u "${host}" bus-width 1
expect u "${host}" max-frequency 25000000
absent "${host}" rockchip,tf-diagnostics
expect s "${host}" pinctrl-names 'default normal idle'
expect u "${host}" vmmc-supply "$(phandle "${rail}")"
expect u "${host}" vqmmc-supply "$(phandle /vcc-3v3)"
expect u "${host}" cd-gpios "$(phandle /pinctrl/gpio@ff550000) 1 1"
expect u "${host}" post-power-on-delay-ms 20
expect u "${host}" power-off-delay-ms 200
normal="$(phandle "${pins}/sdmmc0/sdmmc0-clk") $(phandle "${pins}/sdmmc0/sdmmc0-cmd") $(phandle "${pins}/sdmmc0/sdmmc0-bus4") $(phandle "${pins}/tf-card/sdmmc0-card-detect")"
expect u "${host}" pinctrl-0 "${normal}"
expect u "${host}" pinctrl-1 "${normal}"
expect u "${host}" pinctrl-2 "$(phandle "${pins}/sdmmc0/sdmmc0-idle-pins") $(phandle "${pins}/tf-card/sdmmc0-card-detect")"
expect u "${rail}" gpio "$(phandle /pinctrl/gpio@ff380000) 1 0"
expect u "${rail}" regulator-min-microvolt 3300000
expect u "${rail}" regulator-max-microvolt 3300000
expect u "${rail}" vin-supply "$(phandle /vcc-3v3)"
expect u "${rail}" pinctrl-0 "$(phandle "${pins}/tf-card/sdmmc0-power-en")"
expect u "${rail}" startup-delay-us 10000
expect u "${rail}" off-on-delay-us 200000
absent "${rail}" regulator-always-on
expect u "${pins}/tf-card/sdmmc0-card-detect" rockchip,pins "3 1 0 $(phandle "${pins}/pcfg-pull-up")"
expect u "${pins}/tf-card/sdmmc0-power-en" rockchip,pins "0 1 0 $(phandle "${pins}/pcfg-pull-none")"
for property in no-sdio no-mmc; do
    fdtget -p "${dtb}" "${host}" | grep -qx "${property}" || fail "missing ${property}"
done
fdtget -p "${dtb}" "${rail}" | grep -qx enable-active-high || fail 'TF supply is not active high'
expect u "${pins}/sdmmc0/sdmmc0-idle-pins" rockchip,pins \
    "3 2 0 $(phandle "${pins}/pcfg-pull-down") 3 3 0 $(phandle "${pins}/pcfg-pull-down") 3 4 0 $(phandle "${pins}/pcfg-pull-down") 3 5 0 $(phandle "${pins}/pcfg-pull-down") 3 6 0 $(phandle "${pins}/pcfg-pull-down") 3 7 0 $(phandle "${pins}/pcfg-pull-down")"
echo 'TF DTB audit passed (SDMMC0, 1-bit, 25 MHz, switchable 3.3 V, normal/idle; eMMC 8-bit, Wi-Fi 4-bit).'
