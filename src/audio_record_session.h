#pragma once

#include "aico_sdk.h"
#include "audio_service_protocol.h"
#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <mutex>
#include <queue>
#include <thread>
#include <vector>

namespace aico {

// Manages a single microphone recording session.
// The session captures PCM from AudioCapture and buffers it in a bounded
// queue so the server can drain it via pop_chunk().
class AudioRecordSession {
public:
    // Frame-count bound; buffered duration varies with the captured frame shape
    // and any sample-rate conversion performed before enqueueing.
    static const size_t kMaxQueueChunks = 512;

    explicit AudioRecordSession(uint64_t session_id, const AudioFormat& fmt);
    ~AudioRecordSession();

    uint64_t id() const { return session_id_; }

    // Start capturing. Returns false if AudioCapture init fails.
    bool start();

    // Block until a PCM chunk is available or timeout_ms elapses.
    // Returns OK with data, TIMEOUT if no data arrived, or OK with
    // end_of_stream=true after stop once the queue has drained.
    AicoServiceStatus pop_chunk(uint32_t timeout_ms, AudioChunkResult* out);

    // Signal the session to stop. Unblocks any pending pop_chunk().
    void stop();

    // Wait for the capture thread to exit. Must call stop() first.
    void join();

    bool is_stopped() const { return stopped_.load(); }

private:
    void capture_loop();
    AudioConfig hardware_capture_config() const;
    void maybe_update_hw_sample_rate(uint64_t timestamp_us, size_t frame_samples_per_channel);

    uint64_t session_id_;
    AudioFormat fmt_;
    int hw_sample_rate_ = 32000;  // actual hardware sample rate
    int hw_channels_    = 2;      // actual hardware channel count
    uint64_t prev_timestamp_us_ = 0;
    AudioCapture capture_;
    std::thread capture_thread_;
    std::mutex mutex_;
    std::condition_variable cv_;
    std::queue<std::vector<uint8_t>> queue_;
    std::atomic<bool> stopped_;
};

}  // namespace aico
