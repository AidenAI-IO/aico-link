// SPDX-License-Identifier: GPL-2.0
/*
 * Vendor media assembly was generated with a 32-byte struct page. Debian's
 * CONFIG_MEMCG changes that layout. Keep page arithmetic in C so it is compiled
 * against the same configured kernel as the module.
 */
#include <linux/mm.h>
#include <linux/module.h>
#include <linux/scatterlist.h>

static unsigned int aiden_page_size = sizeof(struct page);
module_param(aiden_page_size, uint, 0444);
MODULE_PARM_DESC(aiden_page_size, "Kernel struct page size used by the media compatibility helpers");
MODULE_INFO(aiden_page_abi, "kernel-helpers-v1");

struct page *aiden_media_pfn_to_page(unsigned long pfn)
{
	return pfn_to_page(pfn);
}

unsigned long aiden_media_sg_phys(struct scatterlist *sg)
{
	return (unsigned long)sg_phys(sg);
}

#ifndef CONFIG_VIDEO_ROCKCHIP_THUNDER_BOOT_SETUP
/* Match drivers/media/i2c/cam-tb-setup.h when thunder boot is disabled. */
unsigned int get_rk_cam_h(void)
{
	return 0;
}
#endif
