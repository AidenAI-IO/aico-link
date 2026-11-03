#include "doctest.h"
#include "frame_service_protocol.h"
#include <cstdint>
#include <string>
#include <vector>

using aico::FrameServiceStatus;
using aico::FrameWirePrefix;
using aico::decode_frame_wire_prefix;
using aico::encode_frame_wire_prefix;
using aico::frame_service_status_from_string;
using aico::frame_service_status_to_string;
using aico::kFrameWirePrefixSize;

TEST_CASE("frame service status converts to stable protocol strings") {
    CHECK(std::string(frame_service_status_to_string(FrameServiceStatus::OK)) == "OK");
    CHECK(std::string(frame_service_status_to_string(FrameServiceStatus::NO_NEW_FRAME)) == "NO_NEW_FRAME");
    CHECK(std::string(frame_service_status_to_string(FrameServiceStatus::FRAME_NOT_FOUND)) == "FRAME_NOT_FOUND");
    CHECK(std::string(frame_service_status_to_string(FrameServiceStatus::SERVICE_RECOVERING)) == "SERVICE_RECOVERING");
    CHECK(std::string(frame_service_status_to_string(FrameServiceStatus::TIMEOUT)) == "TIMEOUT");
    CHECK(std::string(frame_service_status_to_string(FrameServiceStatus::TRANSPORT_ERROR)) == "TRANSPORT_ERROR");
    CHECK(std::string(frame_service_status_to_string(FrameServiceStatus::INTERNAL_ERROR)) == "INTERNAL_ERROR");
}

TEST_CASE("frame service status parses known protocol strings") {
    FrameServiceStatus status = FrameServiceStatus::INTERNAL_ERROR;

    REQUIRE(frame_service_status_from_string("OK", &status));
    CHECK(status == FrameServiceStatus::OK);

    REQUIRE(frame_service_status_from_string("TIMEOUT", &status));
    CHECK(status == FrameServiceStatus::TIMEOUT);

    REQUIRE(frame_service_status_from_string("TRANSPORT_ERROR", &status));
    CHECK(status == FrameServiceStatus::TRANSPORT_ERROR);
}

TEST_CASE("frame service status rejects unknown protocol strings") {
    FrameServiceStatus status = FrameServiceStatus::OK;

    CHECK_FALSE(frame_service_status_from_string("BOOM", &status));
    CHECK(status == FrameServiceStatus::OK);
    CHECK_FALSE(frame_service_status_from_string(nullptr, &status));
    CHECK_FALSE(frame_service_status_from_string("OK", nullptr));
}

TEST_CASE("frame wire prefix encodes header and payload lengths as little endian") {
    std::vector<uint8_t> bytes = encode_frame_wire_prefix(0x01020304u, 0x0102030405060708ull);

    REQUIRE(bytes.size() == kFrameWirePrefixSize);
    CHECK(bytes[0] == 0x04);
    CHECK(bytes[1] == 0x03);
    CHECK(bytes[2] == 0x02);
    CHECK(bytes[3] == 0x01);
    CHECK(bytes[4] == 0x08);
    CHECK(bytes[5] == 0x07);
    CHECK(bytes[6] == 0x06);
    CHECK(bytes[7] == 0x05);
    CHECK(bytes[8] == 0x04);
    CHECK(bytes[9] == 0x03);
    CHECK(bytes[10] == 0x02);
    CHECK(bytes[11] == 0x01);
}

TEST_CASE("frame wire prefix decodes header and payload lengths") {
    std::vector<uint8_t> bytes = encode_frame_wire_prefix(18, 4096);
    FrameWirePrefix prefix{};

    REQUIRE(decode_frame_wire_prefix(bytes.data(), bytes.size(), &prefix));
    CHECK(prefix.header_len == 18);
    CHECK(prefix.payload_len == 4096);
}

TEST_CASE("frame wire prefix rejects incomplete input") {
    uint8_t short_bytes[11] = {};
    uint8_t full_bytes[kFrameWirePrefixSize] = {};
    FrameWirePrefix prefix{};

    CHECK_FALSE(decode_frame_wire_prefix(short_bytes, sizeof(short_bytes), &prefix));
    CHECK_FALSE(decode_frame_wire_prefix(nullptr, kFrameWirePrefixSize, &prefix));
    CHECK_FALSE(decode_frame_wire_prefix(full_bytes, sizeof(full_bytes), nullptr));
}
