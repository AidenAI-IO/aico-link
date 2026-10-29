cmake_minimum_required(VERSION 3.16)
project(aico_link_sdk DESCRIPTION "Aico Link hardware SDK (https://aico.link)" LANGUAGES C CXX)

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_CXX_STANDARD 11)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

option(AICO_TESTS "Build host-native unit tests only (skips Pico targets)" OFF)
option(AICO_ENABLE_LINK_MAPS "Write a linker map for each target executable" OFF)

if(AICO_TESTS)
    enable_testing()
    add_subdirectory(tests)
    return()
endif()

set(AICO_TARGET_PLATFORM "rv1106-debian-glibc" CACHE STRING
    "Target userspace platform")
set_property(CACHE AICO_TARGET_PLATFORM PROPERTY STRINGS
    rv1106-debian-glibc
)

set(SDK_PATH "${CMAKE_SOURCE_DIR}/pico-sdk" CACHE PATH "Luckfox Pico SDK path")
set(_aico_platform_file
    "${CMAKE_SOURCE_DIR}/cmake/platforms/${AICO_TARGET_PLATFORM}.cmake")
if(NOT EXISTS "${_aico_platform_file}")
    message(FATAL_ERROR "Unsupported AICO_TARGET_PLATFORM: ${AICO_TARGET_PLATFORM}")
endif()
include("${_aico_platform_file}")

# Build-tree executables resolve ../lib. Packaged executables live in /usr/lib/aico;
# the rootfs registers /usr/lib/aico/platform/lib with ldconfig.
set(CMAKE_BUILD_RPATH "\$ORIGIN/../lib")
set(CMAKE_INSTALL_RPATH "\$ORIGIN/../lib")
set(CMAKE_BUILD_RPATH_USE_ORIGIN ON)
set(CMAKE_BUILD_WITH_INSTALL_RPATH ON)

set(CMAKE_ARCHIVE_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_LIBRARY_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_RUNTIME_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/bin)

if(NOT OpenCV_DIR)
    set(OpenCV_DIR "${AICO_OPENCV_DIR}" CACHE PATH "OpenCV CMake package path" FORCE)
endif()
find_package(OpenCV REQUIRED)

if(TARGET opencv_imgcodecs)
    set(AICO_OPENCV_CODEC_TARGET opencv_imgcodecs)
elseif(TARGET opencv_highgui)
    set(AICO_OPENCV_CODEC_TARGET opencv_highgui)
else()
    message(FATAL_ERROR "OpenCV must provide imgcodecs or the opencv-mobile highgui shim")
endif()

# opencv-mobile import sets $<LINK_ONLY:rt>; macOS has no librt.
if(APPLE)
    foreach(_ocv_t opencv_core opencv_imgproc opencv_photo opencv_video opencv_features2d opencv_highgui)
        if(TARGET ${_ocv_t})
            get_target_property(_il ${_ocv_t} INTERFACE_LINK_LIBRARIES)
            if(_il)
                string(REPLACE "$<LINK_ONLY:rt>;" "" _il "${_il}")
                string(REPLACE ";$<LINK_ONLY:rt>" "" _il "${_il}")
                set_target_properties(${_ocv_t} PROPERTIES INTERFACE_LINK_LIBRARIES "${_il}")
            endif()
        endif()
    endforeach()
endif()

add_library(aico_image STATIC
    src/image_process.cpp
)
target_include_directories(aico_image
    PUBLIC
        ${CMAKE_SOURCE_DIR}/src
        ${OpenCV_INCLUDE_DIRS}
        ${CMAKE_SOURCE_DIR}/third_party/stb
)
target_link_libraries(aico_image PUBLIC opencv_imgproc)

add_executable(image_process src/image_process_cli.cpp)
target_link_libraries(image_process PRIVATE aico_image)

add_library(cjson STATIC src/cJSON/cJSON.c)
target_include_directories(cjson PUBLIC ${CMAKE_SOURCE_DIR}/src)

if(AICO_PLATFORM_COMPAT_SOURCES)
    add_library(aico_platform_compat STATIC ${AICO_PLATFORM_COMPAT_SOURCES})
    set(AICO_PLATFORM_COMPAT_TARGET aico_platform_compat)
endif()

add_library(aico STATIC
    src/aico_log.cpp
    src/aico_sdk.cpp
    src/rockit_system.cpp
    src/service_status.cpp
    src/uds_message.cpp
    src/uds_client.cpp
    src/uds_server.cpp
    src/frame_ipc.cpp
    src/frame_capture_manager.cpp
    src/frame_camera_capture_source.cpp
    src/rk628_hdmi_mode.cpp
    src/frame_layout.cpp
    src/frame_processing.cpp
    src/frame_ring_buffer.cpp
    src/frame_service_client.cpp
    src/frame_service_protocol.cpp
    src/frame_service_server.cpp
    src/audio_service_protocol.cpp
    src/audio_volume_state.cpp
    src/audio_record_session.cpp
    src/audio_playback_session.cpp
    src/audio_session_manager.cpp
    src/audio_service_server.cpp
    src/audio_service_client.cpp
    src/frame_jpeg_encoder.cpp
)

target_include_directories(aico
    PUBLIC
        ${CMAKE_SOURCE_DIR}/src
        ${AICO_ROCKIT_INCLUDE_DIR}
        ${OpenCV_INCLUDE_DIRS}
)

target_link_libraries(aico
    PRIVATE
        cjson
        aico_image
    PUBLIC
        Rockchip::Rockit
        Rockchip::MPP
        Rockchip::RGA
        ${AICO_OPTIONAL_MEDIA_TARGETS}
        ${AICO_PLATFORM_COMPAT_TARGET}
        pthread
        dl
        rt
        m
        opencv_core
        opencv_imgproc
        ${AICO_OPENCV_CODEC_TARGET}
)

add_executable(hello src/hello.c)
add_executable(trigger src/trigger.c)

add_executable(example_wakeup src/example_wakeup.cpp)
target_link_libraries(example_wakeup PRIVATE aico)

add_executable(example_audio_capture src/example_audio_capture.cpp)
target_link_libraries(example_audio_capture PRIVATE aico)

add_executable(example_audio_play src/example_audio_play.cpp)
target_link_libraries(example_audio_play PRIVATE aico)

add_executable(example_camera_capture src/example_camera_capture.cpp)
target_link_libraries(example_camera_capture PRIVATE aico)

add_executable(example_usb_hid src/example_usb_hid.cpp src/hid_server.cpp)
target_include_directories(example_usb_hid PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(example_usb_hid PRIVATE aico aico_image)

add_executable(frame_service src/frame_service_main.cpp)
target_include_directories(frame_service PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(frame_service PRIVATE aico pthread)
if(AICO_FRAME_SERVICE_LINK_OPTIONS)
    target_link_options(frame_service PRIVATE ${AICO_FRAME_SERVICE_LINK_OPTIONS})
endif()

add_executable(frame_service_cli src/frame_service_cli.cpp)
target_include_directories(frame_service_cli PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(frame_service_cli PRIVATE aico)

add_executable(audio_service src/audio_service_main.cpp)
target_include_directories(audio_service PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(audio_service PRIVATE aico pthread)

add_executable(audio_service_cli src/audio_service_cli.cpp)
target_include_directories(audio_service_cli PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(audio_service_cli PRIVATE aico)

add_executable(audio_stream src/audio_stream.cpp)
target_link_libraries(audio_stream PRIVATE aico)

add_executable(rknn_vad
    src/rknn_vad.cpp
    src/aico_log.cpp
    ${AICO_RKNN_COMPAT_SOURCES}
)
target_include_directories(rknn_vad PRIVATE
    ${CMAKE_SOURCE_DIR}/src
    ${AICO_RKNN_INCLUDE_DIR}
)
target_compile_definitions(rknn_vad PRIVATE ${AICO_RKNN_COMPILE_DEFINITIONS})
target_link_libraries(rknn_vad PRIVATE ${AICO_RKNN_TARGET} pthread dl m)
if(AICO_RKNN_COMPAT_SOURCES)
    target_compile_definitions(rknn_vad PRIVATE AICO_RKNN_GLIBC_COMPAT=1)
endif()

add_executable(smart_turn_helper
    src/smart_turn/smart_turn_helper.c
    src/smart_turn/smart_turn.c
    src/smart_turn/whisper_mel.c
    ${AICO_RKNN_COMPAT_SOURCES}
)
target_include_directories(smart_turn_helper PRIVATE
    ${CMAKE_SOURCE_DIR}/src
    ${CMAKE_SOURCE_DIR}/src/smart_turn
    ${AICO_RKNN_INCLUDE_DIR}
)
target_compile_definitions(smart_turn_helper PRIVATE ${AICO_RKNN_COMPILE_DEFINITIONS})
target_link_libraries(smart_turn_helper PRIVATE ${AICO_RKNN_TARGET} pthread dl m)
if(AICO_RKNN_COMPAT_SOURCES)
    target_compile_definitions(smart_turn_helper PRIVATE AICO_RKNN_GLIBC_COMPAT=1)
endif()

add_executable(cpu_vad src/cpu_vad.cpp src/aico_log.cpp)
target_include_directories(cpu_vad PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(cpu_vad PRIVATE pthread m)

foreach(_vad_target rknn_vad cpu_vad smart_turn_helper)
    target_compile_options(${_vad_target} PRIVATE -O3)
    if(CMAKE_SYSTEM_PROCESSOR MATCHES "^(arm|ARM)")
        target_compile_options(${_vad_target} PRIVATE
            -mcpu=cortex-a7
            -mfpu=neon-vfpv4
            -mfloat-abi=hard
        )
    endif()
endforeach()

add_executable(aico_environment
    src/system_env_generator_main.cpp
    src/system_env_generator.cpp
    src/system_env_parser.cpp
)
target_include_directories(aico_environment PRIVATE ${CMAKE_SOURCE_DIR}/src)
set_target_properties(aico_environment PROPERTIES OUTPUT_NAME aico-environment)

if(AICO_ENABLE_LINK_MAPS)
    file(MAKE_DIRECTORY "${CMAKE_BINARY_DIR}/maps")
    foreach(_aico_executable
            hello
            trigger
            image_process
            example_wakeup
            example_audio_capture
            example_audio_play
            example_camera_capture
            example_usb_hid
            frame_service
            frame_service_cli
            audio_service
            audio_service_cli
            audio_stream
            rknn_vad
            cpu_vad
            smart_turn_helper
            aico_environment)
        target_link_options(${_aico_executable} PRIVATE
            "LINKER:-Map,${CMAKE_BINARY_DIR}/maps/${_aico_executable}.map")
    endforeach()
endif()
