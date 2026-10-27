#include "aico_sdk.h"
#include "audio_player_test_hooks.h"

#include <algorithm>
#include <atomic>
#include <condition_variable>
#include <cstddef>
#include <mutex>

namespace aico {

namespace {

const size_t kAudioPlayerOperationCount = 3;

size_t operation_index(test::AudioPlayerOperation operation) {
    return static_cast<size_t>(operation);
}

struct AudioPlayerTestState {
    std::mutex mutex;
    std::condition_variable cv;
    bool blocked[kAudioPlayerOperationCount] = {};
    size_t entries[kAudioPlayerOperationCount] = {};
    int active_operations = 0;
    int max_active_operations = 0;
};

AudioPlayerTestState& audio_player_test_state() {
    static AudioPlayerTestState state;
    return state;
}

class AudioPlayerOperationGuard {
public:
    explicit AudioPlayerOperationGuard(test::AudioPlayerOperation operation)
        : operation_(operation) {
        AudioPlayerTestState& state = audio_player_test_state();
        std::unique_lock<std::mutex> lock(state.mutex);
        const size_t index = operation_index(operation_);
        ++state.entries[index];
        ++state.active_operations;
        state.max_active_operations =
            std::max(state.max_active_operations, state.active_operations);
        state.cv.notify_all();
        state.cv.wait(lock, [&state, index] { return !state.blocked[index]; });
    }

    ~AudioPlayerOperationGuard() {
        AudioPlayerTestState& state = audio_player_test_state();
        std::lock_guard<std::mutex> lock(state.mutex);
        --state.active_operations;
        state.cv.notify_all();
    }

private:
    test::AudioPlayerOperation operation_;
};

}  // namespace

namespace test {

void reset_audio_player_test_state() {
    AudioPlayerTestState& state = audio_player_test_state();
    std::lock_guard<std::mutex> lock(state.mutex);
    for (size_t i = 0; i < kAudioPlayerOperationCount; ++i) {
        state.blocked[i] = false;
        state.entries[i] = 0;
    }
    state.active_operations = 0;
    state.max_active_operations = 0;
    state.cv.notify_all();
}

void block_audio_player_operation(AudioPlayerOperation operation) {
    AudioPlayerTestState& state = audio_player_test_state();
    std::lock_guard<std::mutex> lock(state.mutex);
    state.blocked[operation_index(operation)] = true;
}

bool wait_for_audio_player_operation(AudioPlayerOperation operation,
                                     std::chrono::milliseconds timeout) {
    AudioPlayerTestState& state = audio_player_test_state();
    std::unique_lock<std::mutex> lock(state.mutex);
    const size_t index = operation_index(operation);
    return state.cv.wait_for(lock, timeout, [&state, index] {
        return state.entries[index] > 0;
    });
}

void unblock_audio_player_operation(AudioPlayerOperation operation) {
    AudioPlayerTestState& state = audio_player_test_state();
    std::lock_guard<std::mutex> lock(state.mutex);
    state.blocked[operation_index(operation)] = false;
    state.cv.notify_all();
}

int max_concurrent_audio_player_operations() {
    AudioPlayerTestState& state = audio_player_test_state();
    std::lock_guard<std::mutex> lock(state.mutex);
    return state.max_active_operations;
}

}  // namespace test

class AudioCaptureImpl {};
class AudioPlayerImpl {
public:
    std::atomic<int> volume{100};
    std::atomic<bool> initialized{false};
};

AudioCapture::AudioCapture() : impl_(new AudioCaptureImpl()) {}
AudioCapture::~AudioCapture() {}

bool AudioCapture::init(const AudioConfig&) { return true; }
bool AudioCapture::start(AudioStreamCallback) { return true; }
void AudioCapture::stop() {}
bool AudioCapture::get_frame(AudioFrame&) { return false; }
void AudioCapture::release_frame() {}
bool AudioCapture::is_running() const { return false; }

AudioPlayer::AudioPlayer() : impl_(new AudioPlayerImpl()) {}
AudioPlayer::~AudioPlayer() {}

bool AudioPlayer::init(const AudioConfig&) {
    impl_->initialized.store(true);
    return true;
}

bool AudioPlayer::play(const void*, uint32_t) {
    AudioPlayerOperationGuard guard(test::AudioPlayerOperation::PLAY);
    return impl_->initialized.load();
}
bool AudioPlayer::play(const AudioFrame& frame) { return play(frame.data, frame.length); }
void AudioPlayer::stop() {
    AudioPlayerOperationGuard guard(test::AudioPlayerOperation::STOP);
    impl_->initialized.store(false);
}
void AudioPlayer::pause() {}
void AudioPlayer::resume() {}

bool AudioPlayer::set_volume(int volume) {
    AudioPlayerOperationGuard guard(test::AudioPlayerOperation::SET_VOLUME);
    if (!impl_->initialized.load()) return false;
    impl_->volume.store(volume);
    return true;
}

int AudioPlayer::get_volume() const { return impl_->volume.load(); }
bool AudioPlayer::is_initialized() const { return impl_->initialized.load(); }

}  // namespace aico
