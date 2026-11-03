#include "doctest.h"
#include "uds_client.h"
#include "uds_server.h"
#include <sys/stat.h>
#include <string>
#include <unistd.h>
#include <vector>

using aico::FrameServiceStatus;
using aico::UdsMessage;
using aico::UdsServer;
using aico::uds_request_once;
using aico::write_uds_message;

namespace {

struct TempSocketPath {
    std::string path;

    TempSocketPath() {
        char tmpl[] = "/tmp/aico_uds_server_XXXXXX";
        int fd = ::mkstemp(tmpl);
        REQUIRE(fd >= 0);
        ::close(fd);
        ::unlink(tmpl);
        path = tmpl;
    }

    ~TempSocketPath() { ::unlink(path.c_str()); }
};

}

TEST_CASE("UdsServer accepts one request and UdsClient returns response") {
    TempSocketPath socket_path;
    std::string seen_header;
    std::vector<uint8_t> seen_payload;
    UdsServer server(socket_path.path.c_str(), [&](const UdsMessage& request, int fd) {
        seen_header = request.header_json;
        seen_payload = request.payload;
        std::vector<uint8_t> response_payload = {9, 8, 7};
        write_uds_message(fd, R"({"type":"response","status":"OK"})", response_payload);
    });
    REQUIRE(server.start() == FrameServiceStatus::OK);

    struct stat socket_stat{};
    REQUIRE(::stat(socket_path.path.c_str(), &socket_stat) == 0);
    CHECK((socket_stat.st_mode & 0777) == 0660);

    std::vector<uint8_t> request_payload = {1, 2, 3};
    UdsMessage response;
    REQUIRE(uds_request_once(socket_path.path.c_str(),
                             R"({"type":"request","method":"ping"})",
                             request_payload,
                             &response,
                             1000) == FrameServiceStatus::OK);

    CHECK(seen_header == R"({"type":"request","method":"ping"})");
    CHECK(seen_payload == request_payload);
    CHECK(response.header_json == R"({"type":"response","status":"OK"})");
    CHECK(response.payload == std::vector<uint8_t>({9, 8, 7}));
    server.stop();
}

TEST_CASE("UdsClient returns transport error when socket is unavailable") {
    TempSocketPath socket_path;
    UdsMessage response;

    CHECK(uds_request_once(socket_path.path.c_str(),
                           R"({"type":"request","method":"ping"})",
                           std::vector<uint8_t>(),
                           &response,
                           1000) == FrameServiceStatus::TRANSPORT_ERROR);
}
