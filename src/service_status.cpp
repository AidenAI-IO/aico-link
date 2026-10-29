#include "service_status.h"
#include <string.h>

namespace aico {

const char* service_status_to_string(AicoServiceStatus status) {
    switch (status) {
        case AicoServiceStatus::OK:                return "OK";
        case AicoServiceStatus::NO_NEW_FRAME:      return "NO_NEW_FRAME";
        case AicoServiceStatus::FRAME_NOT_FOUND:   return "FRAME_NOT_FOUND";
        case AicoServiceStatus::SESSION_NOT_FOUND: return "SESSION_NOT_FOUND";
        case AicoServiceStatus::SERVICE_RECOVERING:return "SERVICE_RECOVERING";
        case AicoServiceStatus::TIMEOUT:           return "TIMEOUT";
        case AicoServiceStatus::TRANSPORT_ERROR:   return "TRANSPORT_ERROR";
        case AicoServiceStatus::INTERNAL_ERROR:    return "INTERNAL_ERROR";
    }
    return "INTERNAL_ERROR";
}

bool service_status_from_string(const char* text, AicoServiceStatus* out) {
    if (!text || !out) return false;

    struct Mapping {
        const char* text;
        AicoServiceStatus status;
    };
    static const Mapping kMappings[] = {
        {"OK",                AicoServiceStatus::OK},
        {"NO_NEW_FRAME",      AicoServiceStatus::NO_NEW_FRAME},
        {"FRAME_NOT_FOUND",   AicoServiceStatus::FRAME_NOT_FOUND},
        {"SESSION_NOT_FOUND", AicoServiceStatus::SESSION_NOT_FOUND},
        {"SERVICE_RECOVERING",AicoServiceStatus::SERVICE_RECOVERING},
        {"TIMEOUT",           AicoServiceStatus::TIMEOUT},
        {"TRANSPORT_ERROR",   AicoServiceStatus::TRANSPORT_ERROR},
        {"INTERNAL_ERROR",    AicoServiceStatus::INTERNAL_ERROR},
    };
    for (size_t i = 0; i < sizeof(kMappings) / sizeof(kMappings[0]); ++i) {
        if (strcmp(text, kMappings[i].text) == 0) {
            *out = kMappings[i].status;
            return true;
        }
    }
    return false;
}

}  // namespace aico
