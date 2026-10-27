// SPDX-License-Identifier: GPL-2.0
/*
 * mia1321 driver
 *
 * Copyright (C) 2023 Rockchip Electronics Co., Ltd.
 *
 * V0.0X01.0X01 first version
 */

#include <linux/clk.h>
#include <linux/device.h>
#include <linux/delay.h>
#include <linux/gpio/consumer.h>
#include <linux/i2c.h>
#include <linux/module.h>
#include <linux/pm_runtime.h>
#include <linux/regulator/consumer.h>
#include <linux/sysfs.h>
#include <linux/slab.h>
#include <linux/version.h>
#include <linux/rk-camera-module.h>
#include <linux/rk-preisp.h>
#include <media/media-entity.h>
#include <media/v4l2-async.h>
#include <media/v4l2-ctrls.h>
#include <media/v4l2-subdev.h>
#include <linux/pinctrl/consumer.h>
#include "../platform/rockchip/isp/rkisp_tb_helper.h"

#define DRIVER_VERSION			KERNEL_VERSION(0, 0x01, 0x01)

#define MIA1321_LANES			2
#define MIA1321_BITS_PER_SAMPLE		12
#define MIA1321_LINK_FREQ_37125		576000000  //37.125mbp

#define PIXEL_RATE_WITH_315M_10BIT	(MIA1321_LINK_FREQ_37125 / MIA1321_BITS_PER_SAMPLE * 2 * \
					MIA1321_LANES)
#define MIA1321_XVCLK_FREQ		26000000

#define CHIP_ID				0x0400
#define MIA1321_REG_CHIP_ID		0x0011

#define MIA1321_REG_CTRL_MODE		0x0126
#define MIA1321_MODE_SW_STANDBY		BIT(0)
#define MIA1321_MODE_STREAMING		0x0

#define MIA1321_REG_EXPOSURE_H		0x00cf
#define MIA1321_REG_EXPOSURE_M		0x00ce
#define MIA1321_REG_EXPOSURE_L		0x00cd
#define MIA1321_REG_EXPOSURE_STEP	50  //depend on ref clk and vts
#define	MIA1321_EXPOSURE_MIN		1
#define	MIA1321_EXPOSURE_STEP		1
#define MIA1321_FETCH_EXP_H(VAL)	(((VAL) >> 16) & 0xFF)
#define MIA1321_FETCH_EXP_M(VAL)	(((VAL) >> 8) & 0xFF)
#define MIA1321_FETCH_EXP_L(VAL)	((VAL) & 0xFF)

#define MIA1321_REG_DIG_GAIN_EN		0
#define MIA1321_REG_DIG_GAIN_ADDRRE_EN	0x0120  // 1 function able;  0 function disable
#define MIA1321_REG_DIG_GAIN_COARSE	0x0122 //[5:0]  setp = 1
#define MIA1321_REG_DIG_GAIN_FINE_H	0x0124   //[9:8]  setp = 1/1024
#define MIA1321_REG_DIG_GAIN_FINE_L	0x0123	 //[7:0]  setp = 1/1024

#define MIA1321_REG_ANA_GAIN_H		0x001b
#define MIA1321_REG_ANA_GAIN_M		0x0019
#define MIA1321_REG_ANA_GAIN_L		0x0018

#define MIA1321_ONCE_GAIN_STEP		0x5dc
#define MIA1321_GAIN_MIN		MIA1321_ONCE_GAIN_STEP
#define MIA1321_AGAIN_MAX		(MIA1321_ONCE_GAIN_STEP * 32) /* just again */
#define MIA1321_GAIN_MAX		(MIA1321_AGAIN_MAX * 1)
#define MIA1321_GAIN_STEP		1
#define MIA1321_GAIN_DEFAULT		MIA1321_ONCE_GAIN_STEP
#define MIA1321_FETCH_DIG_COARSE_GAIN_H(VAL)	(((VAL) >> 8) & 0x05)
#define MIA1321_FETCH_DIG_FINE_GAIN_H(VAL)	((VAL) & 0xFF)
#define MIA1321_FETCH_DIG_FINE_GAIN_L(VAL)	((VAL) & 0xFF)

#define MIA1321_VTS_MAX			0xffff
//#define MIA1321_REG_VTS_H		0x1027
//#define MIA1321_REG_VTS_L		0x1026
//#define MIA1321_REG_VTS_VALUE	 1080

#define MIA1321_MIRROR_REG		0x009a
#define MIA1321_FLIP_REG		0x0099
#define MIRROR_BIT_MASK			BIT(0)
#define FLIP_BIT_MASK			BIT(1)
#define MIA1321_FETCH_MIRROR(VAL, ENABLE)	(ENABLE ? VAL | 0x01 : VAL & 0xfe)
#define MIA1321_FETCH_FLIP(VAL, ENABLE)		(ENABLE ? VAL | 0x10 : VAL & 0xfd)

//#define MIA1321_REG_TEST_PATTERN	0x3500
//#define MIA1321_TEST_PATTERN_BIT_MASK	BIT(0)

#define REG_DELAY			0xFFFE
#define REG_NULL			0xFFFF

#define MIA1321_REG_VALUE_08BIT		1
#define MIA1321_REG_VALUE_16BIT		2
#define MIA1321_REG_VALUE_24BIT		3

#define OF_CAMERA_PINCTRL_STATE_DEFAULT	"rockchip,camera_default"
#define OF_CAMERA_PINCTRL_STATE_SLEEP	"rockchip,camera_sleep"
#define MIA1321_NAME			"mia1321"

static const char *const mia1321_supply_names[] = {
	"avdd",		/* Analog power */
	"dovdd",	/* Digital I/O power */
	"dvdd",		/* Digital core power */
};

#define MIA1321_NUM_SUPPLIES ARRAY_SIZE(mia1321_supply_names)

struct regval {
	u16 addr;
	u8 val;
};

struct mia1321_mode {
	u32 bus_fmt;
	u32 width;
	u32 height;
	struct v4l2_fract max_fps;
	u32 hts_def;
	u32 vts_def;
	u32 exp_def;
	const struct regval *reg_list;
	u32 hdr_mode;
	u32 vc[PAD_MAX];
};

struct mia1321 {
	struct i2c_client	*client;
	struct clk		*xvclk;
	struct gpio_desc	*reset_gpio;
	struct gpio_desc	*pwdn_gpio;
	struct regulator_bulk_data supplies[MIA1321_NUM_SUPPLIES];

	struct pinctrl		*pinctrl;
	struct pinctrl_state	*pins_default;
	struct pinctrl_state	*pins_sleep;

	struct v4l2_subdev	subdev;
	struct media_pad	pad;
	struct v4l2_ctrl_handler ctrl_handler;
	struct v4l2_ctrl	*exposure;
	struct v4l2_ctrl	*anal_gain;
	struct v4l2_ctrl	*digi_gain;
	struct v4l2_ctrl	*hblank;
	struct v4l2_ctrl	*vblank;
	struct v4l2_ctrl	*test_pattern;
	struct mutex		mutex;
	bool			streaming;
	bool			power_on;
	const struct mia1321_mode *cur_mode;
	struct v4l2_fract	cur_fps;
	u32			module_index;
	const char		*module_facing;
	const char		*module_name;
	const char		*len_name;
	u32			cur_vts;
	bool			is_thunderboot;
	bool			is_first_streamoff;
	bool			is_mirror;
	bool			is_flip;
};

#define to_mia1321(sd) container_of(sd, struct mia1321, subdev)

/*
 * Xclk 24Mhz
 */
static const struct regval mia1321_global_regs[] = {
	{REG_NULL, 0x00},
};

static const struct regval mia1321_linear_60_1280x1080_regs[] = {
	//input:26M
	//size:1280*1080
	//fps:60
	//mipi 2lane
	
	{0x011d, 0x00}, //10BIT

	{0x011f, 0x00},

	{0x012e, 0x02},
	{0x012b, 0x01},
	
	{0x00bd, 0x00},
	{0x00bc, 0x01},
	
	{0x00bf, 0x00},
	{0x00c0, 0x00},

	{0x00cd, 0x01},
	{0x00ce, 0x01},
	{0x00cf, 0x00},
	{0x00e1, 0x00},
	{0x011c, 0x00},
	{0x0120, 0x00},
	{0x0125, 0x00},
	{0x003c, 0x01},
	{0x003d, 0x03},
	{0x1201, 0xf0},
	{0x1051, 0x1e},
	{0x1202, 0x70},
	{0x1203, 0x10},
	{0x1070, 0x02},
	{0x1205, 0x00},
	{0x1208, 0x01},
	{0x1000, 0x16},
	{0x1024, 0x00},
	{0x1025, 0x05},
	{0x1026, 0x38},
	{0x1027, 0x04},
	{0x1020, 0x2a},
	{0x1042, 0x03},
	{0x0010, 0x05},
	{0x0012, 0x01},
	{0x0043, 0x03},
	{0x003f, 0x3f},
	{0x0041, 0xff},	//0x3f default: mipi single strength adjust

	{0x009a, 0x01}, //MIRROR
	{0x0099, 0x01},	//FLIP

	{0x00ca, 0x01},
	{0x00e1, 0x00},
	{0x00e2, 0x00},
	{0x0030, 0xc0},
	{0x012c, 0x01},
	
	//60fps
	{0x004a, 0x01}, //PLL_OUTDIV 
	{0x004b, 0x90}, //PLL_FBDIV
	{0x004c, 0x03}, //PLL_DIV_ADC 
	{0x004e, 0x01}, //PLL_DIV_BITCLK
	{0x0051, 0x03}, //PLL_DIV_PCLK 
	{0x0053, 0x03}, //PLL_DIV_CPCLK

	{0x00d0, 0x9a},	//fot_num 666
	{0x00d1, 0x02},	//fot_num 666
	{0x00df, 0x42},	//fot_line
	{0x01c9, 0x9a},	//col_n 666(HS)
	{0x01ca, 0x02},	//col_n 666(HS)
	{0x0043, 0x01},
	{0x02fd, 0x58},
	{0x02fe, 0x42},
	{0x031f, 0xb0},
	{0x0320, 0x04},
	{0x0305, 0x08},
	{0x0306, 0x87},
	{0x0307, 0xfc},
	{0x0308, 0x08},
	{0x0317, 0x80},
	{0x0318, 0x0c},
	{0x030f, 0xfa},
	{0x0310, 0x0f},
	{0x02ff, 0xfa},
	{0x0300, 0x8f},
	{0x0309, 0xfa},
	{0x030a, 0x8f},
	{0x00ce, 0x03},
	{0x1000, 0x06},
	{0x1018, 0x01},
	{0x1018, 0x00},
	{0x012a, 0x01},
	{0x012a, 0x00},
	{0x00ce, 0x00},
	{0x00cd, 0x01},

	{0x012a, 0x01},
	{0x012a, 0x00},
	{REG_NULL, 0x00},
};
static const struct regval mia1321_linear_120_1280x1080_regs[] = {
	//input:26M
	//size:1280*1080
	//fps:117
	//mipi 2lane
	
	{0x011d, 0x00}, //10BIT

	{0x011f, 0x00},

	{0x012e, 0x02},
	{0x012b, 0x01},
	
	{0x00bd, 0x00},
	{0x00bc, 0x01},
	
	{0x00bf, 0x00},
	{0x00c0, 0x00},

	{0x00cd, 0x01},
	{0x00ce, 0x01},
	{0x00cf, 0x00},
	{0x00e1, 0x00},
	{0x011c, 0x00},
	{0x0120, 0x00},
	{0x0125, 0x00},
	{0x003c, 0x01},
	{0x003d, 0x03},
	{0x1201, 0xf0},
	{0x1051, 0x1e},
	{0x1202, 0x70},
	{0x1203, 0x10},
	{0x1070, 0x02},
	{0x1205, 0x00},
	{0x1208, 0x01},
	{0x1000, 0x16},
	{0x1024, 0x00},
	{0x1025, 0x05},
	{0x1026, 0x38},
	{0x1027, 0x04},
	{0x1020, 0x2a},
	{0x1042, 0x03},
	{0x0010, 0x05},
	{0x0012, 0x01},
	{0x0043, 0x03},
	{0x003f, 0x3f},
	{0x0041, 0xff},	//0x3f default: mipi single strength adjust
	{0x00ca, 0x01},
	{0x00e1, 0x00},
	{0x00e2, 0x00},
	{0x0030, 0xc0},
	{0x012c, 0x01},
	
	//117fps	
	{0x004a, 0x01},//PLL_OUTDIV 
	{0x004b, 0xd8},//PLL_FBDIV 117fps
	//{0x004b, 0xC0},//PLL_FBDIV 100fps
	{0x004c, 0x02},//PLL_DIV_ADC 
	{0x004e, 0x01},//PLL_DIV_BITCLK
	{0x0051, 0x02},//PLL_DIV_PCLK 
	{0x0053, 0x02},//PLL_DIV_CPCLK
		
	{0x00d0, 0x9a},	//fot_num 666
	{0x00d1, 0x02},	//fot_num 666
	{0x00df, 0x42},	//fot_line
	{0x01c9, 0x9a},	//col_n 666(HS)
	{0x01ca, 0x02},	//col_n 666(HS)
	{0x0043, 0x01},
	{0x02fd, 0x58},
	{0x02fe, 0x42},
	{0x031f, 0xb0},
	{0x0320, 0x04},
	{0x0305, 0x08},
	{0x0306, 0x87},
	{0x0307, 0xfc},
	{0x0308, 0x08},
	{0x0317, 0x80},
	{0x0318, 0x0c},
	{0x030f, 0xfa},
	{0x0310, 0x0f},
	{0x02ff, 0xfa},
	{0x0300, 0x8f},
	{0x0309, 0xfa},
	{0x030a, 0x8f},
	{0x00ce, 0x03},
	{0x1000, 0x06},
	{0x1018, 0x01},
	{0x1018, 0x00},
	{0x012a, 0x01},
	{0x012a, 0x00},
	{0x00ce, 0x00},
	{0x00cd, 0x01},

	{0x012a, 0x01},
	{0x012a, 0x00},
	{REG_NULL, 0x00},
};
static const struct regval mia1321_linear_10_1280x720_regs[] = {
	//input:24M
	//size:1280*1080
	//fps:30
	//pclk:24M
	//mipi 2lane
	//hts:666*4
	//vts:1080+48+6+66

	{0x011d, 0x01},
	{0x011f, 0x00},
	{0x012e, 0x02},
	{0x012b, 0x01},
	{0x00bd, 0x00},
	{0x00bc, 0x01},
	{0x00bf, 0x05},
	{0x00c0, 0x00},
	{0x00cd, 0x01},
	{0x00ce, 0x01},
	{0x00cf, 0x00},
	{0x00e1, 0x00},
	{0x011c, 0x00},
	{0x0120, 0x00},
	{0x0125, 0x00},
	{0x003c, 0x01},
	{0x003d, 0x03},
	{0x1201, 0xf0},
	{0x1051, 0x1e},
	{0x1202, 0x70},
	{0x1203, 0x10},
	{0x1070, 0x02},
	{0x1205, 0x00},
	{0x1208, 0x01},
	{0x1000, 0x16},
	{0x1024, 0x00},
	{0x1025, 0x05},
	{0x1026, 0x38},
	{0x1027, 0x04},
	{0x1020, 0x2a},
	{0x1042, 0x03},
	{0x0010, 0x05},
	{0x0012, 0x01},
	{0x0043, 0x03},
	{0x003f, 0x3f},
	{0x0041, 0xff},	//0x3f default: mipi single strength adjust
	{0x00ca, 0x01},
	{0x00e1, 0x00},
	{0x00e2, 0x00},
	{0x0030, 0xc0},
	{0x012c, 0x01},
	{0x004a, 0x01},
	{0x004b, 0x60},
	{0x00d0, 0x9a},	//fot_num 666
	{0x00d1, 0x02},	//fot_num 666
	{0x00df, 0x42},	//fot_line
	{0x01c9, 0x9a},	//col_n 666(HS)
	{0x01ca, 0x02},	//col_n 666(HS)
	{0x0043, 0x01},
	{0x02fd, 0x58},
	{0x02fe, 0x42},
	{0x031f, 0xb0},
	{0x0320, 0x04},
	{0x0305, 0x08},
	{0x0306, 0x87},
	{0x0307, 0xfc},
	{0x0308, 0x08},
	{0x0317, 0x80},
	{0x0318, 0x0c},
	{0x030f, 0xfa},
	{0x0310, 0x0f},
	{0x02ff, 0xfa},
	{0x0300, 0x8f},
	{0x0309, 0xfa},
	{0x030a, 0x8f},
	{0x00ce, 0x03},
	{0x1000, 0x06},
	{0x1018, 0x01},
	{0x1018, 0x00},
	{0x012a, 0x01},
	{0x012a, 0x00},
	{0x00ce, 0x00},
	{0x00cd, 0x01},

	{0x012a, 0x01},
	{0x012a, 0x00},
	{REG_NULL, 0x00},
};


static const struct mia1321_mode supported_modes[] = {
	{
		.width = 1280,
		.height = 1080,
		.max_fps = {
			.numerator = 10000,
			.denominator = 600000,
		},
		.exp_def = 0x01f4,
		.hts_def = 0x0a68,
		.vts_def = 0x04b0,
		.bus_fmt = MEDIA_BUS_FMT_SGRBG10_1X10,
		.reg_list = mia1321_linear_60_1280x1080_regs,
		.hdr_mode = NO_HDR,
		.vc[PAD0] = V4L2_MBUS_CSI2_CHANNEL_0,
	},
	{
		.width = 1280,
		.height = 1080,
		.max_fps = {
			.numerator = 10000,
			.denominator = 1170000,
		},
		.exp_def = 0x01f4,
		.hts_def = 0x0a68,
		.vts_def = 0x04b0,
		.bus_fmt = MEDIA_BUS_FMT_SGRBG10_1X10,
		.reg_list = mia1321_linear_120_1280x1080_regs,
		.hdr_mode = NO_HDR,
		.vc[PAD0] = V4L2_MBUS_CSI2_CHANNEL_0,
	},
	{
		.width = 1280,
		.height = 1080,
		.max_fps = {
			.numerator = 10000,
			.denominator = 300000,
		},
		.exp_def = 0x0052,
		.hts_def = 0x0a68,
		.vts_def = 0x04b0,
		.bus_fmt = MEDIA_BUS_FMT_SGRBG12_1X12,
		.reg_list = mia1321_linear_10_1280x720_regs,
		.hdr_mode = NO_HDR,
		.vc[PAD0] = V4L2_MBUS_CSI2_CHANNEL_0,
	}
};

static const s64 link_freq_menu_items[] = {
	MIA1321_LINK_FREQ_37125
};

static const char *const mia1321_test_pattern_menu[] = {
	"Disabled",
	"Vertical Color Bar Type 1",
	"Vertical Color Bar Type 2",
	"Vertical Color Bar Type 3",
	"Vertical Color Bar Type 4"
};

/* Write registers up to 4 at a time */
static int mia1321_write_reg(struct i2c_client *client, u16 reg,
			     u32 len, u32 val)
{
	u32 buf_i, val_i;
	u8 buf[6];
	u8 *val_p;
	__be32 val_be;

	if (len > 4)
		return -EINVAL;

	buf[0] = reg >> 8;
	buf[1] = reg & 0xff;

	val_be = cpu_to_be32(val);
	val_p = (u8 *)&val_be;
	buf_i = 2;
	val_i = 4 - len;

	while (val_i < 4)
		buf[buf_i++] = val_p[val_i++];

	if (i2c_master_send(client, buf, len + 2) != len + 2)
		return -EIO;

	return 0;
}

static int mia1321_write_array(struct i2c_client *client,
			       const struct regval *regs)
{
	u32 i;
	int ret = 0;

	for (i = 0; ret == 0 && regs[i].addr != REG_NULL; i++) {
		if (regs[i].addr == REG_DELAY)
			mdelay(regs[i].val);
		else
			ret = mia1321_write_reg(client, regs[i].addr,
						MIA1321_REG_VALUE_08BIT, regs[i].val);
	}

	return ret;
}

/* Read registers up to 4 at a time */
static int mia1321_read_reg(struct i2c_client *client, u16 reg, unsigned int len,
			    u32 *val)
{
	struct i2c_msg msgs[2];
	u8 *data_be_p;
	__be32 data_be = 0;
	__be16 reg_addr_be = cpu_to_be16(reg);
	int ret;

	if (len > 4 || !len)
		return -EINVAL;

	data_be_p = (u8 *)&data_be;
	/* Write register address */
	msgs[0].addr = client->addr;
	msgs[0].flags = 0;
	msgs[0].len = 2;
	msgs[0].buf = (u8 *)&reg_addr_be;

	/* Read data from register */
	msgs[1].addr = client->addr;
	msgs[1].flags = I2C_M_RD;
	msgs[1].len = len;
	msgs[1].buf = &data_be_p[4 - len];

	ret = i2c_transfer(client->adapter, msgs, ARRAY_SIZE(msgs));
	if (ret != ARRAY_SIZE(msgs))
		return -EIO;

	*val = be32_to_cpu(data_be);

	return 0;
}

static void mia1321_set_orientation_reg(struct mia1321 *mia1321, u32 en_flip_mir)
{
	switch (en_flip_mir) {
	case  0:
		mia1321->is_flip = false;
		mia1321->is_mirror = false;
		mia1321_write_reg(mia1321->client, MIA1321_FLIP_REG,
				  MIA1321_REG_VALUE_08BIT, 0x00);
		mia1321_write_reg(mia1321->client, MIA1321_MIRROR_REG,
				  MIA1321_REG_VALUE_08BIT, 0x00);
		break;
	case  1:
		mia1321->is_flip = false;
		mia1321->is_mirror = true;
		mia1321_write_reg(mia1321->client, MIA1321_FLIP_REG,
				  MIA1321_REG_VALUE_08BIT, 0x00);
		mia1321_write_reg(mia1321->client, MIA1321_MIRROR_REG,
				  MIA1321_REG_VALUE_08BIT, 0x01);
		break;
	case  2:
		mia1321->is_flip = true;
		mia1321->is_mirror = false;
		mia1321_write_reg(mia1321->client, MIA1321_FLIP_REG,
				  MIA1321_REG_VALUE_08BIT, 0x01);
		mia1321_write_reg(mia1321->client, MIA1321_MIRROR_REG,
				  MIA1321_REG_VALUE_08BIT, 0x00);
		break;
	case  3:
		mia1321->is_flip = true;
		mia1321->is_mirror = true;
		mia1321_write_reg(mia1321->client, MIA1321_FLIP_REG,
				  MIA1321_REG_VALUE_08BIT, 0x01);
		mia1321_write_reg(mia1321->client, MIA1321_MIRROR_REG,
				  MIA1321_REG_VALUE_08BIT, 0x01);
		break;
	default:
		mia1321->is_flip = false;
		mia1321->is_mirror = false;
		mia1321_write_reg(mia1321->client, MIA1321_FLIP_REG,
				  MIA1321_REG_VALUE_08BIT, 0x00);
		mia1321_write_reg(mia1321->client, MIA1321_MIRROR_REG,
				  MIA1321_REG_VALUE_08BIT, 0x00);
		break;
	}
}

struct s_again {
	// total_gain = 2^ramp_gainctrl_1[8:7]*(1+ramp_gainctrl_1[6:0]/16)
	u32 again;
	u32 reg_0018; // ramp_gainctrl_1[7:0] [0]
	u32 reg_0019; // ramp_gainctrl_1[8] [0]
	u32 reg_001b;
};

static const struct s_again mia1321_again[] = {
	//again, reg_0x18, reg_0x19, reg_0x1b
	{800,    0x0,      0x0,      0x1f},
	{1000,   0x4,      0x0,      0x1f},
	{1250,   0x9,      0x0,      0x1f},
	{1500,   0xE,      0x0,      0x1f},
	{1750,   0x13,     0x0,      0x2b},
	{2200,   0x1C,     0x0,      0x2b},
	{2450,   0x21,     0x0,      0x2b},
	{2700,   0x26,     0x0,      0x2b},
	{2950,   0x2B,     0x0,      0x2b},
	{3400,   0x34,     0x0,      0x30},
	{3650,   0x39,     0x0,      0x30},
	{3900,   0x3E,     0x0,      0x30},
	{4150,   0x43,     0x0,      0x30},
	{4600,   0x4C,     0x0,      0x30},
	{4850,   0x51,     0x0,      0x30},
	{5100,   0x56,     0x0,      0x30},
	{5350,   0x5B,     0x0,      0x30},
	{5800,   0x64,     0x0,      0x30},
	{6050,   0x69,     0x0,      0x30},
	{6300,   0x6E,     0x0,      0x30},
	{6550,   0x73,     0x0,      0x32},
	{7000,   0x7c,     0x0,      0x32},
	{7800,   0xbe,     0x0,      0x32},
	{8800,   0xc8,     0x0,      0x32},
	{9800,   0xd2,     0x0,      0x32},
	{10800,  0xdc,     0x0,      0x32},
	{12600,  0xee,     0x0,      0x32},
	{13600,  0xf8,     0x0,      0x32},
	{14600,  0x39,     0x1,      0x32},
	{15600,  0x3E,     0x1,      0x32},
	{17400,  0x47,     0x1,      0x32},
	{18400,  0x4C,     0x1,      0x32},
	{19400,  0x51,     0x1,      0x32},
	{20400,  0x56,     0x1,      0x32},
	{22200,  0x5F,     0x1,      0x32},
	{23200,  0x64,     0x1,      0x32},
	{24200,  0x69,     0x1,      0x32},
	{25200,  0x6E,     0x1,      0x32},
	{25600,  0x70,     0x1,      0x32},// MIA1321_GAIN_MAX
};

static int mia1321_set_gain_reg(struct mia1321 *mia1321, u32 gain)
{

	int ret = 0, i = 0;//, dgain;
	u8 gain_0x0018 = 0x00;
	u8 gain_0x0019 = 0x00;
	u8 gain_0x001b = 0x1f;

	//struct device *dev = &mia1321->client->dev;
	dev_info(&(mia1321->client->dev), "-----gain ------------ %d\n", gain);
	if (gain <= MIA1321_GAIN_MIN)
		gain = MIA1321_GAIN_MIN;

	if (gain > MIA1321_GAIN_MAX - 1)
		gain = MIA1321_GAIN_MAX - 1;

	while (i < ARRAY_SIZE(mia1321_again)) {
		if (gain <= mia1321_again[i].again) {
			gain_0x001b = mia1321_again[i].reg_001b;
			gain_0x0019 = mia1321_again[i].reg_0019;
			gain_0x0018 = mia1321_again[i].reg_0018;
			break;
		}
		if (gain >= MIA1321_GAIN_MAX - 1) {
			gain_0x001b = 0x32;
			gain_0x0019 = 1;
			gain_0x0018 = 0x70;
			break;
		}
		i++;
	}

	ret |= mia1321_write_reg(mia1321->client,
				 MIA1321_REG_ANA_GAIN_H,
				 MIA1321_REG_VALUE_08BIT,
				 gain_0x001b);
	ret |= mia1321_write_reg(mia1321->client,
				 MIA1321_REG_ANA_GAIN_M,
				 MIA1321_REG_VALUE_08BIT,
				 gain_0x0019);
	ret |= mia1321_write_reg(mia1321->client,
				 MIA1321_REG_ANA_GAIN_L,
				 MIA1321_REG_VALUE_08BIT,
				 gain_0x0018);

#if MIA1321_REG_DIG_GAIN_EN == 1
	ret |= mia1321_write_reg(mia1321->client,
				 MIA1321_REG_DIG_GAIN_ADDRRE_EN,
				 MIA1321_REG_VALUE_08BIT,
				 MIA1321_REG_DIG_GAIN_EN);
	ret |= mia1321_write_reg(mia1321->client,
				 MIA1321_REG_DIG_GAIN_COARSE,
				 MIA1321_REG_VALUE_08BIT,
				 MIA1321_FETCH_DIG_FINE_GAIN_H(dgain));
	ret |= mia1321_write_reg(mia1321->client,
				 MIA1321_REG_DIG_GAIN_FINE_H,
				 MIA1321_REG_VALUE_08BIT,
				 MIA1321_FETCH_DIG_FINE_GAIN_H(dgain));
	ret |= mia1321_write_reg(mia1321->client,
				 MIA1321_REG_DIG_GAIN_FINE_L,
				 MIA1321_REG_VALUE_08BIT,
				 MIA1321_FETCH_DIG_FINE_GAIN_L(dgain));
#endif

	return ret;
}

static int mia1321_get_reso_dist(const struct mia1321_mode *mode,
				 struct v4l2_mbus_framefmt *framefmt)
{
	return abs(mode->width - framefmt->width) +
	       abs(mode->height - framefmt->height);
}

static const struct mia1321_mode *
mia1321_find_best_fit(struct v4l2_subdev_format *fmt)
{
	struct v4l2_mbus_framefmt *framefmt = &fmt->format;
	int dist;
	int cur_best_fit = 0;
	int cur_best_fit_dist = -1;
	unsigned int i;

	for (i = 0; i < ARRAY_SIZE(supported_modes); i++) {
		dist = mia1321_get_reso_dist(&supported_modes[i], framefmt);
		if (cur_best_fit_dist == -1 || dist < cur_best_fit_dist) {
			cur_best_fit_dist = dist;
			cur_best_fit = i;
		}
	}

	return &supported_modes[cur_best_fit];
}

static int mia1321_set_fmt(struct v4l2_subdev *sd,
			   struct v4l2_subdev_pad_config *cfg,
			   struct v4l2_subdev_format *fmt)
{
	struct mia1321 *mia1321 = to_mia1321(sd);
	const struct mia1321_mode *mode;
	s64 h_blank, vblank_def;

	mutex_lock(&mia1321->mutex);

	mode = mia1321_find_best_fit(fmt);
	fmt->format.code = mode->bus_fmt;
	fmt->format.width = mode->width;
	fmt->format.height = mode->height;
	fmt->format.field = V4L2_FIELD_NONE;
	if (fmt->which == V4L2_SUBDEV_FORMAT_TRY) {
#ifdef CONFIG_VIDEO_V4L2_SUBDEV_API
		*v4l2_subdev_get_try_format(sd, cfg, fmt->pad) = fmt->format;
#else
		mutex_unlock(&mia1321->mutex);
		return -ENOTTY;
#endif
	} else {
		mia1321->cur_mode = mode;
		h_blank = mode->hts_def - mode->width;
		__v4l2_ctrl_modify_range(mia1321->hblank, h_blank,
					 h_blank, 1, h_blank);
		vblank_def = mode->vts_def - mode->height;
		__v4l2_ctrl_modify_range(mia1321->vblank, vblank_def,
					 MIA1321_VTS_MAX - mode->height,
					 1, vblank_def);
		mia1321->cur_fps = mode->max_fps;
	}

	mutex_unlock(&mia1321->mutex);

	return 0;
}

static int mia1321_get_fmt(struct v4l2_subdev *sd,
			   struct v4l2_subdev_pad_config *cfg,
			   struct v4l2_subdev_format *fmt)
{
	struct mia1321 *mia1321 = to_mia1321(sd);
	const struct mia1321_mode *mode = mia1321->cur_mode;

	mutex_lock(&mia1321->mutex);
	if (fmt->which == V4L2_SUBDEV_FORMAT_TRY) {
#ifdef CONFIG_VIDEO_V4L2_SUBDEV_API
		fmt->format = *v4l2_subdev_get_try_format(sd, cfg, fmt->pad);
#else
		mutex_unlock(&mia1321->mutex);
		return -ENOTTY;
#endif
	} else {
		fmt->format.width = mode->width;
		fmt->format.height = mode->height;
		fmt->format.code = mode->bus_fmt;
		fmt->format.field = V4L2_FIELD_NONE;
		/* format info: width/height/data type/virctual channel */
		if (fmt->pad < PAD_MAX && mode->hdr_mode != NO_HDR)
			fmt->reserved[0] = mode->vc[fmt->pad];
		else
			fmt->reserved[0] = mode->vc[PAD0];
	}
	mutex_unlock(&mia1321->mutex);

	return 0;
}

static int mia1321_enum_mbus_code(struct v4l2_subdev *sd,
				  struct v4l2_subdev_pad_config *cfg,
				  struct v4l2_subdev_mbus_code_enum *code)
{
	struct mia1321 *mia1321 = to_mia1321(sd);

	if (code->index != 0)
		return -EINVAL;
	code->code = mia1321->cur_mode->bus_fmt;

	return 0;
}

static int mia1321_enum_frame_sizes(struct v4l2_subdev *sd,
				    struct v4l2_subdev_pad_config *cfg,
				    struct v4l2_subdev_frame_size_enum *fse)
{
	if (fse->index >= ARRAY_SIZE(supported_modes))
		return -EINVAL;

	if (fse->code != supported_modes[0].bus_fmt)
		return -EINVAL;

	fse->min_width  = supported_modes[fse->index].width;
	fse->max_width  = supported_modes[fse->index].width;
	fse->max_height = supported_modes[fse->index].height;
	fse->min_height = supported_modes[fse->index].height;

	return 0;
}

static int mia1321_enable_test_pattern(struct mia1321 *mia1321, u32 pattern)
{
	//u32 val = 0;
	int ret = 0;

	/*
	ret = mia1321_read_reg(mia1321->client, MIA1321_REG_TEST_PATTERN,
				 MIA1321_REG_VALUE_08BIT, &val);
	if (pattern)
		val |= MIA1321_TEST_PATTERN_BIT_MASK;
	else
		val &= ~MIA1321_TEST_PATTERN_BIT_MASK;

	ret |= mia1321_write_reg(mia1321->client, MIA1321_REG_TEST_PATTERN,
				MIA1321_REG_VALUE_08BIT, val);
	*/
	return ret;
}

static int mia1321_g_frame_interval(struct v4l2_subdev *sd,
				    struct v4l2_subdev_frame_interval *fi)
{
	struct mia1321 *mia1321 = to_mia1321(sd);
	const struct mia1321_mode *mode = mia1321->cur_mode;

	if (mia1321->streaming)
		fi->interval = mia1321->cur_fps;
	else
		fi->interval = mode->max_fps;

	return 0;
}

static int mia1321_g_mbus_config(struct v4l2_subdev *sd,
				 unsigned int pad_id,
				 struct v4l2_mbus_config *config)
{
	struct mia1321 *mia1321 = to_mia1321(sd);
	const struct mia1321_mode *mode = mia1321->cur_mode;
	u32 val = 1 << (MIA1321_LANES - 1) |
		  V4L2_MBUS_CSI2_CHANNEL_0 |
		  V4L2_MBUS_CSI2_CONTINUOUS_CLOCK;

	if (mode->hdr_mode != NO_HDR)
		val |= V4L2_MBUS_CSI2_CHANNEL_1;
	if (mode->hdr_mode == HDR_X3)
		val |= V4L2_MBUS_CSI2_CHANNEL_2;

	config->type = V4L2_MBUS_CSI2_DPHY;
	config->flags = val;

	return 0;
}

static void mia1321_get_module_inf(struct mia1321 *mia1321,
				   struct rkmodule_inf *inf)
{
	memset(inf, 0, sizeof(*inf));
	strscpy(inf->base.sensor, MIA1321_NAME, sizeof(inf->base.sensor));
	strscpy(inf->base.module, mia1321->module_name,
		sizeof(inf->base.module));
	strscpy(inf->base.lens, mia1321->len_name, sizeof(inf->base.lens));
}

static long mia1321_ioctl(struct v4l2_subdev *sd, unsigned int cmd, void *arg)
{
	struct mia1321 *mia1321 = to_mia1321(sd);
	struct rkmodule_hdr_cfg *hdr;
	u32 i, h, w;
	long ret = 0;
	u32 stream = 0;

	switch (cmd) {
	case RKMODULE_GET_MODULE_INFO:
		mia1321_get_module_inf(mia1321, (struct rkmodule_inf *)arg);
		break;
	case RKMODULE_GET_HDR_CFG:
		hdr = (struct rkmodule_hdr_cfg *)arg;
		hdr->esp.mode = HDR_NORMAL_VC;
		hdr->hdr_mode = mia1321->cur_mode->hdr_mode;
		break;
	case RKMODULE_SET_HDR_CFG:
		hdr = (struct rkmodule_hdr_cfg *)arg;
		w = mia1321->cur_mode->width;
		h = mia1321->cur_mode->height;
		for (i = 0; i < ARRAY_SIZE(supported_modes); i++) {
			if (w == supported_modes[i].width &&
			    h == supported_modes[i].height &&
			    supported_modes[i].hdr_mode == hdr->hdr_mode) {
				mia1321->cur_mode = &supported_modes[i];
				break;
			}
		}
		if (i == ARRAY_SIZE(supported_modes)) {
			dev_err(&mia1321->client->dev,
				"not find hdr mode:%d %dx%d config\n",
				hdr->hdr_mode, w, h);
			ret = -EINVAL;
		} else {
			w = mia1321->cur_mode->hts_def - mia1321->cur_mode->width;
			h = mia1321->cur_mode->vts_def - mia1321->cur_mode->height;
			__v4l2_ctrl_modify_range(mia1321->hblank, w, w, 1, w);
			__v4l2_ctrl_modify_range(mia1321->vblank, h,
						 MIA1321_VTS_MAX - mia1321->cur_mode->height, 1, h);
		}
		break;
	case PREISP_CMD_SET_HDRAE_EXP:
		break;
	case RKMODULE_SET_QUICK_STREAM:

		stream = *((u32 *)arg);

		if (stream)
			ret = mia1321_write_reg(mia1321->client, MIA1321_REG_CTRL_MODE,
						MIA1321_REG_VALUE_08BIT, MIA1321_MODE_STREAMING);
		else
			ret = mia1321_write_reg(mia1321->client, MIA1321_REG_CTRL_MODE,
						MIA1321_REG_VALUE_08BIT, MIA1321_MODE_SW_STANDBY);
		break;
	default:
		ret = -ENOIOCTLCMD;
		break;
	}

	return ret;
}

#ifdef CONFIG_COMPAT
static long mia1321_compat_ioctl32(struct v4l2_subdev *sd,
				   unsigned int cmd, unsigned long arg)
{
	void __user *up = compat_ptr(arg);
	struct rkmodule_inf *inf;
	struct rkmodule_hdr_cfg *hdr;
	struct preisp_hdrae_exp_s *hdrae;
	long ret;
	u32 stream = 0;

	switch (cmd) {
	case RKMODULE_GET_MODULE_INFO:
		inf = kzalloc(sizeof(*inf), GFP_KERNEL);
		if (!inf) {
			ret = -ENOMEM;
			return ret;
		}

		ret = mia1321_ioctl(sd, cmd, inf);
		if (!ret) {
			if (copy_to_user(up, inf, sizeof(*inf)))
				ret = -EFAULT;
		}
		kfree(inf);
		break;
	case RKMODULE_GET_HDR_CFG:
		hdr = kzalloc(sizeof(*hdr), GFP_KERNEL);
		if (!hdr) {
			ret = -ENOMEM;
			return ret;
		}

		ret = mia1321_ioctl(sd, cmd, hdr);
		if (!ret) {
			if (copy_to_user(up, hdr, sizeof(*hdr)))
				ret = -EFAULT;
		}
		kfree(hdr);
		break;
	case RKMODULE_SET_HDR_CFG:
		hdr = kzalloc(sizeof(*hdr), GFP_KERNEL);
		if (!hdr) {
			ret = -ENOMEM;
			return ret;
		}

		ret = copy_from_user(hdr, up, sizeof(*hdr));
		if (!ret)
			ret = mia1321_ioctl(sd, cmd, hdr);
		else
			ret = -EFAULT;
		kfree(hdr);
		break;
	case PREISP_CMD_SET_HDRAE_EXP:
		hdrae = kzalloc(sizeof(*hdrae), GFP_KERNEL);
		if (!hdrae) {
			ret = -ENOMEM;
			return ret;
		}

		ret = copy_from_user(hdrae, up, sizeof(*hdrae));
		if (!ret)
			ret = mia1321_ioctl(sd, cmd, hdrae);
		else
			ret = -EFAULT;
		kfree(hdrae);
		break;
	case RKMODULE_SET_QUICK_STREAM:
		ret = copy_from_user(&stream, up, sizeof(u32));
		if (!ret)
			ret = mia1321_ioctl(sd, cmd, &stream);
		else
			ret = -EFAULT;
		break;
	default:
		ret = -ENOIOCTLCMD;
		break;
	}

	return ret;
}
#endif

static int __mia1321_start_stream(struct mia1321 *mia1321)
{
	int ret;

	if (!mia1321->is_thunderboot) {
		ret = mia1321_write_array(mia1321->client, mia1321->cur_mode->reg_list);
		if (ret) 
			return ret;

		/* In case these controls are set before streaming */
		ret = __v4l2_ctrl_handler_setup(&mia1321->ctrl_handler);

		if (ret)
			return ret;
	}

	ret = mia1321_write_reg(mia1321->client, MIA1321_REG_CTRL_MODE,
				MIA1321_REG_VALUE_08BIT, MIA1321_MODE_STREAMING);

	mia1321_write_reg(mia1321->client, MIA1321_FLIP_REG, MIA1321_REG_VALUE_08BIT, 0x01);
	mia1321_write_reg(mia1321->client, MIA1321_MIRROR_REG, MIA1321_REG_VALUE_08BIT, 0x01);

	return ret;
}

static int __mia1321_stop_stream(struct mia1321 *mia1321)
{
	if (mia1321->is_thunderboot) {
		mia1321->is_first_streamoff = true;
		pm_runtime_put(&mia1321->client->dev);
	}
	return mia1321_write_reg(mia1321->client, MIA1321_REG_CTRL_MODE,
				 MIA1321_REG_VALUE_08BIT, MIA1321_MODE_SW_STANDBY);
}

static int __mia1321_power_on(struct mia1321 *mia1321);
static int mia1321_s_stream(struct v4l2_subdev *sd, int on)
{
	struct mia1321 *mia1321 = to_mia1321(sd);
	struct i2c_client *client = mia1321->client;
	int ret = 0;

	mutex_lock(&mia1321->mutex);
	on = !!on;
	if (on == mia1321->streaming)
		goto unlock_and_return;

	if (on) {
		if (mia1321->is_thunderboot && rkisp_tb_get_state() == RKISP_TB_NG) {
			mia1321->is_thunderboot = false;
			__mia1321_power_on(mia1321);
		}

		ret = pm_runtime_get_sync(&client->dev);
		if (ret < 0) {
			pm_runtime_put_noidle(&client->dev);
			goto unlock_and_return;
		}

		ret = __mia1321_start_stream(mia1321);
		if (ret) {
			v4l2_err(sd, "start stream failed while write regs\n");
			pm_runtime_put(&client->dev);
			goto unlock_and_return;
		}
	} else {
		__mia1321_stop_stream(mia1321);
		pm_runtime_put(&client->dev);
	}

	mia1321->streaming = on;

unlock_and_return:
	mutex_unlock(&mia1321->mutex);

	return ret;
}

static int mia1321_s_power(struct v4l2_subdev *sd, int on)
{
	struct mia1321 *mia1321 = to_mia1321(sd);
	struct i2c_client *client = mia1321->client;
	int ret = 0;

	mutex_lock(&mia1321->mutex);

	/* If the power state is not modified - no work to do. */
	if (mia1321->power_on == !!on)
		goto unlock_and_return;

	if (on) {
		ret = pm_runtime_get_sync(&client->dev);
		if (ret < 0) {
			pm_runtime_put_noidle(&client->dev);
			goto unlock_and_return;
		}

		if (!mia1321->is_thunderboot) {
			ret = mia1321_write_array(mia1321->client, mia1321_global_regs);
			if (ret) {
				v4l2_err(sd, "could not set init registers\n");
				pm_runtime_put_noidle(&client->dev);
				goto unlock_and_return;
			}
		}

		mia1321->power_on = true;
	} else {
		pm_runtime_put(&client->dev);
		mia1321->power_on = false;
	}

unlock_and_return:
	mutex_unlock(&mia1321->mutex);

	return ret;
}

/* Calculate the delay in us by clock rate and clock cycles */
static inline u32 mia1321_cal_delay(u32 cycles)
{
	return DIV_ROUND_UP(cycles, MIA1321_XVCLK_FREQ / 1000 / 1000);
}

static int __mia1321_power_on(struct mia1321 *mia1321)
{
	int ret;
	u32 delay_us;
	struct device *dev = &mia1321->client->dev;

	if (!IS_ERR_OR_NULL(mia1321->pins_default)) {
		ret = pinctrl_select_state(mia1321->pinctrl,
					   mia1321->pins_default);
		if (ret < 0)
			dev_err(dev, "could not set pins\n");
	}
	ret = clk_set_rate(mia1321->xvclk, MIA1321_XVCLK_FREQ);
	if (ret < 0)
		dev_warn(dev, "Failed to set xvclk rate (24MHz)\n");
	if (clk_get_rate(mia1321->xvclk) != MIA1321_XVCLK_FREQ)
		dev_warn(dev, "xvclk mismatched, modes are based on 24MHz\n");
	ret = clk_prepare_enable(mia1321->xvclk);
	if (ret < 0) {
		dev_err(dev, "Failed to enable xvclk\n");
		return ret;
	}
	if (mia1321->is_thunderboot)
		return 0;

	if (!IS_ERR(mia1321->reset_gpio))
		gpiod_set_value_cansleep(mia1321->reset_gpio, 0);

	ret = regulator_bulk_enable(MIA1321_NUM_SUPPLIES, mia1321->supplies);
	if (ret < 0) {
		dev_err(dev, "Failed to enable regulators\n");
		goto disable_clk;
	}

	if (!IS_ERR(mia1321->reset_gpio))
		gpiod_set_value_cansleep(mia1321->reset_gpio, 1);

	usleep_range(500, 1000);
	if (!IS_ERR(mia1321->pwdn_gpio))
		gpiod_set_value_cansleep(mia1321->pwdn_gpio, 1);

	if (!IS_ERR(mia1321->reset_gpio))
		usleep_range(6000, 8000);
	else
		usleep_range(12000, 16000);

	/* 8192 cycles prior to first SCCB transaction */
	delay_us = mia1321_cal_delay(8192);
	usleep_range(delay_us, delay_us * 2);
	return 0;

disable_clk:
	clk_disable_unprepare(mia1321->xvclk);

	return ret;
}

static void __mia1321_power_off(struct mia1321 *mia1321)
{
	int ret;
	struct device *dev = &mia1321->client->dev;

	clk_disable_unprepare(mia1321->xvclk);
	if (mia1321->is_thunderboot) {
		if (mia1321->is_first_streamoff) {
			mia1321->is_thunderboot = false;
			mia1321->is_first_streamoff = false;
		} else {
			return;
		}
	}

	if (!IS_ERR(mia1321->pwdn_gpio))
		gpiod_set_value_cansleep(mia1321->pwdn_gpio, 0);
	if (!IS_ERR(mia1321->reset_gpio))
		gpiod_set_value_cansleep(mia1321->reset_gpio, 0);
	if (!IS_ERR_OR_NULL(mia1321->pins_sleep)) {
		ret = pinctrl_select_state(mia1321->pinctrl,
					   mia1321->pins_sleep);
		if (ret < 0)
			dev_dbg(dev, "could not set pins\n");
	}
	regulator_bulk_disable(MIA1321_NUM_SUPPLIES, mia1321->supplies);
}

static int __maybe_unused mia1321_runtime_resume(struct device *dev)
{
	struct i2c_client *client = to_i2c_client(dev);
	struct v4l2_subdev *sd = i2c_get_clientdata(client);
	struct mia1321 *mia1321 = to_mia1321(sd);

	return __mia1321_power_on(mia1321);
}

static int __maybe_unused mia1321_runtime_suspend(struct device *dev)
{
	struct i2c_client *client = to_i2c_client(dev);
	struct v4l2_subdev *sd = i2c_get_clientdata(client);
	struct mia1321 *mia1321 = to_mia1321(sd);

	__mia1321_power_off(mia1321);

	return 0;
}

#ifdef CONFIG_VIDEO_V4L2_SUBDEV_API
static int mia1321_open(struct v4l2_subdev *sd, struct v4l2_subdev_fh *fh)
{
	struct mia1321 *mia1321 = to_mia1321(sd);
	struct v4l2_mbus_framefmt *try_fmt =
		v4l2_subdev_get_try_format(sd, fh->pad, 0);
	const struct mia1321_mode *def_mode = &supported_modes[0];

	mutex_lock(&mia1321->mutex);
	/* Initialize try_fmt */
	try_fmt->width = def_mode->width;
	try_fmt->height = def_mode->height;
	try_fmt->code = def_mode->bus_fmt;
	try_fmt->field = V4L2_FIELD_NONE;

	mutex_unlock(&mia1321->mutex);
	/* No crop or compose */

	return 0;
}
#endif

static int mia1321_enum_frame_interval(struct v4l2_subdev *sd,
				       struct v4l2_subdev_pad_config *cfg,
				       struct v4l2_subdev_frame_interval_enum *fie)
{
	if (fie->index >= ARRAY_SIZE(supported_modes))
		return -EINVAL;

	fie->code = supported_modes[fie->index].bus_fmt;
	fie->width = supported_modes[fie->index].width;
	fie->height = supported_modes[fie->index].height;
	fie->interval = supported_modes[fie->index].max_fps;
	fie->reserved[0] = supported_modes[fie->index].hdr_mode;
	return 0;
}

static const struct dev_pm_ops mia1321_pm_ops = {
	SET_RUNTIME_PM_OPS(mia1321_runtime_suspend,
	mia1321_runtime_resume, NULL)
};

#ifdef CONFIG_VIDEO_V4L2_SUBDEV_API
static const struct v4l2_subdev_internal_ops mia1321_internal_ops = {
	.open = mia1321_open,
};
#endif

static const struct v4l2_subdev_core_ops mia1321_core_ops = {
	.s_power = mia1321_s_power,
	.ioctl = mia1321_ioctl,
#ifdef CONFIG_COMPAT
	.compat_ioctl32 = mia1321_compat_ioctl32,
#endif
};

static const struct v4l2_subdev_video_ops mia1321_video_ops = {
	.s_stream = mia1321_s_stream,
	.g_frame_interval = mia1321_g_frame_interval,
};

static const struct v4l2_subdev_pad_ops mia1321_pad_ops = {
	.enum_mbus_code = mia1321_enum_mbus_code,
	.enum_frame_size = mia1321_enum_frame_sizes,
	.enum_frame_interval = mia1321_enum_frame_interval,
	.get_fmt = mia1321_get_fmt,
	.set_fmt = mia1321_set_fmt,
	.get_mbus_config = mia1321_g_mbus_config,
};

static const struct v4l2_subdev_ops mia1321_subdev_ops = {
	.core = &mia1321_core_ops,
	.video = &mia1321_video_ops,
	.pad = &mia1321_pad_ops,
};

/*
static void mia1321_modify_fps_info(struct mia1321 *mia1321)
{
	const struct mia1321_mode *mode = mia1321->cur_mode;

	mia1321->cur_fps.denominator = mode->max_fps.denominator * mode->vts_def /
						mia1321->cur_vts;
}
*/

static int mia1321_set_ctrl(struct v4l2_ctrl *ctrl)
{
	struct mia1321 *mia1321 = container_of(ctrl->handler,
					       struct mia1321, ctrl_handler);
	struct i2c_client *client = mia1321->client;
	s64 max;
	int ret = 0;
	u32 val = 0;
	//dev_info(&client->dev, "---22--gain ------------ %d--- %d--- %d\n",
	//	  mia1321->cur_vts, mia1321->cur_mode->vts_def, mia1321->cur_mode->height);
	/* Propagate change of current control to all related controls */
	switch (ctrl->id) {
	case V4L2_CID_VBLANK:
		/* Update max exposure while meeting expected vblanking */
		max = mia1321->cur_mode->height + ctrl->val - 1;
		__v4l2_ctrl_modify_range(mia1321->exposure,
					 mia1321->exposure->minimum, max,
					 mia1321->exposure->step,
					 mia1321->exposure->default_value);
		break;
	}

	if (!pm_runtime_get_if_in_use(&client->dev))
		return 0;

	switch (ctrl->id) {
	case V4L2_CID_EXPOSURE:
		dev_info(&(mia1321->client->dev), "set exposure 0x%x\n", ctrl->val);

		if (mia1321->cur_mode->hdr_mode == NO_HDR) {
			val = ctrl->val;

			ret = mia1321_write_reg(mia1321->client,
						MIA1321_REG_EXPOSURE_H,
						MIA1321_REG_VALUE_08BIT,
						MIA1321_FETCH_EXP_H(val));
			ret = mia1321_write_reg(mia1321->client,
						MIA1321_REG_EXPOSURE_M,
						MIA1321_REG_VALUE_08BIT,
						MIA1321_FETCH_EXP_M(val));
			ret |= mia1321_write_reg(mia1321->client,
						 MIA1321_REG_EXPOSURE_L,
						 MIA1321_REG_VALUE_08BIT,
						 MIA1321_FETCH_EXP_L(val));
		}
		break;
	case V4L2_CID_ANALOGUE_GAIN:
		dev_dbg(&client->dev, "set gain 0x%x\n", ctrl->val);
		if (mia1321->cur_mode->hdr_mode == NO_HDR)
			ret = mia1321_set_gain_reg(mia1321, ctrl->val);
		break;
	case V4L2_CID_VBLANK:
		/*TODO stable vts*/
		dev_info(&(mia1321->client->dev), "set vblank 0x%x\n", ctrl->val);
		/*
		ret = mia1321_write_reg(mia1321->client,
					MIA1321_REG_VTS_H,
					MIA1321_REG_VALUE_08BIT,
					(ctrl->val + mia1321->cur_mode->height)
					>> 8);
		ret |= mia1321_write_reg(mia1321->client,
					 MIA1321_REG_VTS_L,
					 MIA1321_REG_VALUE_08BIT,
					 (ctrl->val + mia1321->cur_mode->height)
					 & 0xff);
		mia1321->cur_vts = ctrl->val + mia1321->cur_mode->height;
		if (mia1321->cur_vts != mia1321->cur_mode->vts_def)
			mia1321_modify_fps_info(mia1321);
		*/
		mia1321->cur_vts = mia1321->cur_mode->vts_def;
		break;
	case V4L2_CID_TEST_PATTERN:
		ret = mia1321_enable_test_pattern(mia1321, ctrl->val);
		break;
	case V4L2_CID_HFLIP:
		ret = mia1321_read_reg(mia1321->client, MIA1321_FLIP_REG,
				       MIA1321_REG_VALUE_08BIT, &val);
		if (ctrl->val)
			val |= MIRROR_BIT_MASK;
		else
			val &= ~MIRROR_BIT_MASK;
		mia1321_set_orientation_reg(mia1321, val);
		break;
	case V4L2_CID_VFLIP:
		ret = mia1321_read_reg(mia1321->client, MIA1321_MIRROR_REG,
				       MIA1321_REG_VALUE_08BIT, &val);
		if (ctrl->val)
			val |= FLIP_BIT_MASK;
		else
			val &= ~FLIP_BIT_MASK;
		mia1321_set_orientation_reg(mia1321, val);
		break;
	default:
		dev_warn(&client->dev, "%s Unhandled id:0x%x, val:0x%x\n",
			 __func__, ctrl->id, ctrl->val);
		break;
	}

	pm_runtime_put(&client->dev);

	return ret;
}

static const struct v4l2_ctrl_ops mia1321_ctrl_ops = {
	.s_ctrl = mia1321_set_ctrl,
};

static int mia1321_initialize_controls(struct mia1321 *mia1321)
{
	const struct mia1321_mode *mode;
	struct v4l2_ctrl_handler *handler;
	struct v4l2_ctrl *ctrl;
	s64 exposure_max, vblank_def;
	u32 h_blank;
	int ret;

	handler = &mia1321->ctrl_handler;
	mode = mia1321->cur_mode;
	ret = v4l2_ctrl_handler_init(handler, 9);
	if (ret)
		return ret;
	handler->lock = &mia1321->mutex;

	ctrl = v4l2_ctrl_new_int_menu(handler, NULL, V4L2_CID_LINK_FREQ,
				      0, 0, link_freq_menu_items);
	if (ctrl)
		ctrl->flags |= V4L2_CTRL_FLAG_READ_ONLY;

	v4l2_ctrl_new_std(handler, NULL, V4L2_CID_PIXEL_RATE,
			  0, PIXEL_RATE_WITH_315M_10BIT, 1, PIXEL_RATE_WITH_315M_10BIT);

	h_blank = mode->hts_def - mode->width;
	mia1321->hblank = v4l2_ctrl_new_std(handler, NULL, V4L2_CID_HBLANK,
					    h_blank, h_blank, 1, h_blank);
	if (mia1321->hblank)
		mia1321->hblank->flags |= V4L2_CTRL_FLAG_READ_ONLY;
	vblank_def = mode->vts_def - mode->height;
	mia1321->vblank = v4l2_ctrl_new_std(handler, &mia1321_ctrl_ops,
					    V4L2_CID_VBLANK, vblank_def,
					    MIA1321_VTS_MAX - mode->height,
					    1, vblank_def);
	mia1321->cur_fps = mode->max_fps;
	exposure_max = mode->vts_def - 1;
	mia1321->exposure = v4l2_ctrl_new_std(handler, &mia1321_ctrl_ops,
					      V4L2_CID_EXPOSURE, MIA1321_EXPOSURE_MIN,
					      exposure_max, MIA1321_EXPOSURE_STEP,
					      mode->exp_def);
	mia1321->anal_gain = v4l2_ctrl_new_std(handler, &mia1321_ctrl_ops,
					       V4L2_CID_ANALOGUE_GAIN, MIA1321_GAIN_MIN,
					       MIA1321_GAIN_MAX, MIA1321_GAIN_STEP,
					       MIA1321_GAIN_DEFAULT);
	mia1321->test_pattern = v4l2_ctrl_new_std_menu_items(handler,
				&mia1321_ctrl_ops,
				V4L2_CID_TEST_PATTERN,
				ARRAY_SIZE(mia1321_test_pattern_menu) - 1,
				0, 0, mia1321_test_pattern_menu);
	v4l2_ctrl_new_std(handler, &mia1321_ctrl_ops,
			  V4L2_CID_HFLIP, 0, 1, 1, 0);
	v4l2_ctrl_new_std(handler, &mia1321_ctrl_ops,
			  V4L2_CID_VFLIP, 0, 1, 1, 0);	
	if (handler->error) {
		ret = handler->error;
		dev_err(&mia1321->client->dev,
			"Failed to init controls(%d)\n", ret);
		goto err_free_handler;
	}

	mia1321->subdev.ctrl_handler = handler;

	return 0;

err_free_handler:
	v4l2_ctrl_handler_free(handler);

	return ret;
}

static int mia1321_check_sensor_id(struct mia1321 *mia1321,
				   struct i2c_client *client)
{
	struct device *dev = &mia1321->client->dev;
	u32 id = 0;
	int ret;

	if (mia1321->is_thunderboot) {
		dev_info(dev, "Enable thunderboot mode, skip sensor id check\n");
		return 0;
	}

	ret = mia1321_read_reg(client, MIA1321_REG_CHIP_ID,
			       MIA1321_REG_VALUE_16BIT, &id);
	if (id != CHIP_ID) {
		dev_err(dev, "Unexpected sensor id(%06x), ret(%d)\n", id, ret);
		return -ENODEV;
	}

	dev_info(dev, "Detected MIA1321(mis%04x) sensor\n", CHIP_ID);

	return 0;
}

static int mia1321_configure_regulators(struct mia1321 *mia1321)
{
	unsigned int i;

	for (i = 0; i < MIA1321_NUM_SUPPLIES; i++)
		mia1321->supplies[i].supply = mia1321_supply_names[i];

	return devm_regulator_bulk_get(&mia1321->client->dev,
				       MIA1321_NUM_SUPPLIES,
				       mia1321->supplies);
}

static int mia1321_probe(struct i2c_client *client,
			 const struct i2c_device_id *id)
{
	struct device *dev = &client->dev;
	struct device_node *node = dev->of_node;
	struct mia1321 *mia1321;
	struct v4l2_subdev *sd;
	char facing[2];
	int ret;

	dev_info(dev, "driver version: %02x.%02x.%02x",
		 DRIVER_VERSION >> 16,
		 (DRIVER_VERSION & 0xff00) >> 8,
		 DRIVER_VERSION & 0x00ff);

	mia1321 = devm_kzalloc(dev, sizeof(*mia1321), GFP_KERNEL);
	if (!mia1321)
		return -ENOMEM;

	ret = of_property_read_u32(node, RKMODULE_CAMERA_MODULE_INDEX,
				   &mia1321->module_index);
	ret |= of_property_read_string(node, RKMODULE_CAMERA_MODULE_FACING,
				       &mia1321->module_facing);
	ret |= of_property_read_string(node, RKMODULE_CAMERA_MODULE_NAME,
				       &mia1321->module_name);
	ret |= of_property_read_string(node, RKMODULE_CAMERA_LENS_NAME,
				       &mia1321->len_name);
	if (ret) {
		dev_err(dev, "could not get module information!\n");
		return -EINVAL;
	}

	mia1321->is_thunderboot = IS_ENABLED(CONFIG_VIDEO_ROCKCHIP_THUNDER_BOOT_ISP);
	mia1321->client = client;
	mia1321->cur_mode = &supported_modes[0];

	mia1321->xvclk = devm_clk_get(dev, "xvclk");
	if (IS_ERR(mia1321->xvclk)) {
		dev_err(dev, "Failed to get xvclk\n");
		return -EINVAL;
	}

	if (mia1321->is_thunderboot) {
		mia1321->reset_gpio = devm_gpiod_get(dev, "reset", GPIOD_ASIS);
		if (IS_ERR(mia1321->reset_gpio))
			dev_warn(dev, "Failed to get reset-gpios\n");

		mia1321->pwdn_gpio = devm_gpiod_get(dev, "pwdn", GPIOD_ASIS);
		if (IS_ERR(mia1321->pwdn_gpio))
			dev_warn(dev, "Failed to get pwdn-gpios\n");
	} else {
		mia1321->reset_gpio = devm_gpiod_get(dev, "reset", GPIOD_OUT_LOW);
		if (IS_ERR(mia1321->reset_gpio))
			dev_warn(dev, "Failed to get reset-gpios\n");

		mia1321->pwdn_gpio = devm_gpiod_get(dev, "pwdn", GPIOD_OUT_LOW);
		if (IS_ERR(mia1321->pwdn_gpio))
			dev_warn(dev, "Failed to get pwdn-gpios\n");
	}
	mia1321->pinctrl = devm_pinctrl_get(dev);
	if (!IS_ERR(mia1321->pinctrl)) {
		mia1321->pins_default =
			pinctrl_lookup_state(mia1321->pinctrl,
					     OF_CAMERA_PINCTRL_STATE_DEFAULT);
		if (IS_ERR(mia1321->pins_default))
			dev_err(dev, "could not get default pinstate\n");

		mia1321->pins_sleep =
			pinctrl_lookup_state(mia1321->pinctrl,
					     OF_CAMERA_PINCTRL_STATE_SLEEP);
		if (IS_ERR(mia1321->pins_sleep))
			dev_err(dev, "could not get sleep pinstate\n");
	} else {
		dev_err(dev, "no pinctrl\n");
	}

	ret = mia1321_configure_regulators(mia1321);
	if (ret) {
		dev_err(dev, "Failed to get power regulators\n");
		return ret;
	}

	mutex_init(&mia1321->mutex);

	sd = &mia1321->subdev;
	v4l2_i2c_subdev_init(sd, client, &mia1321_subdev_ops);
	ret = mia1321_initialize_controls(mia1321);
	if (ret)
		goto err_destroy_mutex;

	ret = __mia1321_power_on(mia1321);
	if (ret)
		goto err_free_handler;

	ret = mia1321_check_sensor_id(mia1321, client);
	if (ret)
		goto err_power_off;

#ifdef CONFIG_VIDEO_V4L2_SUBDEV_API
	sd->internal_ops = &mia1321_internal_ops;
	sd->flags |= V4L2_SUBDEV_FL_HAS_DEVNODE |
		     V4L2_SUBDEV_FL_HAS_EVENTS;
#endif
#if defined(CONFIG_MEDIA_CONTROLLER)
	mia1321->pad.flags = MEDIA_PAD_FL_SOURCE;
	sd->entity.function = MEDIA_ENT_F_CAM_SENSOR;
	ret = media_entity_pads_init(&sd->entity, 1, &mia1321->pad);
	if (ret < 0)
		goto err_power_off;
#endif

	memset(facing, 0, sizeof(facing));
	if (strcmp(mia1321->module_facing, "back") == 0)
		facing[0] = 'b';
	else
		facing[0] = 'f';

	snprintf(sd->name, sizeof(sd->name), "m%02d_%s_%s %s",
		 mia1321->module_index, facing,
		 MIA1321_NAME, dev_name(sd->dev));
	ret = v4l2_async_register_subdev_sensor_common(sd);
	if (ret) {
		dev_err(dev, "v4l2 async register subdev failed\n");
		goto err_clean_entity;
	}

	pm_runtime_set_active(dev);
	pm_runtime_enable(dev);
	if (mia1321->is_thunderboot)
		pm_runtime_get_sync(dev);
	else
		pm_runtime_idle(dev);
	
	return 0;

err_clean_entity:
#if defined(CONFIG_MEDIA_CONTROLLER)
	media_entity_cleanup(&sd->entity);
#endif
err_power_off:
	__mia1321_power_off(mia1321);
err_free_handler:
	v4l2_ctrl_handler_free(&mia1321->ctrl_handler);
err_destroy_mutex:
	mutex_destroy(&mia1321->mutex);

	return ret;
}

static int mia1321_remove(struct i2c_client *client)
{
	struct v4l2_subdev *sd = i2c_get_clientdata(client);
	struct mia1321 *mia1321 = to_mia1321(sd);

	v4l2_async_unregister_subdev(sd);
#if defined(CONFIG_MEDIA_CONTROLLER)
	media_entity_cleanup(&sd->entity);
#endif
	v4l2_ctrl_handler_free(&mia1321->ctrl_handler);
	mutex_destroy(&mia1321->mutex);

	pm_runtime_disable(&client->dev);
	if (!pm_runtime_status_suspended(&client->dev))
		__mia1321_power_off(mia1321);
	pm_runtime_set_suspended(&client->dev);

	return 0;
}

#if IS_ENABLED(CONFIG_OF)
static const struct of_device_id mia1321_of_match[] = {
	{ .compatible = "imagedesign,mia1321" },
	{},
};
MODULE_DEVICE_TABLE(of, mia1321_of_match);
#endif

static const struct i2c_device_id mia1321_match_id[] = {
	{ "imagedesign,mia1321", 0 },
	{ },
};

static struct i2c_driver mia1321_i2c_driver = {
	.driver = {
		.name = MIA1321_NAME,
		.pm = &mia1321_pm_ops,
		.of_match_table = of_match_ptr(mia1321_of_match),
	},
	.probe = &mia1321_probe,
	.remove = &mia1321_remove,
	.id_table = mia1321_match_id,
};

static int __init sensor_mod_init(void)
{
	return i2c_add_driver(&mia1321_i2c_driver);
}

static void __exit sensor_mod_exit(void)
{
	i2c_del_driver(&mia1321_i2c_driver);
}

#if defined(CONFIG_VIDEO_ROCKCHIP_THUNDER_BOOT_ISP) && !defined(CONFIG_INITCALL_ASYNC)
subsys_initcall(sensor_mod_init);
#else
device_initcall_sync(sensor_mod_init);
#endif
module_exit(sensor_mod_exit);

MODULE_DESCRIPTION("imagedesign mia1321 sensor driver");
MODULE_LICENSE("GPL");

