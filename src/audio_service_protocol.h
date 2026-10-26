#pragma once

#include "service_status.h"
#include <cstdint>
#include <string>
#include <vector>

namespace aico {

// -----------------------------------------------------------------------
// Wire types
// -----------------------------------------------------------------------

struct AudioFormat {
    uint32_t sample_rate = 16000;
    uint32_t channels    = 1;
    uint32_t bit_width   = 16;  // bits per sample
};

// -----------------------------------------------------------------------
// Request / response structs
// -----------------------------------------------------------------------

struct RecordStartResult {
    uint64_t session_id = 0;
};

struct AudioChunkResult {
    std::vector<uint8_t> pcm;
    bool end_of_stream = false;  // set when stop_recording was called server-side
};

struct PlaybackStartResult {
    uint64_t session_id = 0;
};

struct AudioHealthResult {
    bool recording_active  = false;
    bool playback_active   = false;
    uint32_t record_sessions   = 0;
    uint32_t playback_sessions = 0;
};

// -----------------------------------------------------------------------
// JSON encode / decode helpers used by server and client
// -----------------------------------------------------------------------

// Encode AudioFormat into a JSON object string (no surrounding braces needed;
// caller wraps it in the full request envelope).
std::string audio_format_to_json(const AudioFormat& fmt);
bool audio_format_from_json(const char* json, AudioFormat* out);

// Build a request envelope: {"op":"<op>", ...extra_fields}
// extra_fields is appended verbatim (without leading comma) if non-empty.
std::string audio_request_json(const char* op, const std::string& extra_fields = "");

// Build a response envelope: {"status":"<status>", ...extra_fields}
std::string audio_response_json(AicoServiceStatus status,
                                const std::string& extra_fields = "");

// Parse the "status" field from a response envelope.
AicoServiceStatus audio_response_status(const char* json);

// Parse a uint64 session_id field from a JSON object.
uint64_t audio_json_u64(const char* json, const char* key);

// Parse a bool field from a JSON object.
bool audio_json_bool(const char* json, const char* key);

// Parse a uint32 field from a JSON object.
uint32_t audio_json_u32(const char* json, const char* key);

}  // namespace aico
