#include "doctest.h"
#include "audio_service_protocol.h"
#include <string>

using aico::AicoServiceStatus;
using aico::AudioFormat;
using aico::audio_format_to_json;
using aico::audio_format_from_json;
using aico::audio_request_json;
using aico::audio_response_json;
using aico::audio_response_status;
using aico::audio_json_u64;
using aico::audio_json_bool;
using aico::audio_json_u32;

// -----------------------------------------------------------------------
// AudioFormat encode / decode
// -----------------------------------------------------------------------

TEST_CASE("audio_format_to_json produces parseable key-value pairs") {
    AudioFormat fmt;
    fmt.sample_rate = 16000;
    fmt.channels    = 1;
    fmt.bit_width   = 16;

    // Wrap in braces so audio_format_from_json can parse it.
    std::string json = "{" + audio_format_to_json(fmt) + "}";

    AudioFormat out{};
    REQUIRE(audio_format_from_json(json.c_str(), &out));
    CHECK(out.sample_rate == 16000);
    CHECK(out.channels    == 1);
    CHECK(out.bit_width   == 16);
}

TEST_CASE("audio_format_from_json uses defaults for missing fields") {
    AudioFormat out{};
    out.sample_rate = 8000;
    out.channels    = 2;
    out.bit_width   = 8;

    // Only sample_rate present; other fields should remain at their prior value.
    REQUIRE(audio_format_from_json(R"({"sample_rate":44100})", &out));
    CHECK(out.sample_rate == 44100);
    CHECK(out.channels    == 2);   // unchanged
    CHECK(out.bit_width   == 8);   // unchanged
}

TEST_CASE("audio_format_from_json returns false for null or invalid JSON") {
    AudioFormat out{};
    CHECK_FALSE(audio_format_from_json(nullptr, &out));
    CHECK_FALSE(audio_format_from_json("not json", &out));
    CHECK_FALSE(audio_format_from_json(R"({"sample_rate":16000})", nullptr));
}

// -----------------------------------------------------------------------
// Request / response envelope builders
// -----------------------------------------------------------------------

TEST_CASE("audio_request_json wraps op with no extra fields") {
    std::string json = audio_request_json("start_recording");
    CHECK(json == R"({"op":"start_recording"})");
}

TEST_CASE("audio_request_json appends extra fields") {
    std::string json = audio_request_json("stop_recording", R"("session_id":"42")");
    CHECK(json == R"({"op":"stop_recording","session_id":"42"})");
}

TEST_CASE("audio_response_json wraps status with no extra fields") {
    std::string json = audio_response_json(AicoServiceStatus::OK);
    CHECK(json == R"({"status":"OK"})");
}

TEST_CASE("audio_response_json appends extra fields") {
    std::string json = audio_response_json(AicoServiceStatus::TIMEOUT, R"("detail":"poll expired")");
    CHECK(json == R"({"status":"TIMEOUT","detail":"poll expired"})");
}

// -----------------------------------------------------------------------
// Response parsers
// -----------------------------------------------------------------------

TEST_CASE("audio_response_status parses status field") {
    CHECK(audio_response_status(R"({"status":"OK"})") == AicoServiceStatus::OK);
    CHECK(audio_response_status(R"({"status":"TIMEOUT"})") == AicoServiceStatus::TIMEOUT);
    CHECK(audio_response_status(R"({"status":"SESSION_NOT_FOUND"})") == AicoServiceStatus::SESSION_NOT_FOUND);
}

TEST_CASE("audio_response_status returns INTERNAL_ERROR for bad input") {
    CHECK(audio_response_status(nullptr)        == AicoServiceStatus::INTERNAL_ERROR);
    CHECK(audio_response_status("not json")     == AicoServiceStatus::TRANSPORT_ERROR);
    CHECK(audio_response_status(R"({"x":"y"})") == AicoServiceStatus::INTERNAL_ERROR);
}

TEST_CASE("audio_json_u64 parses string-encoded uint64") {
    CHECK(audio_json_u64(R"({"session_id":"12345678901234"})", "session_id") == 12345678901234ULL);
    CHECK(audio_json_u64(R"({"session_id":42})", "session_id") == 42ULL);
    CHECK(audio_json_u64(R"({"other":"1"})", "session_id") == 0ULL);
    CHECK(audio_json_u64(nullptr, "session_id") == 0ULL);
}

TEST_CASE("audio_json_bool parses boolean field") {
    CHECK(audio_json_bool(R"({"is_final":true})",  "is_final") == true);
    CHECK(audio_json_bool(R"({"is_final":false})", "is_final") == false);
    CHECK(audio_json_bool(R"({"other":true})",     "is_final") == false);
    CHECK(audio_json_bool(nullptr, "is_final")                 == false);
}

TEST_CASE("audio_json_u32 parses integer field") {
    CHECK(audio_json_u32(R"({"timeout_ms":2000})", "timeout_ms") == 2000u);
    CHECK(audio_json_u32(R"({"other":1})",         "timeout_ms") == 0u);
    CHECK(audio_json_u32(nullptr, "timeout_ms")                  == 0u);
}
