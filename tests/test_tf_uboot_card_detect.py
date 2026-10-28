#!/usr/bin/env python3
"""Exercise the vendor card-detect code with the compiled TF board's GPIO flag."""

from pathlib import Path
import re
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[1]
UBOOT = ROOT / "pico-sdk/sysdrv/source/uboot/u-boot"


def function(path, name):
    source = path.read_text()
    match = re.search(rf"^(?:static )?int {name}\(.*?^\}}", source, re.M | re.S)
    if match is None:
        raise AssertionError(f"Missing function {name} in {path}")
    return match.group(0) + "\n"


def main():
    gpio = subprocess.check_output([
        "fdtget", "-t", "u", sys.argv[1], "/mmc@ffaa0000", "cd-gpios",
    ], text=True).split()
    assert len(gpio) == 3 and int(gpio[1]) == 1, gpio
    active_low = int(gpio[2]) & 1
    harness = r"""
#include <assert.h>
#include <stdbool.h>
#include <string.h>
#define GPIOD_ACTIVE_LOW 1
#define GPIOD_IS_IN 2
#define DWMCI_CDETECT 0
struct dwmci_host { unsigned int detect; };
struct mmc { struct dwmci_host *priv; };
struct udevice { struct mmc *mmc; };
struct gpio_desc { struct udevice *dev; unsigned int offset, flags; };
static int raw_level, request_error, request_count, free_count;
static struct mmc *mmc_get_mmc_dev(struct udevice *dev) { return dev->mmc; }
static unsigned int dwmci_readl(struct dwmci_host *host, int reg)
{ (void)reg; return host->detect; }
static int check_reserved(const struct gpio_desc *desc, const char *name)
{ (void)desc; (void)name; return 0; }
static int read_gpio(struct udevice *dev, unsigned int offset)
{ (void)dev; assert(offset == 1); return raw_level; }
static struct { int (*get_value)(struct udevice *, unsigned int); } gpio_ops = {
    .get_value = read_gpio,
};
#define gpio_get_ops(dev) (&gpio_ops)
static int gpio_request_by_name(struct udevice *dev, const char *name,
                                int index, struct gpio_desc *desc, int flags)
{
    assert(strcmp(name, "cd-gpios") == 0 && index == 0);
    assert(flags == GPIOD_IS_IN);
    if (request_error) return request_error;
    desc->dev = dev; desc->offset = 1;
    desc->flags = BOARD_ACTIVE_LOW ? GPIOD_ACTIVE_LOW : 0;
    ++request_count;
    return 0;
}
static void dm_gpio_free(struct udevice *dev, struct gpio_desc *desc)
{ assert(desc->dev == dev); ++free_count; }
"""
    harness += function(UBOOT / "drivers/gpio/gpio-uclass.c", "dm_gpio_get_value")
    harness += function(UBOOT / "drivers/mmc/dw_mmc.c", "dwmci_get_cd")
    harness += r"""
int main(void)
{
    struct dwmci_host host = {0};
    struct mmc mmc = { .priv = &host };
    struct udevice dev = { .mmc = &mmc };

    /* SCH v1 J4 detect is physically low with a card inserted. Check
     * repeated insert/remove transitions and the opposite controller bit
     * to prove the GPIO path determines presence. */
    for (int repeat = 0; repeat < 3; ++repeat) {
        for (raw_level = 0; raw_level <= 1; ++raw_level) {
            host.detect = !raw_level;
            assert(dwmci_get_cd(&dev) == (raw_level == 0));
            assert(request_count == free_count);
        }
    }
    assert(request_count == 6);

    /* A missing GPIO uses the controller's active-low detect register. */
    request_error = -2;
    host.detect = 0;
    assert(dwmci_get_cd(&dev) == 1);
    host.detect = 1;
    assert(dwmci_get_cd(&dev) == 0);
    assert(request_count == 6 && free_count == 6);
    return 0;
}
"""
    with tempfile.TemporaryDirectory(prefix="aico-tf-card-detect-") as temporary:
        work = Path(temporary)
        source = work / "card-detect.c"
        source.write_text(harness)
        for stage in ("uboot", "spl"):
            executable = work / stage
            flags = ["-DCONFIG_DM_GPIO=1", f"-DBOARD_ACTIVE_LOW={active_low}"]
            if stage == "spl":
                flags += ["-DCONFIG_SPL_BUILD=1", "-DCONFIG_SPL_GPIO_SUPPORT=1"]
            subprocess.run([
                "gcc", "-std=c11", "-Wall", "-Wextra", "-Werror", *flags,
                str(source), "-o", str(executable),
            ], check=True)
            subprocess.run([str(executable)], check=True)
    print("TF SPL/U-Boot card detect passed (inserted, removed, GPIO fallback).")


if __name__ == "__main__":
    main()
