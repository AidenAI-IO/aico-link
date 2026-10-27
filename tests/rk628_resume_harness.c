/* Exercise the production PM callback and PLL code across receiver power loss. */
#include <assert.h>
#include <errno.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

typedef uint8_t u8;
typedef uint16_t u16;
typedef uint32_t u32;
typedef uint64_t u64;
enum { RK628_UNKNOWN, RK628D_VERSION, RK628F_VERSION };
#define BIT(n) (1U << (n))
#define GENMASK(h, l) ((UINT32_MAX >> (31 - (h))) & (UINT32_MAX << (l)))
#define do_div(n, d) ((n) /= (d))
#define DIV_ROUND_UP(n, d) (((n) + (d) - 1) / (d))
#define DIV_ROUND_CLOSEST_ULL(n, d) (((n) + (d) / 2) / (d))
#define dev_info(...) ((void)0)
#define v4l2_info(...) ((void)0)
#define v4l2_err(...) ((void)0)
#define mdelay(...) ((void)0)
#define msecs_to_jiffies(n) (n)
/* DRIVER_REGISTERS */

struct device { int unused; };
struct i2c_client { int unused; };
struct v4l2_subdev { int unused; };
struct rk628 { unsigned int version; bool is_suspend; };
struct rk628_csi {
    struct rk628 *rk628;
    int plugin_irq, hdmirx_irq, delayed_work_enable_hotplug;
    int delayed_work_res_change, audio_info;
};
static struct rk628 chip;
static struct rk628_csi csi = { .rk628 = &chip };
static struct i2c_client client;
static struct v4l2_subdev sd;
static u32 registers[0x220 / 4];
static unsigned int enabled_irqs, scheduled;
static unsigned int disabled_irqs;
static unsigned int lock_reads, lock_after = 1, poll_delays, audio_writes;
static int lock_read_error;
static bool initialized, powered;

static void usleep_range(unsigned int minimum, unsigned int maximum)
{
    assert(minimum >= 1000 && maximum >= minimum);
    ++poll_delays;
}

static int rk628_i2c_read(struct rk628 *rk, u32 reg, u32 *value)
{
    if (reg == GRF_SYSTEM_STATUS0) {
        *value = I2C_ONLY_FLAG;
        return 0;
    }
    assert(reg >= CRU_REG(0) && reg < CRU_REG(sizeof(registers)));
    *value = registers[(reg - CRU_REG(0)) / 4];
    if (reg == CRU_CPLL_CON1 || reg == CRU_GPLL_CON1 || reg == CRU_APLL_CON1) {
        ++lock_reads;
        *value &= ~PLL_LOCK;
        if (lock_read_error) {
            *value |= PLL_LOCK; /* An error must win over an unusable value. */
            return lock_read_error;
        }
        if (lock_after && lock_reads >= lock_after)
            *value |= PLL_LOCK;
    }
    return 0;
}

static int rk628_i2c_write(struct rk628 *rk, u32 reg, u32 value)
{
    assert(reg >= CRU_REG(0) && reg < CRU_REG(sizeof(registers)));
    u32 *stored = &registers[(reg - CRU_REG(0)) / 4];
    if (reg == CRU_CLKSEL_CON05)
        ++audio_writes;
    if (reg == CRU_CPLL_CON2 || reg == CRU_GPLL_CON2 || reg == CRU_APLL_CON2)
        *stored = value;
    else
        *stored = (*stored & ~(value >> 16)) | (value & (value >> 16));
    return 0;
}

/* CRU_FUNCTIONS */

static int rk628_csi_power_on(struct rk628_csi *receiver)
{
    /* Reset loses the previously programmed PLL divisors and fraction. */
    memset(registers, 0, sizeof(registers));
    registers[(CRU_CPLL_CON1 - CRU_REG(0)) / 4] = 0x1042;
    initialized = false;
    powered = true;
    return 0;
}

static int rk628_csi_power_off(struct rk628_csi *receiver)
{
    powered = false;
    return 0;
}

static void rk628_csi_initial(struct v4l2_subdev *subdev)
{
    /* Timing measurement must use the same reference as on initial probe. */
    assert(rk628_cru_clk_get_rate_pll(&chip, CGU_CLK_CPLL) == 1194000000UL);
    initialized = true;
}

static struct i2c_client *to_i2c_client(struct device *dev) { return &client; }
static struct v4l2_subdev *i2c_get_clientdata(struct i2c_client *dev) { return &sd; }
static struct rk628_csi *to_csi(struct v4l2_subdev *subdev) { return &csi; }
static void rk628_hdmirx_plugout(struct v4l2_subdev *subdev) { assert(initialized); }
static void enable_irq(int irq) { assert(initialized && irq > 0); ++enabled_irqs; }
static void disable_irq(int irq) { assert(initialized && irq > 0); ++disabled_irqs; }
static void cancel_delayed_work_sync(int *work) { assert(powered); }
static void rk628_hdmirx_audio_cancel_work_audio(int audio, bool cancel) { assert(powered && cancel); }
static void schedule_delayed_work(int *work, int delay)
{
    assert(initialized && delay > 0);
    ++scheduled;
}

/* DRIVER_FUNCTIONS */

static void test_pll_failures(void)
{
    const unsigned long rates[] = {24000000UL, 1194000000UL};
    for (unsigned int i = 0; i < 2; ++i) {
        /* Both bypass and programmed-rate waits must terminate. */
        lock_after = 0;
        lock_reads = poll_delays = 0;
        assert(rk628_clk_set_rate(&chip, CGU_CLK_CPLL, rates[i]) == -ETIMEDOUT);
        assert(lock_reads > 1 && lock_reads <= 100 && poll_delays == lock_reads);
        lock_read_error = -EIO;
        lock_reads = poll_delays = 0;
        assert(rk628_clk_set_rate(&chip, CGU_CLK_CPLL, rates[i]) == -EIO);
        assert(lock_reads == 1 && poll_delays == 0);
        lock_read_error = 0;
        lock_after = 3;
        lock_reads = poll_delays = 0;
        assert(rk628_clk_set_rate(&chip, CGU_CLK_CPLL, rates[i]) == 0);
        assert(lock_reads == 3 && poll_delays == 2);
    }
    assert(rk628_clk_set_rate(&chip, CGU_CLK_CPLL, 0) == -EINVAL);

    /* Audio must propagate either parent-PLL failure before dividing its rate. */
    for (unsigned int version = RK628D_VERSION; version <= RK628F_VERSION; ++version) {
        chip.version = version;
        audio_writes = 0;
        lock_after = 0;
        assert(rk628_clk_set_rate(&chip, CGU_CLK_HDMIRX_AUD, 12288000) == -ETIMEDOUT);
        lock_read_error = -EIO;
        assert(rk628_clk_set_rate(&chip, CGU_CLK_HDMIRX_AUD, 12288000) == -EIO);
        assert(audio_writes == 0);
        lock_read_error = 0;
        lock_after = 1;
        assert(rk628_clk_set_rate(&chip, CGU_CLK_HDMIRX_AUD, 12288000) == 0);
        assert(audio_writes == 1);
    }
}

static void test_resume_failures(struct device *dev)
{
    for (unsigned int i = 0; i < 2; ++i) {
        assert(rk628_csi_suspend(dev) == 0);
        assert(chip.is_suspend && !powered);
        unsigned int disabled = disabled_irqs;
        enabled_irqs = scheduled = 0;
        lock_after = 0;
        lock_read_error = i ? -EIO : 0;
        assert(rk628_csi_resume(dev) == (i ? -EIO : -ETIMEDOUT));
        assert(chip.is_suspend && !initialized && !powered);
        assert(enabled_irqs == 0 && scheduled == 0);
        /* Another system suspend must not disable the same IRQs twice. */
        assert(rk628_csi_suspend(dev) == 0);
        assert(disabled_irqs == disabled);
        lock_read_error = 0;
        lock_after = 1;
        assert(rk628_csi_resume(dev) == 0);
        assert(!chip.is_suspend && initialized && powered);
    }
}

int main(void)
{
    struct device dev = {0};
    chip.version = RK628F_VERSION;
    /* Confirm the reset + generic CRU setup reproduces the observed bad rate. */
    rk628_csi_power_on(&csi);
    rk628_cru_initialize(&chip);
    assert(rk628_cru_clk_get_rate_pll(&chip, CGU_CLK_CPLL) == 1188000000UL);

    for (unsigned int cycle = 0; cycle < 3; ++cycle) {
        chip.is_suspend = true;
        csi.plugin_irq = cycle == 0 ? 0 : 42;
        csi.hdmirx_irq = 43;
        enabled_irqs = scheduled = 0;
        assert(rk628_csi_resume(&dev) == 0);
        assert(!chip.is_suspend && initialized);
        assert(enabled_irqs == (cycle == 0 ? 1 : 2) && scheduled == 1);
        assert(rk628_cru_clk_get_rate_pll(&chip, CGU_CLK_CPLL) == 1194000000UL);
    }
    test_pll_failures();
    test_resume_failures(&dev);
    puts("PASS: RK628 PLL waits are bounded and resume propagates clock failures");
    return 0;
}
