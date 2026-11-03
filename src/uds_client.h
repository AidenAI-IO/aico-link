#pragma once

#include "uds_message.h"
#include <string>
#include <vector>

namespace aico {

FrameServiceStatus uds_request_once(const std::string& socket_path,
                                    const std::string& request_json,
                                    const std::vector<uint8_t>& request_payload,
                                    UdsMessage* response,
                                    uint32_t timeout_ms = 5000);

}  // namespace aico
