/*
 * SPDX-License-Identifier:     GPL-2.0+
 *
 * (C) Copyright 2022 Rockchip Electronics Co., Ltd
 */

#include <common.h>
#include <asm/io.h>
#include <dm.h>
#include <dwc3-uboot.h>
#include <g_dnl.h>
#include <misc.h>
#include <rockusb.h>
#include <usb.h>

DECLARE_GLOBAL_DATA_PTR;

#define CRU_BASE		0xFF3B2000
#define CRU_SOFTRST_CON04	0x0A10
#define USB2PHY_APB_BASE	0xFF3E0000
#define USB2PHY_PRE_EMPHASIS	0x0030
#define USB2PHY_PRE_EMPHASIS_STRENGTH	0x0040
#define USB2PHY_RX_SQUELCH	0x0064
#define USB2PHY_HS_DISCONNECT	0x0070
#define USB2PHY_FSLS_RECEIVER	0x0100
#define USB2PHY_HS_ODT		0x011c
#define USB2PHY_TX_EYE_HEIGHT	0x0124
#define USB2PHY_SQUELCH_CALIB0	0x01a4
#define USB2PHY_SQUELCH_CALIB1	0x01b4

/* cpu-version@8 efuse cell: bits[5:3] encode the RV1106 CPU revision. */
#define RV1106_OTP_CPU_VERSION_OFFSET	0x08

#ifdef CONFIG_USB_DWC3
static struct dwc3_device dwc3_device_data = {
	.maximum_speed = USB_SPEED_HIGH,
	.base = 0xffb00000,
	.dr_mode = USB_DR_MODE_PERIPHERAL,
	.index = 0,
	.dis_u2_susphy_quirk = 1,
	.usb2_phyif_utmi_width = 16,
};

int usb_gadget_handle_interrupts(int index)
{
	dwc3_uboot_handle_interrupt(0);
	return 0;
}

static void usb_reset_otg_controller(void)
{
	writel(0x1 << 7 | 0x1 << 23, CRU_BASE + CRU_SOFTRST_CON04);
	mdelay(1);
	writel(0x0 << 7 | 0x1 << 23, CRU_BASE + CRU_SOFTRST_CON04);

	mdelay(1);
}

static void usb2phy_update_bits(u32 offset, u32 mask, u32 value)
{
	u32 reg = readl(USB2PHY_APB_BASE + offset);

	reg &= ~mask;
	reg |= value & mask;
	writel(reg, USB2PHY_APB_BASE + offset);
}

#if defined(CONFIG_ROCKCHIP_OTP) && !defined(CONFIG_SPL_BUILD) && \
	!defined(CONFIG_TPL_BUILD)
static u32 rv1106_get_cpu_version(void)
{
	struct udevice *dev;
	u8 val = 0;

	if (uclass_get_device_by_driver(UCLASS_MISC,
					DM_GET_DRIVER(rockchip_otp), &dev))
		return 0;

	if (misc_read(dev, RV1106_OTP_CPU_VERSION_OFFSET, &val, sizeof(val)))
		return 0;

	return (val >> 3) & GENMASK(2, 0);
}
#else
static u32 rv1106_get_cpu_version(void)
{
	return 0;
}
#endif

static void rv1106_usb2phy_tuning(void)
{
	/* Keep the U-Boot RockUSB electrical setup aligned with Linux. */
	usb2phy_update_bits(USB2PHY_PRE_EMPHASIS, GENMASK(2, 0), 0x07);

	/*
	 * Linux lowers the Tx HS pre-emphasis strength when the CPU version
	 * is non-zero; keep the same choice so link training matches.
	 */
	if (rv1106_get_cpu_version())
		usb2phy_update_bits(USB2PHY_PRE_EMPHASIS_STRENGTH,
				    GENMASK(5, 3), 0x01 << 3);
	else
		usb2phy_update_bits(USB2PHY_PRE_EMPHASIS_STRENGTH,
				    GENMASK(5, 3), 0x03 << 3);
	usb2phy_update_bits(USB2PHY_RX_SQUELCH, GENMASK(6, 3), 0x00 << 3);
	usb2phy_update_bits(USB2PHY_FSLS_RECEIVER, BIT(6), 0);
	usb2phy_update_bits(USB2PHY_HS_ODT, GENMASK(4, 0), 0x1f);
	usb2phy_update_bits(USB2PHY_TX_EYE_HEIGHT,
			    GENMASK(4, 2), 0x03 << 2);
	usb2phy_update_bits(USB2PHY_SQUELCH_CALIB0,
			    GENMASK(7, 4), 0x01 << 4);
	usb2phy_update_bits(USB2PHY_SQUELCH_CALIB1,
			    GENMASK(7, 4), 0x01 << 4);
	usb2phy_update_bits(USB2PHY_HS_DISCONNECT, BIT(2), BIT(2));
}

int board_usb_init(int index, enum usb_init_type init)
{
	u32 odt, eye, pre;

	usb_reset_otg_controller();
	rv1106_usb2phy_tuning();
	writel(0x01ff0000, 0xff000050); /* Resume usb2 phy to normal mode */
	mdelay(2); /* Match Linux's 1.5-2 ms UTMI clock stabilization delay. */

	odt = readl(USB2PHY_APB_BASE + USB2PHY_HS_ODT) & GENMASK(4, 0);
	eye = (readl(USB2PHY_APB_BASE + USB2PHY_TX_EYE_HEIGHT) &
	       GENMASK(4, 2)) >> 2;
	pre = readl(USB2PHY_APB_BASE + USB2PHY_PRE_EMPHASIS) & GENMASK(2, 0);
	printf("RV1106 USB2 PHY tuned: odt=%#x eye=%#x pre=%#x\n",
	       odt, eye, pre);

	return dwc3_uboot_init(&dwc3_device_data);
}

#ifdef CONFIG_USB_GADGET_DOWNLOAD
int g_dnl_board_usb_cable_connected(void)
{
	/*
	 * This is a USB device port and the download key is the operator's
	 * cable-present signal. Keeping this true prevents a transient host
	 * reset or slow enumeration from falling through to BootROM, which
	 * would discard the tuned PHY setting.
	 */
	return 1;
}
#endif
#endif
