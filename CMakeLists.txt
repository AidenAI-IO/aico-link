cmake_minimum_required(VERSION 3.16)
project(aiden_sdk LANGUAGES C CXX)

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_CXX_STANDARD 11)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

option(AIDEN_TESTS "Build host-native unit tests only (skips Pico targets)" OFF)
option(AIDEN_ENABLE_LINK_MAPS "Write a linker map for each target executable" OFF)

if(AIDEN_TESTS)
    enable_testing()
    add_subdirectory(tests)
    return()
endif()

set(AIDEN_TARGET_PLATFORM "rv1106-debian-glibc" CACHE STRING
    "Target userspace platform")
set_property(CACHE AIDEN_TARGET_PLATFORM PROPERTY STRINGS
    rv1106-debian-glibc
)

set(SDK_PATH "${CMAKE_SOURCE_DIR}/pico-sdk" CACHE PATH "Luckfox Pico SDK path")
set(_aiden_platform_file
    "${CMAKE_SOURCE_DIR}/cmake/platforms/${AIDEN_TARGET_PLATFORM}.cmake")
if(NOT EXISTS "${_aiden_platform_file}")
    message(FATAL_ERROR "Unsupported AIDEN_TARGET_PLATFORM: ${AIDEN_TARGET_PLATFORM}")
endif()
include("${_aiden_platform_file}")

# Build-tree executables resolve ../lib. Packaged executables live in /usr/lib/aiden;
# the rootfs registers /usr/lib/aiden/platform/lib with ldconfig.
set(CMAKE_BUILD_RPATH "\$ORIGIN/../lib")
set(CMAKE_INSTALL_RPATH "\$ORIGIN/../lib")
set(CMAKE_BUILD_RPATH_USE_ORIGIN ON)
set(CMAKE_BUILD_WITH_INSTALL_RPATH ON)

set(CMAKE_ARCHIVE_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_LIBRARY_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_RUNTIME_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/bin)

if(NOT OpenCV_DIR)
    set(OpenCV_DIR "${AIDEN_OPENCV_DIR}" CACHE PATH "OpenCV CMake package path" FORCE)
endif()
find_package(OpenCV REQUIRED)

if(TARGET opencv_imgcodecs)
    set(AIDEN_OPENCV_CODEC_TARGET opencv_imgcodecs)
elseif(TARGET opencv_highgui)
    set(AIDEN_OPENCV_CODEC_TARGET opencv_highgui)
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

add_library(aiden_image STATIC
    src/image_process.cpp
)
target_include_directories(aiden_image
    PUBLIC
        ${CMAKE_SOURCE_DIR}/src
        ${OpenCV_INCLUDE_DIRS}
        ${CMAKE_SOURCE_DIR}/third_party/stb
)
target_link_libraries(aiden_image PUBLIC opencv_imgproc)

add_executable(image_process src/image_process_cli.cpp)
target_link_libraries(image_process PRIVATE aiden_image)

add_library(cjson STATIC src/cJSON/cJSON.c)
target_include_directories(cjson PUBLIC ${CMAKE_SOURCE_DIR}/src)

if(AIDEN_PLATFORM_COMPAT_SOURCES)
    add_library(aiden_platform_compat STATIC ${AIDEN_PLATFORM_COMPAT_SOURCES})
    set(AIDEN_PLATFORM_COMPAT_TARGET aiden_platform_compat)
endif()

add_library(aiden STATIC
    src/aiden_log.cpp
    src/aiden_sdk.cpp
    src/rockit_system.cpp
    src/service_status.cpp
    src/uds_message.cpp
    src/uds_client.cpp
    src/uds_server.cpp
    src/frame_ipc.cpp
    src/frame_capture_manager.cpp
    src/frame_camera_capture_source.cpp
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

target_include_directories(aiden
    PUBLIC
        ${CMAKE_SOURCE_DIR}/src
        ${AIDEN_ROCKIT_INCLUDE_DIR}
        ${OpenCV_INCLUDE_DIRS}
)

target_link_libraries(aiden
    PRIVATE
        cjson
        aiden_image
    PUBLIC
        Rockchip::Rockit
        Rockchip::MPP
        Rockchip::RGA
        ${AIDEN_OPTIONAL_MEDIA_TARGETS}
        ${AIDEN_PLATFORM_COMPAT_TARGET}
        pthread
        dl
        rt
        m
        opencv_core
        opencv_imgproc
        ${AIDEN_OPENCV_CODEC_TARGET}
)

add_executable(hello src/hello.c)
add_executable(trigger src/trigger.c)

add_executable(example_wakeup src/example_wakeup.cpp)
target_link_libraries(example_wakeup PRIVATE aiden)

add_executable(example_audio_capture src/example_audio_capture.cpp)
target_link_libraries(example_audio_capture PRIVATE aiden)

add_executable(example_audio_play src/example_audio_play.cpp)
target_link_libraries(example_audio_play PRIVATE aiden)

add_executable(example_camera_capture src/example_camera_capture.cpp)
target_link_libraries(example_camera_capture PRIVATE aiden)

add_executable(example_usb_hid src/example_usb_hid.cpp src/hid_server.cpp)
target_include_directories(example_usb_hid PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(example_usb_hid PRIVATE aiden aiden_image)

add_executable(frame_service src/frame_service_main.cpp)
target_include_directories(frame_service PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(frame_service PRIVATE aiden pthread)
if(AIDEN_FRAME_SERVICE_LINK_OPTIONS)
    target_link_options(frame_service PRIVATE ${AIDEN_FRAME_SERVICE_LINK_OPTIONS})
endif()

add_executable(frame_service_cli src/frame_service_cli.cpp)
target_include_directories(frame_service_cli PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(frame_service_cli PRIVATE aiden)

add_executable(audio_service src/audio_service_main.cpp)
target_include_directories(audio_service PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(audio_service PRIVATE aiden pthread)

add_executable(audio_service_cli src/audio_service_cli.cpp)
target_include_directories(audio_service_cli PRIVATE ${CMAKE_SOURCE_DIR}/src)
target_link_libraries(audio_service_cli PRIVATE aiden)

add_executable(audio_stream src/audio_stream.cpp)
target_link_libraries(audio_stream PRIVATE aiden)

add_executable(rknn_vad
    src/rknn_vad.cpp
    src/aiden_log.cpp
    ${AIDEN_RKNN_COMPAT_SOURCES}
)
target_include_directories(rknn_vad PRIVATE
    ${CMAKE_SOURCE_DIR}/src
    ${AIDEN_RKNN_INCLUDE_DIR}
)
target_compile_definitions(rknn_vad PRIVATE ${AIDEN_RKNN_COMPILE_DEFINITIONS})
target_link_libraries(rknn_vad PRIVATE ${AIDEN_RKNN_TARGET} pthread dl m)
if(AIDEN_RKNN_COMPAT_SOURCES)
    target_compile_definitions(rknn_vad PRIVATE AIDEN_RKNN_GLIBC_COMPAT=1)
endif()

add_executable(smart_turn_helper
    src/smart_turn/smart_turn_helper.c
    src/smart_turn/smart_turn.c
    src/smart_turn/whisper_mel.c
    ${AIDEN_RKNN_COMPAT_SOURCES}
)
target_include_directories(smart_turn_helper PRIVATE
    ${CMAKE_SOURCE_DIR}/src
    ${CMAKE_SOURCE_DIR}/src/smart_turn
    ${AIDEN_RKNN_INCLUDE_DIR}
)
target_compile_definitions(smart_turn_helper PRIVATE ${AIDEN_RKNN_COMPILE_DEFINITIONS})
target_link_libraries(smart_turn_helper PRIVATE ${AIDEN_RKNN_TARGET} pthread dl m)
if(AIDEN_RKNN_COMPAT_SOURCES)
    target_compile_definitions(smart_turn_helper PRIVATE AIDEN_RKNN_GLIBC_COMPAT=1)
endif()

add_executable(cpu_vad src/cpu_vad.cpp src/aiden_log.cpp)
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

add_executable(aiden_environment
    src/system_env_generator_main.cpp
    src/system_env_generator.cpp
    src/system_env_parser.cpp
)
target_include_directories(aiden_environment PRIVATE ${CMAKE_SOURCE_DIR}/src)
set_target_properties(aiden_environment PROPERTIES OUTPUT_NAME aiden-environment)

if(AIDEN_ENABLE_LINK_MAPS)
    file(MAKE_DIRECTORY "${CMAKE_BINARY_DIR}/maps")
    foreach(_aiden_executable
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
            aiden_environment)
        target_link_options(${_aiden_executable} PRIVATE
            "LINKER:-Map,${CMAKE_BINARY_DIR}/maps/${_aiden_executable}.map")
    endforeach()
endif()
