#pragma once

#include "frame_service_protocol.h"
#include <string>
#include <vector>

namespace aico {

struct UdsMessage {
    std::string header_json;
    std::vector<uint8_t> payload;
};

FrameServiceStatus write_uds_message(int fd,
                                     const std::string& header_json,
                                     const std::vector<uint8_t>& payload);
FrameServiceStatus read_uds_message(int fd, UdsMessage* out);

}  // namespace aico
