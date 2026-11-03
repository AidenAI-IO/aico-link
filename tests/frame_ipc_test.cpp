#include "doctest.h"
#include "frame_ipc.h"
#include <string>
#include <sys/socket.h>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

using aico::FrameIpcMessage;
using aico::FrameServiceStatus;
using aico::read_frame_message;
using aico::write_frame_message;

namespace {

struct SocketPair {
    int fds[2];

    SocketPair() : fds{-1, -1} {
        REQUIRE(::socketpair(AF_UNIX, SOCK_STREAM, 0, fds) == 0);
    }

    ~SocketPair() {
        if (fds[0] >= 0) ::close(fds[0]);
        if (fds[1] >= 0) ::close(fds[1]);
    }
};

}

TEST_CASE("frame ipc sends and receives json-only messages") {
    SocketPair sockets;
    FrameIpcMessage message;

    REQUIRE(write_frame_message(sockets.fds[0], R"({"type":"request","method":"health"})", std::vector<uint8_t>()) == FrameServiceStatus::OK);
    REQUIRE(read_frame_message(sockets.fds[1], &message) == FrameServiceStatus::OK);

    CHECK(message.header_json == R"({"type":"request","method":"health"})");
    CHECK(message.payload.empty());
}

TEST_CASE("frame ipc preserves arbitrary binary payload bytes") {
    SocketPair sockets;
    FrameIpcMessage message;
    std::vector<uint8_t> payload = {0x00, 0xff, '{', '}', 0x01, 0x02};

    REQUIRE(write_frame_message(sockets.fds[0], R"({"type":"response","status":"OK"})", payload) == FrameServiceStatus::OK);
    REQUIRE(read_frame_message(sockets.fds[1], &message) == FrameServiceStatus::OK);

    CHECK(message.header_json == R"({"type":"response","status":"OK"})");
    CHECK(message.payload == payload);
}

TEST_CASE("frame ipc returns transport error for incomplete prefix") {
    SocketPair sockets;
    const uint8_t partial_prefix[] = {1, 2, 3};
    FrameIpcMessage message;

    REQUIRE(::write(sockets.fds[0], partial_prefix, sizeof(partial_prefix)) == (ssize_t)sizeof(partial_prefix));
    ::close(sockets.fds[0]);
    sockets.fds[0] = -1;

    CHECK(read_frame_message(sockets.fds[1], &message) == FrameServiceStatus::TRANSPORT_ERROR);
}

TEST_CASE("frame ipc returns transport error for incomplete payload") {
    SocketPair sockets;
    std::vector<uint8_t> prefix = aico::encode_frame_wire_prefix(2, 4);
    FrameIpcMessage message;

    REQUIRE(::write(sockets.fds[0], prefix.data(), prefix.size()) == (ssize_t)prefix.size());
    REQUIRE(::write(sockets.fds[0], "{}", 2) == 2);
    REQUIRE(::write(sockets.fds[0], "ab", 2) == 2);
    ::close(sockets.fds[0]);
    sockets.fds[0] = -1;

    CHECK(read_frame_message(sockets.fds[1], &message) == FrameServiceStatus::TRANSPORT_ERROR);
}

TEST_CASE("frame ipc rejects oversized payload without retaining partial output") {
    SocketPair sockets;
    const uint64_t too_large_payload = 64ull * 1024ull * 1024ull + 1ull;
    std::vector<uint8_t> prefix = aico::encode_frame_wire_prefix(2, too_large_payload);
    FrameIpcMessage message;

    REQUIRE(::write(sockets.fds[0], prefix.data(), prefix.size()) == (ssize_t)prefix.size());
    REQUIRE(::write(sockets.fds[0], "{}", 2) == 2);
    ::close(sockets.fds[0]);
    sockets.fds[0] = -1;

    CHECK(read_frame_message(sockets.fds[1], &message) == FrameServiceStatus::TRANSPORT_ERROR);
    CHECK(message.header_json.empty());
    CHECK(message.payload.empty());
}

TEST_CASE("frame ipc writer reports transport error instead of terminating on closed peer") {
    SocketPair sockets;
    ::close(sockets.fds[1]);
    sockets.fds[1] = -1;

    pid_t pid = ::fork();
    REQUIRE(pid >= 0);
    if (pid == 0) {
        FrameServiceStatus status = write_frame_message(
            sockets.fds[0],
            R"({"type":"request","method":"health"})",
            std::vector<uint8_t>());
        _exit(status == FrameServiceStatus::TRANSPORT_ERROR ? 0 : 2);
    }

    int status = 0;
    REQUIRE(::waitpid(pid, &status, 0) == pid);
    REQUIRE(WIFEXITED(status));
    CHECK(WEXITSTATUS(status) == 0);
}
