/* Exercise production timing acquisition with stale, valid AVI metadata. */
#include <assert.h>
#include <errno.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

typedef uint32_t u32;
typedef uint8_t u8;
#define RK628F_VERSION 2
#define HDMI_RX_SCDC_REGS1 0x30824
#define HDMIRX_GET_TIMING_CNT 20
#define BUS_FMT_RGB 0
#define dev_info(...) ((void)0)
#define msleep(...) ((void)0)
#define usleep_range(...) ((void)0)

struct rk628 { int version; void *dev, *hdmirx_det_gpio; };
struct v4l2_bt_timings { u32 width, height; uint64_t pixelclock; };
struct v4l2_dv_timings { struct v4l2_bt_timings bt; };

static int status_reads, timing_reads, lock_mode, read_error, timing_error;
static bool plugged;

static int rk628_i2c_read(struct rk628 *rk, u32 reg, u32 *value)
{
    assert(reg == HDMI_RX_SCDC_REGS1);
    ++status_reads;
    *value = 0xf00;
    if (lock_mode == 1 || (lock_mode == 4 && status_reads <= 8))
        *value = 0x100;
    if (lock_mode == 2)
        *value = 0xb00;
    /* Lose lock during each timing read, or every third timing sample. */
    if ((lock_mode == 3 && status_reads % 2 == 0) ||
        (lock_mode == 5 && status_reads % 6 == 0))
        *value = 0x100;
    return read_error;
}

static int rk628_hdmirx_read_timing(struct rk628 *rk,
                                  struct v4l2_dv_timings *timings)
{
    ++timing_reads;
    /* The AVI lookup reports this even when no valid pixels arrive. */
    timings->bt.width = 1920;
    timings->bt.height = 1080;
    timings->bt.pixelclock = 148500000;
    return timing_error;
}

static bool rk628_hdmirx_tx_5v_power_detect(void *gpio) { return plugged; }
static u8 rk628_hdmirx_get_format(struct rk628 *rk) { return BUS_FMT_RGB; }

/* DRIVER_FUNCTIONS */

static void reset_fixture(struct v4l2_dv_timings *out)
{
    memset(out, 0xff, sizeof(*out));
    status_reads = timing_reads = lock_mode = read_error = timing_error = 0;
    plugged = true;
}

int main(void)
{
    struct rk628 chip = { .version = RK628F_VERSION };
    struct v4l2_dv_timings out;
    reset_fixture(&out);
    assert(!rk628_hdmirx_get_timings(&chip, &out));
    assert(out.bt.width == 1920 && out.bt.height == 1080);

    for (int mode = 1; mode <= 5; ++mode) {
        reset_fixture(&out);
        lock_mode = mode;
        int ret = rk628_hdmirx_get_timings(&chip, &out);
        if (mode == 4) {
            /* Allow an 80 ms lock drop to settle before collecting samples. */
            assert(!ret && out.bt.width == 1920);
        } else {
            assert(ret == -ENOLCK);
            assert(!out.bt.width && !out.bt.height && !out.bt.pixelclock);
        }
    }

    reset_fixture(&out);
    read_error = -EIO;
    assert(rk628_hdmirx_get_timings(&chip, &out) == -EIO);
    assert(!out.bt.width && !out.bt.pixelclock && !timing_reads);

    reset_fixture(&out);
    timing_error = -EREMOTEIO;
    assert(rk628_hdmirx_get_timings(&chip, &out) == -EREMOTEIO);
    assert(!out.bt.width && !out.bt.pixelclock);

    reset_fixture(&out);
    plugged = false;
    assert(rk628_hdmirx_get_timings(&chip, &out) == -ENOLCK);
    assert(!out.bt.width && !out.bt.pixelclock && !timing_reads);

    /* The F-only SCDC check must preserve older receiver behavior. */
    reset_fixture(&out);
    chip.version = RK628F_VERSION - 1;
    read_error = -EIO;
    assert(!rk628_hdmirx_get_timings(&chip, &out));
    assert(!status_reads && out.bt.width == 1920);
    puts("PASS: live HDMI channel lock gates stable AVI timing acquisition");
    return 0;
}
