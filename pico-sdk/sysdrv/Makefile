SYSDRV_DIR=$(shell pwd)

export LC_ALL=C
SOURCE_DATE_EPOCH ?= 0
export SOURCE_DATE_EPOCH
AIDEN_BUILDROOT_REPRODUCIBLE_STATE_VERSION := 2
AIDEN_BOARDTOOLS_REPRODUCIBLE_STATE_VERSION := 1
SHELL:=/bin/bash
MAKE=make
AT?=@

################################################################################
##   Input Configuraton Overlay
################################################################################
# SYSDRV_BUILD_VERSION (RELEASE/DEBUG)
SYSDRV_BUILD_VERSION ?= RELEASE
ifneq ($(RK_BUILD_VERSION_TYPE),)
SYSDRV_BUILD_VERSION := $(RK_BUILD_VERSION_TYPE)
endif

ifneq ($(RK_JOBS),)
SYSDRV_JOBS := $(RK_JOBS)
endif

ifneq ($(RK_TOOLCHAIN_CROSS),)
SYSDRV_CROSS:= $(RK_TOOLCHAIN_CROSS)
endif

ifneq ($(RK_LIBC_TPYE),)
SYSDRV_LIB_TYPE := $(RK_LIBC_TPYE)
else
SYSDRV_LIB_TYPE ?= glibc
endif

ifneq ($(RK_BUILDROOT_DEFCONFIG),)
BUILDROOT_DEFCONFIG := $(RK_BUILDROOT_DEFCONFIG)
endif
ifeq ($(BUILDROOT_DEFCONFIG),)
BUILDROOT_DEFCONFIG := luckfox_pico_defconfig
endif


SYSDRV_ROOTFS_FS_TYPE ?= $(fs_type)
ifneq ($(RK_ROOTFS_FS_TPYE),)
SYSDRV_ROOTFS_FS_TYPE := $(RK_ROOTFS_FS_TPYE)
endif

SYSDRV_ROOTFS_FS_COMP ?= $(fs_comp)
ifneq ($(RK_ROOTFS_FS_COMP),)
SYSDRV_ROOTFS_FS_COMP := $(RK_ROOTFS_FS_COMP)
endif

ifneq ($(RK_BOOT_MEDIUM),)
BOOT_MEDIUM := $(RK_BOOT_MEDIUM)
endif

ifeq ($(BOOT_MEDIUM),)
BOOT_MEDIUM := emmc
endif

ifneq ($(RK_KERNEL_DTS),)
KERNEL_DTS := $(RK_KERNEL_DTS)
endif
ifneq ($(RK_KERNEL_DEFCONFIG),)
KERNEL_CFG := $(RK_KERNEL_DEFCONFIG)
endif
ifneq ($(RK_KERNEL_DEFCONFIG_FRAGMENT),)
KERNEL_CFG_FRAGMENT := $(RK_KERNEL_DEFCONFIG_FRAGMENT)
endif
ifneq ($(RK_UBOOT_DEFCONFIG),)
UBOOT_CFG := $(RK_UBOOT_DEFCONFIG)
endif
ifneq ($(RK_UBOOT_DEFCONFIG_FRAGMENT),)
UBOOT_CFG_FRAGMENT := $(RK_UBOOT_DEFCONFIG_FRAGMENT)
endif

ifneq ($(RK_NAND_PAGE_SIZE),)
UBOOT_NAND_PAGE_SIZE := $(shell echo $$((RK_NAND_PAGE_SIZE)))
endif
ifneq ($(RK_NAND_BLOCK_SIZE),)
UBOOT_NAND_BLOCK_SIZE := $(shell echo $$(($(RK_NAND_BLOCK_SIZE) / 1024)))
endif

ifneq ($(RK_PROJECT_OUTPUT_IMAGE),)
OUTPUT_IMAGE_PATH := $(RK_PROJECT_OUTPUT_IMAGE)
else
OUTPUT_IMAGE_PATH :=
endif

ifneq ($(RK_PROJECT_PATH_BOARD_BIN),)
OUTPUT_BOARD_BIN_PATH := $(RK_PROJECT_PATH_BOARD_BIN)
else
OUTPUT_BOARD_BIN_PATH :=
endif

ifneq ($(RK_PROJECT_PATH_SYSDRV),)
OUTPUT_ROOTFS_SOURCE_DIR := $(RK_PROJECT_PATH_SYSDRV)
OUTPUT_ROOTFS_TOOLS_DIR := $(RK_PROJECT_PATH_SYSDRV)
else
OUTPUT_ROOTFS_SOURCE_DIR :=
OUTPUT_ROOTFS_TOOLS_DIR :=
endif

ifeq ($(RK_ENABLE_FASTBOOT),y)
OUTPUT_FASTBOOT_IMAGE_PATH := $(RK_PROJECT_PATH_FASTBOOT)
else
OUTPUT_FASTBOOT_IMAGE_PATH :=
endif

ifneq ($(OUTPUT_SYSDRV_RAMDISK_DIR),)
SYSDRV_RAMDISK_DIR := $(OUTPUT_SYSDRV_RAMDISK_DIR)
else
SYSDRV_KERNEL_INSTALL_BOOTIMG := y
SYSDRV_KERNEL_UPDATE_DTB := y
endif

################################################################################
##   rootfs Select
################################################################################
ifeq ($(RK_BOOT_MEDIUM),emmc)
ENABLE_EMMC := YES
endif

ifneq ($(LF_TARGET_ROOTFS),)
   TARGET_ROOTFS := $(LF_TARGET_ROOTFS)
else
#    TARGET_ROOTFS := buildroot
     TARGET_ROOTFS := busybox
#    TARGET_ROOTFS := custom
endif

ifneq ($(findstring $(TARGET_ROOTFS),custom),)
   ROOTFS_BUILD_ENV := custom drv 
else ifneq ($(findstring $(TARGET_ROOTFS),buildroot),)
   ROOTFS_BUILD_ENV := rootfs_prepare pctools buildroot boardtools drv
 else #busybox
   ROOTFS_BUILD_ENV := rootfs_prepare pctools busybox boardtools drv
endif

################################################################################
##   Public Configuraton
################################################################################
include $(SYSDRV_DIR)/Makefile.param

ifneq ($(RK_PARTITION_CMD_IN_ENV),)
CONFIG_SYSDRV_PARTITION := $(RK_PARTITION_CMD_IN_ENV)
endif

CONFIG_SYSDRV_PARTITION ?=
ROOTFS_PART_SIZE ?= $(shell $(SYSDRV_DIR)/tools/pc/toolkits/get_part_info.sh \
					PART_SIZE "$(CONFIG_SYSDRV_PARTITION)" rootfs)
ROOTFS_PART_NUM ?= $(shell $(SYSDRV_DIR)/tools/pc/toolkits/get_part_info.sh \
				   PART_NUM "$(CONFIG_SYSDRV_PARTITION)" rootfs $(BOOT_MEDIUM))
ENV_PART_SIZE ?= $(shell $(SYSDRV_DIR)/tools/pc/toolkits/get_part_info.sh \
				 PART_SIZE "$(CONFIG_SYSDRV_PARTITION)" env)
ENV_PART_OFFSET ?= $(shell $(SYSDRV_DIR)/tools/pc/toolkits/get_part_info.sh \
				   PART_OFFSET "$(CONFIG_SYSDRV_PARTITION)" env)

ifneq ($(findstring ext4 ext2 jffs2 squashfs ubifs erofs,$(RK_PROJECT_ROOTFS_TYPE)),)
ifneq ($(findstring FAIL,$(ROOTFS_PART_SIZE)),)
$(error ## ROOTFS_PART_SIZE is invalid)
endif
endif
ifneq ($(findstring FAIL,$(ENV_PART_SIZE)),)
$(warning ## ENV_PART_SIZE is invalid)
endif
ifneq ($(findstring FAIL,$(ENV_PART_OFFSET)),)
$(warning ## ENV_PART_OFFSET is invalid)
endif

ifneq ($(findstring $(BOOT_MEDIUM),spi_nand slc_nand),)
SYSDRV_ROOTFS_BUILD_TARGET := rootfs_ubi
ifeq ($(SYSDRV_ROOTFS_FS_TYPE),)
SYSDRV_ROOTFS_FS_TYPE := ubifs
endif
ifeq ($(SYSDRV_ROOTFS_FS_TYPE),ubifs)
KERNEL_CMDLINE_FRAGMENT += root=ubi0:rootfs
endif
ifneq ($(findstring $(SYSDRV_ROOTFS_FS_TYPE),squashfs erofs),)
KERNEL_CMDLINE_FRAGMENT += ubi.block=0,rootfs root=/dev/ubiblock0_0
endif
KERNEL_CMDLINE_FRAGMENT += rootfstype=$(SYSDRV_ROOTFS_FS_TYPE) \
						   ubi.mtd=$(ROOTFS_PART_NUM)
endif

ifeq ($(BOOT_MEDIUM), emmc)
SYSDRV_ROOTFS_BUILD_TARGET := rootfs_ext4
ifeq ($(SYSDRV_ROOTFS_FS_TYPE),)
SYSDRV_ROOTFS_FS_TYPE := ext4
endif
KERNEL_CMDLINE_FRAGMENT += rootfstype=$(SYSDRV_ROOTFS_FS_TYPE) \
						   root=/dev/mmcblk0p$(ROOTFS_PART_NUM)
endif

ifeq ($(BOOT_MEDIUM), sd_card)
SYSDRV_ROOTFS_BUILD_TARGET := rootfs_ext4
ifeq ($(SYSDRV_ROOTFS_FS_TYPE),)
SYSDRV_ROOTFS_FS_TYPE := ext4
endif
KERNEL_CMDLINE_FRAGMENT += rootfstype=$(SYSDRV_ROOTFS_FS_TYPE) \
						   root=/dev/mmcblk1p$(ROOTFS_PART_NUM)
endif

ifeq ($(BOOT_MEDIUM), spi_nor)
SYSDRV_ROOTFS_BUILD_TARGET := rootfs_jffs2
ifeq ($(SYSDRV_ROOTFS_FS_TYPE),)
SYSDRV_ROOTFS_FS_TYPE := jffs2
endif
KERNEL_CMDLINE_FRAGMENT += rootfstype=$(SYSDRV_ROOTFS_FS_TYPE) \
						   root=/dev/mtdblock$(ROOTFS_PART_NUM)
endif

ifneq ($(RK_KERNEL_CMDLINE_FRAGMENT),)
KERNEL_CMDLINE_FRAGMENT := $(RK_KERNEL_CMDLINE_FRAGMENT)
endif
ifeq ($(SYSDRV_ROOTFS_FS_TYPE),squashfs)
SYSDRV_ROOTFS_BUILD_TARGET := rootfs_squashfs
ifneq ($(findstring $(BOOT_MEDIUM),spi_nand slc_nand),)
SYSDRV_ROOTFS_BUILD_TARGET := rootfs_ubi
endif
endif

ifeq ($(SYSDRV_ROOTFS_FS_TYPE),initramfs)
SYSDRV_ROOTFS_BUILD_TARGET := rootfs_initramfs
endif

ifeq ($(ENABLE_RNDIS),y)
ifeq ($(CHIP),rv1106)
override KERNEL_CFG_FRAGMENT += rv1106-rndis.config
else
$(error TODO rndis for $(CHIP) ...)
endif
endif

ifeq ($(RK_ENABLE_FASTBOOT), y)
	ifneq ($(SYSDRV_BUILD_RECOVERY), y)
		ifeq ($(CHIP),rv1106)
			SYSDRV_DRIVER_MODULE := y
		endif
	endif
endif

# TODO: rename defconfig with BOOT_MEDIUM
KERNEL_CFG ?=
KERNEL_DTS ?=
KERNEL_DIR ?= $(SYSDRV_DIR)/source/kernel
KERNEL_DIR_DRV_KO ?= $(SYSDRV_KERNEL_OBJS_OUTPUT_DIR)/drv_ko
KERNEL_DTB :=
ifeq ($(ARCH),arm)
KERNEL_DTB := $(SYSDRV_KERNEL_OBJS_OUTPUT_DIR)/arch/$(ARCH)/boot/dts/$(KERNEL_DTS:dts=dtb)
endif
ifeq ($(ARCH),arm64)
KERNEL_DTB := $(SYSDRV_KERNEL_OBJS_OUTPUT_DIR)/arch/$(ARCH)/boot/dts/rockchip/$(KERNEL_DTS:dts=dtb)
endif

TOOLCHAIN_FILE := $(shell which $(SYSDRV_CROSS)-gcc )
TOOLCHAIN_DIR := $(shell dirname $(shell dirname $(TOOLCHAIN_FILE)))
RUNTIMELIB_DIR = $(shell dirname $(TOOLCHAIN_DIR))/$(RUNTIME_LIB)

TOOLCHAIN_RUNTIME_LIB:=runtime_lib

RKBIN_DIR := $(SYSDRV_DIR)/source/uboot/rkbin
UBOOT_DIR := $(SYSDRV_DIR)/source/uboot/u-boot
DOWNLOAD_SRC_BIN := *_download_v*.bin
DOWNLOAD_BIN := download.bin
IDBLOCK_IMG := idblock.img
IDBLOCK_SRC_BIN := *_idblock_v*.img
UBOOT_COMPILE_MAKE := $(UBOOT_DIR)/make.sh

ifneq ($(SYSDRV_UBOOT_RKBIN_OVERLAY_INI),)
UBOOT_COMPILE_MAKE_OPTS ?= --spl-new $(SYSDRV_UBOOT_RKBIN_OVERLAY_INI)
else
UBOOT_COMPILE_MAKE_OPTS ?= --spl-new
endif

UBOOT_CFG ?=
UBOOT_CFG_FRAGMENT ?=

ifeq ($(SYSDRV_BUILD_RECOVERY),y)
BUSYBOX_CFG := config_tiny_arm
endif
BUSYBOX_CFG ?= config_normal
BUSYBOX_VER ?= busybox-1.27.2
BUSYBOX_DIR := $(SYSDRV_DIR)/source/busybox
SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR ?= $(BUSYBOX_DIR)/objs_$(BUSYBOX_CFG)

BUILDROOT_CFG ?= config_normal
BUILDROOT_VER ?= buildroot-2023.02.6
BUILDROOT_DIR := $(SYSDRV_DIR)/source/buildroot

CUSTOM_VER ?= custom

TOOLCHAIN_RUNTIME_LIB_C:=lib.tar.bz2

EXT4_TOOL:=mkfs_ext4.sh
UBI_TOOL:=mkfs_ubi.sh
SQUASHFS_TOOL:=mkfs_squashfs.sh
JFFS2_TOOL:=mkfs_jffs2.sh
EROFS_TOOL:=mkfs_erofs.sh
INITRAMFS_TOOL:=mkfs_initramfs.sh

export OUT_BOARD:=board_$(SYSDRV_LIB_TYPE)_$(CHIP)
export OUT_IMAGE:=image_$(SYSDRV_LIB_TYPE)_$(CHIP)
export OUT_ROOTFS:=rootfs_$(SYSDRV_LIB_TYPE)_$(CHIP)

SYSDRV_DIR_OUT_IMAGE := $(SYSDRV_DIR)/out/$(OUT_IMAGE)
SYSDRV_DIR_OUT_BOARD := $(SYSDRV_DIR)/out/bin/$(OUT_BOARD)
SYSDRV_DIR_OUT_PC := $(SYSDRV_DIR)/out/bin/pc
SYSDRV_DIR_OUT_ROOTFS:=$(SYSDRV_DIR)/out/$(OUT_ROOTFS)

SYSDRV_KERNEL_MOD_PATH:=$(SYSDRV_DIR)/out/kernel_drv_ko
SYSDRV_ROOTFS_OUT_IMAGE := $(SYSDRV_DIR_OUT_IMAGE)/rootfs_base.img
SYSDRV_UBOOT_ENV_CFG_FILE :=$(SYSDRV_DIR_OUT_IMAGE)/.env.txt

SYSDRV_RAMDISK_DIR ?= $(SYSDRV_DIR_OUT_BOARD)

HCITOOL_TOOL_PATH :=$(SYSDRV_DIR)/tools/board/buildroot/hcitool_patch
MPV_PATCH_PATH :=$(SYSDRV_DIR)/tools/board/buildroot/mpv_patch
BUSYBOX_PATCH_PATH :=$(SYSDRV_DIR)/tools/board/buildroot/busybox_patch

define sync_buildroot_board_config
	cp $(SYSDRV_DIR)/tools/board/buildroot/luckfox_pico_defconfig $(BUILDROOT_DIR)/$(BUILDROOT_VER)/configs/
	cp $(SYSDRV_DIR)/tools/board/buildroot/luckfox_pico_w_defconfig $(BUILDROOT_DIR)/$(BUILDROOT_VER)/configs/
	cp $(SYSDRV_DIR)/tools/board/buildroot/busybox.config $(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/busybox/
	cp ${HCITOOL_TOOL_PATH}/*.patch $(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/bluez5_utils/
	cp ${MPV_PATCH_PATH}/*.patch $(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/mpv/
	cp ${BUSYBOX_PATCH_PATH}/*.patch $(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/busybox/
	$(call inject_android_tools_aiden_pkg)
	$(call inject_opkg_aiden_pkg)
	$(call inject_python_charset_normalizer_aiden_pkg)
	$(call pin_libarchive_optouts)
	$(call pin_libsolv_semantics)
endef

# Buildroot's libsolv.mk sets only the static/shared flags, so libsolv is built
# with MULTI_SEMANTICS and ENABLE_DEBIAN both defaulting to OFF. That produces a
# library hardwired to rpm version semantics, and opkg then dies at runtime on
# its very first install:
#
#     libsolv not built with Debian or Multi semantics
#     Could not initialize libsolv solver
#
# opkg_solver_libsolv.c calls pool_setdisttype(pool, DISTTYPE_DEB), and libsolv's
# pool_setdisttype is compiled to `return pool->disttype == disttype ? disttype :
# -1` unless MULTI_SEMANTICS is on. Every mutating opkg operation -- install,
# upgrade, remove -- fails closed with exit 255; read-only ones (list, info,
# download) still work, which is why a smoke test that stops at `opkg --version`
# looks perfectly healthy. Observed on the board, not theorised.
#
# MULTI_SEMANTICS=ON is exactly what Yocto (which maintains opkg) builds libsolv
# with, and it is the only flag opkg needs: opkg never uses ENABLE_COMPLEX_DEPS,
# and libsolv's zstd support is irrelevant to Entware's gzip feeds -- enabling it
# would add an undeclared zstd dependency to libsolv.mk for nothing.
#
# Not switched off again for other packages, unlike the libarchive pins above:
# libsolv exists in this config only because opkg wants it, and MULTI_SEMANTICS
# only adds runtime-selectable distribution types.
#
# The flag is inserted *before* $(eval $(cmake-package)), where every other
# LIBSOLV_CONF_OPTS assignment already lives. Appending to the end of the file
# instead would mean setting a package variable after the infrastructure that
# consumes it has been expanded -- whether that still works depends on when make
# expands the generated recipe, which is not a subtlety worth betting a 1.5 h
# build on. The guard below therefore checks placement, not just presence.
#
# Idempotent: the insert is skipped once the flag is present, so re-running on an
# already-patched tree is a no-op.
define pin_libsolv_semantics
	@set -e; \
		libsolv_mk="$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/libsolv/libsolv.mk"; \
		if ! grep -q -- '-DMULTI_SEMANTICS=ON' "$$libsolv_mk"; then \
			sed -i '/^\$$(eval \$$(cmake-package))$$/i LIBSOLV_CONF_OPTS += -DMULTI_SEMANTICS=ON' \
				"$$libsolv_mk"; \
		fi; \
		flag_line=$$(grep -n -- '-DMULTI_SEMANTICS=ON' "$$libsolv_mk" | head -1 | cut -d: -f1); \
		eval_line=$$(grep -n -- 'cmake-package' "$$libsolv_mk" | head -1 | cut -d: -f1); \
		if [ -z "$$flag_line" ] || [ -z "$$eval_line" ] || [ "$$flag_line" -ge "$$eval_line" ]; then \
			echo "failed to enable MULTI_SEMANTICS in libsolv.mk (flag=$$flag_line eval=$$eval_line)" >&2; \
			exit 1; \
		fi
endef

# opkg drags in libarchive: its Config.in selects BR2_PACKAGE_LIBARCHIVE and its
# recipe hard-depends on it. Buildroot packages that key off that symbol then
# silently gain a feature they never had here before -- and mpv 0.33.1 does not
# survive it. mpv's stream/stream_libarchive.h declares a POSIX-2008 `locale_t`,
# which this uClibc-ng toolchain never typedefs (bits/uClibc_locale.h only
# provides `__locale_t`, and there is no xlocale.h), so enabling the feature
# fails the build outright with "unknown type name 'locale_t'".
#
# Adding a package manager must not change what unrelated packages do, so every
# consumer is pinned back to the state it had before opkg existed. samba4 is
# pinned for the same reason even though it has not been observed to fail: its
# libarchive feature (smbclient tar support) is unused here, and it builds late
# and slowly, so discovering a problem there costs a full CI cycle for nothing.
# Drop a line below if that package's libarchive feature is ever actually wanted.
#
# Both substitutions are idempotent: after the rewrite the matched pattern is
# gone, so re-running on an already-patched tree is a no-op.
define pin_libarchive_optouts
	@set -e; \
		mpv_mk="$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/mpv/mpv.mk"; \
		samba4_mk="$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/samba4/samba4.mk"; \
		sed -i 's|^MPV_CONF_OPTS += --enable-libarchive$$|MPV_CONF_OPTS += --disable-libarchive|' "$$mpv_mk"; \
		sed -i 's|^SAMBA4_CONF_OPTS += --with-libarchive$$|SAMBA4_CONF_OPTS += --without-libarchive|' "$$samba4_mk"; \
		if grep -q -- '--enable-libarchive' "$$mpv_mk"; then \
			echo "failed to pin mpv off libarchive" >&2; exit 1; \
		fi; \
		if grep -q -- '+= --with-libarchive' "$$samba4_mk"; then \
			echo "failed to pin samba4 off libarchive" >&2; exit 1; \
		fi
endef

# android-tools-aiden is an out-of-tree Buildroot package (modern adb
# client, reports 1.0.41). The buildroot tree is extracted fresh from a
# tarball at build time, so the package source, its vendored tarball and
# its registration in package/Config.in must be re-injected after every
# extraction.
ANDROID_TOOLS_AIDEN_SRC := $(SYSDRV_DIR)/tools/board/buildroot/android-tools-aiden
ANDROID_TOOLS_AIDEN_TARBALL := android-tools-aiden-30.0.5p1.tar.xz
define inject_android_tools_aiden_pkg
	mkdir -p $(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/android-tools-aiden
	cp $(ANDROID_TOOLS_AIDEN_SRC)/Config.in \
		$(ANDROID_TOOLS_AIDEN_SRC)/android-tools-aiden.mk \
		$(ANDROID_TOOLS_AIDEN_SRC)/android-tools-aiden.hash \
		$(ANDROID_TOOLS_AIDEN_SRC)/*.patch \
		$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/android-tools-aiden/
	mkdir -p $(BUILDROOT_DIR)/$(BUILDROOT_VER)/dl/android-tools-aiden
	cp $(ANDROID_TOOLS_AIDEN_SRC)/$(ANDROID_TOOLS_AIDEN_TARBALL) \
		$(BUILDROOT_DIR)/$(BUILDROOT_VER)/dl/android-tools-aiden/
	grep -q 'package/android-tools-aiden/Config.in' \
		$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/Config.in || \
		{ \
			sed -i 's|\(\s*\)source "package/android-tools/Config.in"|\1source "package/android-tools/Config.in"\n\1source "package/android-tools-aiden/Config.in"|' \
				$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/Config.in; \
			grep -q 'package/android-tools-aiden/Config.in' \
				$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/Config.in || \
				{ echo "failed to inject android-tools-aiden into package/Config.in" >&2; exit 1; }; \
		}
endef

# opkg is an in-tree Buildroot package, but 2023.02.6 carries an older release
# (0.4.5). opkg-aiden holds a verbatim backport of Buildroot master's recipe
# for 0.7.0, which is still autotools, so the integration is an in-place
# overwrite of package/opkg rather than an out-of-tree package: both
# BR2_PACKAGE_OPKG and its package/Config.in registration already exist, so no
# sed injection is needed here.
#
# The overwrite is re-applied on every build because the buildroot tree is not
# always re-extracted: buildroot_create does start from a fresh tarball, but the
# plain buildroot target only extracts when the tree is absent, and the
# `make clean` triggered by a config-state change clears output/ and leaves
# package/ alone. So the patch set has to be *synced*, not merely topped up --
# removing or renaming a patch under opkg-aiden must remove it from the tree
# too, or a stale patch keeps being applied and V10 reproducibility breaks.
# Upstream 2023.02.6 ships no patches for this package, so clearing them all is
# what restores the baseline.
OPKG_AIDEN_SRC := $(SYSDRV_DIR)/tools/board/buildroot/opkg-aiden
define inject_opkg_aiden_pkg
	cp $(OPKG_AIDEN_SRC)/opkg.mk \
		$(OPKG_AIDEN_SRC)/opkg.hash \
		$(OPKG_AIDEN_SRC)/Config.in \
		$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/opkg/
	@set -e; \
		rm -f $(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/opkg/*.patch; \
		if ls $(OPKG_AIDEN_SRC)/*.patch >/dev/null 2>&1; then \
			cp $(OPKG_AIDEN_SRC)/*.patch \
				$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/opkg/; \
		fi; \
		grep -q '^OPKG_VERSION = 0.7.0$$' \
			$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/opkg/opkg.mk || \
			{ echo "failed to inject opkg-aiden into package/opkg" >&2; exit 1; }
endef

# Buildroot 2023.02.6 combines aiohttp 3.8.3, whose package metadata requires
# charset-normalizer >=2.0,<3.0, with charset-normalizer 3.0.1. Buildroot does
# not validate Python package metadata after assembling the target rootfs, so
# the image builds successfully but `python3 -m pip check` fails on the board.
#
# Keep the upstream package registration and overwrite only its recipe and hash
# with the last compatible 2.x release. Like the opkg backport above, this must
# run for every build because an already-extracted Buildroot tree can otherwise
# retain files from a previous branch. The project-owned directory is also part
# of buildroot_config_state_value below so changing the pin invalidates cached
# package output.
PYTHON_CHARSET_NORMALIZER_AIDEN_SRC := $(SYSDRV_DIR)/tools/board/buildroot/python-charset-normalizer-aiden
define inject_python_charset_normalizer_aiden_pkg
	cp $(PYTHON_CHARSET_NORMALIZER_AIDEN_SRC)/python-charset-normalizer.mk \
		$(PYTHON_CHARSET_NORMALIZER_AIDEN_SRC)/python-charset-normalizer.hash \
		$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/python-charset-normalizer/
	grep -q '^PYTHON_CHARSET_NORMALIZER_VERSION = 2.1.1$$' \
		$(BUILDROOT_DIR)/$(BUILDROOT_VER)/package/python-charset-normalizer/python-charset-normalizer.mk || \
		{ echo "failed to inject compatible python-charset-normalizer recipe" >&2; exit 1; }
endef

define buildroot_config_state_value
$$( \
	{ \
		printf '%s\n' "AIDEN_BUILDROOT_REPRODUCIBLE_STATE_VERSION=$(AIDEN_BUILDROOT_REPRODUCIBLE_STATE_VERSION)"; \
		printf '%s\n' "SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH)"; \
		sha256sum "$(SYSDRV_DIR)/Makefile"; \
		sha256sum "$(BUILDROOT_DIR)/$(BUILDROOT_VER)/.config"; \
		sha256sum "$(SYSDRV_DIR)/tools/board/buildroot/luckfox_pico_defconfig"; \
		sha256sum "$(SYSDRV_DIR)/tools/board/buildroot/luckfox_pico_w_defconfig"; \
		sha256sum "$(SYSDRV_DIR)/tools/board/buildroot/busybox.config"; \
		find "$(HCITOOL_TOOL_PATH)" "$(MPV_PATCH_PATH)" "$(BUSYBOX_PATCH_PATH)" -type f -name '*.patch' -print | sort | xargs -r sha256sum; \
		find "$(ANDROID_TOOLS_AIDEN_SRC)" "$(OPKG_AIDEN_SRC)" "$(PYTHON_CHARSET_NORMALIZER_AIDEN_SRC)" -type f -print | sort | xargs -r sha256sum; \
	} | sha256sum | awk '{print $$1}' \
)
endef

define sync_buildroot_config_state_inputs
	$(MAKE) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) olddefconfig -C $(BUILDROOT_DIR)/$(BUILDROOT_VER)
endef

# The stamp records which configuration produced the tree currently in
# output/. It is provenance, not a record of the last successful build, so it
# is written here -- before the packages are built -- and in the same shell
# block as the clean decision that consumes it.
#
# Writing it only after a successful build looks safer but is strictly worse: a
# build that dies halfway still leaves generated files behind, and leaving the
# previous configuration's hash in place makes the next build with that
# configuration conclude nothing changed and reuse packages configured for a
# different one. That is how a branch enabling BR2_PACKAGE_LIBARCHIVE (pulled
# in by opkg) left mpv configured with --enable-libarchive in a tree main then
# reused, breaking every main build afterwards. Keep the write in this define:
# splitting it back out is what let the two drift apart in the first place.
define refresh_buildroot_config_state
	@set -e; \
		stamp="$(BUILDROOT_DIR)/$(BUILDROOT_VER)/output/.aiden_buildroot_config_state"; \
		state="$(call buildroot_config_state_value)"; \
		needs_rebuild=0; \
		if [ -d "$(BUILDROOT_DIR)/$(BUILDROOT_VER)/output/build" ]; then \
			if [ ! -f "$$stamp" ]; then \
				needs_rebuild=1; \
			elif [ "$$(cat "$$stamp")" != "$$state" ]; then \
				needs_rebuild=1; \
			fi; \
		fi; \
		if [ "$$needs_rebuild" -eq 1 ]; then \
			echo "Buildroot configuration changed; rebuilding generated Buildroot state"; \
			$(MAKE) -C $(BUILDROOT_DIR)/$(BUILDROOT_VER) clean; \
		fi; \
		mkdir -p "$$(dirname "$$stamp")"; \
		printf '%s\n' "$$state" > "$$stamp"
endef

define refresh_boardtools_config_state
	@set -e; \
		stamp="$(SYSDRV_DIR)/out/.aiden_boardtools_config_state"; \
		board_tool_state_files="$$( \
			if git -C "$(SYSDRV_DIR)" rev-parse --is-inside-work-tree >/dev/null 2>&1; then \
				git -C "$(SYSDRV_DIR)" ls-files \
					Makefile \
					Makefile.param \
					cfg \
					tools/board/Makefile.tools.board.mk \
					tools/board/android-tools \
					tools/board/dosfstools \
					tools/board/e2fsprogs \
					tools/board/eudev \
					tools/board/exfatprogs \
					tools/board/fio \
					tools/board/gdb \
					tools/board/i2c-tools \
					tools/board/idb_bootconfig \
					tools/board/memtester \
					tools/board/mtd-utils \
					tools/board/rk_ota \
					tools/board/rndis \
					tools/board/rockchip_test \
					tools/board/strace \
					tools/board/stressapptest \
					tools/board/sysstat \
					tools/board/toolkits \
					tools/board/toolkits/openssl \
					tools/board/toolkits/zlib; \
			else \
				find "$(SYSDRV_DIR)/cfg" "$(SYSDRV_DIR)/tools/board" \
					-path "$(SYSDRV_DIR)/tools/board/buildroot" -prune -o \
					-path "$(SYSDRV_DIR)/tools/board/busybox" -prune -o \
					-path "$(SYSDRV_DIR)/tools/board/emmc" -prune -o \
					-path "$(SYSDRV_DIR)/tools/board/mirror_select" -prune -o \
					-type f -print | sed "s#^$(SYSDRV_DIR)/##"; \
				printf '%s\n' Makefile Makefile.param; \
			fi \
		)"; \
		state="$$( \
			{ \
				printf '%s\n' "AIDEN_BOARDTOOLS_REPRODUCIBLE_STATE_VERSION=$(AIDEN_BOARDTOOLS_REPRODUCIBLE_STATE_VERSION)"; \
				printf '%s\n' "SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH)"; \
				printf '%s\n' "CHIP=$(CHIP)"; \
				printf '%s\n' "ARCH=$(ARCH)"; \
				printf '%s\n' "BOOT_MEDIUM=$(BOOT_MEDIUM)"; \
				printf '%s\n' "SYSDRV_CROSS=$(SYSDRV_CROSS)"; \
				printf '%s\n' "SYSDRV_CROSS_CFLAGS=$(SYSDRV_CROSS_CFLAGS)"; \
				printf '%s\n' "SYSDRV_OPTS=$(SYSDRV_OPTS)"; \
				printf '%s\n' "SYSDRV_LIB_TYPE=$(SYSDRV_LIB_TYPE)"; \
				printf '%s\n' "ENABLE_ADBD=$(ENABLE_ADBD)"; \
				printf '%s\n' "ENABLE_EUDEV=$(ENABLE_EUDEV)"; \
				printf '%s\n' "ENABLE_GDB=$(ENABLE_GDB)"; \
				printf '%s\n' "ENABLE_RNDIS=$(ENABLE_RNDIS)"; \
				printf '%s\n' "ENABLE_OTA=$(ENABLE_OTA)"; \
				printf '%s\n' "ENABLE_ROCKCHIP_TEST=$(ENABLE_ROCKCHIP_TEST)"; \
				printf '%s\n' "ENABLE_SYSSTAT=$(ENABLE_SYSSTAT)"; \
				printf '%s\n' "ENABLE_FIO=$(ENABLE_FIO)"; \
				printf '%s\n' "ENABLE_I2C_TOOLS=$(ENABLE_I2C_TOOLS)"; \
				printf '%s\n' "ENABLE_EXFATPROGS=$(ENABLE_EXFATPROGS)"; \
				printf '%s\n' "$$board_tool_state_files" | sort -u | while IFS= read -r path; do \
					test -n "$$path" || continue; \
					sha256sum "$(SYSDRV_DIR)/$$path"; \
				done; \
			} | sha256sum | awk '{print $$1}' \
		)"; \
		has_boardtools_output="$$(find "$(SYSDRV_DIR)/tools/board" -type d -name out -print -quit)"; \
		needs_rebuild=0; \
		if [ -n "$$has_boardtools_output" ]; then \
			if [ ! -f "$$stamp" ]; then \
				needs_rebuild=1; \
			elif [ "$$(cat "$$stamp")" != "$$state" ]; then \
				needs_rebuild=1; \
			fi; \
		fi; \
		if [ "$$needs_rebuild" -eq 1 ]; then \
			echo "Board tools reproducibility inputs changed; rebuilding generated board tool state"; \
			$(MAKE) -C $(SYSDRV_DIR) tools_board-clean; \
		fi; \
		mkdir -p "$$(dirname "$$stamp")"; \
		printf '%s\n' "$$state" > "$$stamp"
endef

.PHONY: refresh_boardtools_config_state
refresh_boardtools_config_state:
	$(call refresh_boardtools_config_state)

export SYSDRV_DIR_OUT_PC
export SYSDRV_DIR_OUT_BOARD
export SYSDRV_DIR_OUT_ROOTFS

export SYSDRV_KERNEL_MOD_PATH
################################################################################
##   Check Configuraton
################################################################################
ifeq ($(ROOTFS_PART_SIZE),0)
$(warning Check ROOTFS_PART_SIZE is 0 on $(CONFIG_SYSDRV_PARTITION))
# default build rootfs partition size 32MB
ROOTFS_PART_SIZE := 32*0x100000
endif

ifeq ($(SYSDRV_DRIVER_MODULE), y)
sysdrv_kernel_driver_rockit_dir :=	$(shell test -d $(SYSDRV_DIR)/drv_ko/rockit/rockit-ko && \
		echo "../../../drv_ko/rockit/rockit-ko" ||\
		echo "../../../drv_ko/rockit/release_rockit-ko_rv1106_arm_asm" \
		)
sysdrv_kernel_driver_kmpp_dir :=	$(shell test -d $(SYSDRV_DIR)/drv_ko/kmpp/kmpp && \
		echo "../../../drv_ko/kmpp/kmpp" ||\
		echo "../../../drv_ko/kmpp/release_kmpp_rv1106_arm_asm" \
		)
endif

################################################################################
##    set task
################################################################################
all: uboot kernel rootfs env

#clean: uboot_clean kernel_clean buildroot_clean busybox_clean pctools_clean boardtools_clean rootfs_clean drv_clean
clean: uboot_clean kernel_clean busybox_clean pctools_clean boardtools_clean rootfs_clean drv_clean

distclean: clean out_clean

info:
	@echo -e "$(C_YELLOW)-------------------------------------------------------------------------$(C_NORMAL)"
	@echo -e "$(C_YELLOW)$(CHIP) sysdrv info:"
	@echo -e "$(C_YELLOW)    BOOT_MEDIUM:         $(BOOT_MEDIUM) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    KERNEL_CFG:          $(KERNEL_CFG)         $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    KERNEL_DTS:          $(KERNEL_DTS)         $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    KERNEL_CFG_FRAGMENT: $(KERNEL_CFG_FRAGMENT)$(C_NORMAL)"
	@echo -e "$(C_YELLOW)    KERNEL_CMDLINE_FRAGMENT: $(KERNEL_CMDLINE_FRAGMENT)$(C_NORMAL)"
	@echo -e "$(C_YELLOW)    UBOOT_CFG:           $(UBOOT_CFG)          $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    UBOOT_CFG_FRAGMENT:  $(UBOOT_CFG_FRAGMENT) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    UBOOT_ENV_SIZE:      $(ENV_PART_SIZE) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    UBOOT_ENV_OFFSET:    $(ENV_PART_OFFSET) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    ROOTFS_PART_SIZE:    $(ROOTFS_PART_SIZE) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    ROOTFS_PART_NUM:     $(ROOTFS_PART_NUM) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    SYSDRV_PARTITION:    $(CONFIG_SYSDRV_PARTITION) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    C LIBRARY TYPE:      $(SYSDRV_LIB_TYPE) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    <> kernel compile: $(C_NORMAL)"
	@echo -e "$(C_YELLOW)       cd $(KERNEL_DIR)$(C_NORMAL)"
ifeq ($(SYSDRV_DRIVER_MODULE), y)
	@echo -e "$(C_YELLOW)       ln -sf $(sysdrv_kernel_driver_rockit_dir) drivers/rockit-ko $(C_NORMAL)"
	@echo -e "$(C_YELLOW)       ln -sf $(sysdrv_kernel_driver_kmpp_dir) drivers/kmpp $(C_NORMAL)"
	@echo -en "$(C_YELLOW)"
	@echo -n "       test $$"
	@echo -n "(grep -wq \"+=.*rockit-ko\" drivers/Makefile; echo $$"
	@echo -n "? ) -eq 0 || echo \"obj-\\$$"
	@echo "(CONFIG_ROCKCHIP_THUNDER_BOOT)    += rockit-ko/\" >> drivers/Makefile"
	@echo -n "       test $$"
	@echo -n "(grep -wq \"+=.*kmpp\" drivers/Makefile; echo $$"
	@echo -n "? ) -eq 0 || echo \"obj-\\$$"
	@echo "(CONFIG_ROCKCHIP_THUNDER_BOOT)    += kmpp/\" >> drivers/Makefile"
	@echo -en "$(C_NORMAL)"
endif
	@echo -e "$(C_YELLOW)       $(MAKE) O=$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR) -C $(KERNEL_DIR) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) $(KERNEL_CFG) $(KERNEL_CFG_FRAGMENT) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)       $(MAKE) O=$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR) -C $(KERNEL_DIR) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) BOOT_ITS=$(KERNEL_DIR)/boot.its $(KERNEL_DTS:dts=img) -j$(SYSDRV_JOBS)$(C_NORMAL)"
	@echo -e "$(C_YELLOW)    <> kernel out image: boot.img $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    $(C_NORMAL)"
	@echo -e "$(C_YELLOW)    <> uboot compile: $(C_NORMAL)"
	@echo -e "$(C_YELLOW)       cd $(UBOOT_DIR)$(C_NORMAL)"
	@echo -e "$(C_YELLOW)       $(MAKE) $(UBOOT_CFG) $(UBOOT_CFG_FRAGMENT)$(C_NORMAL)"
	@echo -e "$(C_YELLOW)       ./make.sh $(UBOOT_COMPILE_MAKE_OPTS) CROSS_COMPILE=$(CROSS_COMPILE) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)       cp -fv ./$(IDBLOCK_SRC_BIN) idblock.img $(C_NORMAL)"
	@echo -e "$(C_YELLOW)       cp -fv ./$(DOWNLOAD_SRC_BIN) $(DOWNLOAD_BIN) $(C_NORMAL)"
ifeq ($(UBOOT_NAND_PAGE_SIZE),4096)
	@echo -e "$(C_YELLOW)       $(RKBIN_DIR)/tools/programmer_image_tool -i $(UBOOT_DIR)/$(DOWNLOAD_SRC_BIN) -b $(UBOOT_NAND_BLOCK_SIZE) -p 4 -2 -t spinand -o $(SYSDRV_DIR_OUT_IMAGE) $(C_NORMAL)"
endif
	@echo -e "$(C_YELLOW)    <> uboot out image: uboot.img idblock.img $(DOWNLOAD_BIN) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)-------------------------------------------------------------------------$(C_NORMAL)"

help:
	@echo -e "$(C_YELLOW)-------------------------------------------------------------------------$(C_NORMAL)"
	@echo -e "$(C_YELLOW)help: 	specified parameters:                              $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        BOOT_MEDIUM: emmc/spi_nor/spi_nand/slc_nand (default emmc) $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        KERNEL_DTS:                $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        KERNEL_CFG:                $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        KERNEL_CFG_FRAGMENT:       $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        UBOOT_CFG:              $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        UBOOT_CFG_FRAGMENT:     $(C_NORMAL)"
	@echo -e "$(C_YELLOW)$(C_NORMAL)"
	@echo -e "$(C_YELLOW)e.g.:$(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make BOOT_MEDIUM=emmc     # default build uboot/kernel/rootfs   $(C_NORMAL)"
	@echo -e "$(C_YELLOW)$(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make uboot   [BOOT_MEDIUM=...]  # build uboot $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make kernel  [BOOT_MEDIUM=...]  # build kernel $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make rootfs  [BOOT_MEDIUM=...]  # build rootfs $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make rootfs_ubi          # force package ubi rootfs image$(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make rootfs_ext4         # force package ext4 rootfs image$(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make rootfs_jffs2        # force package jffs2 rootfs image$(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make rootfs_squashfs     # force package squashfs rootfs image$(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make rootfs_erofs        # force package erofs rootfs image$(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make rootfs_initramfs    # force package initramfs rootfs image$(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make busybox_menuconfig  # config busybox $(C_NORMAL)"
	@echo -e "$(C_YELLOW)        make buildroot_menuconfig# config buildroot $(C_NORMAL)"
	@echo -e "$(C_YELLOW)------------------------------------------------------------------------- $(C_NORMAL)"

################################################################################
#	prepare
################################################################################
prepare:
	@mkdir -p $(SYSDRV_DIR_OUT_IMAGE)
	@mkdir -p $(SYSDRV_DIR_OUT_BOARD)
	@mkdir -p $(SYSDRV_DIR_OUT_PC)
	@mkdir -p $(SYSDRV_DIR_OUT_ROOTFS)

##########################################################################################
#	build uboot
##########################################################################################
uboot: prepare
	@echo -e "$(C_GREEN) ==sysdrv== build uboot  $(C_NORMAL)"
	@$(MAKE) -C $(UBOOT_DIR) $(UBOOT_CFG) $(UBOOT_CFG_FRAGMENT) || exit 1
	pushd $(UBOOT_DIR);$(UBOOT_COMPILE_MAKE) $(UBOOT_COMPILE_MAKE_OPTS) CROSS_COMPILE=$(CROSS_COMPILE) || exit 1;popd
	$(AT)test ! -f $(UBOOT_DIR)/uboot.img || cp -fv $(UBOOT_DIR)/uboot.img $(SYSDRV_DIR_OUT_IMAGE)
ifeq ($(UBOOT_NAND_PAGE_SIZE),4096)
	$(RKBIN_DIR)/tools/programmer_image_tool -i $(UBOOT_DIR)/$(DOWNLOAD_SRC_BIN) \
		-b $(UBOOT_NAND_BLOCK_SIZE) -p 4 -2 -t spinand -o $(SYSDRV_DIR_OUT_IMAGE) || exit 1
else
	$(AT)cp -fv $(UBOOT_DIR)/$(IDBLOCK_SRC_BIN) $(SYSDRV_DIR_OUT_IMAGE)/$(IDBLOCK_IMG)
endif
	$(AT)cp -fv $(UBOOT_DIR)/$(DOWNLOAD_SRC_BIN) $(SYSDRV_DIR_OUT_IMAGE)/$(DOWNLOAD_BIN)
		$(AT)pushd $(UBOOT_DIR);tar cjf $(SYSDRV_DIR_OUT_BOARD)/uboot.debug.tar.bz2 \
		./spl/u-boot-spl \
		./u-boot \
		./u-boot.map \
		./spl/u-boot-spl.bin \
		./u-boot.bin; popd;
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_IMAGE_PATH), \
		$(SYSDRV_DIR_OUT_IMAGE)/$(IDBLOCK_IMG) \
		$(SYSDRV_DIR_OUT_IMAGE)/uboot.img \
		$(SYSDRV_DIR_OUT_IMAGE)/$(DOWNLOAD_BIN))
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_BOARD_BIN_PATH), \
		$(SYSDRV_DIR_OUT_BOARD)/uboot.debug.tar.bz2)

uboot_clean:
	rm -rf $(UBOOT_DIR)/fit
	pushd $(UBOOT_DIR);$(MAKE) CROSS_COMPILE=$(CROSS_COMPILE) distclean >/dev/null;popd

##########################################################################################
#	build kernel
##########################################################################################
kernel_menuconfig:
	cp  $(KERNEL_DIR)/arch/arm/configs/$(KERNEL_CFG)  $(KERNEL_DIR)/.config
	$(MAKE) -C $(KERNEL_DIR) ARCH=$(ARCH) menuconfig
	$(MAKE) -C $(KERNEL_DIR) ARCH=$(ARCH) savedefconfig

kernel_savedefconfig:
	cp  $(KERNEL_DIR)/defconfig $(KERNEL_DIR)/arch/arm/configs/$(KERNEL_CFG)

kernel: prepare
	@echo -e "$(C_GREEN) ==sysdrv== build kernel  $(C_NORMAL)"
	$(MAKE) -C $(KERNEL_DIR) ARCH=$(ARCH) mrproper
ifeq ($(SYSDRV_DRIVER_MODULE), y)
	$(call MACRO_CHK_DRIVER_MODULE, $(SYSDRV_DIR)/drv_ko/rockit/, rockit-ko)
	$(call MACRO_CHK_DRIVER_MODULE, $(SYSDRV_DIR)/drv_ko/kmpp/, kmpp)
endif
	$(AT)test ! -f $(KERNEL_DIR)/vmlinux || make -C $(KERNEL_DIR) distclean
	@rm -rf $(KERNEL_DTB)
	$(MAKE) O=$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR) -C $(KERNEL_DIR) ARCH=$(ARCH) \
		CROSS_COMPILE=$(CROSS_COMPILE) $(KERNEL_CFG) $(KERNEL_CFG_FRAGMENT) || exit 1
	$(MAKE) O=$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR) -C $(KERNEL_DIR) ARCH=$(ARCH) \
		CROSS_COMPILE=$(CROSS_COMPILE) $(KERNEL_DTS:dts=img) BOOT_ITS=$(KERNEL_DIR)/boot.its -j$(SYSDRV_JOBS) || exit 1
ifeq ($(SYSDRV_DRIVER_MODULE), y)
	$(call MACRO_CLEAN_DRIVER_MODULE, rockit-ko)
	$(call MACRO_CLEAN_DRIVER_MODULE, kmpp)
endif
ifeq ($(SYSDRV_KERNEL_INSTALL_BOOTIMG),y)
	cp -fv $(SYSDRV_KERNEL_OBJS_OUTPUT_DIR)/boot.img $(SYSDRV_DIR_OUT_IMAGE)/boot.img
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_IMAGE_PATH), $(SYSDRV_DIR_OUT_IMAGE)/boot.img)
endif
	# Start install kernel drivers
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(SYSDRV_RAMDISK_DIR), \
		$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR)/arch/$(ARCH)/boot/Image* \
		$(KERNEL_DTB) \
		$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR)/arch/$(ARCH)/boot/zImage \
		$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR)/resource.img)
	cp -fv $(SYSDRV_KERNEL_OBJS_OUTPUT_DIR)/vmlinux $(SYSDRV_DIR_OUT_BOARD)
	cp -fv $(KERNEL_DTB) $(SYSDRV_DIR_OUT_BOARD)
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_BOARD_BIN_PATH), \
		$(SYSDRV_DIR_OUT_BOARD)/$(KERNEL_DTS:dts=dtb) $(SYSDRV_DIR_OUT_BOARD)/vmlinux)

kernel_clean:
	$(MAKE) O=$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR) -C $(KERNEL_DIR) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) distclean
	@-rm -rf $(SYSDRV_DIR)/source/objs_*
	@-rm -rf $(SYSDRV_KERNEL_MOD_PATH) $(KERNEL_DIR_DRV_KO)

##########################################################################################
#	prepare rootfs
##########################################################################################
rootfs_prepare: prepare
	@echo -e "$(C_GREEN) ==sysdrv== prepare rootfs   $(C_NORMAL)"
	rm -rf $(SYSDRV_DIR_OUT_ROOTFS)
	mkdir -p $(SYSDRV_DIR_OUT_ROOTFS)
	tar xf $(SYSDRV_DIR)/tools/board/rootfs_script.tar -C $(SYSDRV_DIR_OUT_ROOTFS)
	tar xjf $(TOOLCHAIN_DIR)/$(TOOLCHAIN_RUNTIME_LIB)/$(TOOLCHAIN_RUNTIME_LIB_C) -C $(SYSDRV_DIR_OUT_ROOTFS)
	pushd $(SYSDRV_DIR_OUT_ROOTFS)/lib/ ; mv -fv \
		libasan_preinit.o \
		libasan.so* \
		libtsan.so* \
		libubsan.so* \
		libSegFault.so \
		libmemusage.so* \
		libpcprofile.so* \
		libthread_db*.so* \
		libBrokenLocale* \
		libinproctrace.so \
		$(SYSDRV_DIR_OUT_BOARD); \
	popd

##########################################################################################
#	build busybox
##########################################################################################
busybox_menuconfig:
	@$(MAKE) O=$(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) menuconfig \
		-C $(BUSYBOX_DIR)/$(BUSYBOX_VER)/
	@cp -f $(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR)/.config \
			$(SYSDRV_DIR)/tools/board/busybox/$(BUSYBOX_CFG);

busybox: prepare
	@echo -e "$(C_GREEN) ==sysdrv== build busybox   $(C_NORMAL)"
		test -f $(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR)/_install/bin/busybox  || (\
		rm $(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR) -rf ;\
		mkdir -p $(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR) ;\
		tar xjf $(SYSDRV_DIR)/tools/board/busybox/$(BUSYBOX_VER).tar.bz2 -C $(BUSYBOX_DIR) ;\
		cp $(SYSDRV_DIR)/tools/board/busybox/$(BUSYBOX_CFG) $(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR)/.config ;\
		cp -fv $(SYSDRV_DIR)/tools/board/busybox/*.patch $(SYSDRV_DIR)/tools/board/busybox/busybox.patch.sh $(BUSYBOX_DIR)/$(BUSYBOX_VER)/;\
		pushd $(BUSYBOX_DIR)/$(BUSYBOX_VER)/;$(SHELL) ./busybox.patch.sh ; $(MAKE) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) -j$(SYSDRV_JOBS) O=$(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR)/ || exit 1; popd ;\
		CFLAGS="$(SYSDRV_CROSS_CFLAGS)" \
		$(MAKE) CROSS_COMPILE=$(CROSS_COMPILE) -C $(BUSYBOX_DIR)/$(BUSYBOX_VER) install O=$(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR) || exit 1;\
	);
ifneq ($(OUTPUT_SYSDRV_RAMDISK_TINY_ROOTFS_DIR),)
	tar xf $(SYSDRV_DIR)/tools/board/rootfs_script.tar -C $(OUTPUT_SYSDRV_RAMDISK_TINY_ROOTFS_DIR)
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_SYSDRV_RAMDISK_TINY_ROOTFS_DIR), \
		$(SYSDRV_DIR)/tools/board/busybox/init \
		$(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR)/_install)
else
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(SYSDRV_DIR_OUT_ROOTFS), \
		$(SYSDRV_DIR)/tools/board/busybox/init \
		$(SYSDRV_BUSYBOX_OBJS_OUTPUT_DIR)/_install)
endif
	# luckfox
	cp $(SYSDRV_DIR)/tools/board/android-tools/S99usb0config $(SYSDRV_DIR_OUT_ROOTFS)/etc/init.d
	touch $(SYSDRV_DIR_OUT_ROOTFS)/userdata/.busybox

busybox_clean:
	$(AT)rm -rf $(BUSYBOX_DIR)/$(BUSYBOX_VER)
	$(AT)rm -rf $(BUSYBOX_DIR)/objs_*


##########################################################################################
#   build buildroot
#   author :luckfox team
#	date:2023-11-11
##########################################################################################

buildroot_menuconfig:
	@echo -e "$(C_GREEN) ==sysdrv== buildroot memnuconfig   $(C_NORMAL)"
	$(MAKE) $(BUILDROOT_DEFCONFIG) -C $(BUILDROOT_DIR)/$(BUILDROOT_VER)
	$(MAKE) menuconfig -C $(BUILDROOT_DIR)/$(BUILDROOT_VER)
#	$(MAKE) source -C $(BUILDROOT_DIR)/$(BUILDROOT_VER)

buildroot_savedefconfig:
	@echo -e "$(C_GREEN) ==sysdrv== buildroot savedefconfig   $(C_NORMAL)"
	$(MAKE) savedefconfig -C $(BUILDROOT_DIR)/$(BUILDROOT_VER) 

buildroot_create:
	@echo -e "$(C_GREEN) ==sysdrv== buildroot create   $(C_NORMAL)"
	rm $(BUILDROOT_DIR)/$(BUILDROOT_VER) -rf ;\
	mkdir -p $(BUILDROOT_DIR)
	tar xzf $(SYSDRV_DIR)/tools/board/buildroot/$(BUILDROOT_VER).tar.gz -C $(BUILDROOT_DIR)
	$(call sync_buildroot_board_config)

buildroot: prepare
	@echo -e "$(C_GREEN) ==sysdrv== build buildroot   $(C_NORMAL)"
	test -d $(BUILDROOT_DIR)/$(BUILDROOT_VER) || (\
		mkdir -p $(BUILDROOT_DIR) ;\
		tar xzf $(SYSDRV_DIR)/tools/board/buildroot/$(BUILDROOT_VER).tar.gz -C $(BUILDROOT_DIR) ;\
	);
	$(call sync_buildroot_board_config)

	$(MAKE) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) $(BUILDROOT_DEFCONFIG) -C $(BUILDROOT_DIR)/$(BUILDROOT_VER) 
	$(SYSDRV_DIR)/tools/board/mirror_select/buildroot_mirror_select.sh $(BUILDROOT_DIR)/$(BUILDROOT_VER)/.config
	$(call sync_buildroot_config_state_inputs)
	$(call refresh_buildroot_config_state)
	$(MAKE) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) source -C $(BUILDROOT_DIR)/$(BUILDROOT_VER) 
	$(MAKE) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) -j$(SYSDRV_JOBS) -C $(BUILDROOT_DIR)/$(BUILDROOT_VER) 

ifeq ($(SYSDRV_BUILD_RAMDISK_FLAG),YES)
    tar xf $(SYSDRV_DIR)/tools/board/rootfs_script.tar -C $(BUILDROOT_DIR)/$(BUILDROOT_VER)/output/target
	ifneq ($(OUTPUT_SYSDRV_RAMDISK_TINY_ROOTFS_DIR),)
		$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_SYSDRV_RAMDISK_TINY_ROOTFS_DIR), \
			$(SYSDRV_DIR)/tools/board/buildroot/busybox/init \
			$(BUILDROOT_DIR)/$(BUILDROOT_VER)/output/target)
	else
		$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(SYSDRV_DIR_OUT_ROOTFS), \
			$(SYSDRV_DIR)/tools/board/buildroot/busybox/init \
			$(BUILDROOT_DIR)/$(BUILDROOT_VER)/output/target)
	endif
else
	tar xf $(BUILDROOT_DIR)/$(BUILDROOT_VER)/output/images/rootfs.tar -C $(SYSDRV_DIR_OUT_ROOTFS)
endif

buildroot_clean:
	$(AT)rm -rf $(BUILDROOT_DIR)/$(BUILDROOT_VER) 

##########################################################################################
#   build custom
#   author :luckfox team
#	date:2024-08-31
##########################################################################################
custom: prepare
	@echo -e "$(C_GREEN) ==sysdrv== build custom   $(C_NORMAL)"
	@echo -e "$(C_GREEN) ==$(LF_TARGET_ROOTFS)== $(C_NORMAL)"
	test ! -f $(SYSDRV_DIR)/tools/board/custom/$(CUSTOM_VER).tar.gz || (\
		mkdir -p $(SYSDRV_DIR_OUT_ROOTFS) ;\
		rm $(SYSDRV_DIR_OUT_ROOTFS)/* -rf ;\
		tar xzf $(SYSDRV_DIR)/tools/board/custom/$(CUSTOM_VER).tar.gz -C $(SYSDRV_DIR_OUT_ROOTFS) ;\
	);

custom_clean:
	$(AT)rm -rf $(SYSDRV_DIR_OUT_ROOTFS)

##########################################################################################
#	build pc tools
##########################################################################################
include $(SYSDRV_DIR)/tools/pc/Makefile.tools.pc.mk
pctools: prepare tools_pc-builds
	@echo -e "$(C_GREEN) ==sysdrv== build tools which run on pc  $(C_NORMAL)"
	-chmod a+x $(SYSDRV_DIR_OUT_PC)/*
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_ROOTFS_TOOLS_DIR), $(SYSDRV_DIR_OUT_PC), SELF)

pctools_clean:
	@echo -e "$(C_GREEN) ==sysdrv== clean tools which run on pc  $(C_NORMAL)"

##########################################################################################
#	build board tools
##########################################################################################
include $(SYSDRV_DIR)/tools/board/Makefile.tools.board.mk
boardtools: refresh_boardtools_config_state
	$(MAKE) -C $(SYSDRV_DIR) tools_board-builds
	@echo -e "$(C_GREEN) ==sysdrv== build tools which run on board   $(C_NORMAL)"

boardtools_clean: tools_board-clean
	@echo -e "$(C_GREEN) ==sysdrv== build $@ done$(C_NORMAL)"

##########################################################################################
#	build driver ko
##########################################################################################
drv: prepare
	@echo -e "$(C_GREEN) ==sysdrv== build drv $(C_NORMAL)"
	@-rm -rf $(SYSDRV_KERNEL_MOD_PATH) $(KERNEL_DIR_DRV_KO)
	@-mkdir -p $(SYSDRV_KERNEL_MOD_PATH)
	$(MAKE) -C $(KERNEL_DIR) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) -j$(SYSDRV_JOBS) \
			O=$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR) \
		modules
	$(MAKE) -C $(KERNEL_DIR) ARCH=$(ARCH) CROSS_COMPILE=$(CROSS_COMPILE) -j$(SYSDRV_JOBS) \
		O=$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR) \
		modules_install INSTALL_MOD_STRIP=1 \
		INSTALL_MOD_PATH=$(KERNEL_DIR_DRV_KO)
	$(AT)find $(KERNEL_DIR_DRV_KO)/lib/modules/*/kernel \
		-name "*.ko" -type f -exec cp -fav {} $(SYSDRV_KERNEL_MOD_PATH) \;
	@test ! -d drv_ko || ($(MAKE) O=$(SYSDRV_KERNEL_OBJS_OUTPUT_DIR) -C $(SYSDRV_DIR)/drv_ko KERNEL_DRV_KO_INSTALL_PATH=$(SYSDRV_KERNEL_MOD_PATH))
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_ROOTFS_SOURCE_DIR), $(SYSDRV_KERNEL_MOD_PATH), SELF)

drv_clean:
	@test ! -d drv_ko || $(MAKE) -C $(SYSDRV_DIR)/drv_ko clean
	@-rm -rf $(SYSDRV_KERNEL_MOD_PATH) $(KERNEL_DIR_DRV_KO)
	@echo -e "$(C_GREEN) ==sysdrv== clean drv $(C_NORMAL)"

##########################################################################################
#	build rootfs
##########################################################################################
rootfs: $(ROOTFS_BUILD_ENV)
	@echo -e "$(C_GREEN) ==sysdrv== build rootfs  $(C_NORMAL)"
	#@if [ "$(TARGET_ROOTFS)" = "custom" ]; then \
	#	test -d $(SYSDRV_DIR)/tools/board/custom_rootfs || ( \
	#		cp -r $(SYSDRV_DIR)/tools/board/custom_rootfs/ $(SYSDRV_DIR)/out/$(OUT_ROOTFS) ; \
	#	); \
	#else \
	#	make -C $(SYSDRV_DIR) strip \
	#fi

	pushd $(SYSDRV_DIR)/out; \
		tar --sort=name --mtime="@$(SOURCE_DATE_EPOCH)" --owner=0 --group=0 --numeric-owner -cf $(OUT_ROOTFS).tar $(OUT_ROOTFS); \
	popd
	@# RK_PARTITION_FS_TYPE_CFG is defined outside of sysdrv,
	@# so if defined RK_PARTITION_FS_TYPE_CFG, we ignore SYSDRV_ROOTFS_BUILD_TARGET
	@test "$(ROOTFS_PART_SIZE)" = "FAIL" || \
		test "$(RK_PARTITION_FS_TYPE_CFG)x" != "x" || \
		make -C $(SYSDRV_DIR) $(SYSDRV_ROOTFS_BUILD_TARGET)
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_ROOTFS_SOURCE_DIR), $(SYSDRV_DIR)/out/$(OUT_ROOTFS).tar )
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(OUTPUT_ROOTFS_TOOLS_DIR), $(SYSDRV_DIR)/out/bin )
	@echo -e "$(C_GREEN) ==sysdrv== finish sysdrv build  $(C_NORMAL)"

strip:
ifeq ($(SYSDRV_BUILD_VERSION),RELEASE)
	@echo -e "$(C_GREEN) ==sysdrv== strip files of rootfs  $(C_NORMAL)"
	$(call MAROC_STRIP_DEBUG_SYMBOL, $(SYSDRV_DIR_OUT_ROOTFS))
endif

rootfs_ubi:
	@echo -e "$(C_GREEN) ==sysdrv== build ubi rootfs  $(C_NORMAL)"
	# build rootfs as follow:
	# the pagesize = 2k, blocksize = 128k, part_size = 32M
	# the pagesize = 2k, blocksize = 256k, part_size = 32M
	# the pagesize = 4k, blocksize = 256k, part_size = 32M
	# command format:
	# $(UBI_TOOL) <source dir> <output dir> <partition size> <partition name> <fs type> <fs comp>"
	$(SYSDRV_DIR_OUT_PC)/$(UBI_TOOL) $(SYSDRV_DIR_OUT_ROOTFS) $(SYSDRV_DIR_OUT_IMAGE) $(ROOTFS_PART_SIZE) rootfs $(SYSDRV_ROOTFS_FS_TYPE) $(SYSDRV_ROOTFS_FS_COMP)

rootfs_jffs2:
	@echo -e "$(C_GREEN) ==sysdrv== build jffs2 rootfs  $(C_NORMAL)"
	# use for spi nor
	$(SYSDRV_DIR_OUT_PC)/$(JFFS2_TOOL) $(SYSDRV_DIR_OUT_ROOTFS) $(SYSDRV_ROOTFS_OUT_IMAGE) $(ROOTFS_PART_SIZE)

rootfs_squashfs:
	@echo -e "$(C_GREEN) ==sysdrv== build squashfs rootfs  $(C_NORMAL)"
	# SYSDRV_ROOTFS_FS_COMP support lz4|lzo|lzma|xz|gzip (default gzip)
	$(SYSDRV_DIR_OUT_PC)/$(SQUASHFS_TOOL) $(SYSDRV_DIR_OUT_ROOTFS) $(SYSDRV_ROOTFS_OUT_IMAGE) $(SYSDRV_ROOTFS_FS_COMP)

rootfs_erofs:
	@echo -e "$(C_GREEN) ==sysdrv== build erofs rootfs  $(C_NORMAL)"
	# SYSDRV_ROOTFS_FS_COMP support lz4|lz4hc (default lz4hc)
	$(SYSDRV_DIR_OUT_PC)/$(EROFS_TOOL) $(SYSDRV_DIR_OUT_ROOTFS) $(SYSDRV_ROOTFS_OUT_IMAGE) $(SYSDRV_ROOTFS_FS_COMP)

rootfs_ext4:
	@echo -e "$(C_GREEN) ==sysdrv== build ext4 rootfs  $(C_NORMAL)"
	$(SYSDRV_DIR_OUT_PC)/$(EXT4_TOOL) $(SYSDRV_DIR_OUT_ROOTFS) $(SYSDRV_ROOTFS_OUT_IMAGE) $(ROOTFS_PART_SIZE)

rootfs_initramfs:
	@echo -e "$(C_GREEN) ==sysdrv== build initramfs rootfs  $(C_NORMAL)"
	$(SYSDRV_DIR_OUT_PC)/$(INITRAMFS_TOOL) $(SYSDRV_DIR_OUT_ROOTFS) $(SYSDRV_ROOTFS_OUT_IMAGE)

rootfs_clean:
	$(AT)rm $(SYSDRV_DIR_OUT_ROOTFS)/ -rf
	test ! -d $(BUILDROOT_DIR)/$(BUILDROOT_VER)/output/target  || (\
		rm -rf $(BUILDROOT_DIR)/$(BUILDROOT_VER)/output/target ;\
		find $(BUILDROOT_DIR)/$(BUILDROOT_VER) -name ".stamp_target_installed" -exec rm {} \; ;\
	);

##########################################################################################
#	build env.img
##########################################################################################
env: prepare
	@$(MAKE) -C $(SYSDRV_DIR)/tools/pc/uboot_tools
ifeq ($(BOOT_MEDIUM),emmc)
	@echo "blkdevparts=mmcblk0:$(CONFIG_SYSDRV_PARTITION)" > $(SYSDRV_UBOOT_ENV_CFG_FILE)
	@test "$(ROOTFS_PART_NUM)" = "FAIL" || \
		echo "sys_bootargs=root=/dev/mmcblk0p$(ROOTFS_PART_NUM)" >> $(SYSDRV_UBOOT_ENV_CFG_FILE)
endif
ifeq ($(BOOT_MEDIUM),sd_card)
	@echo "blkdevparts=mmcblk1:$(CONFIG_SYSDRV_PARTITION)" > $(SYSDRV_UBOOT_ENV_CFG_FILE)
	@test "$(ROOTFS_PART_NUM)" = "FAIL" || \
		echo "sys_bootargs=root=/dev/mmcblk1p$(ROOTFS_PART_NUM)" >> $(SYSDRV_UBOOT_ENV_CFG_FILE)
endif
ifeq ($(BOOT_MEDIUM),spi_nor)
	@echo "mtdparts=sfc_nor:$(CONFIG_SYSDRV_PARTITION)" > $(SYSDRV_UBOOT_ENV_CFG_FILE)
	@test "$(ROOTFS_PART_NUM)" = "FAIL" || \
		echo "sys_bootargs=root=/dev/mtdblock$(ROOTFS_PART_NUM)" >> $(SYSDRV_UBOOT_ENV_CFG_FILE)
endif
ifneq ($(findstring $(BOOT_MEDIUM),spi_nand slc_nand),)
	@echo "mtdparts=rk-nand:$(CONFIG_SYSDRV_PARTITION)" > $(SYSDRV_UBOOT_ENV_CFG_FILE)
	@test "$(ROOTFS_PART_NUM)" = "FAIL" || \
		echo "sys_bootargs=root=ubi.mtd=$(ROOTFS_PART_NUM)" >> $(SYSDRV_UBOOT_ENV_CFG_FILE)
endif
ifeq ($(findstring FAIL,$(ENV_PART_SIZE)),)
	$(SYSDRV_DIR_OUT_PC)/mkenvimage -s $(ENV_PART_SIZE) -p 0x0 -o $(SYSDRV_DIR_OUT_IMAGE)/env.img $(SYSDRV_UBOOT_ENV_CFG_FILE)
endif

##########################################################################################
#	clean out
##########################################################################################
out_clean:
	@rm $(SYSDRV_DIR)/out -rf

# printvars prints all the variables currently defined in our
# Makefiles. Alternatively, if a non-empty VARS variable is passed,
# only the variables matching the make pattern passed in VARS are
# displayed.
.PHONY: printvars
printvars:
	@:$(foreach V, \
		$(sort $(if $(VARS),$(filter $(VARS),$(.VARIABLES)),$(.VARIABLES))), \
		$(if $(filter-out environment% default automatic, \
				$(origin $V)), \
		$(if $(QUOTED_VARS),\
			$(info $V='$(subst ','\'',$(if $(RAW_VARS),$(value $V),$($V)))'), \
			$(info $V=$(if $(RAW_VARS),$(value $V),$($V))))))
