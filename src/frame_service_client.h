#pragma once

#include "frame_service_protocol.h"
#include <string>
#include <vector>

namespace aico {

struct HealthResult {
    std::string state;
    std::string capture_mode;
    uint64_t latest_seq = 0;
    uint64_t frame_age_ms = 0;
    uint32_t ring_buffer_size = 0;
    uint32_t ring_buffer_used = 0;
    uint32_t consecutive_failures = 0;
    std::string last_error;
    uint64_t last_recovery_ts = 0;
    double avg_frame_serve_latency_ms = 0.0;
    double avg_capture_copy_latency_ms = 0.0;
};

struct FrameResult {
    FrameMetadata metadata;
    std::vector<uint8_t> data;
};

struct FrameListResult {
    std::vector<FrameMetadata> frames;
};

class FrameServiceClient {
public:
    explicit FrameServiceClient(const char* socket_path);

    FrameServiceStatus health(HealthResult* out);
    FrameServiceStatus latest_frame(uint64_t since_seq, uint32_t timeout_ms, FrameResult* out);
    FrameServiceStatus get_frame(uint64_t seq, FrameResult* out);
    FrameServiceStatus list_frames(uint32_t count, FrameListResult* out);
    FrameServiceStatus restart();

private:
    std::string socket_path_;
};

}  // namespace aico
