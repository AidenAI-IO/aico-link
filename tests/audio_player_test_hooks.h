#pragma once

#include <chrono>

namespace aico {
namespace test {

enum class AudioPlayerOperation {
    PLAY,
    STOP,
    SET_VOLUME,
};

void reset_audio_player_test_state();
void block_audio_player_operation(AudioPlayerOperation operation);
bool wait_for_audio_player_operation(AudioPlayerOperation operation,
                                     std::chrono::milliseconds timeout);
void unblock_audio_player_operation(AudioPlayerOperation operation);
int max_concurrent_audio_player_operations();

}  // namespace test
}  // namespace aico
