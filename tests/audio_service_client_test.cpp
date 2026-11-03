#include "doctest.h"
#include "audio_service_client.h"
#include "audio_service_protocol.h"
#include "uds_server.h"
#include "uds_message.h"
#include <string>
#include <unistd.h>

using aico::AicoServiceStatus;
using aico::AudioServiceClient;
using aico::UdsServer;
using aico::UdsMessage;
using aico::write_uds_message;
using aico::audio_response_json;
using aico::audio_json_u64;

namespace {

struct TempSocketPath {
    std::string path;

    TempSocketPath() {
        char tmpl[] = "/tmp/aico_audio_client_XXXXXX";
        int fd = ::mkstemp(tmpl);
        REQUIRE(fd >= 0);
        ::close(fd);
        ::unlink(tmpl);
        path = tmpl;
    }

    ~TempSocketPath() { ::unlink(path.c_str()); }
};

// Minimal stub server: responds to a single request with a fixed JSON reply.
struct StubServer {
    TempSocketPath sock;
    UdsServer server;

    explicit StubServer(const std::string& response_json)
        : server(sock.path.c_str(),
                 [response_json](const UdsMessage&, int fd) {
                     write_uds_message(fd, response_json, {});
                 }) {
        REQUIRE(server.start() == AicoServiceStatus::OK);
    }

    ~StubServer() { server.stop(); }

    const char* path() const { return sock.path.c_str(); }
};

}  // namespace

// -----------------------------------------------------------------------
// health
// -----------------------------------------------------------------------

TEST_CASE("AudioServiceClient::health parses OK response") {
    StubServer stub(
        R"({"status":"OK","recording_active":false,"playback_active":true,)"
        R"("record_sessions":0,"playback_sessions":1})");

    AudioServiceClient client(stub.path());
    aico::AudioHealthResult h;
    REQUIRE(client.health(&h) == AicoServiceStatus::OK);
    CHECK(h.recording_active  == false);
    CHECK(h.playback_active   == true);
    CHECK(h.record_sessions   == 0);
    CHECK(h.playback_sessions == 1);
}

// -----------------------------------------------------------------------
// start_recording
// -----------------------------------------------------------------------

TEST_CASE("AudioServiceClient::start_recording parses session_id from response") {
    StubServer stub(R"({"status":"OK","session_id":"42"})");

    AudioServiceClient client(stub.path());
    aico::RecordStartResult rs;
    REQUIRE(client.start_recording({}, &rs) == AicoServiceStatus::OK);
    CHECK(rs.session_id == 42ULL);
}

TEST_CASE("AudioServiceClient::start_recording propagates error status") {
    StubServer stub(R"({"status":"INTERNAL_ERROR"})");

    AudioServiceClient client(stub.path());
    aico::RecordStartResult rs;
    CHECK(client.start_recording({}, &rs) == AicoServiceStatus::INTERNAL_ERROR);
}

// -----------------------------------------------------------------------
// stop_recording
// -----------------------------------------------------------------------

TEST_CASE("AudioServiceClient::stop_recording returns OK on success") {
    StubServer stub(R"({"status":"OK"})");

    AudioServiceClient client(stub.path());
    CHECK(client.stop_recording(1) == AicoServiceStatus::OK);
}

TEST_CASE("AudioServiceClient::stop_recording propagates SESSION_NOT_FOUND") {
    StubServer stub(R"({"status":"SESSION_NOT_FOUND"})");

    AudioServiceClient client(stub.path());
    CHECK(client.stop_recording(999) == AicoServiceStatus::SESSION_NOT_FOUND);
}

// -----------------------------------------------------------------------
// start_playback
// -----------------------------------------------------------------------

TEST_CASE("AudioServiceClient::start_playback parses session_id from response") {
    StubServer stub(R"({"status":"OK","session_id":"7"})");

    AudioServiceClient client(stub.path());
    aico::PlaybackStartResult ps;
    REQUIRE(client.start_playback({}, &ps) == AicoServiceStatus::OK);
    CHECK(ps.session_id == 7ULL);
}

// -----------------------------------------------------------------------
// write_play_chunk
// -----------------------------------------------------------------------

TEST_CASE("AudioServiceClient::write_play_chunk returns OK on success") {
    StubServer stub(R"({"status":"OK"})");

    AudioServiceClient client(stub.path());
    uint8_t pcm[8] = {0};
    CHECK(client.write_play_chunk(1, pcm, sizeof(pcm), false) == AicoServiceStatus::OK);
}

// -----------------------------------------------------------------------
// Transport error when service is not running
// -----------------------------------------------------------------------

TEST_CASE("AudioServiceClient returns TRANSPORT_ERROR when service is not running") {
    TempSocketPath sock;  // socket file does not exist
    AudioServiceClient client(sock.path.c_str());

    aico::AudioHealthResult h;
    CHECK(client.health(&h) == AicoServiceStatus::TRANSPORT_ERROR);
}
