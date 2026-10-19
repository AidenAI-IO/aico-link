#!/usr/bin/env python3
"""Exercise the production RK628 recovery functions with simulated registers."""

import os
from pathlib import Path
import re
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]
DRIVER = ROOT / "pico-sdk/sysdrv/source/kernel/drivers/media/i2c/rk628/rk628_csi_v4l2.c"

PRELUDE = r"""
#include <assert.h>
#include <errno.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
typedef uint32_t u32;
typedef uint8_t u8;
#define BIT(n) (1U << (n))
#define RK628F_VERSION 1
#define HDMI_RX_SCDC_REGS1 0x30824
#define HDMI_RX_MD_HT1 0x3014c
#define HDMI_RX_MD_VTL 0x30170
#define HDMI_RX_MD_IEN_CLR 0x30fc0
#define HDMI_RX_MD_IEN_SET 0x30fc4
#define HDMI_RX_MD_IEN 0x30fcc
#define HDMI_RX_MD_ICLR 0x30fd0
#define HDMI_RX_PDEC_IEN_CLR 0x30f78
#define HDMI_RX_PDEC_IEN_SET 0x30f7c
#define HDMI_RX_PDEC_IEN 0x30f84
#define HDMI_RX_PDEC_ICLR 0x30f88
#define HDMI_RX_AUD_FIFO_IEN_CLR 0x30fa8
#define GRF_INTR0_EN 0x100
#define GRF_INTR0_CLR_EN 0x104
#define GRF_SYSTEM_CON0 0
#define AVI_RCV_ENSET BIT(18)
#define AVI_CKS_CHG_ICLR BIT(24)
#define VACT_LIN_ENSET BIT(9)
#define HACT_PIX_ENSET BIT(6)
#define HS_CLK_ENSET BIT(5)
#define DE_ACTIVITY_ENSET BIT(2)
#define VS_ACT_ENSET BIT(1)
#define HS_ACT_ENSET BIT(0)
#define VS_CLK_ENSET BIT(8)
#define RK_HDMIRX_V4L2_EVENT_SIGNAL_LOST 1
#define SW_I2S_DATA_OEN_MASK BIT(10)
#define SW_I2S_DATA_OEN(n) ((n) << 10)
struct v4l2_bt_timings {
    u32 width, height, hfrontporch, hsync, hbackporch;
    u32 vfrontporch, vsync, vbackporch;
    uint64_t pixelclock;
};
struct v4l2_dv_timings { struct v4l2_bt_timings bt; };
struct v4l2_subdev { const char *name; void *devnode; };
struct v4l2_event { int type; };
struct work_struct { int unused; };
struct delayed_work { struct work_struct work; };
struct rk628 {
    int version;
    u8 color_format, color_range, color_space;
    bool force_eq;
};
struct rk628_csi {
    struct v4l2_subdev sd;
    struct rk628 *rk628;
    struct v4l2_dv_timings src_timings;
    struct v4l2_dv_timings configured_timings;
    u8 configured_format, configured_range, configured_space;
    int confctl_mutex, irq_mutex;
    bool nosignal, avi_rcv_rdy, vid_ints_en;
    struct delayed_work delayed_work_res_change, delayed_work_enable_hotplug;
    void *audio_info;
    struct { bool enable; } hdcp;
};
static struct rk628 chip;
static struct rk628_csi csi;
static struct v4l2_dv_timings next_timings;
static u8 next_format, next_range, next_space;
static int debug, rk628_csi_timings_cap, timing_error, read_error;
static int timing_reads, status_reads, stops, resets, scheduled, events;
static bool plugged, valid_timings;
static u32 lock_status[16], parent_mask, writes[64], values[64];
static unsigned int write_count;
static struct rk628_csi *to_csi(struct v4l2_subdev *sd) { return &csi; }
#define container_of(ptr, type, member) ((type *)((char *)(ptr) - offsetof(type, member)))
#define to_delayed_work(ptr) container_of(ptr, struct delayed_work, work)
#define v4l2_dbg(...) ((void)0)
#define v4l2_print_dv_timings(...) ((void)0)
#define usleep_range(...) ((void)0)
#define msecs_to_jiffies(n) (n)
#define div_u64(n, d) ((n) / (d))
#define tx_5v_power_present(sd) plugged
#define v4l2_valid_dv_timings(...) valid_timings
#define rk628_set_bg_enable(...) ((void)0)
#define rk628_hdmirx_audio_cancel_work_audio(...) ((void)0)
#define rk628_hdmirx_audio_setup(...) ((void)0)
#define rk628_hdmirx_set_hdcp(...) ((void)0)
#define rk628_hdmirx_controller_setup(...) ((void)0)
#define rk628_hdmirx_hpd_ctrl(...) ((void)0)
#define rk628_hdmirx_config_all(...) ((void)0)
#define rk628_hdmirx_inno_phy_power_off(...) ((void)0)
#define rk628_hdmirx_verisyno_phy_power_off(...) ((void)0)
#define rk628_csi_hdmirx_reset(...) (++resets)
#define rk628_i2c_update_bits(...) ((void)0)
#define rk628_csi_format_change(...) ((void)0)
#define schedule_delayed_work(...) (++scheduled)
#define v4l2_event_queue(...) (++events)
static void enable_stream(struct v4l2_subdev *sd, bool en) { if (!en) ++stops; }
static bool mutex_trylock(int *lock) { if (*lock) return false; *lock = 1; return true; }
static void mutex_lock(int *lock) { assert(mutex_trylock(lock)); }
static void mutex_unlock(int *lock) { assert(*lock); *lock = 0; }
static int rk628_i2c_read(struct rk628 *rk, u32 reg, u32 *value) {
    *value = 0;
    if (reg == HDMI_RX_SCDC_REGS1) {
        assert(status_reads < 16);
        *value = lock_status[status_reads++];
        return read_error;
    }
    return 0;
}
static int rk628_i2c_write(struct rk628 *rk, u32 reg, u32 value) {
    assert(csi.irq_mutex);
    assert(write_count < 64);
    writes[write_count] = reg;
    values[write_count++] = value;
    if (reg == GRF_INTR0_EN) {
        parent_mask = (parent_mask & ~(value >> 16)) | (value & (value >> 16));
        if (value & 0xffff) assert(csi.vid_ints_en);
    } else {
        /* Leaf sources must only be edited while the receiver is masked. */
        assert(!(parent_mask & BIT(rk->version >= RK628F_VERSION ? 9 : 8)));
    }
    return 0;
}
static int rk628_hdmirx_get_timings(struct rk628 *rk, struct v4l2_dv_timings *out) {
    ++timing_reads;
    *out = next_timings;
    rk->color_format = next_format;
    rk->color_range = next_range;
    rk->color_space = next_space;
    return timing_error;
}
static int rk628_csi_get_detected_timings(struct v4l2_subdev *sd, struct v4l2_dv_timings *out) {
    csi.src_timings = next_timings;
    return rk628_hdmirx_get_timings(&chip, out);
}
static bool v4l2_match_dv_timings(const struct v4l2_dv_timings *a,
                                const struct v4l2_dv_timings *b,
                                unsigned int tolerance, bool reduced) {
    struct v4l2_dv_timings x = *a, y = *b;
    uint64_t delta = x.bt.pixelclock > y.bt.pixelclock
        ? x.bt.pixelclock - y.bt.pixelclock : y.bt.pixelclock - x.bt.pixelclock;
    x.bt.pixelclock = y.bt.pixelclock = 0;
    return !memcmp(&x, &y, sizeof(x)) && delta <= tolerance;
}
static void rk628_csi_enable_interrupts(struct v4l2_subdev *, bool);
"""

CASES = r"""
static void reset_fixture(void) {
    memset(&chip, 0, sizeof(chip));
    memset(&csi, 0, sizeof(csi));
    memset(&next_timings, 0, sizeof(next_timings));
    chip.version = RK628F_VERSION;
    csi.rk628 = &chip;
    csi.sd.devnode = &csi;
    csi.src_timings.bt.width = 1920;
    csi.src_timings.bt.height = 1080;
    csi.src_timings.bt.pixelclock = 148500000;
    next_timings = csi.src_timings;
    csi.configured_timings = csi.src_timings;
    next_format = next_range = next_space = 0;
    timing_error = read_error = timing_reads = status_reads = 0;
    stops = resets = scheduled = events = 0;
    plugged = valid_timings = true;
    for (unsigned int i = 0; i < 16; ++i) lock_status[i] = 0xf00;
    parent_mask = BIT(6) | BIT(9);
    write_count = 0;
}
int main(void) {
    struct v4l2_dv_timings out;
    reset_fixture();
    next_timings.bt.pixelclock += 3750;
    assert(!rk628_check_resulotion_change(&csi.sd));
    rk628_delayed_work_res_change(&csi.delayed_work_res_change.work);
    assert(!stops && !resets && !events && !csi.nosignal && csi.vid_ints_en);

    reset_fixture();
    next_timings.bt.width = 1280;
    next_timings.bt.height = 720;
    rk628_delayed_work_res_change(&csi.delayed_work_res_change.work);
    assert(stops == 1 && resets == 1 && scheduled == 1 && events == 1 && csi.nosignal);

    /* A query during debounce must not hide a real change from the worker. */
    reset_fixture();
    next_timings.bt.width = 1280;
    next_timings.bt.height = 720;
    assert(!rk628_csi_query_dv_timings(&csi.sd, &out));
    assert(csi.src_timings.bt.width == 1280);
    assert(csi.configured_timings.bt.width == 1920);
    rk628_delayed_work_res_change(&csi.delayed_work_res_change.work);
    assert(resets == 1 && events == 1);

    reset_fixture();
    lock_status[0] = 0x100;
    assert(!rk628_check_resulotion_change(&csi.sd));
    assert(timing_reads == 1 && status_reads == 3);
    reset_fixture();
    lock_status[1] = 0x100;
    assert(!rk628_check_resulotion_change(&csi.sd));
    assert(timing_reads == 1 && status_reads == 3);
    reset_fixture();
    for (unsigned int i = 0; i < 16; ++i) lock_status[i] = 0x100;
    assert(rk628_check_resulotion_change(&csi.sd));
    assert(!timing_reads);
    reset_fixture();
    for (unsigned int i = 1; i < 16; ++i) lock_status[i] = 0x100;
    assert(rk628_check_resulotion_change(&csi.sd));
    reset_fixture();
    read_error = -EIO;
    assert(rk628_check_resulotion_change(&csi.sd));
    reset_fixture();
    timing_error = -ENOLCK;
    assert(rk628_check_resulotion_change(&csi.sd));
    reset_fixture();
    next_format = 1;
    assert(rk628_check_resulotion_change(&csi.sd));
    reset_fixture();
    next_range = 1;
    assert(rk628_check_resulotion_change(&csi.sd));
    reset_fixture();
    next_space = 1;
    assert(rk628_check_resulotion_change(&csi.sd));
    reset_fixture();
    next_timings.bt.pixelclock *= 2;
    assert(rk628_check_resulotion_change(&csi.sd));

    for (int version = 0; version <= RK628F_VERSION; ++version) {
        reset_fixture();
        chip.version = version;
        u32 bit = BIT(version >= RK628F_VERSION ? 9 : 8);
        parent_mask = BIT(6) | bit;
        csi.vid_ints_en = true;
        rk628_csi_enable_interrupts(&csi.sd, false);
        assert(writes[0] == GRF_INTR0_EN && values[0] == (bit << 16));
        assert(parent_mask == BIT(6) && !csi.vid_ints_en);
        rk628_csi_enable_interrupts(&csi.sd, true);
        assert(parent_mask == (BIT(6) | bit) && csi.vid_ints_en);
    }

    reset_fixture();
    csi.confctl_mutex = 1;
    memset(&out, 0xff, sizeof(out));
    assert(rk628_csi_query_dv_timings(&csi.sd, &out) == -EAGAIN);
    assert(!out.bt.width && !out.bt.pixelclock && !timing_reads && csi.confctl_mutex);
    reset_fixture();
    csi.nosignal = true;
    assert(rk628_csi_query_dv_timings(&csi.sd, &out) == -ENOLCK);
    assert(!timing_reads && !csi.confctl_mutex);
    reset_fixture();
    plugged = false;
    assert(rk628_csi_query_dv_timings(&csi.sd, &out) == -ENOLCK);
    assert(!timing_reads && !csi.confctl_mutex);
    reset_fixture();
    assert(!rk628_csi_query_dv_timings(&csi.sd, &out));
    assert(out.bt.width == 1920 && out.bt.height == 1080 && !csi.confctl_mutex);
    valid_timings = false;
    assert(rk628_csi_query_dv_timings(&csi.sd, &out) == -ERANGE);
    puts("PASS: RK628 recovery, IRQ ordering and nonblocking timing queries");
    return 0;
}
"""


def main():
    source = DRIVER.read_text()
    functions = []
    for name in (
        "rk628_csi_clear_hdmirx_interrupts",
        "rk628_csi_enable_interrupts_locked",
        "rk628_csi_enable_interrupts",
        "rk628_csi_rx_locked",
        "rk628_check_resulotion_change",
        "rk628_delayed_work_res_change",
        "rk628_csi_query_dv_timings",
    ):
        match = re.search(r"^static (?:bool|int|void) " + name + r"\([^;]*?\)\n\{.*?^\}",
                          source, re.MULTILINE | re.DOTALL)
        if not match:
            raise AssertionError(f"Production function not found: {name}")
        functions.append(match.group())
    with tempfile.TemporaryDirectory(prefix="rk628-recovery-") as directory:
        harness = Path(directory) / "recovery.c"
        binary = Path(directory) / "recovery"
        harness.write_text(PRELUDE + "\n".join(functions) + CASES)
        subprocess.run([os.environ.get("CC", "gcc"), "-std=c11", "-Werror",
                        str(harness), "-o", str(binary)], check=True)
        subprocess.run([str(binary)], check=True, timeout=10)


if __name__ == "__main__":
    main()
