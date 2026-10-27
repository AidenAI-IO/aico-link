#include "frame_service_client.h"
#include "cJSON/cJSON.h"
#include "uds_client.h"
#include <errno.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <utility>

namespace aico {

FrameServiceClient::FrameServiceClient(const char* socket_path)
    : socket_path_(socket_path ? socket_path : "") {}

static uint64_t json_u64(cJSON* object, const char* key) {
    cJSON* item = cJSON_GetObjectItem(object, key);
    if (!item) {
        return 0;
    }
    if (item->type == cJSON_String && item->valuestring) {
        char* end = nullptr;
        errno = 0;
        unsigned long long value = strtoull(item->valuestring, &end, 10);
        if (errno == 0 && end && *end == '\0') {
            return static_cast<uint64_t>(value);
        }
        return 0;
    }
    if (item->type != cJSON_Number) {
        return 0;
    }
    return static_cast<uint64_t>(item->valuedouble);
}

static uint32_t json_u32(cJSON* object, const char* key) {
    return static_cast<uint32_t>(json_u64(object, key));
}

static double json_double(cJSON* object, const char* key) {
    cJSON* item = cJSON_GetObjectItem(object, key);
    if (!item || item->type != cJSON_Number) {
        return 0.0;
    }
    return item->valuedouble;
}

static std::string json_string(cJSON* object, const char* key) {
    cJSON* item = cJSON_GetObjectItem(object, key);
    if (!item || item->type != cJSON_String || !item->valuestring) {
        return std::string();
    }
    return item->valuestring;
}

static bool json_bool(cJSON* object, const char* key) {
    cJSON* item = cJSON_GetObjectItem(object, key);
    return item && item->type == cJSON_True;
}

static FrameServiceStatus response_status(cJSON* root) {
    std::string status_text = json_string(root, "status");
    FrameServiceStatus status = FrameServiceStatus::INTERNAL_ERROR;
    if (!frame_service_status_from_string(status_text.c_str(), &status)) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }
    return status;
}

static FrameServiceStatus parse_response_root(const UdsMessage& response,
                                               cJSON** root_out,
                                               FrameServiceStatus* status_out) {
    cJSON* root = cJSON_Parse(response.header_json.c_str());
    if (!root) {
        return FrameServiceStatus::TRANSPORT_ERROR;
    }

    FrameServiceStatus status = response_status(root);
    *root_out = root;
    *status_out = status;
    return FrameServiceStatus::OK;
}

static void parse_frame_metadata(cJSON* frame_json, FrameMetadata* metadata) {
    metadata->planes.clear();
    metadata->seq = json_u64(frame_json, "seq");
    metadata->capture_ts_ns = json_u64(frame_json, "capture_ts_ns");
    metadata->width = json_u32(frame_json, "width");
    metadata->height = json_u32(frame_json, "height");
    metadata->source_width = json_u32(frame_json, "source_width");
    metadata->source_height = json_u32(frame_json, "source_height");
    metadata->crop_x = json_u32(frame_json, "crop_x");
    metadata->crop_y = json_u32(frame_json, "crop_y");
    metadata->crop_width = json_u32(frame_json, "crop_width");
    metadata->crop_height = json_u32(frame_json, "crop_height");
    metadata->pixel_format = json_string(frame_json, "pixel_format");
    metadata->stride = json_u32(frame_json, "stride");
    metadata->bytes = json_u64(frame_json, "bytes");
    metadata->stale = json_bool(frame_json, "stale");

    cJSON* planes = cJSON_GetObjectItem(frame_json, "planes");
    if (planes && planes->type == cJSON_Array) {
        int n = cJSON_GetArraySize(planes);
        for (int i = 0; i < n; ++i) {
            cJSON* plane_json = cJSON_GetArrayItem(planes, i);
            if (!plane_json) {
                continue;
            }
            FramePlaneMetadata plane;
            plane.offset = json_u32(plane_json, "offset");
            plane.stride = json_u32(plane_json, "stride");
            plane.bytes = json_u32(plane_json, "bytes");
            metadata->planes.push_back(plane);
        }
    }
}

static bool payload_matches_metadata(const FrameMetadata& metadata,
                                     const std::vector<uint8_t>& payload) {
    return metadata.bytes == static_cast<uint64_t>(payload.size());
}

FrameServiceStatus FrameServiceClient::health(HealthResult* out) {
    if (!out) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }

    UdsMessage response;
    FrameServiceStatus transport = uds_request_once(socket_path_,
                                                    "{\"type\":\"request\",\"method\":\"health\"}",
                                                    std::vector<uint8_t>(),
                                                    &response,
                                                    5000);
    if (transport != FrameServiceStatus::OK) {
        return transport;
    }

    cJSON* root = nullptr;
    FrameServiceStatus status = FrameServiceStatus::INTERNAL_ERROR;
    FrameServiceStatus parsed = parse_response_root(response, &root, &status);
    if (parsed != FrameServiceStatus::OK) {
        return parsed;
    }
    if (status == FrameServiceStatus::OK) {
        out->state = json_string(root, "state");
        out->capture_mode = json_string(root, "capture_mode");
        out->latest_seq = json_u64(root, "latest_seq");
        out->frame_age_ms = json_u64(root, "frame_age_ms");
        out->ring_buffer_size = json_u32(root, "ring_buffer_size");
        out->ring_buffer_used = json_u32(root, "ring_buffer_used");
        out->consecutive_failures = json_u32(root, "consecutive_failures");
        out->last_error = json_string(root, "last_error");
        out->last_recovery_ts = json_u64(root, "last_recovery_ts");
        out->avg_frame_serve_latency_ms = json_double(root, "avg_frame_serve_latency_ms");
        out->avg_capture_copy_latency_ms = json_double(root, "avg_capture_copy_latency_ms");
    }
    cJSON_Delete(root);
    return status;
}

FrameServiceStatus FrameServiceClient::latest_frame(uint64_t since_seq,
                                                    uint32_t timeout_ms,
                                                    FrameResult* out) {
    if (!out) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }

    char request[256];
    snprintf(request, sizeof(request),
             "{\"type\":\"request\",\"method\":\"latest_frame\",\"since_seq\":\"%llu\",\"timeout_ms\":%u}",
             static_cast<unsigned long long>(since_seq), timeout_ms);

    UdsMessage response;
    uint32_t response_timeout_ms = timeout_ms > 0 ? timeout_ms + 1000 : 5000;
    FrameServiceStatus transport = uds_request_once(socket_path_, request, std::vector<uint8_t>(), &response, response_timeout_ms);
    if (transport != FrameServiceStatus::OK) {
        return transport;
    }

    cJSON* root = nullptr;
    FrameServiceStatus status = FrameServiceStatus::INTERNAL_ERROR;
    FrameServiceStatus parsed = parse_response_root(response, &root, &status);
    if (parsed != FrameServiceStatus::OK) {
        return parsed;
    }
    if (status == FrameServiceStatus::OK) {
        cJSON* frame_json = cJSON_GetObjectItem(root, "frame");
        if (!frame_json) {
            cJSON_Delete(root);
            return FrameServiceStatus::TRANSPORT_ERROR;
        }
        parse_frame_metadata(frame_json, &out->metadata);
        if (!payload_matches_metadata(out->metadata, response.payload)) {
            cJSON_Delete(root);
            return FrameServiceStatus::TRANSPORT_ERROR;
        }
        out->data = std::move(response.payload);
    }
    cJSON_Delete(root);
    return status;
}

FrameServiceStatus FrameServiceClient::get_frame(uint64_t seq, FrameResult* out) {
    if (!out) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }

    char request[192];
    snprintf(request, sizeof(request),
             "{\"type\":\"request\",\"method\":\"get_frame\",\"seq\":\"%llu\"}",
             static_cast<unsigned long long>(seq));

    UdsMessage response;
    FrameServiceStatus transport = uds_request_once(socket_path_, request, std::vector<uint8_t>(), &response, 5000);
    if (transport != FrameServiceStatus::OK) {
        return transport;
    }

    cJSON* root = nullptr;
    FrameServiceStatus status = FrameServiceStatus::INTERNAL_ERROR;
    FrameServiceStatus parsed = parse_response_root(response, &root, &status);
    if (parsed != FrameServiceStatus::OK) {
        return parsed;
    }
    if (status == FrameServiceStatus::OK) {
        cJSON* frame_json = cJSON_GetObjectItem(root, "frame");
        if (!frame_json) {
            cJSON_Delete(root);
            return FrameServiceStatus::TRANSPORT_ERROR;
        }
        parse_frame_metadata(frame_json, &out->metadata);
        if (!payload_matches_metadata(out->metadata, response.payload)) {
            cJSON_Delete(root);
            return FrameServiceStatus::TRANSPORT_ERROR;
        }
        out->data = std::move(response.payload);
    }
    cJSON_Delete(root);
    return status;
}

FrameServiceStatus FrameServiceClient::list_frames(uint32_t count, FrameListResult* out) {
    if (!out) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }

    char request[160];
    snprintf(request, sizeof(request),
             "{\"type\":\"request\",\"method\":\"list_frames\",\"count\":%u}",
             count);

    UdsMessage response;
    FrameServiceStatus transport = uds_request_once(socket_path_, request, std::vector<uint8_t>(), &response, 5000);
    if (transport != FrameServiceStatus::OK) {
        return transport;
    }

    cJSON* root = nullptr;
    FrameServiceStatus status = FrameServiceStatus::INTERNAL_ERROR;
    FrameServiceStatus parsed = parse_response_root(response, &root, &status);
    if (parsed != FrameServiceStatus::OK) {
        return parsed;
    }
    if (status == FrameServiceStatus::OK) {
        out->frames.clear();
        cJSON* frames = cJSON_GetObjectItem(root, "frames");
        if (frames && frames->type == cJSON_Array) {
            int n = cJSON_GetArraySize(frames);
            for (int i = 0; i < n; ++i) {
                cJSON* frame_json = cJSON_GetArrayItem(frames, i);
                if (!frame_json || frame_json->type != cJSON_Object) {
                    continue;
                }
                FrameMetadata metadata;
                parse_frame_metadata(frame_json, &metadata);
                out->frames.push_back(metadata);
            }
        }
    }
    cJSON_Delete(root);
    return status;
}

FrameServiceStatus FrameServiceClient::restart() {
    UdsMessage response;
    FrameServiceStatus transport = uds_request_once(socket_path_,
                                                    "{\"type\":\"request\",\"method\":\"restart\"}",
                                                    std::vector<uint8_t>(),
                                                    &response,
                                                    5000);
    if (transport != FrameServiceStatus::OK) {
        return transport;
    }

    cJSON* root = nullptr;
    FrameServiceStatus status = FrameServiceStatus::INTERNAL_ERROR;
    FrameServiceStatus parsed = parse_response_root(response, &root, &status);
    if (parsed != FrameServiceStatus::OK) {
        return parsed;
    }
    cJSON_Delete(root);
    return status;
}

}  // namespace aico
