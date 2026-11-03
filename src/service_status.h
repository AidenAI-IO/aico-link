#pragma once

namespace aico {

// Shared status codes for all Aico Link IPC services (frame_service, audio_service, ...).
// Each service maps its own domain errors onto these codes in its protocol layer.
enum class AicoServiceStatus {
    OK,
    NO_NEW_FRAME,       // frame_service: ring buffer has no newer frame than requested seq
    FRAME_NOT_FOUND,    // frame_service: requested seq not in ring buffer
    SESSION_NOT_FOUND,  // audio_service: session ID is unknown or already closed
    SERVICE_RECOVERING, // service is restarting; client should retry
    TIMEOUT,            // long-poll or connect timed out
    TRANSPORT_ERROR,    // socket read/write failure
    INTERNAL_ERROR,     // unexpected server-side failure
};

const char* service_status_to_string(AicoServiceStatus status);
bool service_status_from_string(const char* text, AicoServiceStatus* out);

}  // namespace aico
