#pragma once

#include "frame_service_server.h"
#include <atomic>
#include <condition_variable>
#include <mutex>
#include <thread>
#include <vector>

namespace aico {

struct CapturedFrame {
    FrameMetadata metadata;
    std::vector<uint8_t> data;
};

class FrameCaptureSource {
public:
    virtual ~FrameCaptureSource() {}
    virtual bool open() = 0;
    // Pause capture DMA while keeping the device and its mapped buffers open.
    virtual bool pause() { return true; }
    // Resume capture DMA before an on-demand frame request.
    virtual bool resume() { return true; }
    virtual bool capture(CapturedFrame* frame) = 0;
    // Dequeue and release a warm-up frame without copying its payload.
    virtual bool discard() {
        CapturedFrame frame;
        return capture(&frame);
    }
    virtual void close() = 0;
};

struct FrameCaptureManagerOptions {
    int recovery_initial_backoff_ms = 1000;
    int recovery_max_backoff_ms = 30000;
    // Use a slower cap until this open session has produced a valid frame.
    // Rebuilding an unavailable HDMI pipeline at the normal recovery cadence
    // wastes CPU while the board is otherwise idle.
    int recovery_idle_max_backoff_ms = 30000;
    int request_timeout_ms = 4000;
    int warmup_frames = 0;
    bool keep_streamon = false;
};

class FrameCaptureManager {
public:
    FrameCaptureManager(FrameCaptureSource* source,
                        FrameServiceServer* server,
                        const FrameCaptureManagerOptions& options);
    ~FrameCaptureManager();

    bool start();
    void stop();
    void request_restart();
    FrameServiceStatus capture(uint32_t timeout_ms, CapturedFrame* frame);
    bool is_running() const;

private:
    void run();
    void maybe_start_jpeg_warmup(const CapturedFrame& frame);
    void recover(int* backoff_ms,
                 int max_backoff_ms,
                 const char* error,
                 bool count_failure);
    int initial_backoff_ms() const {
        return options_.recovery_initial_backoff_ms > 0
            ? options_.recovery_initial_backoff_ms
            : 1;
    }

    FrameCaptureSource* source_;
    FrameServiceServer* server_;
    FrameCaptureManagerOptions options_;
    std::atomic<bool> running_;
    std::atomic<bool> restart_requested_;
    std::thread thread_;
    std::thread jpeg_warmup_thread_;
    std::atomic<bool> jpeg_warmup_running_;
    uint64_t received_requests_;
    uint64_t recovery_count_;
    bool have_jpeg_warmup_key_;
    uint32_t jpeg_warmup_width_;
    uint32_t jpeg_warmup_height_;
    std::mutex request_mutex_;
    std::mutex mutex_;
    std::condition_variable work_cv_;
    std::condition_variable capture_cv_;
    uint64_t requested_generation_;
    uint64_t completed_generation_;
    FrameServiceStatus completed_status_;
    CapturedFrame completed_frame_;
};

}  // namespace aico
