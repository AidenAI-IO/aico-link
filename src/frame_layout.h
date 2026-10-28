#pragma once

#include <cstddef>
#include <cstdint>
#include <vector>

namespace aico {

// Bounds of the valid image payload inside one V4L2-mapped plane. The offset
// is relative to the beginning of the mapped plane and the length is relative
// to the returned data pointer (i.e. after the offset).
struct CapturePayloadBounds {
    size_t data_offset = 0;
    size_t payload_length = 0;
};

// Resolve V4L2 bytesused/data_offset semantics without touching a device.
// V4L2 specifies that data_offset is included in bytesused, so a non-zero
// bytesused produces payload_length = bytesused - data_offset. A zero
// bytesused is accepted only when sizeimage describes a complete image.
bool capture_payload_bounds(size_t mapped_length,
                            uint32_t bytes_used,
                            uint32_t data_offset,
                            uint32_t size_image,
                            CapturePayloadBounds* bounds);

// Convert a single-plane NV12 buffer with an arbitrary row stride into the
// tight layout used by frame_service and its raw-frame consumers.
bool compact_nv12(const uint8_t* source,
                  size_t source_size,
                  uint32_t width,
                  uint32_t height,
                  uint32_t stride,
                  std::vector<uint8_t>* compact);

}  // namespace aico
