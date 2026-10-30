#define private public
#include "audio_session_manager.h"
#undef private

#include "doctest.h"

#include <chrono>
#include <future>
#include <thread>

TEST_CASE("AudioRecordSession captures at 16 kHz for a 24 kHz target") {
    aico::AudioFormat fmt;
    fmt.sample_rate = 24000;
    fmt.channels = 1;
    fmt.bit_width = 16;

    aico::AudioRecordSession session(1, fmt);
    REQUIRE(session.start());
    CHECK(session.hw_sample_rate_ == 16000);
    session.stop();
    session.join();
}

TEST_CASE("AudioRecordSession waits for capture tail before reporting EOF") {
    aico::AudioFormat fmt;
    aico::AudioRecordSession session(2, fmt);
    session.capture_done_.store(false);
    session.stop();
    aico::AudioChunkResult chunk;
    CHECK(session.pop_chunk(1, &chunk) == aico::AicoServiceStatus::TIMEOUT);
    session.enqueue_pcm({1, 2, 3, 4});
    session.capture_done_.store(true);
    REQUIRE(session.pop_chunk(1, &chunk) == aico::AicoServiceStatus::OK);
    CHECK_FALSE(chunk.end_of_stream);
    CHECK(chunk.pcm == std::vector<uint8_t>{1, 2, 3, 4});
    REQUIRE(session.pop_chunk(1, &chunk) == aico::AicoServiceStatus::OK);
    CHECK(chunk.end_of_stream);
}

TEST_CASE("AudioSessionManager keeps a stopped record session readable until EOF") {
    aico::AudioSessionManager manager;

    aico::AudioFormat fmt;
    fmt.sample_rate = 16000;
    fmt.channels = 1;
    fmt.bit_width = 16;

    const uint64_t session_id = 42;
    auto session = std::make_shared<aico::AudioRecordSession>(session_id, fmt);
    {
        std::lock_guard<std::mutex> lock(session->mutex_);
        session->queue_.push(std::vector<uint8_t>{1, 2, 3, 4});
    }
    session->cv_.notify_all();

    {
        std::lock_guard<std::mutex> lock(manager.mutex_);
        manager.record_sessions_[session_id] = session;
        manager.record_last_active_[session_id] = aico::AudioSessionManager::Clock::now();
    }

    CHECK(manager.stop_recording(session_id) == aico::AicoServiceStatus::OK);

    aico::AudioChunkResult chunk;
    CHECK(manager.read_record_chunk(session_id, 1, &chunk) == aico::AicoServiceStatus::OK);
    CHECK(chunk.end_of_stream == false);
    CHECK(chunk.pcm == std::vector<uint8_t>{1, 2, 3, 4});

    aico::AudioChunkResult end;
    CHECK(manager.read_record_chunk(session_id, 1, &end) == aico::AicoServiceStatus::OK);
    CHECK(end.end_of_stream == true);
    CHECK(end.pcm.empty());

    aico::AudioChunkResult missing;
    CHECK(manager.read_record_chunk(session_id, 1, &missing) ==
          aico::AicoServiceStatus::SESSION_NOT_FOUND);
}

TEST_CASE("AudioSessionManager rejects new playback while another session is draining") {
    aico::AudioSessionManager manager;

    aico::AudioFormat fmt;
    fmt.sample_rate = 16000;
    fmt.channels = 1;
    fmt.bit_width = 16;

    manager.draining_playback_state_->count.store(1, std::memory_order_relaxed);

    aico::PlaybackStartResult out;
    CHECK(manager.start_playback(fmt, &out) == aico::AicoServiceStatus::SERVICE_RECOVERING);
}

TEST_CASE("AudioSessionManager stops draining playback sessions") {
    aico::AudioSessionManager manager;

    aico::AudioFormat fmt;
    fmt.sample_rate = 16000;
    fmt.channels = 1;
    fmt.bit_width = 16;

    const uint64_t session_id = 99;
    auto session = std::make_shared<aico::AudioPlaybackSession>(session_id, fmt);

    {
        std::lock_guard<std::mutex> lock(manager.draining_playback_state_->mutex);
        manager.draining_playback_state_->sessions[session_id] = session;
    }

    CHECK(manager.stop_playback(session_id) == aico::AicoServiceStatus::OK);
    CHECK(session->is_stopped());
    CHECK(manager.stop_playback(session_id) == aico::AicoServiceStatus::SESSION_NOT_FOUND);
}

TEST_CASE("AudioSessionManager publishes draining before manager operations resume") {
    aico::AudioSessionManager manager;

    aico::AudioFormat fmt;
    fmt.sample_rate = 16000;
    fmt.channels = 1;
    fmt.bit_width = 16;

    const uint64_t session_id = 100;
    auto session = std::make_shared<aico::AudioPlaybackSession>(session_id, fmt);
    REQUIRE(session->start());
    {
        std::lock_guard<std::mutex> lock(manager.mutex_);
        manager.playback_sessions_[session_id] = session;
        manager.playback_last_active_[session_id] =
            aico::AudioSessionManager::Clock::now();
    }

    // Hold the draining lock so the migration must wait at its publication
    // point. The manager lock must remain held until that publication is done.
    std::unique_lock<std::mutex> draining_lock(manager.draining_playback_state_->mutex);
    const uint8_t chunk[] = {1, 2, 3, 4};
    auto final_future = std::async(std::launch::async, [&manager, session_id, &chunk]() {
        return manager.write_play_chunk(session_id, chunk, sizeof(chunk), true);
    });

    bool final_received = false;
    const auto final_deadline = std::chrono::steady_clock::now() +
                                std::chrono::seconds(1);
    while (std::chrono::steady_clock::now() < final_deadline) {
        {
            std::lock_guard<std::mutex> lock(session->mutex_);
            final_received = session->final_received_;
        }
        if (final_received) break;
        std::this_thread::yield();
    }
    CHECK(final_received);
    if (!final_received) {
        draining_lock.unlock();
        final_future.wait();
        return;
    }

    // Once the migration starts, the manager lock must stay held while the
    // draining lock is unavailable. The old ordering released it first.
    bool migration_holds_manager = false;
    const auto migration_deadline = std::chrono::steady_clock::now() +
                                    std::chrono::seconds(1);
    while (std::chrono::steady_clock::now() < migration_deadline) {
        if (manager.mutex_.try_lock()) {
            manager.mutex_.unlock();
            std::this_thread::yield();
            continue;
        }

        migration_holds_manager = true;
        for (int i = 0; i < 10; ++i) {
            std::this_thread::sleep_for(std::chrono::milliseconds(1));
            if (manager.mutex_.try_lock()) {
                manager.mutex_.unlock();
                migration_holds_manager = false;
                break;
            }
        }
        if (migration_holds_manager) break;
    }
    CHECK(migration_holds_manager);
    if (!migration_holds_manager) {
        draining_lock.unlock();
        final_future.wait();
        return;
    }

    SUBCASE("start observes the draining session") {
        auto start_future = std::async(std::launch::async, [&manager, &fmt]() {
            aico::PlaybackStartResult out;
            return manager.start_playback(fmt, &out);
        });
        CHECK(start_future.wait_for(std::chrono::milliseconds(50)) ==
              std::future_status::timeout);

        draining_lock.unlock();
        REQUIRE(final_future.wait_for(std::chrono::seconds(2)) == std::future_status::ready);
        CHECK(final_future.get() == aico::AicoServiceStatus::OK);
        REQUIRE(start_future.wait_for(std::chrono::seconds(2)) == std::future_status::ready);
        CHECK(start_future.get() == aico::AicoServiceStatus::SERVICE_RECOVERING);
        CHECK(manager.stop_playback(session_id) == aico::AicoServiceStatus::OK);
    }

    SUBCASE("stop finds the draining session") {
        auto stop_future = std::async(std::launch::async, [&manager, session_id]() {
            return manager.stop_playback(session_id);
        });
        CHECK(stop_future.wait_for(std::chrono::milliseconds(50)) ==
              std::future_status::timeout);

        draining_lock.unlock();
        REQUIRE(final_future.wait_for(std::chrono::seconds(2)) == std::future_status::ready);
        CHECK(final_future.get() == aico::AicoServiceStatus::OK);
        REQUIRE(stop_future.wait_for(std::chrono::seconds(2)) == std::future_status::ready);
        CHECK(stop_future.get() == aico::AicoServiceStatus::OK);
    }
}
