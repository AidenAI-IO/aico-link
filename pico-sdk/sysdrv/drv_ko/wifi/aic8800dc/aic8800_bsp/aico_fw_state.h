#ifndef AICO_FW_STATE_H
#define AICO_FW_STATE_H

struct aic_sdio_dev;

struct aico_fw_version_cfm {
	u32 version_lmac;
	u32 version_machw_1;
	u32 version_machw_2;
	u32 version_phy_1;
	u32 version_phy_2;
	u32 features;
	u16 max_sta_nb;
	u8 max_vif_nb;
};

bool aico_fw_state_enabled(struct aic_sdio_dev *sdiodev);
int aico_fw_prepare(struct aic_sdio_dev *sdiodev);
int aico_fw_finish(struct aic_sdio_dev *sdiodev);
const char *aico_fw_state_name(void);
void aico_fw_state_failed(void);
int rwnx_send_mm_version_req(struct aic_sdio_dev *sdiodev,
			     struct aico_fw_version_cfm *cfm);

#endif
