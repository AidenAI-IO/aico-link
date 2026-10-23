#pragma once

#include "frame_ring_buffer.h"
#include "uds_server.h"
#include <atomic>
#include <condition_variable>
#include <functional>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

namespace aico {

class FrameServiceServer {
public:
    typedef std::function<FrameServiceStatus(uint32_t timeout_ms,
                                             FrameMetadata* metadata,
                                             std::vector<uint8_t>* data)> CaptureHandler;

    FrameServiceServer(const char* socket_path, size_t ring_capacity);
    ~FrameServiceServer();

    FrameServiceStatus start();
    void stop();

    FrameServiceStatus append_frame(const FrameMetadata& metadata,
                                    const uint8_t* data,
                                    size_t bytes,
                                    uint64_t* seq_out);

    void set_state(const std::string& state);
    void set_capture_handler(const CaptureHandler& handler);
    void set_restart_handler(const std::function<void()>& handler);
    void record_recovery(const std::string& error, bool count_failure);

private:
    void handle_request(const UdsMessage& request, int fd);
    bool is_recovering() const;
    uint64_t frame_age_ms() const;
    void record_serve_latency(uint64_t started_ns);
    void record_capture_copy_latency(uint64_t started_ns);
    double avg_frame_serve_latency_ms() const;
    bool acquire_payload_send_slot();
    void release_payload_send_slot();
    FrameServiceStatus write_payload_message(int fd,
                                             const std::string& header,
                                             const std::vector<uint8_t>& payload);

    std::unique_ptr<UdsServer> uds_server_;
    FrameRingBuffer ring_;
    std::string state_;
    bool running_;
    mutable std::mutex mutex_;
    std::condition_variable frame_cv_;
    std::condition_variable payload_cv_;
    CaptureHandler capture_handler_;
    std::function<void()> restart_handler_;
    uint64_t on_demand_latest_seq_;
    uint64_t on_demand_capture_ts_ns_;
    double avg_frame_serve_latency_ms_;
    uint32_t serve_latency_samples_;
    double avg_capture_copy_latency_ms_;
    uint32_t capture_copy_latency_samples_;
    uint32_t consecutive_failures_;
    std::string last_error_;
    uint64_t last_recovery_ts_;
    uint64_t capture_request_count_;
    uint32_t active_payload_sends_;
    uint32_t max_payload_sends_;
};

}  // namespace aico
