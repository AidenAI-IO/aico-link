// SPDX-License-Identifier: GPL-2.0-only
/* RV1106 WFI idle, including tick-frozen suspend-to-idle. */

#include <linux/cpuidle.h>
#include <linux/init.h>
#include <linux/module.h>
#include <linux/of.h>
#include <asm/cpuidle.h>

static int rv1106_enter_idle(struct cpuidle_device *dev,
			    struct cpuidle_driver *drv, int index)
{
	/* WFI returns with IRQs masked, as required by enter_s2idle(). */
	cpu_do_idle();
	return index;
}

static struct cpuidle_driver rv1106_idle_driver = {
	.name = "rv1106_idle",
	.owner = THIS_MODULE,
	.states = {
		ARM_CPUIDLE_WFI_STATE,
		{
			/*
			 * The 5.10 core only selects indices above zero for
			 * enter_s2idle. Normal idle uses the same WFI instruction;
			 * s2idle additionally freezes timekeeping in the core.
			 * No PMU power-down or DDR self-refresh is requested.
			 */
			.enter = rv1106_enter_idle,
			.enter_s2idle = rv1106_enter_idle,
			.exit_latency = 1,
			.target_residency = 1,
			.name = "WFI-S2",
			.desc = "WFI with frozen s2idle tick",
		},
	},
	.safe_state_index = 0,
	.state_count = 2,
};

static int __init rv1106_idle_init(void)
{
	/* Older RV1106G3 board DTs omit the generic SoC fallback. */
	if (!of_machine_is_compatible("rockchip,rv1106") &&
	    !of_machine_is_compatible("rockchip,rv1106g3"))
		return -ENODEV;

	return cpuidle_register(&rv1106_idle_driver, NULL);
}
device_initcall(rv1106_idle_init);
