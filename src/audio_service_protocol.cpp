#include "audio_service_protocol.h"
#include "cJSON/cJSON.h"
#include <errno.h>
#include <stdlib.h>

namespace aico {

// -----------------------------------------------------------------------
// AudioFormat
// -----------------------------------------------------------------------

std::string audio_format_to_json(const AudioFormat& fmt) {
    return "\"sample_rate\":" + std::to_string(fmt.sample_rate) +
           ",\"channels\":"   + std::to_string(fmt.channels) +
           ",\"bit_width\":"  + std::to_string(fmt.bit_width);
}

bool audio_format_from_json(const char* json, AudioFormat* out) {
    if (!json || !out) return false;
    cJSON* root = cJSON_Parse(json);
    if (!root) return false;

    cJSON* sr = cJSON_GetObjectItem(root, "sample_rate");
    cJSON* ch = cJSON_GetObjectItem(root, "channels");
    cJSON* bw = cJSON_GetObjectItem(root, "bit_width");

    if (sr && sr->type == cJSON_Number) out->sample_rate = static_cast<uint32_t>(sr->valueint);
    if (ch && ch->type == cJSON_Number) out->channels    = static_cast<uint32_t>(ch->valueint);
    if (bw && bw->type == cJSON_Number) out->bit_width   = static_cast<uint32_t>(bw->valueint);

    cJSON_Delete(root);
    return true;
}

// -----------------------------------------------------------------------
// Envelope builders
// -----------------------------------------------------------------------

std::string audio_request_json(const char* op, const std::string& extra_fields) {
    std::string json = "{\"op\":\"";
    json += op;
    json += "\"";
    if (!extra_fields.empty()) {
        json += ",";
        json += extra_fields;
    }
    json += "}";
    return json;
}

std::string audio_response_json(AicoServiceStatus status,
                                const std::string& extra_fields) {
    std::string json = "{\"status\":\"";
    json += service_status_to_string(status);
    json += "\"";
    if (!extra_fields.empty()) {
        json += ",";
        json += extra_fields;
    }
    json += "}";
    return json;
}

// -----------------------------------------------------------------------
// Response parsers
// -----------------------------------------------------------------------

AicoServiceStatus audio_response_status(const char* json) {
    if (!json) return AicoServiceStatus::INTERNAL_ERROR;
    cJSON* root = cJSON_Parse(json);
    if (!root) return AicoServiceStatus::TRANSPORT_ERROR;

    AicoServiceStatus status = AicoServiceStatus::INTERNAL_ERROR;
    cJSON* s = cJSON_GetObjectItem(root, "status");
    if (s && s->type == cJSON_String && s->valuestring) {
        service_status_from_string(s->valuestring, &status);
    }
    cJSON_Delete(root);
    return status;
}

uint64_t audio_json_u64(const char* json, const char* key) {
    if (!json || !key) return 0;
    cJSON* root = cJSON_Parse(json);
    if (!root) return 0;

    uint64_t value = 0;
    cJSON* item = cJSON_GetObjectItem(root, key);
    if (item) {
        if (item->type == cJSON_String && item->valuestring) {
            char* end = nullptr;
            errno = 0;
            unsigned long long v = strtoull(item->valuestring, &end, 10);
            if (errno == 0 && end && *end == '\0') value = static_cast<uint64_t>(v);
        } else if (item->type == cJSON_Number) {
            value = static_cast<uint64_t>(item->valuedouble);
        }
    }
    cJSON_Delete(root);
    return value;
}

bool audio_json_bool(const char* json, const char* key) {
    if (!json || !key) return false;
    cJSON* root = cJSON_Parse(json);
    if (!root) return false;
    cJSON* item = cJSON_GetObjectItem(root, key);
    bool result = item && item->type == cJSON_True;
    cJSON_Delete(root);
    return result;
}

uint32_t audio_json_u32(const char* json, const char* key) {
    if (!json || !key) return 0;
    cJSON* root = cJSON_Parse(json);
    if (!root) return 0;
    uint32_t value = 0;
    cJSON* item = cJSON_GetObjectItem(root, key);
    if (item && item->type == cJSON_Number) {
        value = static_cast<uint32_t>(item->valueint);
    }
    cJSON_Delete(root);
    return value;
}

}  // namespace aico
