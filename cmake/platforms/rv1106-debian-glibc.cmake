set(AICO_DEBIAN_VENDOR_LIB_DIR
    "${SDK_PATH}/project/cfg/BoardConfig_IPC/overlay/overlay-luckfox-glibc-rockchip/usr/lib"
    CACHE PATH "Rockchip RV1106 glibc library directory")
set(AICO_DEBIAN_OPENCV_DIR
    "${CMAKE_SOURCE_DIR}/output/debian-apps/opencv-mobile/lib/cmake/opencv4"
    CACHE PATH "Debian armhf OpenCV CMake package path")
set(AICO_RKNPU2_ROOT
    "${CMAKE_SOURCE_DIR}/third_party/rknpu2/v2.3.2"
    CACHE PATH "Rockchip RKNPU2 armhf runtime root")

set(AICO_OPENCV_DIR "${AICO_DEBIAN_OPENCV_DIR}")
set(AICO_ROCKIT_INCLUDE_DIR
    "${SDK_PATH}/media/rockit/rockit/mpi/sdk/include")

add_library(Rockchip::Rockit STATIC IMPORTED GLOBAL)
set_target_properties(Rockchip::Rockit PROPERTIES
    IMPORTED_LOCATION "${AICO_DEBIAN_VENDOR_LIB_DIR}/librockit.a")

add_library(Rockchip::MPP STATIC IMPORTED GLOBAL)
set_target_properties(Rockchip::MPP PROPERTIES
    IMPORTED_LOCATION "${AICO_DEBIAN_VENDOR_LIB_DIR}/librockchip_mpp.a")

add_library(Rockchip::RGA SHARED IMPORTED GLOBAL)
set_target_properties(Rockchip::RGA PROPERTIES
    IMPORTED_LOCATION "${AICO_DEBIAN_VENDOR_LIB_DIR}/librga.so")

# The SDK contains no glibc RVE/IVE runtime. Aico Link does not call RVE directly,
# so Debian intentionally omits the legacy unconditional dependency.
set(AICO_OPTIONAL_MEDIA_TARGETS "")
set(AICO_PLATFORM_COMPAT_SOURCES
    "${CMAKE_SOURCE_DIR}/src/rockchip_glibc_compat.c")
set(AICO_RKNN_COMPAT_SOURCES
    "${CMAKE_SOURCE_DIR}/src/rknn_glibc_compat.c")

add_library(Rockchip::RKNNMicroStatic STATIC IMPORTED GLOBAL)
set_target_properties(Rockchip::RKNNMicroStatic PROPERTIES
    IMPORTED_LOCATION "${AICO_RKNPU2_ROOT}/lib/librknnmrt.a")
set(AICO_RKNN_TARGET Rockchip::RKNNMicroStatic)
set(AICO_RKNN_INCLUDE_DIR "${CMAKE_SOURCE_DIR}/src")
set(AICO_RKNN_COMPILE_DEFINITIONS AICO_RKNN_MICRO_RUNTIME=1)

# The glibc SDK ships MPP only as a static archive. OpenCV-Mobile's Rockchip
# JPEG path resolves MPP through dlsym(), so export the exact symbols it needs
# from frame_service and force the linker to retain their archive members.
set(_aico_mpp_export_symbols
    mpp_buffer_get_with_tag
    mpp_buffer_put_with_caller
    mpp_buffer_get_ptr_with_caller
    mpp_frame_init
    mpp_frame_deinit
    mpp_frame_set_width
    mpp_frame_set_height
    mpp_frame_set_hor_stride
    mpp_frame_set_ver_stride
    mpp_frame_set_pts
    mpp_frame_set_eos
    mpp_frame_set_jpege_chan_id
    mpp_frame_set_buffer
    mpp_frame_set_fmt
    mpp_create_ext
    mpp_init
    mpp_destroy
    mpp_enc_cfg_init
    mpp_enc_cfg_deinit
    mpp_enc_cfg_set_s32
    mpp_enc_cfg_set_u32
)
foreach(_aico_mpp_symbol IN LISTS _aico_mpp_export_symbols)
    list(APPEND AICO_FRAME_SERVICE_LINK_OPTIONS
        "LINKER:--undefined=${_aico_mpp_symbol}"
        "LINKER:--export-dynamic-symbol=${_aico_mpp_symbol}")
endforeach()
unset(_aico_mpp_symbol)
unset(_aico_mpp_export_symbols)
