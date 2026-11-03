#include "frame_layout.h"

#include <cstring>
#include <limits>

namespace aico {

namespace {

bool checked_mul(size_t a, size_t b, size_t* result) {
    if (!result || (a != 0 && b > std::numeric_limits<size_t>::max() / a)) {
        return false;
    }
    *result = a * b;
    return true;
}

bool checked_add(size_t a, size_t b, size_t* result) {
    if (!result || b > std::numeric_limits<size_t>::max() - a) {
        return false;
    }
    *result = a + b;
    return true;
}

}  // namespace

bool capture_payload_bounds(size_t mapped_length,
                            uint32_t bytes_used,
                            uint32_t data_offset,
                            uint32_t size_image,
                            CapturePayloadBounds* bounds) {
    if (!bounds || data_offset > mapped_length) {
        return false;
    }

    const size_t offset = static_cast<size_t>(data_offset);
    size_t end = 0;
    if (bytes_used != 0) {
        // Per V4L2, bytesused includes data_offset.
        if (bytes_used < data_offset) {
            return false;
        }
        end = static_cast<size_t>(bytes_used);
    } else {
        // sizeimage is measured from the beginning of the plane. Do not
        // silently cap it: a short mapping cannot safely contain a frame.
        if (size_image == 0 || size_image < data_offset) {
            return false;
        }
        end = static_cast<size_t>(size_image);
    }

    if (end > mapped_length || end < offset) {
        return false;
    }

    bounds->data_offset = offset;
    bounds->payload_length = end - offset;
    return bounds->payload_length != 0;
}

bool compact_nv12(const uint8_t* source,
                  size_t source_size,
                  uint32_t width,
                  uint32_t height,
                  uint32_t stride,
                  std::vector<uint8_t>* compact) {
    if (!source || !compact || width == 0 || height == 0 ||
        (width & 1U) != 0 || (height & 1U) != 0 || stride < width) {
        return false;
    }

    size_t source_y_bytes = 0;
    if (!checked_mul(static_cast<size_t>(stride), static_cast<size_t>(height),
                     &source_y_bytes)) {
        return false;
    }
    size_t source_required = 0;
    if (!checked_add(source_y_bytes, source_y_bytes / 2U, &source_required)) {
        return false;
    }
    if (source_size < source_required) {
        return false;
    }

    size_t compact_y_bytes = 0;
    if (!checked_mul(static_cast<size_t>(width), static_cast<size_t>(height),
                     &compact_y_bytes)) {
        return false;
    }
    size_t compact_size = 0;
    if (!checked_add(compact_y_bytes, compact_y_bytes / 2U, &compact_size)) {
        return false;
    }
    compact->assign(compact_size, 0);
    const uint8_t* y_source = source;
    const uint8_t* uv_source = source + source_y_bytes;
    uint8_t* y_compact = compact->data();
    uint8_t* uv_compact = y_compact + compact_y_bytes;

    for (uint32_t y = 0; y < height; ++y) {
        std::memcpy(y_compact + static_cast<size_t>(y) * width,
                    y_source + static_cast<size_t>(y) * stride,
                    width);
    }
    for (uint32_t y = 0; y < height / 2U; ++y) {
        std::memcpy(uv_compact + static_cast<size_t>(y) * width,
                    uv_source + static_cast<size_t>(y) * stride,
                    width);
    }
    return true;
}

}  // namespace aico
