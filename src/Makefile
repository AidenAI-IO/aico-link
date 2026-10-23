CROSS_COMPILE := ../pico-sdk/tools/linux/toolchain/arm-rockchip830-linux-uclibcgnueabihf/bin/arm-rockchip830-linux-uclibcgnueabihf-
CC := $(CROSS_COMPILE)gcc
CXX := $(CROSS_COMPILE)g++
AR := $(CROSS_COMPILE)ar

SDK_PATH := ../pico-sdk
ROCKIT_PATH := $(SDK_PATH)/media/rockit/rockit
MPP_PATH := $(SDK_PATH)/media/mpp/release_mpp_rv1106_arm-rockchip830-linux-uclibcgnueabihf
RGA_PATH := $(SDK_PATH)/media/rga/release_rga_rv1106_arm-rockchip830-linux-uclibcgnueabihf
IVE_PATH := $(SDK_PATH)/media/ive/ive

BUILD_DIR ?= ../build
OBJ_DIR := $(BUILD_DIR)/obj
LIB_DIR := $(BUILD_DIR)/lib
BIN_DIR := $(BUILD_DIR)/bin

CFLAGS := -Wall -O2
CXXFLAGS := -Wall -O2 -std=c++11 -I$(ROCKIT_PATH)/mpi/sdk/include
LDFLAGS := -L$(ROCKIT_PATH)/lib/lib32 -L$(MPP_PATH)/lib -L$(RGA_PATH)/lib -L$(IVE_PATH)/lib -Wl,-rpath,/usr/lib/aico/platform/lib -lrockit -lrockchip_mpp -lrga -lrve -lpthread -lstdc++ -lm

SDK_LIBRARY := $(LIB_DIR)/libaico.a
SDK_OBJECT := $(OBJ_DIR)/aico_sdk.o
C_BINARIES := $(BIN_DIR)/hello $(BIN_DIR)/trigger
TOOL_BINARIES := $(BIN_DIR)/audio_service_cli
EXAMPLE_BINARIES := \
	$(BIN_DIR)/example_wakeup \
	$(BIN_DIR)/example_audio_capture \
	$(BIN_DIR)/example_audio_play \
	$(BIN_DIR)/example_camera_capture
AUDIO_SERVICE_CLI_OBJECTS := \
	$(OBJ_DIR)/audio_service_cli.o \
	$(OBJ_DIR)/audio_service_client.o \
	$(OBJ_DIR)/audio_service_protocol.o \
	$(OBJ_DIR)/uds_client.o \
	$(OBJ_DIR)/uds_message.o \
	$(OBJ_DIR)/service_status.o \
	$(OBJ_DIR)/cjson.o

all: directories $(C_BINARIES) $(SDK_LIBRARY) tools examples

examples: directories $(EXAMPLE_BINARIES)
tools: directories $(TOOL_BINARIES)

directories:
	mkdir -p $(OBJ_DIR) $(LIB_DIR) $(BIN_DIR)

$(BIN_DIR)/hello: hello.c | directories
	$(CC) $(CFLAGS) $< -o $@

$(BIN_DIR)/trigger: trigger.c | directories
	$(CC) $(CFLAGS) $< -o $@

$(SDK_LIBRARY): $(SDK_OBJECT) | directories
	rm -f $@
	$(AR) rcs $@ $^

$(OBJ_DIR)/aico_sdk.o: aico_sdk.cpp aico_sdk.h aico_log.h | directories
	$(CXX) $(CXXFLAGS) -c $< -o $@

$(BIN_DIR)/example_wakeup: example_wakeup.cpp $(SDK_LIBRARY) | directories
	$(CXX) $(CXXFLAGS) $< -L$(LIB_DIR) -laico $(LDFLAGS) -o $@

$(BIN_DIR)/example_audio_capture: example_audio_capture.cpp $(SDK_LIBRARY) | directories
	$(CXX) $(CXXFLAGS) $< -L$(LIB_DIR) -laico $(LDFLAGS) -o $@

$(BIN_DIR)/example_audio_play: example_audio_play.cpp $(SDK_LIBRARY) | directories
	$(CXX) $(CXXFLAGS) $< -L$(LIB_DIR) -laico $(LDFLAGS) -o $@

$(BIN_DIR)/example_camera_capture: example_camera_capture.cpp $(SDK_LIBRARY) | directories
	$(CXX) $(CXXFLAGS) $< -L$(LIB_DIR) -laico $(LDFLAGS) -o $@

$(BIN_DIR)/audio_service_cli: $(AUDIO_SERVICE_CLI_OBJECTS) | directories
	$(CXX) $(CXXFLAGS) $^ -lpthread -o $@

$(OBJ_DIR)/audio_service_cli.o: audio_service_cli.cpp audio_service_client.h | directories
	$(CXX) $(CXXFLAGS) -c $< -o $@

$(OBJ_DIR)/audio_service_client.o: audio_service_client.cpp audio_service_client.h | directories
	$(CXX) $(CXXFLAGS) -c $< -o $@

$(OBJ_DIR)/audio_service_protocol.o: audio_service_protocol.cpp audio_service_protocol.h | directories
	$(CXX) $(CXXFLAGS) -c $< -o $@

$(OBJ_DIR)/uds_client.o: uds_client.cpp uds_client.h | directories
	$(CXX) $(CXXFLAGS) -c $< -o $@

$(OBJ_DIR)/uds_message.o: uds_message.cpp uds_message.h | directories
	$(CXX) $(CXXFLAGS) -c $< -o $@

$(OBJ_DIR)/service_status.o: service_status.cpp service_status.h | directories
	$(CXX) $(CXXFLAGS) -c $< -o $@

$(OBJ_DIR)/cjson.o: cJSON/cJSON.c cJSON/cJSON.h | directories
	$(CC) $(CFLAGS) -c $< -o $@

clean:
	rm -rf $(BUILD_DIR)
	rm -f aico_sdk.o libaico.a
	rm -f ../output/hello ../output/trigger
	rm -f ../output/example_wakeup ../output/example_audio_capture ../output/example_audio_play ../output/example_camera_capture

.PHONY: all clean directories examples tools
