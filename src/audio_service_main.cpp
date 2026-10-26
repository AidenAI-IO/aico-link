#include "audio_service_server.h"
#include "audio_volume_state.h"
#include "aico_log.h"
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string>
#include <thread>
#include <chrono>
#include <unistd.h>

namespace {

volatile sig_atomic_t g_quit = 0;

void signal_handler(int) {
    g_quit = 1;
}

struct Options {
    std::string socket_path = "/run/audio_service/audio_service.sock";
    std::string volume_state_path = aico::kDefaultAudioVolumeStatePath;
};

void usage(const char* program) {
    fprintf(stderr, "Usage: %s [--socket PATH] [--volume-state PATH]\n", program);
}

bool parse_options(int argc, char** argv, Options* opts) {
    const char* env_socket = getenv("AUDIO_SERVICE_SOCKET");
    if (env_socket && env_socket[0] != '\0') {
        opts->socket_path = env_socket;
    }
    const char* env_volume_state = getenv("AUDIO_SERVICE_VOLUME_STATE");
    if (env_volume_state && env_volume_state[0] != '\0') {
        opts->volume_state_path = env_volume_state;
    }

    for (int i = 1; i < argc; ++i) {
        std::string arg = argv[i];
        if (arg == "--socket" && i + 1 < argc) {
            opts->socket_path = argv[++i];
        } else if (arg == "--volume-state" && i + 1 < argc) {
            opts->volume_state_path = argv[++i];
        } else if (arg == "--help") {
            usage(argv[0]);
            exit(0);
        } else {
            return false;
        }
    }
    return true;
}

}  // namespace

int main(int argc, char** argv) {
    Options opts;
    if (!parse_options(argc, argv, &opts)) {
        usage(argv[0]);
        return 2;
    }

    aico::set_log_service("audio_service");

    signal(SIGINT,  signal_handler);
    signal(SIGTERM, signal_handler);
    signal(SIGHUP,  SIG_IGN);
    signal(SIGPIPE, SIG_IGN);

    aico::AudioServiceServer server(opts.socket_path.c_str(),
                                     opts.volume_state_path.c_str());
    if (server.start() != aico::AicoServiceStatus::OK) {
        AICO_LOG_ERROR("server", "socket_start_failed",
                        "socket_path=%s", opts.socket_path.c_str());
        return 1;
    }

    AICO_LOG_INFO("server", "listening", "socket_path=%s", opts.socket_path.c_str());

    while (!g_quit) {
        std::this_thread::sleep_for(std::chrono::milliseconds(200));
    }

    server.stop();
    AICO_LOG_INFO("server", "stopped", "socket_path=%s", opts.socket_path.c_str());
    return 0;
}
