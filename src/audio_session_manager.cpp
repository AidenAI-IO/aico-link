#include "audio_session_manager.h"
#include "audio_volume_state.h"
#include "aico_log.h"
#include <chrono>
#include <stdio.h>
#include <vector>

namespace aico {

AudioSessionManager::AudioSessionManager()
    : AudioSessionManager(nullptr) {}

AudioSessionManager::AudioSessionManager(const char* volume_state_path)
    : stop_reaper_(false),
      draining_playback_state_(std::make_shared<DrainingPlaybackState>()),
      volume_state_path_(volume_state_path ? volume_state_path : ""),
      playback_volume_(100),
      last_persisted_playback_volume_(-1),
      next_id_(1) {
    if (!volume_state_path_.empty()) {
        int persisted_volume = 100;
        std::string error;
        AudioVolumeStateLoadStatus load_status =
            load_playback_volume_state(volume_state_path_.c_str(), &persisted_volume, &error);
        if (load_status == AudioVolumeStateLoadStatus::LOADED) {
            playback_volume_ = persisted_volume;
            last_persisted_playback_volume_ = persisted_volume;
            AICO_LOG_INFO("volume", "state_loaded", "volume=%d path=%s",
                           playback_volume_, volume_state_path_.c_str());
        } else if (load_status == AudioVolumeStateLoadStatus::MISSING) {
            last_persisted_playback_volume_ = playback_volume_;
        } else if (load_status == AudioVolumeStateLoadStatus::INVALID ||
                   load_status == AudioVolumeStateLoadStatus::ERROR) {
            AICO_LOG_WARN("volume", "state_ignored", "path=%s error=%s",
                           volume_state_path_.c_str(), error.c_str());
        }
    }
    reaper_thread_ = std::thread([this]() { reaper_loop(); });
}

AudioSessionManager::~AudioSessionManager() {
    stop_reaper_.store(true);
    if (reaper_thread_.joinable()) {
        reaper_thread_.join();
    }

    // Sessions own their threads; destructors join them.
    std::lock_guard<std::mutex> lock(mutex_);
    record_sessions_.clear();
    playback_sessions_.clear();
    record_last_active_.clear();
    playback_last_active_.clear();
}

uint64_t AudioSessionManager::next_session_id() {
    return next_id_++;
}

bool AudioSessionManager::persist_playback_volume_if_changed(int volume) {
    if (volume_state_path_.empty() || volume == last_persisted_playback_volume_) {
        return true;
    }

    std::string error;
    if (!save_playback_volume_state(volume_state_path_.c_str(), volume, &error)) {
        AICO_LOG_ERROR("volume", "state_persist_failed", "path=%s error=%s",
                        volume_state_path_.c_str(), error.c_str());
        return false;
    }

    last_persisted_playback_volume_ = volume;
    return true;
}

// -----------------------------------------------------------------------
// Recording
// -----------------------------------------------------------------------

AicoServiceStatus AudioSessionManager::start_recording(const AudioFormat& fmt,
                                                         RecordStartResult* out) {
    // Serialize the entire teardown+start sequence so two concurrent
    // start_recording() calls cannot overlap hardware access.
    std::lock_guard<std::mutex> lifecycle_lock(record_lifecycle_mutex_);

    const Clock::time_point now = Clock::now();
    std::vector<std::pair<uint64_t, std::shared_ptr<AudioRecordSession>>> stale_records;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        for (auto it = record_sessions_.begin(); it != record_sessions_.end(); ++it) {
            stale_records.push_back(std::make_pair(it->first, it->second));
        }
        record_sessions_.clear();
        record_last_active_.clear();
    }
    for (size_t i = 0; i < stale_records.size(); ++i) {
        stale_records[i].second->stop();
        stale_records[i].second->join();
        AICO_LOG_INFO("recording", "session_replaced", "session_id=%llu",
                       static_cast<unsigned long long>(stale_records[i].first));
    }

    uint64_t id;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        id = next_session_id();
    }
    auto session = std::make_shared<AudioRecordSession>(id, fmt);
    if (!session->start()) {
        return AicoServiceStatus::INTERNAL_ERROR;
    }
    out->session_id = id;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        record_sessions_[id] = std::move(session);
        record_last_active_[id] = now;
    }
    AICO_LOG_INFO("recording", "session_started", "session_id=%llu",
                   static_cast<unsigned long long>(id));
    return AicoServiceStatus::OK;
}

AicoServiceStatus AudioSessionManager::read_record_chunk(uint64_t session_id,
                                                           uint32_t timeout_ms,
                                                           AudioChunkResult* out) {
    reap_idle_sessions();

    std::shared_ptr<AudioRecordSession> session;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        const Clock::time_point now = Clock::now();
        auto it = record_sessions_.find(session_id);
        if (it == record_sessions_.end()) {
            return AicoServiceStatus::SESSION_NOT_FOUND;
        }
        session = it->second;
        record_last_active_[session_id] = now;
    }
    // pop_chunk blocks without holding the manager lock.
    AicoServiceStatus status = session->pop_chunk(timeout_ms, out);

    // Clean up stopped sessions lazily.
    if (out->end_of_stream) {
        std::lock_guard<std::mutex> lock(mutex_);
        record_sessions_.erase(session_id);
        record_last_active_.erase(session_id);
    }
    return status;
}

AicoServiceStatus AudioSessionManager::stop_recording(uint64_t session_id) {
    std::shared_ptr<AudioRecordSession> session;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        auto it = record_sessions_.find(session_id);
        if (it == record_sessions_.end()) {
            return AicoServiceStatus::SESSION_NOT_FOUND;
        }
        session = it->second;
        record_last_active_[session_id] = Clock::now();
    }
    session->stop();
    AICO_LOG_INFO("recording", "session_stopped", "session_id=%llu",
                   static_cast<unsigned long long>(session_id));
    return AicoServiceStatus::OK;
}

// -----------------------------------------------------------------------
// Playback
// -----------------------------------------------------------------------

AicoServiceStatus AudioSessionManager::start_playback(const AudioFormat& fmt,
                                                         PlaybackStartResult* out) {
    const Clock::time_point now = Clock::now();
    std::lock_guard<std::mutex> lock(mutex_);
    if (!playback_sessions_.empty() ||
        draining_playback_state_->count.load(std::memory_order_acquire) > 0) {
        AICO_LOG_WARN("playback", "session_start_rejected", "reason=active_or_draining");
        return AicoServiceStatus::SERVICE_RECOVERING;
    }
    uint64_t id = next_session_id();
    auto session = std::make_shared<AudioPlaybackSession>(id, fmt);
    if (!session->start()) {
        return AicoServiceStatus::INTERNAL_ERROR;
    }
    if (!session->set_volume(playback_volume_)) {
        session->stop();
        return AicoServiceStatus::INTERNAL_ERROR;
    }
    out->session_id = id;
    playback_sessions_[id] = std::move(session);
    playback_last_active_[id] = now;
    AICO_LOG_INFO("playback", "session_started", "session_id=%llu volume=%d",
                   static_cast<unsigned long long>(id), playback_volume_);
    return AicoServiceStatus::OK;
}

AicoServiceStatus AudioSessionManager::write_play_chunk(uint64_t session_id,
                                                          const uint8_t* data,
                                                          size_t len,
                                                          bool is_final) {
    reap_idle_sessions();

    std::shared_ptr<AudioPlaybackSession> session;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        const Clock::time_point now = Clock::now();
        auto it = playback_sessions_.find(session_id);
        if (it == playback_sessions_.end()) {
            return AicoServiceStatus::SESSION_NOT_FOUND;
        }
        session = it->second;
        playback_last_active_[session_id] = now;
    }
    AicoServiceStatus status = session->push_chunk(data, len, is_final);

    if (is_final && status == AicoServiceStatus::OK) {
        // Move the session into a draining set so no further writes are
        // accepted, but stop_playback() can still interrupt the tail drain.
        std::shared_ptr<AudioPlaybackSession> owned;
        const std::shared_ptr<DrainingPlaybackState> draining_state =
            draining_playback_state_;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            auto it = playback_sessions_.find(session_id);
            if (it != playback_sessions_.end()) {
                owned = it->second;
                // Publish the draining state before removing the active
                // entry. start_playback() and stop_playback() both serialize
                // on mutex_, so they cannot observe a partially moved session.
                std::lock_guard<std::mutex> draining_lock(draining_state->mutex);
                draining_state->sessions[session_id] = owned;
                draining_state->count.fetch_add(1, std::memory_order_release);
                playback_sessions_.erase(it);
            }
            playback_last_active_.erase(session_id);
        }
        if (owned && draining_state) {
            // Keep the draining session alive on a detached thread until the
            // playback thread exits or stop_playback() interrupts it.
            std::thread([session_id, owned, draining_state]() {
                owned->wait_until_done();
                owned->stop();
                {
                    std::lock_guard<std::mutex> lock(draining_state->mutex);
                    draining_state->sessions.erase(session_id);
                }
                draining_state->count.fetch_sub(1, std::memory_order_release);
            }).detach();
        }
    }
    return status;
}

AicoServiceStatus AudioSessionManager::stop_playback(uint64_t session_id) {
    std::shared_ptr<AudioPlaybackSession> session;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        auto it = playback_sessions_.find(session_id);
        if (it != playback_sessions_.end()) {
            session = it->second;
            playback_sessions_.erase(it);
            playback_last_active_.erase(session_id);
        } else {
            std::lock_guard<std::mutex> draining_lock(draining_playback_state_->mutex);
            auto draining_it = draining_playback_state_->sessions.find(session_id);
            if (draining_it == draining_playback_state_->sessions.end()) {
                return AicoServiceStatus::SESSION_NOT_FOUND;
            }
            session = draining_it->second;
            draining_playback_state_->sessions.erase(draining_it);
        }
    }
    session->stop();
    AICO_LOG_INFO("playback", "session_stopped", "session_id=%llu",
                   static_cast<unsigned long long>(session_id));
    return AicoServiceStatus::OK;
}

AicoServiceStatus AudioSessionManager::set_playback_volume(int volume) {
    if (volume < 0 || volume > 100) {
        return AicoServiceStatus::INTERNAL_ERROR;
    }

    std::lock_guard<std::mutex> volume_lock(volume_set_mutex_);
    std::vector<std::shared_ptr<AudioPlaybackSession>> sessions;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        playback_volume_ = volume;
        for (auto it = playback_sessions_.begin(); it != playback_sessions_.end(); ++it) {
            sessions.push_back(it->second);
        }
        {
            std::lock_guard<std::mutex> draining_lock(draining_playback_state_->mutex);
            for (auto it = draining_playback_state_->sessions.begin(); it != draining_playback_state_->sessions.end(); ++it) {
                sessions.push_back(it->second);
            }
        }
    }

    for (size_t i = 0; i < sessions.size(); ++i) {
        if (!sessions[i]->set_volume(volume)) {
            return AicoServiceStatus::INTERNAL_ERROR;
        }
    }
    if (!persist_playback_volume_if_changed(volume)) {
        return AicoServiceStatus::INTERNAL_ERROR;
    }
    AICO_LOG_INFO("volume", "updated", "volume=%d", volume);
    return AicoServiceStatus::OK;
}

AicoServiceStatus AudioSessionManager::get_playback_volume(uint32_t* out) const {
    if (!out) return AicoServiceStatus::INTERNAL_ERROR;
    std::lock_guard<std::mutex> lock(mutex_);
    *out = static_cast<uint32_t>(playback_volume_);
    return AicoServiceStatus::OK;
}

// -----------------------------------------------------------------------
// Health
// -----------------------------------------------------------------------

void AudioSessionManager::fill_health(AudioHealthResult* out) const {
    std::lock_guard<std::mutex> lock(mutex_);
    const uint32_t draining = draining_playback_state_->count.load(std::memory_order_acquire);
    out->record_sessions   = static_cast<uint32_t>(record_sessions_.size());
    out->playback_sessions = static_cast<uint32_t>(playback_sessions_.size()) + draining;
    out->recording_active  = !record_sessions_.empty();
    out->playback_active   = out->playback_sessions > 0;
}

void AudioSessionManager::reaper_loop() {
    while (!stop_reaper_.load()) {
        std::this_thread::sleep_for(std::chrono::seconds(1));
        reap_idle_sessions();
    }
}

void AudioSessionManager::reap_idle_sessions() {
	const Clock::time_point now = Clock::now();
	const std::chrono::milliseconds timeout(kSessionTimeoutMs);
	std::vector<std::pair<uint64_t, std::shared_ptr<AudioRecordSession>>> expired_records;
	std::vector<std::pair<uint64_t, std::shared_ptr<AudioPlaybackSession>>> expired_playbacks;

	{
		std::lock_guard<std::mutex> lock(mutex_);
		for (auto it = record_sessions_.begin(); it != record_sessions_.end();) {
			auto at = record_last_active_.find(it->first);
			if (at == record_last_active_.end()) {
				record_last_active_[it->first] = now;
				++it;
				continue;
			}
			if (now - at->second >= timeout) {
				expired_records.push_back(std::make_pair(it->first, it->second));
				record_last_active_.erase(at);
				it = record_sessions_.erase(it);
			} else {
				++it;
			}
		}
		for (auto it = playback_sessions_.begin(); it != playback_sessions_.end();) {
			auto at = playback_last_active_.find(it->first);
			if (at == playback_last_active_.end()) {
				playback_last_active_[it->first] = now;
                ++it;
                continue;
            }
            if (now - at->second >= timeout) {
                expired_playbacks.push_back(std::make_pair(it->first, it->second));
                playback_last_active_.erase(at);
                it = playback_sessions_.erase(it);
            } else {
                ++it;
            }
		}
	}

	for (size_t i = 0; i < expired_records.size(); ++i) {
		expired_records[i].second->stop();
		AICO_LOG_INFO("recording", "idle_session_reaped", "session_id=%llu",
		               static_cast<unsigned long long>(expired_records[i].first));
	}
	for (size_t i = 0; i < expired_playbacks.size(); ++i) {
		expired_playbacks[i].second->stop();
		AICO_LOG_INFO("playback", "idle_session_reaped", "session_id=%llu",
                       static_cast<unsigned long long>(expired_playbacks[i].first));
    }
}

}  // namespace aico
