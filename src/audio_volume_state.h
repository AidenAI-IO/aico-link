#pragma once

#include <string>

namespace aico {

static const char kDefaultAudioVolumeStatePath[] =
    "/userdata/audio_service/playback_volume";

enum class AudioVolumeStateLoadStatus {
    LOADED,
    MISSING,
    INVALID,
    ERROR,
};

AudioVolumeStateLoadStatus load_playback_volume_state(const char* path,
                                                      int* volume,
                                                      std::string* error = nullptr);

bool save_playback_volume_state(const char* path,
                                int volume,
                                std::string* error = nullptr);

}  // namespace aico
