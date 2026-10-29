#include "doctest.h"
#include "frame_layout.h"

#include <cstdint>
#include <limits>
#include <vector>

using aico::compact_nv12;

TEST_CASE("capture_payload_bounds applies V4L2 data_offset semantics") {
    aico::CapturePayloadBounds bounds;

    REQUIRE(aico::capture_payload_bounds(128, 100, 16, 128, &bounds));
    CHECK(bounds.data_offset == 16);
    CHECK(bounds.payload_length == 84);

    CHECK_FALSE(aico::capture_payload_bounds(128, 15, 16, 128, &bounds));
    CHECK_FALSE(aico::capture_payload_bounds(90, 100, 16, 100, &bounds));
}

TEST_CASE("capture_payload_bounds uses safe sizeimage fallback only for zero bytesused") {
    aico::CapturePayloadBounds bounds;

    REQUIRE(aico::capture_payload_bounds(128, 0, 16, 100, &bounds));
    CHECK(bounds.data_offset == 16);
    CHECK(bounds.payload_length == 84);

    CHECK_FALSE(aico::capture_payload_bounds(128, 0, 101, 100, &bounds));
    CHECK_FALSE(aico::capture_payload_bounds(90, 0, 16, 100, &bounds));
    CHECK_FALSE(aico::capture_payload_bounds(128, 0, 16, 0, &bounds));
}

TEST_CASE("compact_nv12 copies padded Y and UV rows") {
    const uint32_t width = 4;
    const uint32_t height = 4;
    const uint32_t stride = 8;
    std::vector<uint8_t> source(static_cast<size_t>(stride) * height * 3 / 2, 0xee);
    for (uint32_t y = 0; y < height; ++y) {
        for (uint32_t x = 0; x < width; ++x) {
            source[static_cast<size_t>(y) * stride + x] =
                static_cast<uint8_t>(10 * y + x);
        }
    }
    const size_t uv_offset = static_cast<size_t>(stride) * height;
    for (uint32_t y = 0; y < height / 2; ++y) {
        for (uint32_t x = 0; x < width; ++x) {
            source[uv_offset + static_cast<size_t>(y) * stride + x] =
                static_cast<uint8_t>(100 + 10 * y + x);
        }
    }

    std::vector<uint8_t> compact;
    REQUIRE(compact_nv12(source.data(), source.size(), width, height, stride, &compact));
    REQUIRE(compact.size() == static_cast<size_t>(width) * height * 3 / 2);
    for (uint32_t y = 0; y < height; ++y) {
        for (uint32_t x = 0; x < width; ++x) {
            CHECK(compact[static_cast<size_t>(y) * width + x] == 10 * y + x);
        }
    }
    for (uint32_t y = 0; y < height / 2; ++y) {
        for (uint32_t x = 0; x < width; ++x) {
            CHECK(compact[static_cast<size_t>(width) * height +
                          static_cast<size_t>(y) * width + x] == 100 + 10 * y + x);
        }
    }
}

TEST_CASE("compact_nv12 accepts tight layout") {
    std::vector<uint8_t> source(4 * 2 * 3 / 2, 7);
    std::vector<uint8_t> compact;
    REQUIRE(compact_nv12(source.data(), source.size(), 4, 2, 4, &compact));
    CHECK(compact == source);
}

TEST_CASE("compact_nv12 rejects invalid or incomplete layouts") {
    std::vector<uint8_t> source(64, 0);
    std::vector<uint8_t> compact;
    CHECK_FALSE(compact_nv12(nullptr, source.size(), 4, 2, 4, &compact));
    CHECK_FALSE(compact_nv12(source.data(), source.size(), 0, 2, 4, &compact));
    CHECK_FALSE(compact_nv12(source.data(), source.size(), 4, 0, 4, &compact));
    CHECK_FALSE(compact_nv12(source.data(), source.size(), 3, 2, 4, &compact));
    CHECK_FALSE(compact_nv12(source.data(), source.size(), 4, 3, 4, &compact));
    CHECK_FALSE(compact_nv12(source.data(), source.size(), 4, 2, 3, &compact));
    CHECK_FALSE(compact_nv12(source.data(), 8, 4, 4, 8, &compact));
    CHECK_FALSE(compact_nv12(source.data(), source.size(), 4, 4,
                             std::numeric_limits<uint32_t>::max(), &compact));
    CHECK_FALSE(compact_nv12(source.data(), source.size(), 4, 2, 4, nullptr));
}
