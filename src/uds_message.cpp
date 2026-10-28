#include "uds_message.h"
#include <errno.h>
#include <limits>
#include <sys/socket.h>
#include <unistd.h>

namespace aico {

static const uint32_t MAX_HEADER_LEN = 1024U * 1024U;
static const uint64_t MAX_PAYLOAD_LEN = 64ULL * 1024ULL * 1024ULL;

static void clear_message(UdsMessage* message) {
    if (!message) {
        return;
    }
    message->header_json.clear();
    message->payload.clear();
}

static FrameServiceStatus write_all(int fd, const uint8_t* data, size_t len) {
#ifdef SO_NOSIGPIPE
    int one = 1;
    ::setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof(one));
#endif

    while (len > 0) {
#ifdef MSG_NOSIGNAL
        ssize_t written = ::send(fd, data, len, MSG_NOSIGNAL);
#else
        ssize_t written = ::send(fd, data, len, 0);
#endif
        if (written < 0) {
            if (errno == EINTR) {
                continue;
            }
            return FrameServiceStatus::TRANSPORT_ERROR;
        }
        if (written == 0) {
            return FrameServiceStatus::TRANSPORT_ERROR;
        }
        data += written;
        len -= static_cast<size_t>(written);
    }
    return FrameServiceStatus::OK;
}

static FrameServiceStatus read_exact(int fd, uint8_t* data, size_t len) {
    while (len > 0) {
        ssize_t received = ::read(fd, data, len);
        if (received < 0) {
            if (errno == EINTR) {
                continue;
            }
            return FrameServiceStatus::TRANSPORT_ERROR;
        }
        if (received == 0) {
            return FrameServiceStatus::TRANSPORT_ERROR;
        }
        data += received;
        len -= static_cast<size_t>(received);
    }
    return FrameServiceStatus::OK;
}

FrameServiceStatus write_uds_message(int fd,
                                     const std::string& header_json,
                                     const std::vector<uint8_t>& payload) {
    std::vector<uint8_t> prefix = encode_frame_wire_prefix(
        static_cast<uint32_t>(header_json.size()),
        static_cast<uint64_t>(payload.size()));

    FrameServiceStatus status = write_all(fd, prefix.data(), prefix.size());
    if (status != FrameServiceStatus::OK) {
        return status;
    }
    if (!header_json.empty()) {
        status = write_all(fd,
                           reinterpret_cast<const uint8_t*>(header_json.data()),
                           header_json.size());
        if (status != FrameServiceStatus::OK) {
            return status;
        }
    }
    if (!payload.empty()) {
        status = write_all(fd, payload.data(), payload.size());
    }
    return status;
}

FrameServiceStatus read_uds_message(int fd, UdsMessage* out) {
    if (!out) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }

    uint8_t prefix_bytes[kFrameWirePrefixSize];
    FrameServiceStatus status = read_exact(fd, prefix_bytes, sizeof(prefix_bytes));
    if (status != FrameServiceStatus::OK) {
        return status;
    }

    FrameWirePrefix prefix{};
    if (!decode_frame_wire_prefix(prefix_bytes, sizeof(prefix_bytes), &prefix)) {
        return FrameServiceStatus::TRANSPORT_ERROR;
    }

    if (prefix.header_len > MAX_HEADER_LEN ||
        prefix.payload_len > MAX_PAYLOAD_LEN ||
        prefix.payload_len > static_cast<uint64_t>(std::numeric_limits<size_t>::max())) {
        clear_message(out);
        return FrameServiceStatus::TRANSPORT_ERROR;
    }

    out->header_json.clear();
    out->payload.clear();
    out->header_json.resize(prefix.header_len);
    if (prefix.header_len > 0) {
        status = read_exact(fd,
                            reinterpret_cast<uint8_t*>(&out->header_json[0]),
                            prefix.header_len);
        if (status != FrameServiceStatus::OK) {
            clear_message(out);
            return status;
        }
    }

    out->payload.resize(static_cast<size_t>(prefix.payload_len));
    if (prefix.payload_len > 0) {
        status = read_exact(fd, out->payload.data(), out->payload.size());
        if (status != FrameServiceStatus::OK) {
            clear_message(out);
        }
    }
    return status;
}

}  // namespace aico
