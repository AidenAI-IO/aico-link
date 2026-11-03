/* Minimal CPUidle environment for production driver and core callbacks. */
#include <assert.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <errno.h>

typedef uint64_t u64;
typedef uint64_t ktime_t;
#define U64_MAX UINT64_MAX
#define __init
#define THIS_MODULE NULL
#define device_initcall(fn)
#define CPUIDLE_FLAG_RCU_IDLE 1
#define WARN_ON_ONCE(x) (x)

struct cpuidle_device;
struct cpuidle_driver;
typedef int (*idle_fn)(struct cpuidle_device *, struct cpuidle_driver *, int);
struct cpuidle_state {
    idle_fn enter, enter_s2idle;
    unsigned int exit_latency, target_residency, flags;
    u64 exit_latency_ns;
    char name[16], desc[32];
};
struct cpuidle_driver {
    const char *name;
    void *owner;
    struct cpuidle_state states[2];
    int state_count, safe_state_index;
};
struct cpuidle_device {
    struct { int disable; u64 s2idle_time, s2idle_usage; } states_usage[2];
};

static bool irq_masked, tick_frozen;
static const char *compatible;
static unsigned int freeze_count, thaw_count, wfi_count;
static u64 monotonic_us, boottime_us;
static struct cpuidle_driver *registered;
static bool of_machine_is_compatible(const char *name)
{
    return compatible && strcmp(name, compatible) == 0;
}
static int cpuidle_register(struct cpuidle_driver *drv, void *mask)
{
    registered = drv;
    for (int i = 0; i < drv->state_count; i++)
        drv->states[i].exit_latency_ns = drv->states[i].exit_latency * 1000;
    return 0;
}
static void cpu_do_idle(void)
{
    assert(irq_masked);
    wfi_count++;
    boottime_us += 360000000;
    if (!tick_frozen)
        monotonic_us += 360000000;
}
static int simple_idle(struct cpuidle_device *dev, struct cpuidle_driver *drv, int i)
{
    cpu_do_idle();
    return i;
}
#define ARM_CPUIDLE_WFI_STATE { .enter = simple_idle, .exit_latency = 1 }
static u64 local_clock(void) { return monotonic_us * 1000; }
static ktime_t ns_to_ktime(u64 value) { return value; }
static u64 ktime_us_delta(ktime_t end, ktime_t start) { return (end - start) / 1000; }
static void tick_freeze(void) { assert(irq_masked && !tick_frozen); tick_frozen = true; freeze_count++; }
static void tick_unfreeze(void) { assert(irq_masked && tick_frozen); tick_frozen = false; thaw_count++; }
static bool irqs_disabled(void) { return irq_masked; }
static void local_irq_disable(void) { irq_masked = true; }
static void local_irq_enable(void) { assert(!tick_frozen); irq_masked = false; }
static void stop_critical_timings(void) {}
static void start_critical_timings(void) {}
static void rcu_idle_enter(void) {}
static void rcu_idle_exit(void) {}

/* PRODUCTION_CODE */

int main(void)
{
    struct cpuidle_device dev = {0};
    assert(rv1106_idle_init() == -ENODEV);
    assert(!registered);
    compatible = "rockchip,rv1106g3";
    assert(rv1106_idle_init() == 0);
    assert(registered == &rv1106_idle_driver);
    compatible = "rockchip,rv1106";
    assert(rv1106_idle_init() == 0);
    assert(registered == &rv1106_idle_driver);

    /* The real 5.10 selection must choose index > 0 and freeze before WFI. */
    for (int i = 0; i < 3; i++) {
        irq_masked = true;
        assert(cpuidle_enter_s2idle(registered, &dev) == 1);
        assert(!irq_masked && !tick_frozen);
        assert(monotonic_us == 0);
        assert(boottime_us == (u64)(i + 1) * 360000000);
    }
    assert(freeze_count == 3 && thaw_count == 3 && wfi_count == 3);
    assert(dev.states_usage[1].s2idle_usage == 3);

    /* Disabling that state falls back without pretending time was frozen. */
    dev.states_usage[1].disable = 1;
    irq_masked = true;
    assert(cpuidle_enter_s2idle(registered, &dev) == 0);
    assert(freeze_count == 3 && wfi_count == 3);
    assert(registered->states[0].enter(&dev, registered, 0) == 0);
    assert(monotonic_us == 360000000);
    puts("PASS: RV1106 s2idle uses the core tick-freeze path on every cycle");
    return 0;
}
