#include "audio_service_client.h"
#include <chrono>
#include <errno.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <string>

static void usage(const char* program) {
    fprintf(stderr,
            "Usage: %s [--socket PATH] <command>\n"
            "\n"
            "Commands:\n"
            "  health                    Print service health\n"
            "  record-stream [--seconds N]  Capture PCM and write to stdout\n"
            "  play-stream [--rate N] [--ch N] [--bits N]  Read PCM from stdin and play\n"
            "  get-volume                Print playback volume (0..100)\n"
            "  set-volume --volume N    Set playback volume (0..100)\n",
            program);
}

static const char* default_socket() {
    const char* env = getenv("AUDIO_SERVICE_SOCKET");
    return (env && env[0] != '\0') ? env : "/run/audio_service/audio_service.sock";
}

static bool parse_non_negative_int(const char* text, int* out) {
    if (!text || !out) return false;
    errno = 0;
    char* end = nullptr;
    long value = strtol(text, &end, 10);
    if (errno != 0 || !end || *end != '\0') return false;
    if (value < 0 || value > INT_MAX) return false;
    *out = static_cast<int>(value);
    return true;
}

static bool parse_positive_u32(const char* text, uint32_t* out) {
    if (!text || !out) return false;
    errno = 0;
    char* end = nullptr;
    unsigned long value = strtoul(text, &end, 10);
    if (errno != 0 || !end || *end != '\0') return false;
    if (value == 0 || value > 0xFFFFFFFFUL) return false;
    *out = static_cast<uint32_t>(value);
    return true;
}

static bool parse_u32(const char* text, uint32_t* out) {
    if (!text || !out) return false;
    errno = 0;
    char* end = nullptr;
    unsigned long value = strtoul(text, &end, 10);
    if (errno != 0 || !end || *end != '\0') return false;
    if (value > 0xFFFFFFFFUL) return false;
    *out = static_cast<uint32_t>(value);
    return true;
}

static int cmd_health(aico::AudioServiceClient& client) {
    aico::AudioHealthResult h;
    aico::AicoServiceStatus s = client.health(&h);
    if (s != aico::AicoServiceStatus::OK) {
        fprintf(stderr, "health failed: %s\n", service_status_to_string(s));
        return 1;
    }
    printf("recording_active=%s playback_active=%s record_sessions=%u playback_sessions=%u\n",
           h.recording_active ? "true" : "false",
           h.playback_active  ? "true" : "false",
           h.record_sessions,
           h.playback_sessions);
    return 0;
}

static int cmd_get_volume(aico::AudioServiceClient& client) {
    uint32_t volume = 0;
    aico::AicoServiceStatus s = client.get_playback_volume(&volume);
    if (s != aico::AicoServiceStatus::OK) {
        fprintf(stderr, "get_playback_volume failed: %s\n", service_status_to_string(s));
        return 1;
    }
    printf("%u\n", volume);
    return 0;
}

static int cmd_set_volume(aico::AudioServiceClient& client, uint32_t volume) {
    if (volume > 100) {
        fprintf(stderr, "invalid volume: %u (expected 0..100)\n", volume);
        return 2;
    }
    aico::AicoServiceStatus s = client.set_playback_volume(volume);
    if (s != aico::AicoServiceStatus::OK) {
        fprintf(stderr, "set_playback_volume failed: %s\n", service_status_to_string(s));
        return 1;
    }
    return cmd_get_volume(client);
}

static int cmd_record_stream(aico::AudioServiceClient& client, int seconds) {
    aico::AudioFormat fmt;
    aico::RecordStartResult rs;
    if (client.start_recording(fmt, &rs) != aico::AicoServiceStatus::OK) {
        fprintf(stderr, "start_recording failed\n");
        return 1;
    }

    // Use a wall-clock deadline rather than a byte count: chunk sizes from
    // audio_service are not fixed, so byte counting can terminate early.
    auto deadline = std::chrono::steady_clock::now() +
                    std::chrono::seconds(seconds);
    bool read_failed = false;

    while (std::chrono::steady_clock::now() < deadline) {
        aico::AudioChunkResult chunk;
        aico::AicoServiceStatus s = client.read_record_chunk(rs.session_id, 2000, &chunk);
        if (s == aico::AicoServiceStatus::TIMEOUT) continue;
        if (s != aico::AicoServiceStatus::OK) {
            fprintf(stderr, "read_record_chunk failed: %s\n", service_status_to_string(s));
            read_failed = true;
            break;
        }
        if (chunk.end_of_stream) break;
        if (!chunk.pcm.empty()) {
            size_t written = fwrite(chunk.pcm.data(), 1, chunk.pcm.size(), stdout);
            if (written != chunk.pcm.size()) {
                fprintf(stderr, "write stdout failed (written=%zu expected=%zu)\n",
                        written, chunk.pcm.size());
                read_failed = true;
                break;
            }
        }
    }

    aico::AicoServiceStatus stop_status = client.stop_recording(rs.session_id);
    if (stop_status != aico::AicoServiceStatus::OK &&
        stop_status != aico::AicoServiceStatus::SESSION_NOT_FOUND) {
        fprintf(stderr, "stop_recording failed: %s\n",
                service_status_to_string(stop_status));
        read_failed = true;
    }
    return read_failed ? 1 : 0;
}

static int cmd_play_stream(aico::AudioServiceClient& client, const aico::AudioFormat& fmt) {
    aico::PlaybackStartResult ps;
    if (client.start_playback(fmt, &ps) != aico::AicoServiceStatus::OK) {
        fprintf(stderr, "start_playback failed\n");
        return 1;
    }

    uint8_t buf[4096];
    bool write_failed = false;
    for (;;) {
        size_t n = fread(buf, 1, sizeof(buf), stdin);
        if (n > 0) {
            aico::AicoServiceStatus ws =
                client.write_play_chunk(ps.session_id, buf, n, false);
            if (ws != aico::AicoServiceStatus::OK) {
                fprintf(stderr, "write_play_chunk failed: %s\n", service_status_to_string(ws));
                write_failed = true;
                break;
            }
        }
        if (n < sizeof(buf)) {
            if (ferror(stdin)) {
                fprintf(stderr, "read stdin failed\n");
                write_failed = true;
            }
            break;
        }
    }

    // Always finalize playback explicitly, even when stdin size is an exact
    // multiple of chunk size.
    aico::AicoServiceStatus final_ws =
        client.write_play_chunk(ps.session_id, nullptr, 0, true);
    if (final_ws != aico::AicoServiceStatus::OK) {
        fprintf(stderr, "final write_play_chunk failed: %s\n",
                service_status_to_string(final_ws));
        write_failed = true;
    }

    return write_failed ? 1 : 0;
}

int main(int argc, char** argv) {
    std::string socket_path = default_socket();
    int seconds = 3;
    aico::AudioFormat play_fmt;

    int i = 1;
    // Parse global --socket option.
    if (i < argc && strcmp(argv[i], "--socket") == 0 && i + 1 < argc) {
        socket_path = argv[++i];
        ++i;
    }

    if (i >= argc) { usage(argv[0]); return 2; }

    std::string cmd = argv[i++];
    aico::AudioServiceClient client(socket_path.c_str());

    if (cmd == "health") {
        return cmd_health(client);
    }

    if (cmd == "get-volume") {
        return cmd_get_volume(client);
    }

    if (cmd == "set-volume") {
        uint32_t volume = 0;
        bool have_volume = false;
        for (; i < argc; ++i) {
            if (strcmp(argv[i], "--volume") == 0 && i + 1 < argc) {
                if (!parse_u32(argv[i + 1], &volume)) {
                    fprintf(stderr, "invalid --volume value: %s\n", argv[i + 1]);
                    return 2;
                }
                have_volume = true;
                ++i;
            }
        }
        if (!have_volume) {
            fprintf(stderr, "set-volume requires --volume N\n");
            return 2;
        }
        return cmd_set_volume(client, volume);
    }

    if (cmd == "record-stream") {
        for (; i < argc; ++i) {
            if (strcmp(argv[i], "--seconds") == 0 && i + 1 < argc) {
                if (!parse_non_negative_int(argv[i + 1], &seconds)) {
                    fprintf(stderr, "invalid --seconds value: %s\n", argv[i + 1]);
                    return 2;
                }
                ++i;
            }
        }
        return cmd_record_stream(client, seconds);
    }

    if (cmd == "play-stream") {
        for (; i < argc; ++i) {
            if (strcmp(argv[i], "--rate") == 0 && i + 1 < argc) {
                if (!parse_positive_u32(argv[i + 1], &play_fmt.sample_rate)) {
                    fprintf(stderr, "invalid --rate value: %s\n", argv[i + 1]);
                    return 2;
                }
                ++i;
            } else if (strcmp(argv[i], "--ch") == 0 && i + 1 < argc) {
                if (!parse_positive_u32(argv[i + 1], &play_fmt.channels)) {
                    fprintf(stderr, "invalid --ch value: %s\n", argv[i + 1]);
                    return 2;
                }
                ++i;
            } else if (strcmp(argv[i], "--bits") == 0 && i + 1 < argc) {
                if (!parse_positive_u32(argv[i + 1], &play_fmt.bit_width)) {
                    fprintf(stderr, "invalid --bits value: %s\n", argv[i + 1]);
                    return 2;
                }
                ++i;
            }
        }
        return cmd_play_stream(client, play_fmt);
    }

    fprintf(stderr, "Unknown command: %s\n", cmd.c_str());
    usage(argv[0]);
    return 2;
}
