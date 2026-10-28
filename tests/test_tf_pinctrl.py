#!/usr/bin/env python3
"""Exercise the SDK's SDMMC/JTAG route lookup and masked register value."""

from pathlib import Path
import re
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]
KERNEL = ROOT / "pico-sdk/sysdrv/source/kernel"
DRIVER = KERNEL / "drivers/pinctrl/pinctrl-rockchip.c"


def definition(source, pattern):
    match = re.search(pattern, source, re.MULTILINE | re.DOTALL)
    if match is None:
        raise AssertionError(f"Missing pinctrl definition: {pattern}")
    return match.group(0) + "\n"


def main():
    source = DRIVER.read_text()
    header = DRIVER.with_suffix(".h").read_text()
    prelude = r"""
#include <assert.h>
#include <stdbool.h>
#include <stdint.h>
#include <dt-bindings/pinctrl/rockchip.h>
typedef uint8_t u8;
typedef uint32_t u32;
#define BIT(n) (1U << (n))
#define GENMASK(h, l) ((~0U << (l)) & (~0U >> (31 - (h))))
#define ARRAY_SIZE(a) (sizeof(a) / sizeof((a)[0]))
"""
    for name in ("WRITE_MASK_VAL", "PIN_BANK_MUX_ROUTE_FLAGS", "RK_MUXROUTE_GRF"):
        prelude += definition(source, rf"^#define {name}\([^\n]*\n(?:[^\n]*\\\n)*[^\n]*")
    prelude += definition(header, r"^enum rockchip_mux_route_location \{.*?^\};")
    prelude += definition(header, r"^struct rockchip_mux_route_data \{.*?^\};")
    prelude += r"""
struct rockchip_pin_ctrl {
    struct rockchip_mux_route_data *iomux_routes;
    u32 niomux_routes;
};
struct rockchip_pinctrl { struct rockchip_pin_ctrl *ctrl; };
struct rockchip_pin_bank {
    u8 bank_num;
    struct rockchip_pinctrl *drvdata;
};
"""
    prelude += definition(source, r"^static struct rockchip_mux_route_data rv1106_sdmmc_mux_route_data\[\] = \{.*?^\};")
    # Use the controller's actual route wiring, so an unregistered table fails.
    controller = definition(source, r"^static struct rockchip_pin_ctrl rv1106_pin_ctrl .*?^\};")
    prelude += "static struct rockchip_pin_ctrl controller = {\n"
    for field in ("iomux_routes", "niomux_routes"):
        prelude += definition(controller, rf"^\s*\.{field}\s*= [^\n]+")
    prelude += "};\n"
    prelude += definition(source, r"^static bool rockchip_get_mux_route\(.*?^\}")
    harness = prelude + r"""
int main(void)
{
    struct rockchip_pinctrl info = { .ctrl = &controller };
    struct rockchip_pin_bank bank = { .drvdata = &info };
    int pin, mux;

    /* Only SDMMC DAT2/DAT3 may disable JTAG: GPIO, explicit JTAG, card
     * detect, eMMC and Wi-Fi selections must not match these routes. */
    for (bank.bank_num = 0; bank.bank_num < 5; ++bank.bank_num) {
        for (pin = 0; pin < 32; ++pin) {
            for (mux = 0; mux < 16; ++mux) {
                u32 loc = 0, reg = 0, value = 0;
                bool expected = bank.bank_num == 3 &&
                                (pin == 6 || pin == 7) && mux == 1;
                bool found = rockchip_get_mux_route(&bank, pin, mux,
                                                     &loc, &reg, &value);
                assert(found == expected);
                if (!found)
                    continue;

                assert(loc == ROCKCHIP_ROUTE_GRF);
                assert(0xff538000U + reg == 0xff5582f4U);
                assert(value == 0x00010000U);
                /* Model the IOC hiword mask for every possible old value:
                 * clear auto-switch bit 0, preserving all other bits. */
                for (u32 old = 0; old <= 0xffff; ++old) {
                    u32 mask = value >> 16;
                    u32 updated = (old & ~mask) | (value & mask);
                    assert(updated == (old & ~1U));
                    assert(((updated & ~mask) | (value & mask)) == updated);
                }
            }
        }
    }
    return 0;
}
"""
    with tempfile.TemporaryDirectory(prefix="aico-tf-pinctrl-") as temporary:
        work = Path(temporary)
        program = work / "pinctrl.c"
        executable = work / "pinctrl"
        program.write_text(harness)
        subprocess.run([
            "gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
            "-Wno-sign-compare", "-I", str(KERNEL / "include"),
            str(program), "-o", str(executable),
        ], check=True)
        subprocess.run([str(executable)], check=True)
    print("TF pinctrl regression passed (DAT2/DAT3, JTAG mask, unrelated pins).")


if __name__ == "__main__":
    main()
