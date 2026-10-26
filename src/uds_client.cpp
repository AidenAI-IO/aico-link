#include "uds_client.h"
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <sys/un.h>
#include <unistd.h>

namespace aico {

static int connect_socket(const std::string& socket_path, uint32_t timeout_ms) {
    if (socket_path.empty()) {
        return -1;
    }

    int fd = ::socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) {
        return -1;
    }

    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    if (socket_path.size() >= sizeof(addr.sun_path)) {
        ::close(fd);
        return -1;
    }
    strncpy(addr.sun_path, socket_path.c_str(), sizeof(addr.sun_path) - 1);

    if (::connect(fd, reinterpret_cast<struct sockaddr*>(&addr), sizeof(addr)) < 0) {
        ::close(fd);
        return -1;
    }

    if (timeout_ms == 0) timeout_ms = 5000;
    struct timeval timeout;
    timeout.tv_sec = timeout_ms / 1000;
    timeout.tv_usec = (timeout_ms % 1000) * 1000;
    ::setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    ::setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));

    return fd;
}

FrameServiceStatus uds_request_once(const std::string& socket_path,
                                    const std::string& request_json,
                                    const std::vector<uint8_t>& request_payload,
                                    UdsMessage* response,
                                    uint32_t timeout_ms) {
    int fd = connect_socket(socket_path, timeout_ms);
    if (fd < 0) {
        return FrameServiceStatus::TRANSPORT_ERROR;
    }

    FrameServiceStatus status = write_uds_message(fd, request_json, request_payload);
    if (status == FrameServiceStatus::OK) {
        status = read_uds_message(fd, response);
    }
    ::close(fd);
    return status;
}

}  // namespace aico
