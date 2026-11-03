#pragma once

#include "audio_service_protocol.h"
#include <string>

namespace aico {

// Client SDK for audio_service.
// Each method opens a fresh UDS connection, sends a request, and reads the
// response — matching the one-shot request/response model used by frame_service.
//
// read_record_chunk() is the exception: it uses a longer socket timeout to
// support long-polling.
class AudioServiceClient {
public:
    explicit AudioServiceClient(const char* socket_path);

    // --- Recording ---

    AicoServiceStatus start_recording(const AudioFormat& fmt,
                                       RecordStartResult* out);

    // Long-poll for the next PCM chunk. timeout_ms controls how long the
    // server waits before returning TIMEOUT if no data is available.
    AicoServiceStatus read_record_chunk(uint64_t session_id,
                                         uint32_t timeout_ms,
                                         AudioChunkResult* out);

    AicoServiceStatus stop_recording(uint64_t session_id);

    // --- Playback ---

    AicoServiceStatus start_playback(const AudioFormat& fmt,
                                      PlaybackStartResult* out);

    // Push a PCM chunk to the playback session.
    // Set is_final=true on the last chunk; the server drains and closes the session.
    AicoServiceStatus write_play_chunk(uint64_t session_id,
                                        const uint8_t* data,
                                        size_t len,
                                        bool is_final);

    AicoServiceStatus stop_playback(uint64_t session_id);
    AicoServiceStatus set_playback_volume(uint32_t volume);
    AicoServiceStatus get_playback_volume(uint32_t* out);

    // --- Health ---

    AicoServiceStatus health(AudioHealthResult* out);

private:
    std::string socket_path_;
};

}  // namespace aico
