#pragma once

#include "service_status.h"
#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace aico {

// FrameServiceStatus is an alias for the shared AicoServiceStatus.
// All frame_service code continues to compile without modification.
using FrameServiceStatus = AicoServiceStatus;

struct FramePlaneMetadata {
    uint32_t offset = 0;
    uint32_t stride = 0;
    uint32_t bytes = 0;
};

struct FrameMetadata {
    uint64_t seq = 0;
    uint64_t capture_ts_ns = 0;
    uint32_t width = 0;
    uint32_t height = 0;
    uint32_t source_width = 0;
    uint32_t source_height = 0;
    uint32_t crop_x = 0;
    uint32_t crop_y = 0;
    uint32_t crop_width = 0;
    uint32_t crop_height = 0;
    std::string pixel_format;
    uint32_t stride = 0;
    uint64_t bytes = 0;
    std::vector<FramePlaneMetadata> planes;
    bool stale = false;
};

struct FrameWirePrefix {
    uint32_t header_len = 0;
    uint64_t payload_len = 0;
};

static const size_t kFrameWirePrefixSize = sizeof(uint32_t) + sizeof(uint64_t);

const char* frame_service_status_to_string(FrameServiceStatus status);
bool frame_service_status_from_string(const char* text, FrameServiceStatus* out);

std::vector<uint8_t> encode_frame_wire_prefix(uint32_t header_len, uint64_t payload_len);
bool decode_frame_wire_prefix(const uint8_t* data, size_t len, FrameWirePrefix* out);

}  // namespace aico
