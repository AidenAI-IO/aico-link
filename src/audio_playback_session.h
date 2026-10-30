#pragma once

#include "aico_sdk.h"
#include "audio_service_protocol.h"
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <mutex>
#include <queue>
#include <thread>
#include <vector>

namespace aico {

inline std::chrono::milliseconds playback_tail_drain_grace(const AudioFormat& fmt,
                                                           size_t last_chunk_bytes) {
    static const uint64_t kMinGraceMs = 300;
    static const uint64_t kPaddingMs = 300;
    static const uint64_t kMaxGraceMs = 3000;
    static const uint64_t kAOFrameSamples = 1024;
    static const uint64_t kAOFrameCount = 4;

    const uint64_t bytes_per_sample = fmt.bit_width / 8;
    if (fmt.sample_rate == 0 || fmt.channels == 0 || bytes_per_sample == 0) {
        return std::chrono::milliseconds(kMinGraceMs);
    }

    const uint64_t bytes_per_second = static_cast<uint64_t>(fmt.sample_rate) *
                                      static_cast<uint64_t>(fmt.channels) *
                                      bytes_per_sample;
    if (bytes_per_second == 0) {
        return std::chrono::milliseconds(kMinGraceMs);
    }

    const uint64_t chunk_ms = (static_cast<uint64_t>(last_chunk_bytes) * 1000 +
                               bytes_per_second - 1) / bytes_per_second;
    const uint64_t frame_ms = (kAOFrameSamples * 1000 + fmt.sample_rate - 1) /
                              fmt.sample_rate;
    const uint64_t queued_frame_ms = chunk_ms > frame_ms ? chunk_ms : frame_ms;
    uint64_t grace_ms = queued_frame_ms * kAOFrameCount + kPaddingMs;
    if (grace_ms < kMinGraceMs) grace_ms = kMinGraceMs;
    if (grace_ms > kMaxGraceMs) grace_ms = kMaxGraceMs;
    return std::chrono::milliseconds(grace_ms);
}

inline size_t playback_queue_byte_limit(const AudioFormat& fmt) {
    const uint64_t bytes_per_sample = fmt.bit_width / 8;
    const uint64_t bytes_per_second = static_cast<uint64_t>(fmt.sample_rate) *
                                      static_cast<uint64_t>(fmt.channels) *
                                      bytes_per_sample;
    // Keep at most about one second of PCM queued. This is a latency bound,
    // rather than a chunk-count bound, so callers cannot bypass it with large
    // provider frames.
    if (bytes_per_second == 0) return 1;
    return bytes_per_second > static_cast<uint64_t>(SIZE_MAX)
               ? SIZE_MAX
               : static_cast<size_t>(bytes_per_second);
}

// Manages a single speaker playback session.
// Callers push PCM chunks via push_chunk(); the session drains them to
// AudioPlayer on a dedicated thread.
class AudioPlaybackSession {
public:
    static const size_t kMaxQueueChunks = 128;

    explicit AudioPlaybackSession(uint64_t session_id, const AudioFormat& fmt);
    ~AudioPlaybackSession();

    uint64_t id() const { return session_id_; }

    // Start the playback thread. Returns false if AudioPlayer init fails.
    bool start();

    // Enqueue a PCM chunk for playback.
    // is_final: when true, the playback thread drains remaining chunks then exits.
    AicoServiceStatus push_chunk(const uint8_t* data, size_t len, bool is_final);

    // Abort playback immediately.
    void stop();

    bool is_stopped() const { return stopped_.load(); }

    bool set_volume(int volume);
    int get_volume() const;

    // Approximate PCM bytes waiting in the application queue. The value is
    // intended for diagnostics and is protected by the session mutex.
    size_t queued_bytes() const;

    // Block until all queued audio has been played (or stop() is called).
    void wait_until_done();

private:
    void playback_loop();

    uint64_t session_id_;
    AudioFormat fmt_;
    AudioPlayer player_;
    // AudioPlayer and its Rockit state are not thread-safe; serialize every
    // hardware operation, including playback teardown and volume updates.
    mutable std::mutex player_mutex_;
    std::thread playback_thread_;
    mutable std::mutex mutex_;
    std::condition_variable cv_;
    std::queue<std::vector<uint8_t>> queue_;
    size_t queue_bytes_;
    const size_t max_queue_bytes_;
    std::atomic<bool> stopped_;
    bool final_received_;
    std::mutex join_mutex_;
    bool joined_;
};

}  // namespace aico
