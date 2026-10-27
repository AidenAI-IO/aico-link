################################################################################
#
# android-tools-aico
#
# Modern adb client (reports version 1.0.41) built from the
# nmeum/android-tools 30.0.5p1 release, which vendors the AOSP
# platform-tools-30 adb sources together with a curated BoringSSL.
#
# Only the adb *client* is built (host mode); adbd, fastboot and the
# ext4/sparse utilities are intentionally dropped. See the trimmed
# vendor/CMakeLists.txt inside the source tarball.
#
# The source tarball is produced locally (scripts/vendor-android-tools.sh):
# the git submodules are checked out at the pinned revisions, the nmeum
# core/libbase/libziparchive portability patches are pre-applied, the
# unused trees are pruned and ANDROID_TOOLS_PATCH_VENDOR is defaulted OFF
# so the build never needs git at configure time.
#
################################################################################

ANDROID_TOOLS_AICO_VERSION = 30.0.5p1
ANDROID_TOOLS_AICO_SOURCE = android-tools-aico-$(ANDROID_TOOLS_AICO_VERSION).tar.xz
# The pinned archive retains its original top-level directory; discard it
# when extracting so the package and archive can use the Aico name unchanged.
ANDROID_TOOLS_AICO_STRIP_COMPONENTS = 1
# Vendored locally; not fetched from the network. Place the tarball in
# $(BR2_DL_DIR)/android-tools-aico/ (the board build pre-populates dl/).
ANDROID_TOOLS_AICO_SITE = file://$(BR2_DL_DIR)/android-tools-aico
ANDROID_TOOLS_AICO_LICENSE = Apache-2.0, OpenSSL (BoringSSL)
ANDROID_TOOLS_AICO_LICENSE_FILES = vendor/boringssl/LICENSE

# BoringSSL's build (vendored, via its own CMake) needs host Go + Perl;
# the adb proto files need host protoc; the rest are the runtime libs the
# adb client links against. avahi provides the libdns_sd (Bonjour) compat
# library that the restored mDNS code links against (<dns_sd.h>).
ANDROID_TOOLS_AICO_DEPENDENCIES = \
	host-go \
	host-protobuf \
	protobuf \
	libusb \
	zlib \
	brotli \
	lz4 \
	zstd \
	avahi

# CMake build. find_package(Protobuf) must use the host protoc but the
# target libprotobuf; Buildroot's toolchain file points FIND_ROOT at the
# host dir for programs and the staging dir for libs, which resolves both.
#
# BUILD_SHARED_LIBS=OFF forces the vendored BoringSSL crypto/ssl (and adb's
# internal libs) to be STATIC, linked directly into the adb binary. Without
# this, Buildroot's default BUILD_SHARED_LIBS=ON builds BoringSSL as
# libcrypto.so/libssl.so which are never installed to the rootfs; adb then
# resolves those NEEDED names against OpenSSL 1.1 at runtime and fails with
# unresolved BoringSSL symbols (SPAKE2_*, EVP_AEAD_*, sk_*, HKDF, ...).
# External deps (protobuf, brotli, lz4, usb, z, zstd) are prebuilt .so found
# via find_*, so they remain dynamically linked and are unaffected.
ANDROID_TOOLS_AICO_CONF_OPTS = \
	-DCMAKE_BUILD_TYPE=Release \
	-DBUILD_SHARED_LIBS=OFF \
	-DANDROID_TOOLS_PATCH_VENDOR=OFF \
	-DProtobuf_PROTOC_EXECUTABLE=$(HOST_DIR)/bin/protoc

# host-go installs `go` onto $(HOST_DIR)/bin which is already on PATH for
# the build; BoringSSL's CMake also wants perl, taken from the host.

define ANDROID_TOOLS_AICO_INSTALL_TARGET_CMDS
	$(INSTALL) -D -m 0755 $(@D)/vendor/adb \
		$(TARGET_DIR)/usr/bin/adb
endef

$(eval $(cmake-package))
