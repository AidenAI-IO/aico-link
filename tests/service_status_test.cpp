#include "doctest.h"
#include "service_status.h"
#include "frame_service_protocol.h"
#include <string>

using aico::AicoServiceStatus;
using aico::service_status_to_string;
using aico::service_status_from_string;

TEST_CASE("service_status_to_string returns stable protocol strings") {
    CHECK(std::string(service_status_to_string(AicoServiceStatus::OK))                == "OK");
    CHECK(std::string(service_status_to_string(AicoServiceStatus::NO_NEW_FRAME))      == "NO_NEW_FRAME");
    CHECK(std::string(service_status_to_string(AicoServiceStatus::FRAME_NOT_FOUND))   == "FRAME_NOT_FOUND");
    CHECK(std::string(service_status_to_string(AicoServiceStatus::SESSION_NOT_FOUND)) == "SESSION_NOT_FOUND");
    CHECK(std::string(service_status_to_string(AicoServiceStatus::SERVICE_RECOVERING))== "SERVICE_RECOVERING");
    CHECK(std::string(service_status_to_string(AicoServiceStatus::TIMEOUT))           == "TIMEOUT");
    CHECK(std::string(service_status_to_string(AicoServiceStatus::TRANSPORT_ERROR))   == "TRANSPORT_ERROR");
    CHECK(std::string(service_status_to_string(AicoServiceStatus::INTERNAL_ERROR))    == "INTERNAL_ERROR");
}

TEST_CASE("service_status_from_string parses known protocol strings") {
    AicoServiceStatus s = AicoServiceStatus::INTERNAL_ERROR;

    REQUIRE(service_status_from_string("OK", &s));
    CHECK(s == AicoServiceStatus::OK);

    REQUIRE(service_status_from_string("SESSION_NOT_FOUND", &s));
    CHECK(s == AicoServiceStatus::SESSION_NOT_FOUND);

    REQUIRE(service_status_from_string("TIMEOUT", &s));
    CHECK(s == AicoServiceStatus::TIMEOUT);

    REQUIRE(service_status_from_string("TRANSPORT_ERROR", &s));
    CHECK(s == AicoServiceStatus::TRANSPORT_ERROR);
}

TEST_CASE("service_status_from_string rejects unknown and null inputs") {
    AicoServiceStatus s = AicoServiceStatus::OK;

    CHECK_FALSE(service_status_from_string("UNKNOWN", &s));
    CHECK(s == AicoServiceStatus::OK);  // unchanged on failure

    CHECK_FALSE(service_status_from_string(nullptr, &s));
    CHECK_FALSE(service_status_from_string("OK", nullptr));
}

TEST_CASE("FrameServiceStatus alias resolves to AicoServiceStatus") {
    // FrameServiceStatus is a typedef alias; verify the wire strings are identical
    // so frame_service and audio_service share the same protocol vocabulary.
    using aico::FrameServiceStatus;
    CHECK(std::string(service_status_to_string(FrameServiceStatus::OK)) == "OK");
    CHECK(std::string(service_status_to_string(FrameServiceStatus::TIMEOUT)) == "TIMEOUT");
}
