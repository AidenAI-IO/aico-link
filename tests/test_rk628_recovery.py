#!/usr/bin/env python3
"""Exercise the production RK628 recovery functions with simulated registers."""

import os
from pathlib import Path
import re
import shlex
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
enum { RK628_UNKNOWN, RK628D_VERSION, RK628F_VERSION };
#define CSI_MODE 1
#define DSI_MODE 2
#define LOCK_OK 0
#define LOCK_FAIL 1
#define LOCK_RESET 2
#define CED_OK 0
#define CED_ERR 1
#define CED_FULL 2
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
#define HDMI_RX_PDEC_CTRL 0x30300
#define GCPFORCE_CLRAVMUTE_MASK BIT(2)
#define GCPFORCE_CLRAVMUTE(n) ((n) << 2)
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
    int version, tx_mode;
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
    int lock_fail_time;
    void *plugin_det_gpio;
    bool nosignal, avi_rcv_rdy, vid_ints_en, is_streaming;
    struct delayed_work delayed_work_res_change, delayed_work_enable_hotplug;
    void *audio_info;
    struct { bool enable; } hdcp;
};
static struct rk628 chip;
static struct rk628_csi csi;
static struct v4l2_dv_timings next_timings;
static u8 next_format, next_range, next_space;
static int debug, rk628_csi_timings_cap, timing_error, read_error;
static int timing_reads, status_reads, starts, stops, resets, scheduled, events;
static int phy_result, ced_result, ced_reads, stream_start_error;
static bool plugged, valid_timings, stream_start_fails, stream_loses_signal;
static u32 lock_status[16], parent_mask, writes[64], values[64];
static unsigned int write_count;
static struct rk628_csi *to_csi(struct v4l2_subdev *sd) { return &csi; }
#define container_of(ptr, type, member) ((type *)((char *)(ptr) - offsetof(type, member)))
#define to_delayed_work(ptr) container_of(ptr, struct delayed_work, work)
#define v4l2_dbg(...) ((void)0)
#define v4l2_info(...) ((void)0)
#define v4l2_err(...) ((void)0)
#define v4l2_print_dv_timings(...) ((void)0)
#define usleep_range(...) ((void)0)
#define msleep(...) ((void)0)
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
#define rk628_hdmirx_phy_setup(...) phy_result
#define rk628_hdmirx_plugout(sd) (to_csi(sd)->nosignal = true)
#define rk628_hdmirx_inno_phy_power_off(...) ((void)0)
#define rk628_hdmirx_verisyno_phy_power_off(...) ((void)0)
#define rk628_csi_hdmirx_reset(...) (++resets)
#define rk628_i2c_update_bits(...) ((void)0)
#define rk628_csi_format_change(...) LOCK_OK
#define rk628_hdmirx_vid_enable(...) ((void)0)
#define rk628_csi_enable_csi_interrupts(...) ((void)0)
#define rk628_mipi_txdata_reset(...) ((void)0)
#define schedule_delayed_work(...) (++scheduled)
#define v4l2_event_queue(...) (++events)
static int enable_csitx(struct v4l2_subdev *sd) {
    assert(csi.confctl_mutex);
    ++starts;
    if (stream_start_error) return stream_start_error;
    csi.is_streaming = !stream_start_fails;
    if (stream_loses_signal) csi.nosignal = true;
    return 0;
}
static void rk628_csi_disable_stream(struct v4l2_subdev *sd) {
    assert(csi.confctl_mutex);
    ++stops;
    csi.is_streaming = false;
}
#define enable_dsitx enable_csitx
#define rk628_disable_dsitx rk628_csi_disable_stream
static int rk628_hdmirx_scdc_ced_err(struct rk628 *rk) {
    /* A second observation can be clear after the first updated CED state. */
    return ++ced_reads == 1 ? ced_result : CED_OK;
}
static bool mutex_trylock(int *lock) { if (*lock) return false; *lock = 1; return true; }
static void mutex_lock(int *lock) { bool locked = mutex_trylock(lock); assert(locked); }
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
static int enable_stream(struct v4l2_subdev *, bool);
"""

CASES = r"""
static void reset_fixture(void) {
    memset(&chip, 0, sizeof(chip));
    memset(&csi, 0, sizeof(csi));
    memset(&next_timings, 0, sizeof(next_timings));
    chip.version = RK628F_VERSION;
    chip.tx_mode = CSI_MODE;
    csi.rk628 = &chip;
    csi.sd.devnode = &csi;
    csi.src_timings.bt.width = 1920;
    csi.src_timings.bt.height = 1080;
    csi.src_timings.bt.pixelclock = 148500000;
    next_timings = csi.src_timings;
    csi.configured_timings = csi.src_timings;
    next_format = next_range = next_space = 0;
    timing_error = read_error = timing_reads = status_reads = 0;
    starts = stops = resets = scheduled = events = 0;
    phy_result = LOCK_OK;
    ced_result = CED_OK;
    ced_reads = 0;
    stream_start_error = 0;
    stream_start_fails = stream_loses_signal = false;
    plugged = valid_timings = true;
    for (unsigned int i = 0; i < 16; ++i) lock_status[i] = 0xf00;
    parent_mask = BIT(6) | BIT(9);
    write_count = 0;
}
int main(void) {
    struct v4l2_dv_timings out;
    /* Updating the CED baseline must not suppress a required training retry. */
    reset_fixture();
    csi.nosignal = true;
    rk628_hdmirx_config_all(&csi.sd);
    assert(ced_reads == 1 && !csi.nosignal && !resets && !scheduled);
    reset_fixture();
    csi.nosignal = true;
    ced_result = CED_ERR;
    rk628_hdmirx_config_all(&csi.sd);
    assert(ced_reads == 1 && csi.nosignal && resets == 1 && scheduled == 1);
    reset_fixture();
    csi.nosignal = true;
    ced_result = CED_FULL;
    rk628_hdmirx_config_all(&csi.sd);
    assert(ced_reads == 1 && csi.nosignal && resets == 1 && scheduled == 1);
    reset_fixture();
    phy_result = LOCK_FAIL;
    rk628_hdmirx_config_all(&csi.sd);
    assert(ced_reads == 1 && csi.nosignal && csi.lock_fail_time == 1 && scheduled == 1);
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

    /* STREAMON must hold the recovery mutex and report readiness failures. */
    /* Historical idle errors must not retune a currently locked receiver. */
    reset_fixture();
    ced_result = CED_FULL;
    chip.force_eq = true;
    assert(!rk628_csi_s_stream(&csi.sd, 1));
    assert(!ced_reads && starts == 1 && !stops && csi.is_streaming);
    assert(!csi.nosignal && !resets && !scheduled && chip.force_eq);
    assert(!csi.confctl_mutex);
    reset_fixture();
    csi.nosignal = true;
    assert(rk628_csi_s_stream(&csi.sd, 1) == -ENOLCK);
    assert(!starts && !csi.confctl_mutex);
    reset_fixture();
    plugged = false;
    assert(rk628_csi_s_stream(&csi.sd, 1) == -ENOLCK);
    assert(!starts && !csi.confctl_mutex);
    reset_fixture();
    stream_start_fails = true;
    assert(rk628_csi_s_stream(&csi.sd, 1) == -EAGAIN);
    assert(starts == 1 && stops == 1 && !csi.is_streaming && !csi.confctl_mutex);
    reset_fixture();
    stream_start_error = -ETIMEDOUT;
    assert(rk628_csi_s_stream(&csi.sd, 1) == -ETIMEDOUT);
    assert(starts == 1 && stops == 1 && !csi.is_streaming && !csi.confctl_mutex);
    reset_fixture();
    stream_loses_signal = true;
    assert(rk628_csi_s_stream(&csi.sd, 1) == -EAGAIN);
    assert(stops == 1 && !csi.is_streaming && !csi.confctl_mutex);
    reset_fixture();
    assert(!rk628_csi_s_stream(&csi.sd, 1));
    assert(starts == 1 && csi.is_streaming && !csi.confctl_mutex);
    csi.nosignal = true;
    assert(!rk628_csi_s_stream(&csi.sd, 0));
    assert(stops == 1 && !csi.is_streaming && !csi.confctl_mutex);
    puts("PASS: RK628 recovery, IRQ ordering, timing queries and stream readiness");
    return 0;
}
"""


def extract_function(source, name):
    match = re.search(r"^(?:static )?(?:bool|int|void) " + name + r"\([^;]*?\)\n\{.*?^\}",
                      source, re.MULTILINE | re.DOTALL)
    if not match:
        raise AssertionError(f"Production function not found: {name}")
    return match.group()


def register_definitions():
    """Keep the CSI startup harness aligned with the driver's register macros."""
    definitions = []
    for name in ("rk628.h", "rk628_hdmirx.h"):
        lines = iter((DRIVER.parent / name).read_text().splitlines())
        for line in lines:
            if not line.startswith("#define "):
                continue
            definitions.append(line)
            while definitions[-1].endswith("\\"):
                definitions.append(next(lines))
    return "\n".join(definitions) + "\n"


def main():
    source = DRIVER.read_text()
    functions = []
    for name in (
        "rk628_csi_clear_hdmirx_interrupts",
        "rk628_csi_enable_interrupts_locked",
        "rk628_csi_enable_interrupts",
        "enable_stream",
        "rk628_hdmirx_config_all",
        "rk628_csi_rx_locked",
        "rk628_check_resulotion_change",
        "rk628_delayed_work_res_change",
        "rk628_csi_query_dv_timings",
        "rk628_csi_s_stream",
    ):
        functions.append(extract_function(source, name))
    with tempfile.TemporaryDirectory(prefix="rk628-recovery-") as directory:
        startup = (ROOT / "tests/rk628_csi_start_harness.c").read_text()
        startup_functions = "\n".join(extract_function(source, name) for name in (
            "mipi_dphy_power_on", "mipi_dphy_power_off",
            "rk628_csi_disable_stream", "rk628_csi_set_csi", "enable_csitx",
            "enable_stream", "rk628_csi_s_stream",
        ))
        startup = startup.replace("/* DRIVER_REGISTERS */", register_definitions())
        startup = startup.replace("/* DRIVER_FUNCTIONS */", startup_functions)
        cif_source = (ROOT / "pico-sdk/sysdrv/source/kernel/drivers/media/platform/rockchip/cif/capture.c").read_text()
        cif_startup = (ROOT / "tests/rkcif_start_harness.c").read_text().replace(
            "/* DRIVER_FUNCTIONS */", extract_function(cif_source, "rkcif_do_start_stream"))
        timing_source = (DRIVER.parent / "rk628_hdmirx.c").read_text()
        timing = (ROOT / "tests/rk628_timing_harness.c").read_text().replace(
            "/* DRIVER_FUNCTIONS */", "\n".join(extract_function(timing_source, name) for name in (
                "rk628_hdmirx_check_lock", "rk628_hdmirx_try_to_get_timing",
                "rk628_hdmirx_get_timings",
            )))
        edid = (ROOT / "tests/rk628_edid_harness.c").read_text().replace(
            "/* DRIVER_FUNCTIONS */", "\n".join(extract_function(source, name) for name in (
                "rk628_csi_get_edid", "rk628_reset_edid",
            )))
        compiler = shlex.split(os.environ.get("CC", "gcc"))
        for name, body in (("recovery", PRELUDE + "\n".join(functions) + CASES),
                           ("startup", startup), ("cif-startup", cif_startup),
                           ("timing", timing), ("edid", edid)):
            harness = Path(directory) / (name + ".c")
            binary = Path(directory) / name
            harness.write_text(body)
            subprocess.run([*compiler, "-std=c11", "-UNDEBUG", "-Werror", "-I", str(DRIVER.parent),
                            str(harness), "-o", str(binary)], check=True)
            subprocess.run([str(binary)], check=True, timeout=10)


if __name__ == "__main__":
    main()
