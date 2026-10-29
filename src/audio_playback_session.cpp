#include "audio_playback_session.h"
#include "aico_log.h"
#include <chrono>
#include <stdio.h>
#include <thread>

namespace aico {

const size_t AudioPlaybackSession::kMaxQueueChunks;

AudioPlaybackSession::AudioPlaybackSession(uint64_t session_id, const AudioFormat& fmt)
    : session_id_(session_id),
      fmt_(fmt),
      queue_bytes_(0),
      max_queue_bytes_(playback_queue_byte_limit(fmt)),
      stopped_(false),
      final_received_(false),
      joined_(false) {}

AudioPlaybackSession::~AudioPlaybackSession() {
    stop();
}

bool AudioPlaybackSession::start() {
    AudioConfig cfg;
    cfg.sample_rate = static_cast<int>(fmt_.sample_rate);
    cfg.channels    = static_cast<int>(fmt_.channels);
    cfg.bit_width   = static_cast<int>(fmt_.bit_width);

    {
        std::lock_guard<std::mutex> player_lock(player_mutex_);
        if (!player_.init(cfg)) {
            AICO_LOG_ERROR("playback", "player_init_failed", "session_id=%llu",
                            static_cast<unsigned long long>(session_id_));
            return false;
        }
    }

    playback_thread_ = std::thread(&AudioPlaybackSession::playback_loop, this);
    return true;
}

void AudioPlaybackSession::stop() {
    if (stopped_.exchange(true)) return;
    {
        // Publish the stop through mutex_ before notifying. playback_loop()
        // evaluates its wait predicate while holding mutex_, and only releases
        // it atomically once it is enqueued on cv_. Without this barrier a
        // notify_all() can land after the predicate read but before the
        // enqueue, where it is dropped -- leaving the loop asleep forever and
        // the join() below blocked for good.
        std::lock_guard<std::mutex> lock(mutex_);
    }
    cv_.notify_all();
    std::lock_guard<std::mutex> lock(join_mutex_);
    if (!joined_ && playback_thread_.joinable()) {
        playback_thread_.join();
        joined_ = true;
    }
}

bool AudioPlaybackSession::set_volume(int volume) {
    if (stopped_.load()) return false;
    std::lock_guard<std::mutex> player_lock(player_mutex_);
    if (stopped_.load()) return false;
    return player_.set_volume(volume);
}

int AudioPlaybackSession::get_volume() const {
    std::lock_guard<std::mutex> player_lock(player_mutex_);
    return player_.get_volume();
}

size_t AudioPlaybackSession::queued_bytes() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return queue_bytes_;
}

AicoServiceStatus AudioPlaybackSession::push_chunk(const uint8_t* data, size_t len,
                                                    bool is_final) {
    if (stopped_.load()) return AicoServiceStatus::SESSION_NOT_FOUND;

    std::unique_lock<std::mutex> lock(mutex_);
    if (stopped_.load() || final_received_) return AicoServiceStatus::SESSION_NOT_FOUND;

    if (len > max_queue_bytes_) return AicoServiceStatus::TIMEOUT;
    while (queue_.size() >= kMaxQueueChunks ||
           queue_bytes_ > max_queue_bytes_ - len) {
        // Apply back-pressure without ever allowing a producer to cross the
        // hard queue bound. A timeout is reported explicitly so callers can
        // account for a saturated playback path instead of silently growing
        // the queue.
        const bool woke = cv_.wait_for(
            lock, std::chrono::milliseconds(200),
            [this, len] {
                return (queue_.size() < kMaxQueueChunks &&
                        queue_bytes_ <= max_queue_bytes_ - len) ||
                       final_received_ || stopped_.load();
            });
        if (final_received_ || stopped_.load()) {
            return AicoServiceStatus::SESSION_NOT_FOUND;
        }
        if (!woke && (queue_.size() >= kMaxQueueChunks ||
                      queue_bytes_ > max_queue_bytes_ - len)) {
            return AicoServiceStatus::TIMEOUT;
        }
    }

    if (data && len > 0) {
        queue_.push(std::vector<uint8_t>(data, data + len));
        queue_bytes_ += len;
    }
    if (is_final) final_received_ = true;
    if (is_final) {
        cv_.notify_all();
    } else {
        cv_.notify_one();
    }
    return AicoServiceStatus::OK;
}

void AudioPlaybackSession::wait_until_done() {
    std::lock_guard<std::mutex> lock(join_mutex_);
    if (!joined_ && playback_thread_.joinable()) {
        playback_thread_.join();
        joined_ = true;
    }
}

void AudioPlaybackSession::playback_loop() {
    size_t last_played_chunk_bytes = 0;
    while (!stopped_.load()) {
        std::vector<uint8_t> chunk;
        bool final_and_drained = false;
        {
            std::unique_lock<std::mutex> lock(mutex_);
            cv_.wait(lock, [this] {
                return !queue_.empty() || final_received_ || stopped_.load();
            });

            if (stopped_.load()) {
                AICO_LOG_INFO("playback", "interrupted_before_drain", "session_id=%llu",
                               static_cast<unsigned long long>(session_id_));
                std::lock_guard<std::mutex> player_lock(player_mutex_);
                player_.stop();
                return;
            }

            if (!queue_.empty()) {
                chunk = std::move(queue_.front());
                queue_.pop();
                queue_bytes_ -= chunk.size();
                cv_.notify_one();  // wake push_chunk back-pressure waiter
            } else if (final_received_) {
                // Queue drained and final chunk received — no more PCM writes are
                // expected; allow AO buffer to drain before session teardown.
                final_and_drained = true;
            }
        }

        if (final_and_drained) {
            std::unique_lock<std::mutex> lock(mutex_);
            cv_.wait_for(lock,
                         playback_tail_drain_grace(fmt_, last_played_chunk_bytes),
                         [this] { return stopped_.load(); });
            if (stopped_.load()) {
                AICO_LOG_INFO("playback", "interrupted_during_final_drain", "session_id=%llu",
                               static_cast<unsigned long long>(session_id_));
                std::lock_guard<std::mutex> player_lock(player_mutex_);
                player_.stop();
                return;
            }
            AICO_LOG_INFO("playback", "drain_completed", "session_id=%llu",
                           static_cast<unsigned long long>(session_id_));
            break;
        }

        if (!chunk.empty()) {
            while (!stopped_.load()) {
                std::lock_guard<std::mutex> player_lock(player_mutex_);
                if (stopped_.load()) break;
                if (player_.play(chunk.data(), static_cast<uint32_t>(chunk.size()))) {
                    last_played_chunk_bytes = chunk.size();
                    break;
                }
                // SendFrame uses 100ms timeout; retry until stopped or success.
            }
        }
    }

    if (stopped_.load()) {
        AICO_LOG_INFO("playback", "interrupted", "session_id=%llu",
                       static_cast<unsigned long long>(session_id_));
    }

    // Tear down hardware from the same thread that called SendFrame — safe.
    {
        std::lock_guard<std::mutex> player_lock(player_mutex_);
        player_.stop();
    }
}

}  // namespace aico
