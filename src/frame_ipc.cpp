#include "frame_ipc.h"

namespace aico {

FrameServiceStatus write_frame_message(int fd,
                                       const std::string& header_json,
                                       const std::vector<uint8_t>& payload) {
    return write_uds_message(fd, header_json, payload);
}

FrameServiceStatus read_frame_message(int fd, FrameIpcMessage* out) {
    return read_uds_message(fd, out);
}

}  // namespace aico
