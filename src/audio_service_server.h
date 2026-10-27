#pragma once

#include "audio_session_manager.h"
#include "uds_server.h"
#include <memory>
#include <string>

namespace aico {

// UDS RPC server for audio_service.
// Dispatches JSON requests to AudioSessionManager and writes JSON responses.
class AudioServiceServer {
public:
    explicit AudioServiceServer(const char* socket_path);
    AudioServiceServer(const char* socket_path, const char* volume_state_path);
    ~AudioServiceServer();

    AicoServiceStatus start();
    void stop();

private:
    void handle_request(const UdsMessage& request, int fd);

    void handle_start_recording(const UdsMessage& req, int fd);
    void handle_read_record_chunk(const UdsMessage& req, int fd);
    void handle_stop_recording(const UdsMessage& req, int fd);
    void handle_start_playback(const UdsMessage& req, int fd);
    void handle_write_play_chunk(const UdsMessage& req, int fd);
    void handle_stop_playback(const UdsMessage& req, int fd);
    void handle_set_playback_volume(const UdsMessage& req, int fd);
    void handle_get_playback_volume(int fd);
    void handle_health(int fd);

    std::unique_ptr<UdsServer> uds_server_;
    AudioSessionManager manager_;
};

}  // namespace aico
