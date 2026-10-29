#include "frame_jpeg_encoder.h"

#include <atomic>

namespace {

std::atomic<int> g_prepare_count(0);
std::atomic<int> g_warmup_count(0);
std::atomic<int> g_cancel_count(0);
std::atomic<uint32_t> g_last_warmup_width(0);
std::atomic<uint32_t> g_last_warmup_height(0);

}  // namespace

namespace aico {

bool encode_frame_to_jpeg_hw(const uint8_t*, uint32_t, uint32_t, int,
                             std::vector<uint8_t>*, uint32_t*, uint32_t*,
                             uint32_t*, uint32_t*, uint32_t, bool, bool) {
    return false;
}

bool encode_yuv_to_jpeg_hw(const std::vector<uint8_t>&, uint32_t, uint32_t,
                           const std::string&, int, std::vector<uint8_t>*,
                           uint32_t*, uint32_t*, uint32_t*, uint32_t*, uint32_t, bool, bool) {
    return false;
}

bool encode_yuv_to_jpeg_hw(const std::vector<uint8_t>&, const FrameMetadata&,
                           int, std::vector<uint8_t>*,
                           uint32_t*, uint32_t*, uint32_t*, uint32_t*,
                           uint32_t, bool) {
    return false;
}

bool encode_yuv_to_jpeg_hw_with_crop(const std::vector<uint8_t>& yuv_data,
                                     uint32_t width, uint32_t height,
                                     const std::string& pixel_format, int,
                                     uint32_t crop_x, uint32_t crop_y,
                                     uint32_t crop_width, uint32_t crop_height,
                                     std::vector<uint8_t>* output) {
    if (!output || (pixel_format != "uyvy" && pixel_format != "yuyv") ||
        yuv_data.size() < static_cast<size_t>(width) * height * 2U ||
        crop_width == 0 || crop_height == 0 || crop_x + crop_width > width ||
        crop_y + crop_height > height || (width & 1U) != 0 ||
        (crop_x & 1U) != 0 || (crop_width & 1U) != 0 ||
        (crop_height & 1U) != 0) {
        return false;
    }
    *output = std::vector<uint8_t>{0xff, 0xd8, 0xff, 0xd9};
    return true;
}

void clear_yuv_jpeg_hw_unavailable() {}

void warmup_jpeg_encoder(const FrameMetadata& metadata, const std::vector<uint8_t>&) {
    g_last_warmup_width = metadata.width;
    g_last_warmup_height = metadata.height;
    ++g_warmup_count;
}

bool prepare_jpeg_encoder_warmup(const FrameMetadata&) {
    ++g_prepare_count;
    return true;
}

void cancel_jpeg_encoder_warmup() {
    ++g_cancel_count;
}

void reset_jpeg_encoder_stub() {
    g_prepare_count = 0;
    g_warmup_count = 0;
    g_cancel_count = 0;
    g_last_warmup_width = 0;
    g_last_warmup_height = 0;
}

int jpeg_encoder_stub_prepare_count() { return g_prepare_count.load(); }
int jpeg_encoder_stub_warmup_count() { return g_warmup_count.load(); }
int jpeg_encoder_stub_cancel_count() { return g_cancel_count.load(); }
uint32_t jpeg_encoder_stub_last_warmup_width() { return g_last_warmup_width.load(); }
uint32_t jpeg_encoder_stub_last_warmup_height() { return g_last_warmup_height.load(); }

}  // namespace aico
