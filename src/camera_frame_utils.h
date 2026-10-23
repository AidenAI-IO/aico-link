#pragma once

#include "aico_sdk.h"

#include <cstdint>
#include <cstring>
#include <limits>
#include <vector>

namespace aico_demo {

inline void set_default_camera_config(aico::CameraConfig* config) {
    if (!config) {
        return;
    }

    config->device_name = "/dev/video0";
    config->width = 1920;
    config->height = 1080;
    config->camera_id = 0;
    config->pixel_format = "uyvy";
    config->subdev_device = "/dev/v4l-subdev2";
    config->edid_path = nullptr;
    config->skip_frames = 1;
    config->trigger_retries = 0;
    config->trigger_delay_ms = 1000;
    config->capture_retries = 2;
    config->enable_hdmi_sync = true;
    config->force_trigger = false;
    config->require_exact_resolution = true;
    config->reject_uniform_frames = true;
}

inline uint8_t clamp_u8(int value) {
    if (value < 0) {
        return 0;
    }
    if (value > 255) {
        return 255;
    }
    return static_cast<uint8_t>(value);
}

inline void yuv_to_rgb(uint8_t y, uint8_t u, uint8_t v, uint8_t* rgb) {
    const int c = static_cast<int>(y) - 16;
    const int d = static_cast<int>(u) - 128;
    const int e = static_cast<int>(v) - 128;
    const int c298 = (c < 0 ? 0 : c) * 298;

    rgb[0] = clamp_u8((c298 + 409 * e + 128) >> 8);
    rgb[1] = clamp_u8((c298 - 100 * d - 208 * e + 128) >> 8);
    rgb[2] = clamp_u8((c298 + 516 * d + 128) >> 8);
}

inline bool convert_frame_to_rgb(const aico::VideoFrame& frame,
                                 const char* pixel_format,
                                 const std::vector<uint8_t>& buffer,
                                 std::vector<uint8_t>* rgb) {
    if (!pixel_format || !rgb || frame.width == 0 || frame.height == 0) {
        return false;
    }

    const uint32_t width = frame.width;
    const uint32_t height = frame.height;
    const size_t pixels = static_cast<size_t>(width) * height;
    rgb->assign(pixels * 3, 0);

    if ((std::strcmp(pixel_format, "uyvy") == 0) ||
        (std::strcmp(pixel_format, "yuyv") == 0)) {
        if ((width & 1U) != 0 || buffer.size() < pixels * 2) {
            return false;
        }

        size_t src = 0;
        size_t dst = 0;
        while (src + 3 < buffer.size() && dst + 5 < rgb->size()) {
            uint8_t y0;
            uint8_t u;
            uint8_t y1;
            uint8_t v;

            if (std::strcmp(pixel_format, "uyvy") == 0) {
                u = buffer[src++];
                y0 = buffer[src++];
                v = buffer[src++];
                y1 = buffer[src++];
            } else {
                y0 = buffer[src++];
                u = buffer[src++];
                y1 = buffer[src++];
                v = buffer[src++];
            }

            yuv_to_rgb(y0, u, v, rgb->data() + dst);
            yuv_to_rgb(y1, u, v, rgb->data() + dst + 3);
            dst += 6;
        }
        return true;
    }

    if ((std::strcmp(pixel_format, "nv12") == 0) ||
        (std::strcmp(pixel_format, "nv16") == 0)) {
        const bool is_nv12 = std::strcmp(pixel_format, "nv12") == 0;
        const size_t y_plane_size = pixels;
        const size_t uv_plane_size = is_nv12 ? pixels / 2 : pixels;
        if (buffer.size() < y_plane_size + uv_plane_size) {
            return false;
        }

        const uint8_t* y_plane = buffer.data();
        const uint8_t* uv_plane = buffer.data() + y_plane_size;

        for (uint32_t y = 0; y < height; ++y) {
            const uint32_t uv_row = is_nv12 ? (y / 2) : y;
            for (uint32_t x = 0; x < width; ++x) {
                const size_t y_index = static_cast<size_t>(y) * width + x;
                const size_t uv_index =
                    static_cast<size_t>(uv_row) * width + (x & ~1U);
                yuv_to_rgb(y_plane[y_index],
                           uv_plane[uv_index],
                           uv_plane[uv_index + 1],
                           rgb->data() + y_index * 3);
            }
        }
        return true;
    }

    return false;
}

inline void write_u16_le(std::vector<uint8_t>* out, size_t offset, uint16_t value) {
    (*out)[offset] = static_cast<uint8_t>(value & 0xff);
    (*out)[offset + 1] = static_cast<uint8_t>((value >> 8) & 0xff);
}

inline void write_u32_le(std::vector<uint8_t>* out, size_t offset, uint32_t value) {
    (*out)[offset] = static_cast<uint8_t>(value & 0xff);
    (*out)[offset + 1] = static_cast<uint8_t>((value >> 8) & 0xff);
    (*out)[offset + 2] = static_cast<uint8_t>((value >> 16) & 0xff);
    (*out)[offset + 3] = static_cast<uint8_t>((value >> 24) & 0xff);
}

inline bool encode_rgb_to_bmp(const std::vector<uint8_t>& rgb,
                              uint32_t width,
                              uint32_t height,
                              std::vector<uint8_t>* bmp) {
    if (!bmp || width == 0 || height == 0) {
        return false;
    }

    const size_t pixels = static_cast<size_t>(width) * height;
    if (rgb.size() < pixels * 3) {
        return false;
    }

    const uint32_t row_stride = (width * 3U + 3U) & ~3U;
    const uint64_t image_size = static_cast<uint64_t>(row_stride) * height;
    const uint64_t file_size = 54U + image_size;

    if (height > static_cast<uint32_t>(std::numeric_limits<int32_t>::max()) ||
        width > static_cast<uint32_t>(std::numeric_limits<int32_t>::max()) ||
        file_size > std::numeric_limits<uint32_t>::max()) {
        return false;
    }

    bmp->assign(static_cast<size_t>(file_size), 0);
    (*bmp)[0] = 'B';
    (*bmp)[1] = 'M';
    write_u32_le(bmp, 2, static_cast<uint32_t>(file_size));
    write_u32_le(bmp, 10, 54);
    write_u32_le(bmp, 14, 40);
    write_u32_le(bmp, 18, width);
    write_u32_le(bmp, 22, static_cast<uint32_t>(-static_cast<int32_t>(height)));
    write_u16_le(bmp, 26, 1);
    write_u16_le(bmp, 28, 24);
    write_u32_le(bmp, 34, static_cast<uint32_t>(image_size));

    for (uint32_t y = 0; y < height; ++y) {
        const size_t src_row = static_cast<size_t>(y) * width * 3;
        const size_t dst_row = 54 + static_cast<size_t>(y) * row_stride;
        for (uint32_t x = 0; x < width; ++x) {
            const size_t src = src_row + static_cast<size_t>(x) * 3;
            const size_t dst = dst_row + static_cast<size_t>(x) * 3;
            (*bmp)[dst] = rgb[src + 2];
            (*bmp)[dst + 1] = rgb[src + 1];
            (*bmp)[dst + 2] = rgb[src];
        }
    }

    return true;
}

}  // namespace aico_demo
