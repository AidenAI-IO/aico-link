/* Exercise CSI startup with PHY status predicates and injected read failures. */
#include <assert.h>
#include <errno.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

typedef uint32_t u32;
typedef uint8_t u8;
#define BIT(n) (1U << (n))
#define GENMASK(h, l) ((UINT32_MAX << (l)) & (UINT32_MAX >> (31 - (h))))
/* DRIVER_REGISTERS */
#include "rk628_csi.h"

enum { RK628D_VERSION = 1, RK628F_VERSION };
enum { CSI_MODE, DSI_MODE };
enum { RK628_DEV_CSI = 4, RK628_DEV_CSI1 = 20 };
enum { COMBTXPHY_MODULEA_EN = BIT(0), COMBTXPHY_MODULEB_EN = BIT(1) };
enum { OUTPUT_MODE_CSI = 4, PHY_MODE_VIDEO_MIPI = 1 };
enum { COLOR_RANGE_AUTO, COLOR_RANGE_LIMIT };
enum { HDMIRX_LIMIT_RANGE = 1 };
#define HZ 100
#define div_u64(n, d) ((n) / (d))
#define v4l2_dbg(...) ((void)0)
#define v4l2_info(...) ((void)0)
#define v4l2_err(...) ((void)0)
#define dev_err(...) ((void)0)
#define usleep_range(...) ((void)0)
#define msleep(...) ((void)0)

struct v4l2_subdev { int unused; };
struct rk628 {
    int version, tx_mode;
    bool enable_csi1, dual_mipi, is_10bit;
    u32 dphy_lane_en;
    void *regmap[21];
};
struct rk628_csi {
    struct v4l2_subdev sd;
    struct rk628 *rk628;
    struct { struct { u32 width; } bt; } timings;
    u8 csi_lanes_in_use, user_color_range;
    bool txphy_pwron, is_streaming, nosignal, avi_rcv_rdy, continues_clk;
    uint64_t lane_mbps;
    int confctl_mutex;
};

static struct rk628 chip;
static struct rk628_csi csi;
static int poll_results[4], poll_count, failed_poll, scheduled;
static u32 poll_values[4];
static u32 transmitter[2];
static bool powered, video_enabled, csi_interrupts, plugged, avi_ready;
static struct rk628_csi *to_csi(struct v4l2_subdev *sd) { return &csi; }
static void mutex_lock(int *lock) { assert(!*lock); *lock = 1; }
static void mutex_unlock(int *lock) { assert(*lock); *lock = 0; }
#define tx_5v_power_present(sd) plugged
#define fps_calc(...) 60
#define rk628_csi_get_lane_rate_mbps(...) 700
#define rk628_mipi_dphy_reset_assert(...) ((void)0)
#define rk628_mipi_dphy_reset_deassert(...) ((void)0)
#define rk628_txphy_set_bus_width(...) ((void)0)
#define rk628_txphy_set_mode(...) ((void)0)
#define rk628_mipi_dphy_init_hsfreqrange(...) ((void)0)
#define rk628_txphy_power_on(...) (powered = true)
#define rk628_txphy_power_off(...) (powered = false)
#define rk628_csi_soft_reset(...) ((void)0)
#define rk628_csi0_cru_reset(...) ((void)0)
#define rk628_csi1_cru_reset(...) ((void)0)
#define rk628_post_process_setup(...) ((void)0)
#define rk628_mipi_txdata_reset(...) ((void)0)
#define rk628_hdmirx_get_range(...) HDMIRX_LIMIT_RANGE
#define rk628_post_process_csc_en(...) ((void)0)
#define rk628_is_avi_ready(...) avi_ready
#define enable_dsitx(...) ((void)0)
#define rk628_disable_dsitx(...) ((void)0)
#define rk628_hdmirx_vid_enable(sd, en) (video_enabled = (en))
#define rk628_csi_enable_csi_interrupts(sd, en) (csi_interrupts = (en))
#define schedule_delayed_work(...) (++scheduled)

static void poll_status(void *map, u32 reg, u32 *value) {
    assert(powered && poll_count < (chip.dual_mipi ? 4 : 2));
    bool secondary = chip.dual_mipi && (poll_count & 1);
    assert(map == chip.regmap[secondary ? RK628_DEV_CSI1 : RK628_DEV_CSI]);
    assert(reg == (secondary ? CSITX1_CSITX_STATUS1 : CSITX_CSITX_STATUS1));
    *value = poll_values[poll_count++];
}
static int poll_result(bool ready) {
    int error = poll_results[poll_count - 1];
    /* A fixed status models a register that stays unchanged until timeout. */
    return error ? error : ready ? 0 : -ETIMEDOUT;
}
#define regmap_read_poll_timeout(map, reg, val, cond, delay, timeout) \
    (poll_status(map, reg, &(val)), poll_result(cond))
static int rk628_i2c_update_bits(struct rk628 *rk, u32 reg, u32 mask, u32 val) {
    if (reg == CSITX_CSITX_EN || reg == CSITX1_CSITX_EN) {
        unsigned int port = reg == CSITX1_CSITX_EN;
        if (val & mask & CSITX_EN_MASK) {
            assert(powered && poll_count == (chip.dual_mipi ? 4 : 2));
            assert(!failed_poll);
        }
        transmitter[port] = (transmitter[port] & ~mask) | (val & mask);
    }
    return 0;
}
static int rk628_i2c_write(struct rk628 *rk, u32 reg, u32 val) { return 0; }
static int rk628_i2c_read(struct rk628 *rk, u32 reg, u32 *val) {
    *val = 0;
    return 0;
}
static int rk628_csi_set_csi(struct v4l2_subdev *);
static int enable_stream(struct v4l2_subdev *, bool);

/* DRIVER_FUNCTIONS */

static void reset_polls(void) {
    memset(poll_results, 0, sizeof(poll_results));
    for (int i = 0; i < 4; ++i)
        poll_values[i] = i < (chip.dual_mipi ? 2 : 1)
            ? DPHY_PLL_LOCK : STOPSTATE_CLK | STOPSTATE_LANE0;
    poll_count = failed_poll = 0;
}

static void reset_fixture(bool dual) {
    memset(&chip, 0, sizeof(chip));
    memset(&csi, 0, sizeof(csi));
    memset(transmitter, 0, sizeof(transmitter));
    chip.version = RK628F_VERSION;
    chip.tx_mode = CSI_MODE;
    chip.dual_mipi = dual;
    reset_polls();
    chip.regmap[RK628_DEV_CSI] = &transmitter[0];
    chip.regmap[RK628_DEV_CSI1] = &transmitter[1];
    csi.rk628 = &chip;
    csi.csi_lanes_in_use = 4;
    csi.timings.bt.width = 1920;
    powered = video_enabled = csi_interrupts = false;
    plugged = avi_ready = true;
    scheduled = 0;
}

static void assert_stopped(void) {
    assert(!powered && !video_enabled && !csi_interrupts);
    assert(!csi.is_streaming && !csi.txphy_pwron && !csi.confctl_mutex);
    for (int port = 0; port < 2; ++port)
        assert(!(transmitter[port] & (CSITX_EN_MASK | DPHY_EN_MASK)));
}

int main(void) {
    for (int dual = 0; dual <= 1; ++dual) {
        int polls = dual ? 4 : 2;
        for (int failure = 0; failure < polls; ++failure) {
            bool lock_check = failure < (dual ? 2 : 1);
            u32 bad_status[] = { 0, lock_check ? STOPSTATE_CLK | STOPSTATE_LANE0 : DPHY_PLL_LOCK,
                                STOPSTATE_CLK, STOPSTATE_LANE0 };
            int status_count = lock_check ? 2 : 4;
            for (int kind = 0; kind <= status_count; ++kind) {
                int error = kind ? -ETIMEDOUT : -EIO;
                reset_fixture(dual);
                if (kind)
                    poll_values[failure] = bad_status[kind - 1];
                else
                    poll_results[failure] = error;
                failed_poll = failure + 1;
                assert(rk628_csi_s_stream(&csi.sd, 1) == error);
                assert(poll_count == failure + 1);
                assert_stopped();

                /* Reopening after a failed start must use a clean PHY. */
                reset_polls();
                assert(!rk628_csi_s_stream(&csi.sd, 1));
                assert(csi.is_streaming && csi.txphy_pwron && powered);
                assert(video_enabled && csi_interrupts && !csi.confctl_mutex);
                for (int port = 0; port <= dual; ++port)
                    assert(transmitter[port] & CSITX_EN_MASK);
                assert(!rk628_csi_s_stream(&csi.sd, 0));
                assert_stopped();
            }
        }
        reset_fixture(dual);
        avi_ready = false;
        assert(rk628_csi_s_stream(&csi.sd, 1) == -EAGAIN);
        assert(csi.nosignal && scheduled == 1);
        assert_stopped();
    }
    puts("PASS: CSI PHY status predicates and read errors gate STREAMON with cleanup and retry");
    return 0;
}
