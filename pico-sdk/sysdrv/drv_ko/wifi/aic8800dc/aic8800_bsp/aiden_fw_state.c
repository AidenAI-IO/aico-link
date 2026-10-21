// SPDX-License-Identifier: GPL-2.0
/* Aiden's MCU keeps the D80 powered across RV1106 resets. Probe the live
 * processor state before deciding whether shared firmware may be downloaded.
 * This policy is deliberately restricted to the verified board/chip/ROM/ABI.
 */
#include <linux/kernel.h>
#include <linux/mmc/card.h>
#include <linux/mmc/host.h>
#include <linux/mmc/sdio_func.h>
#include <linux/property.h>
#include "aicsdio.h"
#include "aic_bsp_driver.h"
#include "aiden_fw_state.h"

#define AIDEN_D80_CHIP_ID 0xfb078820
#define AIDEN_D80_CPUID 0x410fc241
#define AIDEN_D80_ROM_VTOR 0x001c2000
#define AIDEN_D80_FW_VTOR 0x00182000
#define AIDEN_D80_FW_VERSION 0x06090101

static const u32 aiden_rom_vectors[] = {
	0x001c8000, 0x00000821, 0x000008e1, 0x00003cd9,
	0x00003ce1, 0x00003ce9, 0x00003cf1, 0x00012bb8,
};
static const u32 aiden_fw_vectors[] = {
	0x001a0000, 0x001201a9, 0x00137da9, 0x00138b2d,
	0x00138b35, 0x00138b3d, 0x00138b45, AIDEN_D80_FW_VERSION,
};

enum aiden_fw_state { FW_LEGACY, FW_PROBING, FW_ROM, FW_PROVISIONED,
	FW_REUSED, FW_FAILED };
static enum aiden_fw_state fw_state;

const char *aiden_fw_state_name(void)
{
	static const char * const names[] = {
		"legacy", "probing", "rom", "provisioned", "reused", "failed",
	};
	return names[READ_ONCE(fw_state)];
}

void aiden_fw_state_failed(void)
{
	WRITE_ONCE(fw_state, FW_FAILED);
}

bool aiden_fw_state_enabled(struct aic_sdio_dev *sdiodev)
{
	return device_property_read_bool(sdiodev->func->card->host->parent,
					"aiden,aic8800d80-retained-sdio");
}

static int aiden_read_word(struct aic_sdio_dev *sdiodev, u32 addr, u32 *value)
{
	struct dbg_mem_read_cfm cfm;
	int ret = rwnx_send_dbg_mem_read_req(sdiodev, addr, &cfm);

	if (ret)
		return ret;
	if (cfm.memaddr != addr)
		return -EPROTO;
	*value = cfm.memdata;
	return 0;
}

static int aiden_match_words(struct aic_sdio_dev *sdiodev, u32 base,
			     const u32 *words, size_t count)
{
	size_t i;
	u32 value;
	int ret;

	for (i = 0; i < count; i++) {
		ret = aiden_read_word(sdiodev, base + 4 * i, &value);
		if (ret)
			return ret;
		if (value != words[i])
			return -EUCLEAN;
	}
	return 0;
}

static int aiden_runtime_ready(struct aic_sdio_dev *sdiodev)
{
	struct aiden_fw_version_cfm cfm;
	int ret;

	ret = aiden_match_words(sdiodev, AIDEN_D80_FW_VTOR,
			       aiden_fw_vectors, ARRAY_SIZE(aiden_fw_vectors));
	if (ret)
		return ret;
	ret = aiden_match_words(sdiodev, RAM_FMAC_FW_ADDR,
			       aiden_fw_vectors, ARRAY_SIZE(aiden_fw_vectors));
	if (ret)
		return ret;
	ret = rwnx_send_mm_version_req(sdiodev, &cfm);
	if (ret)
		return ret;
	if (cfm.version_lmac != AIDEN_D80_FW_VERSION ||
	    cfm.version_machw_1 != 0x0002fdfb ||
	    cfm.version_machw_2 != 0x00014047 ||
	    cfm.version_phy_1 != 0x5ee24111 ||
	    cfm.version_phy_2 != 0x01020000 ||
	    cfm.features != 0x01e877d7 ||
	    cfm.max_sta_nb != 32 || cfm.max_vif_nb != 4)
		return -EPROTONOSUPPORT;
	return 0;
}

/* Return 1 only for positively verified running firmware, 0 only for the
 * supported ROM state, and a negative error for every unresolved state. */
static int aiden_classify(struct aic_sdio_dev *sdiodev, bool allow_rom)
{
	struct mmc_card *card = sdiodev->func->card;
	u32 chip, cpuid, vtor, value;
	int ret;

	if (sdiodev->chipid != PRODUCT_ID_AIC8800D80 ||
	    aicbsp_info.cpmode != AICBSP_CPMODE_WORK ||
	    card->sdio_funcs != 2 || !card->sdio_func[0] || !card->sdio_func[1] ||
	    card->sdio_func[0]->vendor != 0xc8a1 ||
	    card->sdio_func[0]->device != 0x0082 ||
	    card->sdio_func[1]->vendor != 0xc8a1 ||
	    card->sdio_func[1]->device != 0x0182)
		return -EOPNOTSUPP;
	ret = aiden_read_word(sdiodev, 0x40500000, &chip);
	if (ret)
		return ret;
	ret = aiden_read_word(sdiodev, 0xe000ed00, &cpuid);
	if (ret)
		return ret;
	ret = aiden_read_word(sdiodev, 0xe000ed08, &vtor);
	if (ret)
		return ret;
	pr_info("aicbsp: live firmware chip=%08x cpuid=%08x vtor=%08x\n",
		chip, cpuid, vtor);
	if (chip != AIDEN_D80_CHIP_ID || cpuid != AIDEN_D80_CPUID)
		return -ENODEV;
	if (vtor == AIDEN_D80_FW_VTOR) {
		ret = aiden_runtime_ready(sdiodev);
		return ret ? ret : 1;
	}
	if (!allow_rom || vtor != AIDEN_D80_ROM_VTOR)
		return -EUCLEAN;
	ret = aiden_match_words(sdiodev, AIDEN_D80_ROM_VTOR,
			       aiden_rom_vectors, ARRAY_SIZE(aiden_rom_vectors));
	if (ret)
		return ret;
	/* A staged Wi-Fi image or applied BT patch table with ROM still active
	 * means partial provisioning, not a fresh module. Do not overwrite it.
	 * PT_INF writes these BT pointers before its POWER_ON stage. */
	ret = aiden_read_word(sdiodev, RAM_FMAC_FW_ADDR, &value);
	if (ret || value == aiden_fw_vectors[0])
		return ret ? ret : -EUCLEAN;
	ret = aiden_read_word(sdiodev, 0x001e6cd8, &value);
	if (ret || value == 0x00201940)
		return ret ? ret : -EUCLEAN;
	ret = aiden_read_word(sdiodev, 0x001e6cdc, &value);
	if (ret || value == 0x001e0000)
		return ret ? ret : -EUCLEAN;
	return 0;
}

int aiden_fw_prepare(struct aic_sdio_dev *sdiodev)
{
	int ret;

	WRITE_ONCE(fw_state, FW_PROBING);
	ret = aiden_classify(sdiodev, true);
	if (ret < 0) {
		aiden_fw_state_failed();
		pr_err("aicbsp: firmware state unresolved (%d); download refused\n", ret);
	} else if (ret == 1) {
		WRITE_ONCE(fw_state, FW_REUSED);
		pr_info("aicbsp: verified running firmware; reusing without download\n");
	} else {
		WRITE_ONCE(fw_state, FW_ROM);
		pr_info("aicbsp: verified ROM state; initial provisioning required\n");
	}
	return ret;
}

int aiden_fw_finish(struct aic_sdio_dev *sdiodev)
{
	int ret = aiden_classify(sdiodev, false);

	if (ret != 1) {
		aiden_fw_state_failed();
		return ret < 0 ? ret : -EUCLEAN;
	}
	WRITE_ONCE(fw_state, FW_PROVISIONED);
	pr_info("aicbsp: initial provisioning verified by live runtime response\n");
	return 0;
}
