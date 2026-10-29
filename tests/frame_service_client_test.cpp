#include "doctest.h"
#include "frame_ipc.h"
#include "frame_service_client.h"
#include <string>
#include <sys/socket.h>
#include <sys/un.h>
#include <thread>
#include <unistd.h>
#include <vector>

using aico::FrameResult;
using aico::FrameServiceClient;
using aico::FrameServiceStatus;
using aico::HealthResult;

namespace {

struct TempSocketPath {
    std::string path;

    TempSocketPath() {
        char tmpl[] = "/tmp/aico_frame_client_XXXXXX";
        int fd = ::mkstemp(tmpl);
        REQUIRE(fd >= 0);
        ::close(fd);
        ::unlink(tmpl);
        path = tmpl;
    }

    ~TempSocketPath() { ::unlink(path.c_str()); }
};

struct SingleReplyServer {
    TempSocketPath path;
    int server_fd;
    std::string reply_header;
    std::vector<uint8_t> reply_payload;
    std::string request_header;
    std::thread thread;

    SingleReplyServer(const std::string& header, const std::vector<uint8_t>& payload)
        : server_fd(-1), reply_header(header), reply_payload(payload) {
        server_fd = ::socket(AF_UNIX, SOCK_STREAM, 0);
        REQUIRE(server_fd >= 0);

        struct sockaddr_un addr;
        memset(&addr, 0, sizeof(addr));
        addr.sun_family = AF_UNIX;
        REQUIRE(path.path.size() < sizeof(addr.sun_path));
        strcpy(addr.sun_path, path.path.c_str());

        REQUIRE(::bind(server_fd, reinterpret_cast<struct sockaddr*>(&addr), sizeof(addr)) == 0);
        REQUIRE(::listen(server_fd, 1) == 0);

        thread = std::thread([this]() {
            int client_fd = ::accept(server_fd, nullptr, nullptr);
            if (client_fd < 0) return;
            aico::FrameIpcMessage request;
            if (aico::read_frame_message(client_fd, &request) == FrameServiceStatus::OK) {
                request_header = request.header_json;
                aico::write_frame_message(client_fd, reply_header, reply_payload);
            }
            ::close(client_fd);
        });
    }

    ~SingleReplyServer() {
        if (thread.joinable()) thread.join();
        if (server_fd >= 0) ::close(server_fd);
    }
};

}

TEST_CASE("FrameServiceClient health sends request and parses health response") {
    SingleReplyServer server(
        R"({"type":"response","method":"health","status":"OK","state":"RUNNING","capture_mode":"on_demand","latest_seq":42,"frame_age_ms":7,"ring_buffer_size":8,"ring_buffer_used":3,"consecutive_failures":0,"last_recovery_ts":"9007199254740993","avg_frame_serve_latency_ms":1.5})",
        std::vector<uint8_t>());
    FrameServiceClient client(server.path.path.c_str());
    HealthResult result;

    REQUIRE(client.health(&result) == FrameServiceStatus::OK);
    CHECK(server.request_header.find(R"("method":"health")") != std::string::npos);
    CHECK(result.state == "RUNNING");
    CHECK(result.capture_mode == "on_demand");
    CHECK(result.latest_seq == 42);
    CHECK(result.frame_age_ms == 7);
    CHECK(result.ring_buffer_size == 8);
    CHECK(result.ring_buffer_used == 3);
    CHECK(result.last_recovery_ts == 9007199254740993ull);
    CHECK(result.avg_frame_serve_latency_ms == doctest::Approx(1.5));
}

TEST_CASE("FrameServiceClient latest_frame sends since and timeout and parses payload") {
    std::vector<uint8_t> payload = {1, 2, 3, 4};
    SingleReplyServer server(
        R"({"type":"response","method":"latest_frame","status":"OK","frame":{"seq":11,"capture_ts_ns":99,"width":2,"height":1,"pixel_format":"uyvy","stride":4,"bytes":4,"stale":false}})",
        payload);
    FrameServiceClient client(server.path.path.c_str());
    FrameResult result;

    REQUIRE(client.latest_frame(10, 5000, &result) == FrameServiceStatus::OK);
    CHECK(server.request_header.find(R"("method":"latest_frame")") != std::string::npos);
    CHECK(server.request_header.find(R"("since_seq":"10")") != std::string::npos);
    CHECK(server.request_header.find(R"("timeout_ms":5000)") != std::string::npos);
    CHECK(result.metadata.seq == 11);
    CHECK(result.metadata.width == 2);
    CHECK(result.metadata.height == 1);
    CHECK(result.metadata.pixel_format == "uyvy");
    CHECK(result.data == payload);
}

TEST_CASE("FrameServiceClient encodes outbound uint64 frame ids as JSON strings") {
    SingleReplyServer latest_server(
        R"({"type":"error","method":"latest_frame","status":"NO_NEW_FRAME"})",
        std::vector<uint8_t>());
    FrameServiceClient latest_client(latest_server.path.path.c_str());
    FrameResult latest_result;

    CHECK(latest_client.latest_frame(9007199254740993ull, 0, &latest_result) == FrameServiceStatus::NO_NEW_FRAME);
    CHECK(latest_server.request_header.find(R"("since_seq":"9007199254740993")") != std::string::npos);

    SingleReplyServer get_server(
        R"({"type":"error","method":"get_frame","status":"FRAME_NOT_FOUND"})",
        std::vector<uint8_t>());
    FrameServiceClient get_client(get_server.path.path.c_str());
    FrameResult get_result;

    CHECK(get_client.get_frame(9007199254740995ull, &get_result) == FrameServiceStatus::FRAME_NOT_FOUND);
    CHECK(get_server.request_header.find(R"("seq":"9007199254740995")") != std::string::npos);
}

TEST_CASE("FrameServiceClient preserves large uint64 metadata encoded as JSON strings") {
    std::vector<uint8_t> payload = {1};
    SingleReplyServer server(
        R"({"type":"response","method":"latest_frame","status":"OK","frame":{"seq":"9007199254740993","capture_ts_ns":"1844674407370955161","width":1,"height":1,"pixel_format":"uyvy","stride":2,"bytes":"1","stale":false}})",
        payload);
    FrameServiceClient client(server.path.path.c_str());
    FrameResult result;

    REQUIRE(client.latest_frame(0, 0, &result) == FrameServiceStatus::OK);
    CHECK(result.metadata.seq == 9007199254740993ull);
    CHECK(result.metadata.capture_ts_ns == 1844674407370955161ull);
    CHECK(result.metadata.bytes == 1);
}

TEST_CASE("FrameServiceClient list_frames preserves large uint64 metadata without payload") {
    SingleReplyServer server(
        R"({"type":"response","method":"list_frames","status":"OK","frames":[{"seq":"9007199254740993","capture_ts_ns":"1844674407370955161","width":1,"height":1,"pixel_format":"uyvy","stride":2,"bytes":"9007199254740995"}]})",
        std::vector<uint8_t>());
    FrameServiceClient client(server.path.path.c_str());
    aico::FrameListResult result;

    REQUIRE(client.list_frames(1, &result) == FrameServiceStatus::OK);
    REQUIRE(result.frames.size() == 1);
    CHECK(result.frames[0].seq == 9007199254740993ull);
    CHECK(result.frames[0].capture_ts_ns == 1844674407370955161ull);
    CHECK(result.frames[0].bytes == 9007199254740995ull);
}

TEST_CASE("FrameServiceClient list_frames skips malformed frame entries") {
    SingleReplyServer server(
        R"({"type":"response","method":"list_frames","status":"OK","frames":[null,{"seq":"7","capture_ts_ns":"8","width":1,"height":1,"pixel_format":"uyvy","stride":2,"bytes":"2"}]})",
        std::vector<uint8_t>());
    FrameServiceClient client(server.path.path.c_str());
    aico::FrameListResult result;

    REQUIRE(client.list_frames(2, &result) == FrameServiceStatus::OK);
    REQUIRE(result.frames.size() == 1);
    CHECK(result.frames[0].seq == 7);
}

TEST_CASE("FrameServiceClient parses plane metadata") {
    std::vector<uint8_t> payload = {1, 2, 3, 4};
    SingleReplyServer server(
        R"({"type":"response","method":"latest_frame","status":"OK","frame":{"seq":1,"capture_ts_ns":2,"width":2,"height":1,"pixel_format":"nv12","stride":2,"bytes":4,"planes":[{"offset":0,"stride":2,"bytes":2},{"offset":2,"stride":2,"bytes":2}],"stale":false}})",
        payload);
    FrameServiceClient client(server.path.path.c_str());
    FrameResult result;

    REQUIRE(client.latest_frame(0, 0, &result) == FrameServiceStatus::OK);
    REQUIRE(result.metadata.planes.size() == 2);
    CHECK(result.metadata.planes[0].offset == 0);
    CHECK(result.metadata.planes[0].stride == 2);
    CHECK(result.metadata.planes[0].bytes == 2);
    CHECK(result.metadata.planes[1].offset == 2);
    CHECK(result.metadata.planes[1].stride == 2);
    CHECK(result.metadata.planes[1].bytes == 2);
}

TEST_CASE("FrameServiceClient rejects frame response with mismatched payload size") {
    std::vector<uint8_t> payload = {1, 2};
    SingleReplyServer server(
        R"({"type":"response","method":"latest_frame","status":"OK","frame":{"seq":1,"capture_ts_ns":2,"width":2,"height":1,"pixel_format":"uyvy","stride":4,"bytes":4,"stale":false}})",
        payload);
    FrameServiceClient client(server.path.path.c_str());
    FrameResult result;

    CHECK(client.latest_frame(0, 0, &result) == FrameServiceStatus::TRANSPORT_ERROR);
}

TEST_CASE("FrameServiceClient maps service error status") {
    SingleReplyServer server(
        R"({"type":"error","method":"latest_frame","status":"SERVICE_RECOVERING","message":"recovering"})",
        std::vector<uint8_t>());
    FrameServiceClient client(server.path.path.c_str());
    FrameResult result;

    CHECK(client.latest_frame(0, 0, &result) == FrameServiceStatus::SERVICE_RECOVERING);
}

TEST_CASE("FrameServiceClient returns transport error when socket is unavailable") {
    TempSocketPath path;
    FrameServiceClient client(path.path.c_str());
    HealthResult result;

    CHECK(client.health(&result) == FrameServiceStatus::TRANSPORT_ERROR);
}
