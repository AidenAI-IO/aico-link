#include "frame_processing.h"
#include <algorithm>
#include <cmath>
#include <limits>
#include <string.h>

namespace aico {

static uint8_t clamp_u8(int value) {
    if (value < 0) return 0;
    if (value > 255) return 255;
    return static_cast<uint8_t>(value);
}

static void yuv_to_rgb(uint8_t y, uint8_t u, uint8_t v, uint8_t* rgb) {
    const int c = static_cast<int>(y) - 16;
    const int d = static_cast<int>(u) - 128;
    const int e = static_cast<int>(v) - 128;
    const int c298 = (c < 0 ? 0 : c) * 298;
    rgb[0] = clamp_u8((c298 + 409 * e + 128) >> 8);
    rgb[1] = clamp_u8((c298 - 100 * d - 208 * e + 128) >> 8);
    rgb[2] = clamp_u8((c298 + 516 * d + 128) >> 8);
}

bool convert_frame_to_rgb(const FrameMetadata& metadata,
                          const std::vector<uint8_t>& frame,
                          std::vector<uint8_t>* rgb) {
    if (!rgb || metadata.width == 0 || metadata.height == 0 || metadata.pixel_format.empty()) {
        return false;
    }

    const uint32_t width = metadata.width;
    const uint32_t height = metadata.height;
    const size_t pixels = static_cast<size_t>(width) * height;
    rgb->assign(pixels * 3, 0);

    if (metadata.pixel_format == "uyvy" || metadata.pixel_format == "yuyv") {
        if ((width & 1U) != 0 || frame.size() < pixels * 2) {
            return false;
        }
        size_t src = 0;
        size_t dst = 0;
        while (src + 3 < frame.size() && dst + 5 < rgb->size()) {
            uint8_t y0;
            uint8_t u;
            uint8_t y1;
            uint8_t v;
            if (metadata.pixel_format == "uyvy") {
                u = frame[src++];
                y0 = frame[src++];
                v = frame[src++];
                y1 = frame[src++];
            } else {
                y0 = frame[src++];
                u = frame[src++];
                y1 = frame[src++];
                v = frame[src++];
            }
            yuv_to_rgb(y0, u, v, rgb->data() + dst);
            yuv_to_rgb(y1, u, v, rgb->data() + dst + 3);
            dst += 6;
        }
        return true;
    }

    if (metadata.pixel_format == "nv12" || metadata.pixel_format == "nv16") {
        const bool is_nv12 = metadata.pixel_format == "nv12";
        const size_t y_plane_size = pixels;
        const size_t uv_plane_size = is_nv12 ? pixels / 2 : pixels;
        if (frame.size() < y_plane_size + uv_plane_size) {
            return false;
        }
        const uint8_t* y_plane = frame.data();
        const uint8_t* uv_plane = frame.data() + y_plane_size;
        for (uint32_t y = 0; y < height; ++y) {
            const uint32_t uv_row = is_nv12 ? (y / 2) : y;
            for (uint32_t x = 0; x < width; ++x) {
                const size_t y_index = static_cast<size_t>(y) * width + x;
                const size_t uv_index = static_cast<size_t>(uv_row) * width + (x & ~1U);
                yuv_to_rgb(y_plane[y_index], uv_plane[uv_index], uv_plane[uv_index + 1],
                           rgb->data() + y_index * 3);
            }
        }
        return true;
    }

    return false;
}

static bool supported_crop_format(const FrameMetadata& metadata) {
    return metadata.pixel_format == "uyvy" || metadata.pixel_format == "yuyv" ||
           metadata.pixel_format == "nv12" || metadata.pixel_format == "nv16";
}

static bool valid_crop_frame(const FrameMetadata& metadata,
                             const std::vector<uint8_t>& frame) {
    if (metadata.width == 0 || metadata.height == 0 ||
        (metadata.width & 1U) != 0 || !supported_crop_format(metadata)) {
        return false;
    }
    if (metadata.pixel_format == "nv12" && (metadata.height & 1U) != 0) {
        return false;
    }
    size_t required = static_cast<size_t>(metadata.width) * metadata.height;
    if (metadata.pixel_format == "uyvy" || metadata.pixel_format == "yuyv" ||
        metadata.pixel_format == "nv16") {
        required *= 2U;
    } else {
        required += required / 2U;
    }
    return frame.size() >= required;
}

static uint8_t frame_luma_at(const FrameMetadata& metadata,
                             const std::vector<uint8_t>& frame,
                             uint32_t x,
                             uint32_t y) {
    if (metadata.pixel_format == "uyvy") {
        return frame[(static_cast<size_t>(y) * metadata.width + x) * 2U + 1U];
    }
    if (metadata.pixel_format == "yuyv") {
        return frame[(static_cast<size_t>(y) * metadata.width + x) * 2U];
    }
    return frame[static_cast<size_t>(y) * metadata.width + x];
}

static bool is_black_luma_line(const FrameMetadata& metadata,
                               const std::vector<uint8_t>& frame,
                               bool vertical,
                               uint32_t position) {
    double sum = 0.0;
    double squared_sum = 0.0;
    const uint32_t length = vertical ? metadata.height : metadata.width;
    for (uint32_t i = 0; i < length; ++i) {
        const uint8_t luma = vertical
                                 ? frame_luma_at(metadata, frame, position, i)
                                 : frame_luma_at(metadata, frame, i, position);
        sum += luma;
        squared_sum += static_cast<double>(luma) * luma;
    }
    const double mean = sum / length;
    const double variance = std::max(0.0, squared_sum / length - mean * mean);
    // Limited-range YUV black is normally 16. Keep a little headroom for
    // capture noise while still rejecting dark, detailed UI content.
    return mean <= 30.0 && std::sqrt(variance) <= 6.0;
}

struct AxisCrop {
    int first;
    int last;
    bool valid;
};

template <typename IsBorder>
static AxisCrop detect_axis_crop(int length, IsBorder is_border) {
    AxisCrop result = {0, length - 1, false};
    if (length <= 1 || !is_border(0) || !is_border(length - 1)) {
        return result;
    }

    const int probes[] = {
        length / 2,
        length / 4,
        (length * 3) / 4,
        length / 8,
        (length * 3) / 8,
        (length * 5) / 8,
        (length * 7) / 8,
    };
    int active_seed = -1;
    for (size_t i = 0; i < sizeof(probes) / sizeof(probes[0]); ++i) {
        const int probe = std::min(length - 1, std::max(0, probes[i]));
        if (!is_border(probe)) {
            active_seed = probe;
            break;
        }
    }
    if (active_seed < 0) {
        return result;
    }

    int border = 0;
    int active = active_seed;
    while (active - border > 1) {
        const int middle = border + (active - border) / 2;
        if (is_border(middle)) {
            border = middle;
        } else {
            active = middle;
        }
    }
    result.first = active;

    active = active_seed;
    border = length - 1;
    while (border - active > 1) {
        const int middle = active + (border - active) / 2;
        if (is_border(middle)) {
            border = middle;
        } else {
            active = middle;
        }
    }
    result.last = active;

    const int active_length = result.last - result.first + 1;
    const int leading = result.first;
    const int trailing = length - 1 - result.last;
    const int symmetry_tolerance = std::max(4, length / 100);
    result.valid = active_length >= length / 5 && active_length <= length * 95 / 100 &&
                   std::abs(leading - trailing) <= symmetry_tolerance;
    return result;
}

static bool resolve_crop_metadata(const FrameMetadata& metadata,
                                  const std::vector<uint8_t>& frame,
                                  uint32_t crop_x,
                                  uint32_t crop_y,
                                  uint32_t crop_width,
                                  uint32_t crop_height,
                                  FrameMetadata* cropped_metadata) {
    if (!cropped_metadata || !valid_crop_frame(metadata, frame) ||
        crop_width == 0 || crop_height == 0 || crop_x + crop_width > metadata.width ||
        crop_y + crop_height > metadata.height || (crop_x & 1U) != 0 ||
        (crop_width & 1U) != 0) {
        return false;
    }
    if (metadata.pixel_format == "nv12" &&
        (((crop_y | crop_height) & 1U) != 0)) {
        return false;
    }

    FrameMetadata result = metadata;
    result.source_width = metadata.width;
    result.source_height = metadata.height;
    result.crop_x = crop_x;
    result.crop_y = crop_y;
    result.crop_width = crop_width;
    result.crop_height = crop_height;
    result.width = crop_width;
    result.height = crop_height;
    result.planes.clear();

    if (metadata.pixel_format == "uyvy" || metadata.pixel_format == "yuyv") {
        result.stride = crop_width * 2U;
        result.bytes = static_cast<uint64_t>(crop_width) * crop_height * 2U;
    } else {
        const uint32_t cropped_uv_rows = metadata.pixel_format == "nv12"
                                             ? crop_height / 2U
                                             : crop_height;
        FramePlaneMetadata y_plane;
        y_plane.offset = 0;
        y_plane.stride = crop_width;
        y_plane.bytes = crop_width * crop_height;
        result.planes.push_back(y_plane);
        FramePlaneMetadata uv_plane;
        uv_plane.offset = y_plane.bytes;
        uv_plane.stride = crop_width;
        uv_plane.bytes = crop_width * cropped_uv_rows;
        result.planes.push_back(uv_plane);
        result.stride = metadata.pixel_format == "nv16" ? crop_width * 2U : crop_width;
        result.bytes = static_cast<uint64_t>(crop_width) *
                       (crop_height + cropped_uv_rows);
    }

    *cropped_metadata = result;
    return true;
}

static bool crop_frame_rect(const FrameMetadata& metadata,
                            const std::vector<uint8_t>& frame,
                            uint32_t crop_x,
                            uint32_t crop_y,
                            uint32_t crop_width,
                            uint32_t crop_height,
                            FrameMetadata* cropped_metadata,
                            std::vector<uint8_t>* cropped_frame) {
    if (!cropped_metadata || !cropped_frame ||
        !resolve_crop_metadata(metadata, frame, crop_x, crop_y, crop_width, crop_height,
                               cropped_metadata)) {
        return false;
    }

    if (metadata.pixel_format == "uyvy" || metadata.pixel_format == "yuyv") {
        const size_t source_row_bytes = static_cast<size_t>(metadata.width) * 2U;
        const size_t cropped_row_bytes = static_cast<size_t>(crop_width) * 2U;
        cropped_frame->resize(cropped_row_bytes * crop_height);
        for (uint32_t y = 0; y < crop_height; ++y) {
            const size_t source_offset = static_cast<size_t>(crop_y + y) * source_row_bytes +
                                         static_cast<size_t>(crop_x) * 2U;
            const size_t cropped_offset = static_cast<size_t>(y) * cropped_row_bytes;
            memcpy(cropped_frame->data() + cropped_offset,
                   frame.data() + source_offset,
                   cropped_row_bytes);
        }
    } else {
        const size_t source_y_bytes = static_cast<size_t>(metadata.width) * metadata.height;
        const uint32_t source_uv_y = metadata.pixel_format == "nv12" ? crop_y / 2U : crop_y;
        const uint32_t cropped_uv_rows = metadata.pixel_format == "nv12"
                                             ? crop_height / 2U
                                             : crop_height;
        cropped_frame->resize(static_cast<size_t>(crop_width) *
                              (crop_height + cropped_uv_rows));
        for (uint32_t y = 0; y < crop_height; ++y) {
            const size_t source_offset = static_cast<size_t>(crop_y + y) * metadata.width + crop_x;
            const size_t cropped_offset = static_cast<size_t>(y) * crop_width;
            memcpy(cropped_frame->data() + cropped_offset,
                   frame.data() + source_offset,
                   crop_width);
        }
        for (uint32_t y = 0; y < cropped_uv_rows; ++y) {
            const size_t source_offset = source_y_bytes +
                                         static_cast<size_t>(source_uv_y + y) * metadata.width +
                                         crop_x;
            const size_t cropped_offset = static_cast<size_t>(crop_width) * crop_height +
                                          static_cast<size_t>(y) * crop_width;
            memcpy(cropped_frame->data() + cropped_offset,
                   frame.data() + source_offset,
                   crop_width);
        }
    }

    cropped_metadata->bytes = cropped_frame->size();
    return true;
}

static uint32_t even_crop_extent(uint32_t extent, uint32_t limit) {
    extent = std::min(extent, limit);
    if ((extent & 1U) == 0) {
        return extent;
    }
    // Prefer retaining one more pixel/row. If an odd source edge cannot be
    // expanded, drop one when possible; a one-pixel source is left intact.
    if (extent < limit) {
        return extent + 1U;
    }
    return extent > 1U ? extent - 1U : extent;
}

struct CenterCropRect {
    uint32_t x;
    uint32_t y;
    uint32_t width;
    uint32_t height;
};

static bool centered_crop_rect(const FrameMetadata& metadata,
                               uint32_t target_width, uint32_t target_height,
                               CenterCropRect* rect) {
    if (!rect || target_width == 0 || target_height == 0) {
        return false;
    }
    rect->width = even_crop_extent(target_width, metadata.width);
    rect->height = even_crop_extent(target_height, metadata.height);
    if (rect->width == 0 || rect->height == 0) {
        return false;
    }
    rect->x = ((metadata.width - rect->width) / 2U) & ~1U;
    rect->y = (metadata.height - rect->height) / 2U;
    if (metadata.pixel_format == "nv12") {
        rect->y &= ~1U;
    }
    return rect->x + rect->width <= metadata.width &&
           rect->y + rect->height <= metadata.height;
}

bool crop_frame_center(const FrameMetadata& metadata,
                       const std::vector<uint8_t>& frame,
                       uint32_t target_width,
                       uint32_t target_height,
                       FrameMetadata* cropped_metadata,
                       std::vector<uint8_t>* cropped_frame) {
    if (!cropped_metadata || !cropped_frame || !valid_crop_frame(metadata, frame) ||
        target_width == 0 || target_height == 0) {
        return false;
    }

    CenterCropRect rect;
    if (!centered_crop_rect(metadata, target_width, target_height, &rect)) {
        return false;
    }
    return crop_frame_rect(metadata, frame, rect.x, rect.y, rect.width, rect.height,
                           cropped_metadata, cropped_frame);
}

bool resolve_frame_center_crop(const FrameMetadata& metadata,
                               const std::vector<uint8_t>& frame,
                               uint32_t target_width,
                               uint32_t target_height,
                               FrameMetadata* cropped_metadata) {
    if (!cropped_metadata || !valid_crop_frame(metadata, frame) ||
        target_width == 0 || target_height == 0) {
        return false;
    }
    CenterCropRect rect;
    if (!centered_crop_rect(metadata, target_width, target_height, &rect)) {
        return false;
    }
    return resolve_crop_metadata(metadata, frame, rect.x, rect.y, rect.width, rect.height,
                                 cropped_metadata);
}

static void centered_aspect_crop_size(uint32_t frame_width, uint32_t frame_height,
                                      uint32_t screen_width, uint32_t screen_height,
                                      uint32_t* crop_width, uint32_t* crop_height) {
    *crop_width = frame_width;
    *crop_height = frame_height;
    if (frame_width == 0 || frame_height == 0 || screen_width == 0 || screen_height == 0) {
        return;
    }
    if (static_cast<uint64_t>(screen_width) * frame_height <=
        static_cast<uint64_t>(screen_height) * frame_width) {
        const uint64_t numerator = static_cast<uint64_t>(frame_height) * screen_width;
        *crop_width = static_cast<uint32_t>(
            std::max<uint64_t>(1, (numerator + screen_height / 2U) / screen_height));
    } else {
        const uint64_t numerator = static_cast<uint64_t>(frame_width) * screen_height;
        *crop_height = static_cast<uint32_t>(
            std::max<uint64_t>(1, (numerator + screen_width / 2U) / screen_width));
    }
    *crop_width = even_crop_extent(*crop_width, frame_width);
    *crop_height = even_crop_extent(*crop_height, frame_height);
}

struct BorderScore {
    uint32_t black;
    uint32_t total;
};

static void score_black_border_band(const FrameMetadata& metadata,
                                    const std::vector<uint8_t>& frame,
                                    bool vertical,
                                    uint32_t begin,
                                    uint32_t end,
                                    BorderScore* score) {
    if (!score || begin >= end) {
        return;
    }
    const uint32_t length = end - begin;
    const uint32_t samples = std::min<uint32_t>(5, length);
    for (uint32_t i = 0; i < samples; ++i) {
        const uint32_t offset = samples == 1
                                    ? 0
                                    : static_cast<uint32_t>(
                                          static_cast<uint64_t>(length - 1) * i /
                                          (samples - 1));
        if (is_black_luma_line(metadata, frame, vertical, begin + offset)) {
            ++score->black;
        }
        ++score->total;
    }
}

static BorderScore centered_crop_border_score(const FrameMetadata& metadata,
                                               const std::vector<uint8_t>& frame,
                                               uint32_t crop_width,
                                               uint32_t crop_height) {
    BorderScore score = {0, 0};
    const uint32_t crop_x = (metadata.width - crop_width) / 2U;
    const uint32_t crop_y = (metadata.height - crop_height) / 2U;
    score_black_border_band(metadata, frame, true, 0, crop_x, &score);
    score_black_border_band(metadata, frame, true, crop_x + crop_width,
                            metadata.width, &score);
    score_black_border_band(metadata, frame, false, 0, crop_y, &score);
    score_black_border_band(metadata, frame, false, crop_y + crop_height,
                            metadata.height, &score);
    return score;
}

bool resolve_frame_center_aspect_auto_crop(const FrameMetadata& metadata,
                                           const std::vector<uint8_t>& frame,
                                           uint32_t screen_width,
                                           uint32_t screen_height,
                                           FrameMetadata* cropped_metadata) {
    if (!cropped_metadata || !valid_crop_frame(metadata, frame) ||
        screen_width == 0 || screen_height == 0) {
        return false;
    }

    const uint32_t short_side = std::min(screen_width, screen_height);
    const uint32_t long_side = std::max(screen_width, screen_height);
    uint32_t portrait_width = metadata.width;
    uint32_t portrait_height = metadata.height;
    uint32_t landscape_width = metadata.width;
    uint32_t landscape_height = metadata.height;
    centered_aspect_crop_size(metadata.width, metadata.height, short_side, long_side,
                              &portrait_width, &portrait_height);
    centered_aspect_crop_size(metadata.width, metadata.height, long_side, short_side,
                              &landscape_width, &landscape_height);

    if (portrait_width == landscape_width && portrait_height == landscape_height) {
        return resolve_frame_center_crop(metadata, frame, portrait_width, portrait_height,
                                         cropped_metadata);
    }
    if (portrait_width == metadata.width && portrait_height == metadata.height) {
        return resolve_frame_center_crop(metadata, frame, portrait_width, portrait_height,
                                         cropped_metadata);
    }
    if (landscape_width == metadata.width && landscape_height == metadata.height) {
        return resolve_frame_center_crop(metadata, frame, landscape_width, landscape_height,
                                         cropped_metadata);
    }

    const BorderScore portrait = centered_crop_border_score(
        metadata, frame, portrait_width, portrait_height);
    const BorderScore landscape = centered_crop_border_score(
        metadata, frame, landscape_width, landscape_height);
    bool use_landscape;
    const uint64_t portrait_weighted = static_cast<uint64_t>(portrait.black) * landscape.total;
    const uint64_t landscape_weighted = static_cast<uint64_t>(landscape.black) * portrait.total;
    if (landscape_weighted != portrait_weighted) {
        use_landscape = landscape_weighted > portrait_weighted;
    } else {
        // When the pixels are ambiguous, derive orientation from this frame's
        // geometry rather than from the cached width/height ordering.
        use_landscape = metadata.width >= metadata.height;
    }

    return use_landscape
               ? resolve_frame_center_crop(metadata, frame, landscape_width, landscape_height,
                                           cropped_metadata)
               : resolve_frame_center_crop(metadata, frame, portrait_width, portrait_height,
                                           cropped_metadata);
}

bool crop_frame_center_aspect_auto(const FrameMetadata& metadata,
                                   const std::vector<uint8_t>& frame,
                                   uint32_t screen_width,
                                   uint32_t screen_height,
                                   FrameMetadata* cropped_metadata,
                                   std::vector<uint8_t>* cropped_frame) {
    FrameMetadata resolved;
    if (!cropped_metadata || !cropped_frame ||
        !resolve_frame_center_aspect_auto_crop(metadata, frame, screen_width, screen_height,
                                               &resolved)) {
        return false;
    }
    return crop_frame_rect(metadata, frame, resolved.crop_x, resolved.crop_y,
                           resolved.crop_width, resolved.crop_height,
                           cropped_metadata, cropped_frame);
}

bool crop_frame_horizontal_center(const FrameMetadata& metadata,
                                  const std::vector<uint8_t>& frame,
                                  uint32_t target_width,
                                  FrameMetadata* cropped_metadata,
                                  std::vector<uint8_t>* cropped_frame) {
    return crop_frame_center(metadata, frame, target_width, metadata.height,
                             cropped_metadata, cropped_frame);
}

bool resolve_frame_black_bars_crop(const FrameMetadata& metadata,
                                   const std::vector<uint8_t>& frame,
                                   FrameMetadata* cropped_metadata) {
    if (!cropped_metadata || !valid_crop_frame(metadata, frame)) {
        return false;
    }

    const AxisCrop horizontal = detect_axis_crop(
        static_cast<int>(metadata.width), [&](int x) {
            return is_black_luma_line(metadata, frame, true, static_cast<uint32_t>(x));
        });
    const AxisCrop vertical = detect_axis_crop(
        static_cast<int>(metadata.height), [&](int y) {
            return is_black_luma_line(metadata, frame, false, static_cast<uint32_t>(y));
        });

    bool crop_horizontal = horizontal.valid;
    bool crop_vertical = vertical.valid;
    if (crop_horizontal && crop_vertical) {
        const double horizontal_removed =
            1.0 - static_cast<double>(horizontal.last - horizontal.first + 1) / metadata.width;
        const double vertical_removed =
            1.0 - static_cast<double>(vertical.last - vertical.first + 1) / metadata.height;
        crop_horizontal = horizontal_removed > vertical_removed;
        crop_vertical = !crop_horizontal;
    }

    uint32_t crop_x = 0;
    uint32_t crop_y = 0;
    uint32_t crop_width = metadata.width;
    uint32_t crop_height = metadata.height;
    if (crop_horizontal) {
        crop_x = static_cast<uint32_t>(horizontal.first) & ~1U;
        const uint32_t right = std::min(metadata.width - 1U,
                                        static_cast<uint32_t>(horizontal.last) | 1U);
        crop_width = right - crop_x + 1U;
    } else if (crop_vertical) {
        crop_y = static_cast<uint32_t>(vertical.first);
        crop_height = static_cast<uint32_t>(vertical.last - vertical.first + 1);
        if (metadata.pixel_format == "nv12") {
            crop_y &= ~1U;
            const uint32_t bottom = std::min(metadata.height - 1U,
                                             static_cast<uint32_t>(vertical.last) | 1U);
            crop_height = bottom - crop_y + 1U;
        }
    }
    return resolve_crop_metadata(metadata, frame, crop_x, crop_y, crop_width, crop_height,
                                 cropped_metadata);
}

bool crop_frame_black_bars(const FrameMetadata& metadata,
                           const std::vector<uint8_t>& frame,
                           FrameMetadata* cropped_metadata,
                           std::vector<uint8_t>* cropped_frame) {
    FrameMetadata resolved;
    if (!cropped_metadata || !cropped_frame ||
        !resolve_frame_black_bars_crop(metadata, frame, &resolved)) {
        return false;
    }
    return crop_frame_rect(metadata, frame, resolved.crop_x, resolved.crop_y,
                           resolved.crop_width, resolved.crop_height,
                           cropped_metadata, cropped_frame);
}

bool crop_frame_horizontal_black_bars(const FrameMetadata& metadata,
                                      const std::vector<uint8_t>& frame,
                                      uint32_t minimal_width,
                                      FrameMetadata* cropped_metadata,
                                      std::vector<uint8_t>* cropped_frame,
                                      bool crop_by_aspect) {
    if (crop_by_aspect && minimal_width > 0) {
        return crop_frame_horizontal_center(metadata, frame, minimal_width,
                                            cropped_metadata, cropped_frame);
    }
    if (!crop_frame_black_bars(metadata, frame, cropped_metadata, cropped_frame)) {
        return false;
    }
    if (minimal_width > 0 && cropped_metadata->width < minimal_width &&
        cropped_metadata->height == metadata.height) {
        return crop_frame_horizontal_center(metadata, frame, minimal_width,
                                            cropped_metadata, cropped_frame);
    }
    return true;
}

static void write_u16_le(std::vector<uint8_t>* out, size_t offset, uint16_t value) {
    (*out)[offset] = static_cast<uint8_t>(value & 0xff);
    (*out)[offset + 1] = static_cast<uint8_t>((value >> 8) & 0xff);
}

static void write_u32_le(std::vector<uint8_t>* out, size_t offset, uint32_t value) {
    (*out)[offset] = static_cast<uint8_t>(value & 0xff);
    (*out)[offset + 1] = static_cast<uint8_t>((value >> 8) & 0xff);
    (*out)[offset + 2] = static_cast<uint8_t>((value >> 16) & 0xff);
    (*out)[offset + 3] = static_cast<uint8_t>((value >> 24) & 0xff);
}

static void append_u32_be(std::vector<uint8_t>* out, uint32_t value) {
    out->push_back(static_cast<uint8_t>((value >> 24) & 0xff));
    out->push_back(static_cast<uint8_t>((value >> 16) & 0xff));
    out->push_back(static_cast<uint8_t>((value >> 8) & 0xff));
    out->push_back(static_cast<uint8_t>(value & 0xff));
}

static uint32_t crc32_bytes(const uint8_t* data, size_t size) {
    uint32_t crc = 0xffffffffU;
    for (size_t i = 0; i < size; ++i) {
        crc ^= data[i];
        for (int bit = 0; bit < 8; ++bit) {
            uint32_t mask = static_cast<uint32_t>(-(static_cast<int32_t>(crc & 1U)));
            crc = (crc >> 1) ^ (0xedb88320U & mask);
        }
    }
    return crc ^ 0xffffffffU;
}

static uint32_t adler32_bytes(const uint8_t* data, size_t size) {
    uint32_t a = 1;
    uint32_t b = 0;
    for (size_t i = 0; i < size; ++i) {
        a = (a + data[i]) % 65521U;
        b = (b + a) % 65521U;
    }
    return (b << 16) | a;
}

static void append_png_chunk(std::vector<uint8_t>* png,
                             const char type[4],
                             const std::vector<uint8_t>& data) {
    append_u32_be(png, static_cast<uint32_t>(data.size()));
    size_t type_offset = png->size();
    png->insert(png->end(), type, type + 4);
    png->insert(png->end(), data.begin(), data.end());
    uint32_t crc = crc32_bytes(png->data() + type_offset, 4 + data.size());
    append_u32_be(png, crc);
}

static bool zlib_store_uncompressed(const std::vector<uint8_t>& input,
                                    std::vector<uint8_t>* out) {
    if (!out) {
        return false;
    }
    out->clear();
    out->push_back(0x78);
    out->push_back(0x01);

    size_t offset = 0;
    do {
        size_t remaining = input.size() - offset;
        uint16_t block_len = static_cast<uint16_t>(remaining > 65535U ? 65535U : remaining);
        bool final_block = offset + block_len == input.size();
        out->push_back(final_block ? 0x01 : 0x00);
        out->push_back(static_cast<uint8_t>(block_len & 0xff));
        out->push_back(static_cast<uint8_t>((block_len >> 8) & 0xff));
        uint16_t nlen = static_cast<uint16_t>(~block_len);
        out->push_back(static_cast<uint8_t>(nlen & 0xff));
        out->push_back(static_cast<uint8_t>((nlen >> 8) & 0xff));
        out->insert(out->end(), input.begin() + offset, input.begin() + offset + block_len);
        offset += block_len;
    } while (offset < input.size());

    append_u32_be(out, adler32_bytes(input.empty() ? nullptr : input.data(), input.size()));
    return true;
}

static bool prepare_bmp(uint32_t width,
                        uint32_t height,
                        std::vector<uint8_t>* bmp,
                        uint32_t* row_stride_out) {
    if (!bmp || width == 0 || height == 0) {
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
    if (row_stride_out) {
        *row_stride_out = row_stride;
    }
    return true;
}

static void write_bmp_pixel(std::vector<uint8_t>* bmp, size_t offset, const uint8_t* rgb) {
    (*bmp)[offset] = rgb[2];
    (*bmp)[offset + 1] = rgb[1];
    (*bmp)[offset + 2] = rgb[0];
}

bool encode_rgb_to_bmp(const std::vector<uint8_t>& rgb,
                       uint32_t width,
                       uint32_t height,
                       std::vector<uint8_t>* bmp) {
    const size_t pixels = static_cast<size_t>(width) * height;
    if (rgb.size() < pixels * 3) {
        return false;
    }

    uint32_t row_stride = 0;
    if (!prepare_bmp(width, height, bmp, &row_stride)) {
        return false;
    }

    for (uint32_t y = 0; y < height; ++y) {
        const size_t src_row = static_cast<size_t>(y) * width * 3;
        const size_t dst_row = 54 + static_cast<size_t>(y) * row_stride;
        for (uint32_t x = 0; x < width; ++x) {
            const size_t src = src_row + static_cast<size_t>(x) * 3;
            const size_t dst = dst_row + static_cast<size_t>(x) * 3;
            write_bmp_pixel(bmp, dst, rgb.data() + src);
        }
    }
    return true;
}

bool encode_frame_to_bmp(const FrameMetadata& metadata,
                         const std::vector<uint8_t>& frame,
                         std::vector<uint8_t>* bmp) {
    const uint32_t width = metadata.width;
    const uint32_t height = metadata.height;
    const size_t pixels = static_cast<size_t>(width) * height;
    uint32_t row_stride = 0;

    if ((metadata.pixel_format == "uyvy" || metadata.pixel_format == "yuyv") &&
        width > 0 && height > 0 && (width & 1U) == 0 && frame.size() >= pixels * 2 &&
        prepare_bmp(width, height, bmp, &row_stride)) {
        size_t src = 0;
        for (uint32_t y = 0; y < height; ++y) {
            const size_t dst_row = 54 + static_cast<size_t>(y) * row_stride;
            for (uint32_t x = 0; x < width; x += 2) {
                uint8_t y0;
                uint8_t u;
                uint8_t y1;
                uint8_t v;
                if (metadata.pixel_format == "uyvy") {
                    u = frame[src++];
                    y0 = frame[src++];
                    v = frame[src++];
                    y1 = frame[src++];
                } else {
                    y0 = frame[src++];
                    u = frame[src++];
                    y1 = frame[src++];
                    v = frame[src++];
                }
                uint8_t rgb[3];
                yuv_to_rgb(y0, u, v, rgb);
                write_bmp_pixel(bmp, dst_row + static_cast<size_t>(x) * 3, rgb);
                yuv_to_rgb(y1, u, v, rgb);
                write_bmp_pixel(bmp, dst_row + static_cast<size_t>(x + 1) * 3, rgb);
            }
        }
        return true;
    }

    if ((metadata.pixel_format == "nv12" || metadata.pixel_format == "nv16") &&
        width > 0 && height > 0 && prepare_bmp(width, height, bmp, &row_stride)) {
        const bool is_nv12 = metadata.pixel_format == "nv12";
        const size_t y_plane_size = pixels;
        const size_t uv_plane_size = is_nv12 ? pixels / 2 : pixels;
        if (frame.size() >= y_plane_size + uv_plane_size) {
            const uint8_t* y_plane = frame.data();
            const uint8_t* uv_plane = frame.data() + y_plane_size;
            for (uint32_t y = 0; y < height; ++y) {
                const uint32_t uv_row = is_nv12 ? (y / 2) : y;
                const size_t dst_row = 54 + static_cast<size_t>(y) * row_stride;
                for (uint32_t x = 0; x < width; ++x) {
                    const size_t y_index = static_cast<size_t>(y) * width + x;
                    const size_t uv_index = static_cast<size_t>(uv_row) * width + (x & ~1U);
                    uint8_t rgb[3];
                    yuv_to_rgb(y_plane[y_index], uv_plane[uv_index], uv_plane[uv_index + 1], rgb);
                    write_bmp_pixel(bmp, dst_row + static_cast<size_t>(x) * 3, rgb);
                }
            }
            return true;
        }
    }

    std::vector<uint8_t> rgb;
    if (!convert_frame_to_rgb(metadata, frame, &rgb)) {
        return false;
    }
    return encode_rgb_to_bmp(rgb, metadata.width, metadata.height, bmp);
}

bool encode_rgb_to_png(const std::vector<uint8_t>& rgb,
                       uint32_t width,
                       uint32_t height,
                       std::vector<uint8_t>* png) {
    if (!png || width == 0 || height == 0) {
        return false;
    }
    const size_t pixels = static_cast<size_t>(width) * height;
    if (rgb.size() < pixels * 3) {
        return false;
    }

    std::vector<uint8_t> filtered;
    filtered.reserve(static_cast<size_t>(height) * (1 + static_cast<size_t>(width) * 3));
    for (uint32_t y = 0; y < height; ++y) {
        filtered.push_back(0);  // PNG filter type 0 (None)
        const size_t row = static_cast<size_t>(y) * width * 3;
        filtered.insert(filtered.end(), rgb.begin() + row, rgb.begin() + row + static_cast<size_t>(width) * 3);
    }

    std::vector<uint8_t> compressed;
    if (!zlib_store_uncompressed(filtered, &compressed)) {
        return false;
    }

    png->clear();
    const uint8_t signature[] = {137, 80, 78, 71, 13, 10, 26, 10};
    png->insert(png->end(), signature, signature + sizeof(signature));

    std::vector<uint8_t> ihdr;
    append_u32_be(&ihdr, width);
    append_u32_be(&ihdr, height);
    ihdr.push_back(8);  // bit depth
    ihdr.push_back(2);  // truecolor RGB
    ihdr.push_back(0);  // compression method
    ihdr.push_back(0);  // filter method
    ihdr.push_back(0);  // no interlace
    append_png_chunk(png, "IHDR", ihdr);
    append_png_chunk(png, "IDAT", compressed);
    std::vector<uint8_t> empty;
    append_png_chunk(png, "IEND", empty);
    return true;
}

bool encode_frame_to_png(const FrameMetadata& metadata,
                         const std::vector<uint8_t>& frame,
                         std::vector<uint8_t>* png) {
    std::vector<uint8_t> rgb;
    if (!convert_frame_to_rgb(metadata, frame, &rgb)) {
        return false;
    }
    return encode_rgb_to_png(rgb, metadata.width, metadata.height, png);
}

bool scale_rgb_nearest(const std::vector<uint8_t>& rgb,
                       uint32_t src_width,
                       uint32_t src_height,
                       uint32_t dst_width,
                       uint32_t dst_height,
                       std::vector<uint8_t>* scaled) {
    if (!scaled || src_width == 0 || src_height == 0 || dst_width == 0 || dst_height == 0) {
        return false;
    }
    if (rgb.size() < static_cast<size_t>(src_width) * src_height * 3) {
        return false;
    }
    scaled->assign(static_cast<size_t>(dst_width) * dst_height * 3, 0);
    for (uint32_t y = 0; y < dst_height; ++y) {
        uint32_t src_y = static_cast<uint32_t>((static_cast<uint64_t>(y) * src_height) / dst_height);
        for (uint32_t x = 0; x < dst_width; ++x) {
            uint32_t src_x = static_cast<uint32_t>((static_cast<uint64_t>(x) * src_width) / dst_width);
            size_t src = (static_cast<size_t>(src_y) * src_width + src_x) * 3;
            size_t dst = (static_cast<size_t>(y) * dst_width + x) * 3;
            (*scaled)[dst] = rgb[src];
            (*scaled)[dst + 1] = rgb[src + 1];
            (*scaled)[dst + 2] = rgb[src + 2];
        }
    }
    return true;
}

}  // namespace aico
