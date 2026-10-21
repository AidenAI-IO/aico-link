#!/bin/bash

#################################################
# 	Board Config
#################################################
export LF_ORIGIN_BOARD_CONFIG=BoardConfig-EMMC-Buildroot-RV1106_Luckfox_Pico_Zero-IPC.mk
# Target CHIP
export RK_CHIP=rv1106

# app config
export RK_APP_TYPE=AGENT

# Config CMA size in environment
export RK_BOOTARGS_CMA_SIZE="100M"

# Kernel dts
# Aiden SCH v1 custom hardware (RV1106G): MP2720/BQ27220 PMIC on I2C1,
# RK628F/H bridge on I2C3, SDMMC0 TF card, UART0 Bluetooth, UART2 ASRPro.
export RK_KERNEL_DTS=rv1106g-aiden-custom.dts

#################################################
#	BOOT_MEDIUM
#################################################

# Target boot medium
export RK_BOOT_MEDIUM=emmc

# Uboot defconfig fragment
export RK_UBOOT_DEFCONFIG_FRAGMENT="rk-emmc.config rv1106-ab.config"

# specify post.sh for delete/overlay files
# export RK_PRE_BUILD_OEM_SCRIPT=rv1103-spi_nor-post.sh

# config partition in environment
# RK_PARTITION_CMD_IN_ENV format:
#     <partdef>[,<partdef>]
#       <partdef> := <size>[@<offset>](part-name)
# Note:
#   If the first partition offset is not 0x0, it must be added. Otherwise, it needn't adding.
export RK_PARTITION_CMD_IN_ENV="32K(env),512K@32K(idblock),256K(uboot),4M(misc),32M(boot_a),32M(boot_b),256M(oem_a),256M(oem_b),1536M(rootfs_a),1536M(rootfs_b),3G(userdata),300M(ota)"

# config partition's filesystem type (squashfs is readonly)
# emmc:    squashfs/ext4
# nand:    squashfs/ubifs
# spi nor: squashfs/jffs2
# RK_PARTITION_FS_TYPE_CFG format:
#     AAAA:/BBBB/CCCC@ext4
#         AAAA ----------> partition name
#         /BBBB/CCCC ----> partition mount point
#         ext4 ----------> partition filesystem type
export RK_PARTITION_FS_TYPE_CFG=rootfs_a@IGNORE@ext4,rootfs_b@IGNORE@ext4,userdata@/userdata@ext4,oem_a@IGNORE@ext4,oem_b@IGNORE@ext4,ota@/userdata/ota@ext4

# config filesystem compress (Just for squashfs or ubifs)
# squashfs: lz4/lzo/lzma/xz/gzip, default xz
# ubifs:    lzo/zlib, default lzo
# export RK_SQUASHFS_COMP=xz
# export RK_UBIFS_COMP=lzo

#################################################
#	TARGET_ROOTFS
#################################################

# Target rootfs
export LF_TARGET_ROOTFS=buildroot

# Buildroot defconfig
export RK_BUILDROOT_DEFCONFIG=luckfox_pico_w_defconfig

#################################################
# 	Defconfig
#################################################

# Target arch
export RK_ARCH=arm

# Target Toolchain Cross Compile
export RK_TOOLCHAIN_CROSS=arm-rockchip830-linux-uclibcgnueabihf

#misc image
export RK_MISC=wipe_all-misc.img

# Uboot defconfig
export RK_UBOOT_DEFCONFIG=luckfox_rv1106_uboot_defconfig

# Kernel defconfig
export RK_KERNEL_DEFCONFIG=luckfox_rv1106_linux_defconfig

# Kernel defconfig fragment
# Keep compressed in-memory swap, enable the UART Bluetooth controller and
# SDIO/WLAN dependencies used by the onboard AIC8800D80 combo module, and
# enable the RK628F HDMI-to-CSI bridge.
export RK_KERNEL_DEFCONFIG_FRAGMENT="aiden-zram.config rv1106-bt.config rv1106-sdiowifi.config aiden-rk628.config"

# Config sensor IQ files
# RK_CAMERA_SENSOR_IQFILES format:
#     "iqfile1 iqfile2 iqfile3 ..."
# ./build.sh media and copy <SDK root dir>/output/out/media_out/isp_iqfiles/$RK_CAMERA_SENSOR_IQFILES
# RK628D outputs YUV and does not use a sensor IQ file.
export RK_CAMERA_SENSOR_IQFILES=

# Config sensor lens CAC calibrattion bin files
#export RK_CAMERA_SENSOR_CAC_BIN="CAC_sc4336_OT01_40IRC_F16"
#export RK_CAMERA_SENSOR_CAC_BIN="CAC_sc4336_OT01_40IRC_F16 CAC_sc530ai_CMK-OT2115-PC1_30IRC-F16"

# build ipc web backend
#export RK_APP_IPCWEB_BACKEND=y

# enable install app to oem partition
export RK_BUILD_APP_TO_OEM_PARTITION=y

# enable rockchip test
# Disabled for production boot-item slimming.
# export RK_ENABLE_ROCKCHIP_TEST=y

# Aiden uses the board as an ADB host/client for external Android devices.
# Keep SDK adbd gadget support disabled.
export RK_ENABLE_ADBD=n

# mkfs.exfat/fsck.exfat for the SD-card exFAT format option
# (Makefile.param defaults this to n).
export RK_ENABLE_EXFATPROGS=y

# enable rockchip wifi
export RK_ENABLE_WIFI=y
# SKI.WB800D80S.1 is the AIC8800D80 SDIO/BT module.  The aic8800dc
# directory contains the shared AIC8800 SDIO driver and selects the chip at
# runtime from its SDIO ID.
export RK_ENABLE_WIFI_CHIP=AIC8800D80

# config wifi ssid and passwd
export LF_WIFI_SSID="Aiden"
export LF_WIFI_PSK="Aiden12333"

#################################################
#  PRE and POST
#################################################

# specify pre.sh for delete/overlay files
export RK_PRE_BUILD_OEM_SCRIPT=luckfox-buildroot-oem-pre.sh

# specify post.sh for delete/overlay files
export RK_PRE_BUILD_USERDATA_SCRIPT=luckfox-userdata-pre.sh

# declare overlay directory
export RK_POST_OVERLAY="overlay-luckfox-config overlay-luckfox-buildroot-init overlay-luckfox-buildroot-shadow overlay-luckfox-buildroot-aiden"
