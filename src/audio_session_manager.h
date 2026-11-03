#pragma once

#include "audio_playback_session.h"
#include "audio_record_session.h"
#include "audio_service_protocol.h"
#include <atomic>
#include <chrono>
#include <cstdint>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <unordered_map>

namespace aico {

// Owns all active record and playback sessions.
// Thread-safe; all public methods may be called from any thread.
class AudioSessionManager {
public:
    // Sessions idle longer than this are reaped automatically.
    static const uint32_t kSessionTimeoutMs = 30000;

    AudioSessionManager();
    explicit AudioSessionManager(const char* volume_state_path);
    ~AudioSessionManager();

    // --- Recording ---

    // Create and start a new record session.
    AicoServiceStatus start_recording(const AudioFormat& fmt,
                                       RecordStartResult* out);

    // Pop the next PCM chunk from a record session (long-poll).
    AicoServiceStatus read_record_chunk(uint64_t session_id,
                                         uint32_t timeout_ms,
                                         AudioChunkResult* out);

    // Stop a record session. Queued PCM remains readable until EOF, then the
    // session is removed lazily by read_record_chunk() or the idle reaper.
    AicoServiceStatus stop_recording(uint64_t session_id);

    // --- Playback ---

    // Create and start a new playback session.
    AicoServiceStatus start_playback(const AudioFormat& fmt,
                                      PlaybackStartResult* out);

    // Push a PCM chunk into a playback session.
    AicoServiceStatus write_play_chunk(uint64_t session_id,
                                        const uint8_t* data,
                                        size_t len,
                                        bool is_final);

    // Stop and remove a playback session.
    AicoServiceStatus stop_playback(uint64_t session_id);

    AicoServiceStatus set_playback_volume(int volume);
    AicoServiceStatus get_playback_volume(uint32_t* out) const;

    // --- Health ---

    void fill_health(AudioHealthResult* out) const;

private:
    using Clock = std::chrono::steady_clock;
    struct DrainingPlaybackState {
        std::mutex mutex;
        std::unordered_map<uint64_t, std::shared_ptr<AudioPlaybackSession>> sessions;
        std::atomic<uint32_t> count{0};
    };

    uint64_t next_session_id();
    bool persist_playback_volume_if_changed(int volume);
    void reaper_loop();
    void reap_idle_sessions();

    mutable std::mutex mutex_;
    std::mutex record_lifecycle_mutex_;  // serializes start_recording hardware access
    std::atomic<bool> stop_reaper_;
    std::thread reaper_thread_;
    std::shared_ptr<DrainingPlaybackState> draining_playback_state_;
    std::string volume_state_path_;
    std::mutex volume_set_mutex_;
    int playback_volume_;
    int last_persisted_playback_volume_;
    uint64_t next_id_;
    std::unordered_map<uint64_t, std::shared_ptr<AudioRecordSession>>   record_sessions_;
    std::unordered_map<uint64_t, std::shared_ptr<AudioPlaybackSession>> playback_sessions_;
    std::unordered_map<uint64_t, Clock::time_point> record_last_active_;
    std::unordered_map<uint64_t, Clock::time_point> playback_last_active_;
};

}  // namespace aico
