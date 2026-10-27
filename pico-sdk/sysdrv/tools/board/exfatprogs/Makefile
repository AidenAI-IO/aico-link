
ifeq ($(SYSDRV_PARAM), )
    SYSDRV_PARAM:=../../../Makefile.param
    include $(SYSDRV_PARAM)
endif

export LC_ALL=C
SHELL:=/bin/bash

CURRENT_DIR := $(shell pwd)
PKG_TARBALL := exfatprogs-v1.2.1.tar.bz2
PKG_NAME := exfatprogs-v1.2.1
PKG_BIN := out

# The sed below swaps configure.ac's AC_PROG_LIBTOOL for AC_PROG_RANLIB:
# nothing in exfatprogs uses libtool (lib/ is a plain noinst_LIBRARIES
# static archive, which needs RANLIB), and the build environment does not
# ship libtool's m4 macros, so autoreconf fails on the unexpanded macro.
all:
	@test -f $(PKG_BIN)/usr/sbin/fsck.exfat || (\
	rm -rf $(CURRENT_DIR)/$(PKG_NAME); \
	tar -xf $(PKG_TARBALL); \
	mkdir -p $(CURRENT_DIR)/$(PKG_NAME)/$(PKG_BIN); \
	mkdir -p $(CURRENT_DIR)/$(PKG_BIN)/usr/; \
	pushd $(CURRENT_DIR)/$(PKG_NAME)/; \
		sed -i 's/^AC_PROG_LIBTOOL$$/AC_PROG_RANLIB/' configure.ac; \
		./autogen.sh || exit -1; \
		./configure --host=$(SYSDRV_CROSS) --target=$(SYSDRV_CROSS) CFLAGS="$(SYSDRV_CROSS_CFLAGS)" \
			LDFLAGS="$(SYSDRV_CROSS_CFLAGS)" --prefix=$(CURRENT_DIR)/$(PKG_BIN) || exit -1; \
		make -j$(SYSDRV_JOBS) > /dev/null || exit -1; \
		make install > /dev/null; \
		rm -rf $(CURRENT_DIR)/$(PKG_BIN)/share ; \
		rm -rf $(CURRENT_DIR)/$(PKG_BIN)/lib ; \
		mv $(CURRENT_DIR)/$(PKG_BIN)/sbin $(CURRENT_DIR)/$(PKG_BIN)/usr ; \
	popd; )
	$(call MAROC_COPY_PKG_TO_SYSDRV_OUTPUT, $(SYSDRV_DIR_OUT_ROOTFS), $(PKG_BIN))

clean: distclean

distclean:
	-rm -rf $(PKG_NAME) $(PKG_BIN)

