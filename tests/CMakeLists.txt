# Host-native unit tests.
# Build only sources that compile without Pico hardware / SDK dependencies.

set(CMAKE_ARCHIVE_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_LIBRARY_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
set(CMAKE_RUNTIME_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/bin)

add_library(doctest INTERFACE)
target_include_directories(doctest INTERFACE
    ${CMAKE_SOURCE_DIR}/third_party/doctest
)

set(AICO_TEST_SOURCES
    ${CMAKE_SOURCE_DIR}/src/aico_log.cpp
    ${CMAKE_SOURCE_DIR}/src/audio_playback_session.cpp
    ${CMAKE_SOURCE_DIR}/src/audio_record_session.cpp
    ${CMAKE_SOURCE_DIR}/src/service_status.cpp
    ${CMAKE_SOURCE_DIR}/src/frame_service_protocol.cpp
    ${CMAKE_SOURCE_DIR}/src/frame_ring_buffer.cpp
    ${CMAKE_SOURCE_DIR}/src/uds_message.cpp
    ${CMAKE_SOURCE_DIR}/src/uds_client.cpp
    ${CMAKE_SOURCE_DIR}/src/uds_server.cpp
    ${CMAKE_SOURCE_DIR}/src/frame_ipc.cpp
    ${CMAKE_SOURCE_DIR}/src/frame_capture_manager.cpp
    ${CMAKE_SOURCE_DIR}/src/rk628_hdmi_mode.cpp
    ${CMAKE_SOURCE_DIR}/src/frame_layout.cpp
    ${CMAKE_SOURCE_DIR}/src/frame_processing.cpp
    ${CMAKE_SOURCE_DIR}/src/frame_service_client.cpp
    ${CMAKE_SOURCE_DIR}/src/frame_service_server.cpp
    ${CMAKE_SOURCE_DIR}/src/audio_service_protocol.cpp
    ${CMAKE_SOURCE_DIR}/src/audio_service_client.cpp
    ${CMAKE_SOURCE_DIR}/src/audio_session_manager.cpp
    ${CMAKE_SOURCE_DIR}/src/audio_volume_state.cpp
    ${CMAKE_SOURCE_DIR}/src/cJSON/cJSON.c
)

# Prefer system OpenCV for normal host tests. Bundled opencv-mobile is ARM/RV1106 only: use it
# when cross-compiling or when CMAKE_C_COMPILER is the Rockchip/ARM toolchain.
find_package(OpenCV QUIET COMPONENTS core imgproc)
if(NOT OpenCV_FOUND)
    if(CMAKE_CROSSCOMPILING OR CMAKE_C_COMPILER MATCHES "rockchip830|arm-rockchip|arm-linux")
        set(OpenCV_DIR "${CMAKE_SOURCE_DIR}/third_party/opencv-mobile/lib/cmake/opencv4")
        find_package(OpenCV QUIET COMPONENTS core imgproc)
    endif()
endif()

# Debian and Ubuntu keep OpenCVConfig.cmake in the all-module libopencv-dev
# metapackage. Host tests need only core and imgproc, so discover those component
# packages directly when the config file is intentionally not installed.
if(NOT OpenCV_FOUND AND NOT CMAKE_CROSSCOMPILING)
    find_path(OpenCV_COMPONENT_INCLUDE_DIR
        NAMES opencv2/core.hpp
        PATH_SUFFIXES opencv4
    )
    find_library(OpenCV_CORE_LIBRARY NAMES opencv_core)
    find_library(OpenCV_IMGPROC_LIBRARY NAMES opencv_imgproc)
    if(OpenCV_COMPONENT_INCLUDE_DIR AND OpenCV_CORE_LIBRARY AND OpenCV_IMGPROC_LIBRARY)
        add_library(opencv_core UNKNOWN IMPORTED)
        set_target_properties(opencv_core PROPERTIES
            IMPORTED_LOCATION "${OpenCV_CORE_LIBRARY}"
            INTERFACE_INCLUDE_DIRECTORIES "${OpenCV_COMPONENT_INCLUDE_DIR}"
        )
        add_library(opencv_imgproc UNKNOWN IMPORTED)
        set_target_properties(opencv_imgproc PROPERTIES
            IMPORTED_LOCATION "${OpenCV_IMGPROC_LIBRARY}"
            INTERFACE_INCLUDE_DIRECTORIES "${OpenCV_COMPONENT_INCLUDE_DIR}"
            INTERFACE_LINK_LIBRARIES opencv_core
        )
        set(OpenCV_FOUND TRUE)
        set(OpenCV_INCLUDE_DIRS "${OpenCV_COMPONENT_INCLUDE_DIR}")
        set(OpenCV_VERSION "system-components")
    endif()
endif()

set(AICO_IMAGE_TEST_SOURCES "")
if(OpenCV_FOUND)
    add_library(aico_image STATIC ${CMAKE_SOURCE_DIR}/src/image_process.cpp)
    target_include_directories(aico_image
        PUBLIC
            ${CMAKE_SOURCE_DIR}/src
            ${OpenCV_INCLUDE_DIRS}
            ${CMAKE_SOURCE_DIR}/third_party/stb
    )
    target_link_libraries(aico_image PUBLIC opencv_imgproc)
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
    list(APPEND AICO_IMAGE_TEST_SOURCES image_process_test.cpp)
    message(STATUS "OpenCV ${OpenCV_VERSION} found; image_process tests enabled in aico_tests")
else()
    message(STATUS "OpenCV not found; image_process tests omitted from aico_tests")
endif()

list(APPEND AICO_IMAGE_TEST_SOURCES frame_jpeg_encoder_stub.cpp)
list(APPEND AICO_IMAGE_TEST_SOURCES aico_sdk_audio_stub.cpp)

add_executable(aico_tests
    test_main.cpp
    contract_fixtures_test.cpp
    aico_log_test.cpp
    frame_service_protocol_test.cpp
    frame_service_defaults_test.cpp
    rk628_hdmi_mode_test.cpp
    uds_message_test.cpp
    uds_client_server_test.cpp
    frame_ring_buffer_test.cpp
    frame_ipc_test.cpp
    frame_service_client_test.cpp
    frame_service_server_test.cpp
    frame_capture_manager_test.cpp
    frame_processing_test.cpp
    frame_layout_test.cpp
    frame_crop_bounds_test.cpp
    terminal_input_test.cpp
    service_status_test.cpp
    vad_common_test.cpp
    system_env_parser_test.cpp
    system_env_generator_test.cpp
    audio_service_protocol_test.cpp
    audio_service_client_test.cpp
    audio_playback_session_test.cpp
    audio_session_manager_test.cpp
    audio_volume_state_test.cpp
    ${CMAKE_SOURCE_DIR}/src/system_env_parser.cpp
    ${CMAKE_SOURCE_DIR}/src/system_env_generator.cpp
    ${AICO_IMAGE_TEST_SOURCES}
    ${AICO_TEST_SOURCES}
)

target_include_directories(aico_tests PRIVATE
    ${CMAKE_SOURCE_DIR}/src
)

target_compile_definitions(aico_tests PRIVATE
    AICO_SOURCE_DIR="${CMAKE_SOURCE_DIR}"
)

target_link_libraries(aico_tests PRIVATE doctest)
if(OpenCV_FOUND)
    target_link_libraries(aico_tests PRIVATE aico_image)
endif()

find_library(UTIL_LIBRARY util)
if(UTIL_LIBRARY)
    target_link_libraries(aico_tests PRIVATE ${UTIL_LIBRARY})
endif()

add_test(NAME aico_tests COMMAND $<TARGET_FILE:aico_tests>)

# Config Web is implemented by the Go Agent binary. Its Go package tests replace
# the legacy E2E driver that launched the retired C++ server. Focused Node tests
# cover the modular frontend instead of retaining source checks for that server.
find_program(GO_EXECUTABLE go)
if(GO_EXECUTABLE)
    add_test(NAME config_web_go
        COMMAND ${GO_EXECUTABLE} test ./internal/configweb
        WORKING_DIRECTORY ${CMAKE_SOURCE_DIR}/src/agent
    )
endif()

add_test(NAME logger_shell_format
    COMMAND sh ${CMAKE_SOURCE_DIR}/scripts/test_logger_format.sh
)

add_test(NAME frame_service_bridge_policy
    COMMAND sh ${CMAKE_SOURCE_DIR}/scripts/test_frame_service_bridge_policy.sh
)

add_test(NAME wifi_proxy_init_watchdog
    COMMAND sh ${CMAKE_SOURCE_DIR}/scripts/test_wifi_proxy_init.sh
)

add_test(NAME board_low_level_smoke_scripts
    COMMAND sh ${CMAKE_SOURCE_DIR}/scripts/test_board_low_level_smoke.sh
)

find_program(NODE_EXECUTABLE node)
if(NODE_EXECUTABLE)
    add_test(NAME config_web_i18n
        COMMAND ${NODE_EXECUTABLE} --experimental-vm-modules ${CMAKE_CURRENT_SOURCE_DIR}/config_web_i18n_test.mjs
    )
    add_test(NAME config_web_api
        COMMAND ${NODE_EXECUTABLE} --experimental-vm-modules ${CMAKE_CURRENT_SOURCE_DIR}/config_web_api_test.mjs
    )
    add_test(NAME config_web_routes
        COMMAND ${NODE_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/config_web_routes_test.mjs
    )
    add_test(NAME config_web_tokens
        COMMAND ${NODE_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/config_web_tokens_test.mjs
    )
    add_test(NAME config_web_ota_progress
        COMMAND ${NODE_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/config_web_ota_progress_test.mjs
    )
    add_test(NAME config_web_settings_shell
        COMMAND ${NODE_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/config_web_settings_shell_test.mjs
    )
    add_test(NAME config_web_ui_components
        COMMAND ${NODE_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/config_web_ui_components_test.mjs
    )
    add_test(NAME config_web_router
        COMMAND ${NODE_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/config_web_router_test.mjs
    )
    add_test(NAME config_web_wifi_service
        COMMAND ${NODE_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/config_web_wifi_service_test.mjs
    )
    add_test(NAME llm_logs_anthropic
        COMMAND ${NODE_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/llm_logs_anthropic_test.mjs
    )
endif()

add_test(NAME debian_ext4_canonicalizer
    COMMAND bash ${CMAKE_SOURCE_DIR}/scripts/test_debian_ext4_canonicalizer.sh
)
set_tests_properties(debian_ext4_canonicalizer PROPERTIES SKIP_RETURN_CODE 77)
