#include "doctest.h"
#include "frame_ring_buffer.h"
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

using aico::FrameBufferFrame;
using aico::FrameMetadata;
using aico::FrameRingBuffer;
using aico::FrameServiceStatus;

namespace {

FrameMetadata metadata(uint32_t width, uint32_t height, const char* pixel_format, uint64_t ts) {
    FrameMetadata meta{};
    meta.width = width;
    meta.height = height;
    meta.pixel_format = pixel_format;
    meta.capture_ts_ns = ts;
    meta.stride = width * 2;
    return meta;
}

std::vector<uint8_t> bytes(std::initializer_list<uint8_t> values) {
    return std::vector<uint8_t>(values);
}

}

TEST_CASE("frame ring buffer assigns service-owned monotonically increasing seq") {
    FrameRingBuffer ring(2);
    uint64_t seq1 = 0;
    uint64_t seq2 = 0;
    std::vector<uint8_t> first = bytes({1, 2, 3});
    std::vector<uint8_t> second = bytes({4, 5});

    REQUIRE(ring.append_frame(metadata(2, 1, "uyvy", 100), first.data(), first.size(), &seq1) == FrameServiceStatus::OK);
    REQUIRE(ring.append_frame(metadata(2, 1, "uyvy", 200), second.data(), second.size(), &seq2) == FrameServiceStatus::OK);

    CHECK(seq1 == 1);
    CHECK(seq2 == 2);
    CHECK(ring.latest_seq() == 2);
    CHECK(ring.size() == 2);
}

TEST_CASE("frame ring buffer returns latest frame newer than since_seq") {
    FrameRingBuffer ring(3);
    std::vector<uint8_t> payload = bytes({9, 8, 7});
    uint64_t seq = 0;
    FrameBufferFrame frame;

    REQUIRE(ring.append_frame(metadata(3, 1, "uyvy", 123), payload.data(), payload.size(), &seq) == FrameServiceStatus::OK);

    REQUIRE(ring.latest_frame(0, &frame) == FrameServiceStatus::OK);
    CHECK(frame.metadata.seq == seq);
    CHECK(frame.metadata.capture_ts_ns == 123);
    CHECK(frame.metadata.width == 3);
    CHECK(std::string(frame.metadata.pixel_format) == "uyvy");
    CHECK(frame.metadata.bytes == payload.size());
    CHECK(frame.data == payload);

    CHECK(ring.latest_frame(seq, &frame) == FrameServiceStatus::NO_NEW_FRAME);
}

TEST_CASE("frame ring buffer reports overwritten seq as not found") {
    FrameRingBuffer ring(2);
    uint64_t seq1 = 0;
    uint64_t seq2 = 0;
    uint64_t seq3 = 0;
    std::vector<uint8_t> one = bytes({1});
    std::vector<uint8_t> two = bytes({2});
    std::vector<uint8_t> three = bytes({3});
    FrameBufferFrame frame;

    REQUIRE(ring.append_frame(metadata(1, 1, "uyvy", 1), one.data(), one.size(), &seq1) == FrameServiceStatus::OK);
    REQUIRE(ring.append_frame(metadata(1, 1, "uyvy", 2), two.data(), two.size(), &seq2) == FrameServiceStatus::OK);
    REQUIRE(ring.append_frame(metadata(1, 1, "uyvy", 3), three.data(), three.size(), &seq3) == FrameServiceStatus::OK);

    CHECK(seq1 == 1);
    CHECK(seq2 == 2);
    CHECK(seq3 == 3);
    CHECK(ring.get_frame(seq1, &frame) == FrameServiceStatus::FRAME_NOT_FOUND);

    REQUIRE(ring.get_frame(seq2, &frame) == FrameServiceStatus::OK);
    CHECK(frame.data == two);
    REQUIRE(ring.get_frame(seq3, &frame) == FrameServiceStatus::OK);
    CHECK(frame.data == three);
}

TEST_CASE("frame ring buffer lists newest available metadata in chronological order") {
    FrameRingBuffer ring(4);
    std::vector<uint8_t> payload = bytes({1});
    uint64_t seq = 0;

    for (uint64_t i = 1; i <= 4; ++i) {
        REQUIRE(ring.append_frame(metadata(1, 1, "uyvy", i * 10), payload.data(), payload.size(), &seq) == FrameServiceStatus::OK);
    }

    std::vector<FrameMetadata> frames = ring.list_frames(2);
    REQUIRE(frames.size() == 2);
    CHECK(frames[0].seq == 3);
    CHECK(frames[1].seq == 4);
    CHECK(frames[0].capture_ts_ns == 30);
    CHECK(frames[1].capture_ts_ns == 40);
}

TEST_CASE("frame ring buffer handles empty state") {
    FrameRingBuffer ring(2);
    FrameBufferFrame frame;

    CHECK(ring.size() == 0);
    CHECK(ring.latest_seq() == 0);
    CHECK(ring.latest_frame(0, &frame) == FrameServiceStatus::NO_NEW_FRAME);
    CHECK(ring.get_frame(1, &frame) == FrameServiceStatus::FRAME_NOT_FOUND);
    CHECK(ring.list_frames(8).empty());
}

TEST_CASE("frame ring buffer returns pinned frame references that survive eviction") {
    FrameRingBuffer ring(2);
    std::vector<uint8_t> one = bytes({1, 1, 1});
    std::vector<uint8_t> two = bytes({2, 2, 2});
    std::vector<uint8_t> three = bytes({3, 3, 3});
    uint64_t seq1 = 0;
    uint64_t seq2 = 0;
    uint64_t seq3 = 0;

    REQUIRE(ring.append_frame(metadata(1, 1, "uyvy", 1), one.data(), one.size(), &seq1) == FrameServiceStatus::OK);
    std::shared_ptr<const FrameBufferFrame> pinned;
    REQUIRE(ring.latest_frame_ref(0, &pinned) == FrameServiceStatus::OK);
    REQUIRE(static_cast<bool>(pinned));
    const uint8_t* pinned_data = pinned->data.data();

    REQUIRE(ring.append_frame(metadata(1, 1, "uyvy", 2), two.data(), two.size(), &seq2) == FrameServiceStatus::OK);
    REQUIRE(ring.append_frame(metadata(1, 1, "uyvy", 3), three.data(), three.size(), &seq3) == FrameServiceStatus::OK);

    CHECK(ring.get_frame(seq1, static_cast<FrameBufferFrame*>(nullptr)) == FrameServiceStatus::INTERNAL_ERROR);
    CHECK(pinned->metadata.seq == seq1);
    CHECK(pinned->data.data() == pinned_data);
    CHECK(pinned->data == one);

    std::shared_ptr<const FrameBufferFrame> latest;
    REQUIRE(ring.latest_frame_ref(seq2, &latest) == FrameServiceStatus::OK);
    CHECK(latest->metadata.seq == seq3);
}
