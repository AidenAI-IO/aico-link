#include "audio_service_server.h"
#include "aico_log.h"
#include "audio_service_protocol.h"
#include "cJSON/cJSON.h"
#include "uds_message.h"
#include <limits>
#include <stdio.h>

namespace aico {

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

static std::string op_from_request(const UdsMessage& req) {
    cJSON* root = cJSON_Parse(req.header_json.c_str());
    if (!root) return "";
    cJSON* op = cJSON_GetObjectItem(root, "op");
    std::string result;
    if (op && op->type == cJSON_String && op->valuestring) {
        result = op->valuestring;
    }
    cJSON_Delete(root);
    return result;
}

static void send_response(int fd, AicoServiceStatus status,
                           const std::string& extra = "") {
    std::string json = audio_response_json(status, extra);
    write_uds_message(fd, json, {});
}

// -----------------------------------------------------------------------
// AudioServiceServer
// -----------------------------------------------------------------------

AudioServiceServer::AudioServiceServer(const char* socket_path)
    : AudioServiceServer(socket_path, nullptr) {}

AudioServiceServer::AudioServiceServer(const char* socket_path,
                                       const char* volume_state_path)
    : uds_server_(new UdsServer(socket_path ? socket_path : "",
                                [this](const UdsMessage& req, int fd) {
                                    handle_request(req, fd);
                                })),
      manager_(volume_state_path) {}

AudioServiceServer::~AudioServiceServer() {
    stop();
}

AicoServiceStatus AudioServiceServer::start() {
    return uds_server_->start();
}

void AudioServiceServer::stop() {
    uds_server_->stop();
}

void AudioServiceServer::handle_request(const UdsMessage& req, int fd) {
    std::string op = op_from_request(req);

    if (op == "start_recording")    { handle_start_recording(req, fd);    return; }
    if (op == "read_record_chunk")  { handle_read_record_chunk(req, fd);  return; }
    if (op == "stop_recording")     { handle_stop_recording(req, fd);     return; }
    if (op == "start_playback")     { handle_start_playback(req, fd);     return; }
    if (op == "write_play_chunk")   { handle_write_play_chunk(req, fd);   return; }
    if (op == "stop_playback")      { handle_stop_playback(req, fd);      return; }
    if (op == "set_playback_volume"){ handle_set_playback_volume(req, fd);return; }
    if (op == "get_playback_volume"){ handle_get_playback_volume(fd);     return; }
    if (op == "health")             { handle_health(fd);                  return; }

    AICO_LOG_WARN("server", "unknown_operation", "operation=%s", op.c_str());
    send_response(fd, AicoServiceStatus::INTERNAL_ERROR);
}

// -----------------------------------------------------------------------
// Recording handlers
// -----------------------------------------------------------------------

void AudioServiceServer::handle_start_recording(const UdsMessage& req, int fd) {
    AudioFormat fmt;
    audio_format_from_json(req.header_json.c_str(), &fmt);

    RecordStartResult result;
    AicoServiceStatus status = manager_.start_recording(fmt, &result);
    if (status != AicoServiceStatus::OK) {
        send_response(fd, status);
        return;
    }
    std::string extra = "\"session_id\":\"" +
                        std::to_string(result.session_id) + "\"";
    send_response(fd, AicoServiceStatus::OK, extra);
}

void AudioServiceServer::handle_read_record_chunk(const UdsMessage& req, int fd) {
    uint64_t session_id = audio_json_u64(req.header_json.c_str(), "session_id");
    uint32_t timeout_ms = audio_json_u32(req.header_json.c_str(), "timeout_ms");
    if (timeout_ms == 0) timeout_ms = 2000;

    AudioChunkResult chunk;
    AicoServiceStatus status = manager_.read_record_chunk(session_id, timeout_ms, &chunk);
    if (status != AicoServiceStatus::OK) {
        send_response(fd, status);
        return;
    }
    std::string extra = std::string("\"end_of_stream\":") +
                        (chunk.end_of_stream ? "true" : "false");
    write_uds_message(fd, audio_response_json(AicoServiceStatus::OK, extra), chunk.pcm);
}

void AudioServiceServer::handle_stop_recording(const UdsMessage& req, int fd) {
    uint64_t session_id = audio_json_u64(req.header_json.c_str(), "session_id");
    AicoServiceStatus status = manager_.stop_recording(session_id);
    send_response(fd, status);
}

// -----------------------------------------------------------------------
// Playback handlers
// -----------------------------------------------------------------------

void AudioServiceServer::handle_start_playback(const UdsMessage& req, int fd) {
    AudioFormat fmt;
    audio_format_from_json(req.header_json.c_str(), &fmt);

    PlaybackStartResult result;
    AicoServiceStatus status = manager_.start_playback(fmt, &result);
    if (status != AicoServiceStatus::OK) {
        send_response(fd, status);
        return;
    }
    std::string extra = "\"session_id\":\"" +
                        std::to_string(result.session_id) + "\"";
    send_response(fd, AicoServiceStatus::OK, extra);
}

void AudioServiceServer::handle_write_play_chunk(const UdsMessage& req, int fd) {
    uint64_t session_id = audio_json_u64(req.header_json.c_str(), "session_id");
    bool is_final       = audio_json_bool(req.header_json.c_str(), "is_final");

    AicoServiceStatus status = manager_.write_play_chunk(
        session_id,
        req.payload.empty() ? nullptr : req.payload.data(),
        req.payload.size(),
        is_final);
    send_response(fd, status);
}

void AudioServiceServer::handle_stop_playback(const UdsMessage& req, int fd) {
    uint64_t session_id = audio_json_u64(req.header_json.c_str(), "session_id");
    AICO_LOG_INFO("playback", "stop_requested", "session_id=%llu",
                   static_cast<unsigned long long>(session_id));
    AicoServiceStatus status = manager_.stop_playback(session_id);
    send_response(fd, status);
}

void AudioServiceServer::handle_set_playback_volume(const UdsMessage& req, int fd) {
    uint32_t volume = audio_json_u32(req.header_json.c_str(), "volume");
    if (volume > static_cast<uint32_t>(std::numeric_limits<int>::max()) || volume > 100) {
        send_response(fd, AicoServiceStatus::INTERNAL_ERROR);
        return;
    }
    AicoServiceStatus status = manager_.set_playback_volume(static_cast<int>(volume));
    send_response(fd, status);
}

void AudioServiceServer::handle_get_playback_volume(int fd) {
    uint32_t volume = 0;
    AicoServiceStatus status = manager_.get_playback_volume(&volume);
    if (status != AicoServiceStatus::OK) {
        send_response(fd, status);
        return;
    }
    std::string extra = "\"volume\":" + std::to_string(volume);
    send_response(fd, AicoServiceStatus::OK, extra);
}

// -----------------------------------------------------------------------
// Health handler
// -----------------------------------------------------------------------

void AudioServiceServer::handle_health(int fd) {
    AudioHealthResult h;
    manager_.fill_health(&h);
    std::string extra =
        std::string("\"recording_active\":") + (h.recording_active ? "true" : "false") +
        ",\"playback_active\":"              + (h.playback_active  ? "true" : "false") +
        ",\"record_sessions\":"             + std::to_string(h.record_sessions) +
        ",\"playback_sessions\":"           + std::to_string(h.playback_sessions);
    send_response(fd, AicoServiceStatus::OK, extra);
}

}  // namespace aico
