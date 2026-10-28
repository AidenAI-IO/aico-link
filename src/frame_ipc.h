#pragma once

#include "uds_message.h"

namespace aico {

using FrameIpcMessage = UdsMessage;

FrameServiceStatus write_frame_message(int fd,
                                       const std::string& header_json,
                                       const std::vector<uint8_t>& payload);
FrameServiceStatus read_frame_message(int fd, FrameIpcMessage* out);

}  // namespace aico
