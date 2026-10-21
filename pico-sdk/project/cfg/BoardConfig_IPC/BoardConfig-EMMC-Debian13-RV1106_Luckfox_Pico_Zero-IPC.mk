#!/usr/bin/env bash

#################################################
# Debian 13 production A/B board configuration
#################################################
export LF_ORIGIN_BOARD_CONFIG=BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk
export RK_CHIP=rv1106
export RK_APP_TYPE=RKIPC_RV1106
export RK_BOOTARGS_CMA_SIZE="100M"
# Keep the established wlan0/usb0 names. The isolated SDK patch appends this
# value to both slot-specific FIT bootargs.
export RK_KERNEL_CMDLINE_EXTRA=net.ifnames$'\x3d'0
export RK_KERNEL_DTS=rv1106g-aiden-custom.dts

export RK_BOOT_MEDIUM=emmc
export RK_UBOOT_DEFCONFIG_FRAGMENT="rk-emmc.config rv1106-ab.config aiden-rv1106-rockusb.config"

# Production layout: never change this in a Debian migration maintenance
# release without a separate data backup and factory recovery design.
export RK_PARTITION_CMD_IN_ENV="32K(env),512K@32K(idblock),256K(uboot),4M(misc),32M(boot_a),32M(boot_b),256M(oem_a),256M(oem_b),1536M(rootfs_a),1536M(rootfs_b),3G(userdata),300M(ota)"
export RK_PARTITION_FS_TYPE_CFG=rootfs_a@IGNORE@ext4,rootfs_b@IGNORE@ext4,userdata@/userdata@ext4,oem_a@IGNORE@ext4,oem_b@IGNORE@ext4,ota@/userdata/ota@ext4

# The SDK builds only U-Boot, the kernel, DTBs and modules. Debian rootfs and
# glibc OEM images are imported later by scripts/debian-system/build.sh.
export LF_TARGET_ROOTFS=debian
export RK_ARCH=arm
export RK_TOOLCHAIN_CROSS=arm-rockchip830-linux-uclibcgnueabihf

export RK_MISC=wipe_all-misc.img
export RK_UBOOT_DEFCONFIG=luckfox_rv1106_uboot_defconfig
export RK_KERNEL_DEFCONFIG=luckfox_rv1106_linux_defconfig
export RK_KERNEL_DEFCONFIG_FRAGMENT="aiden-zram.config rv1106-bt.config rv1106-sdiowifi.config aiden-rk628.config debian-system.config"

export RK_CAMERA_SENSOR_IQFILES=""
export RK_BUILD_APP_TO_OEM_PARTITION=y
export RK_ENABLE_ADBD=n
export RK_ENABLE_WIFI=y
export RK_ENABLE_WIFI_CHIP=AIC8800D80

# Image assembly is owned by the system stage. Do not import any Buildroot
# overlays or package the vendor IPC application into Debian artifacts.
export RK_POST_OVERLAY=""
