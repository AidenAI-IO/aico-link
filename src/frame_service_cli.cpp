#include "frame_processing.h"
#include "frame_service_client.h"
#include <stdio.h>
#include <string.h>

namespace {

void usage(const char* program) {
    fprintf(stderr,
            "Usage: %s [--socket PATH] <health|latest-frame|screenshot|list-frames|restart> [--out PATH]\n",
            program);
}

bool write_file(const char* path, const std::vector<uint8_t>& bytes) {
    FILE* fp = fopen(path, "wb");
    if (!fp) return false;
    size_t written = fwrite(bytes.data(), 1, bytes.size(), fp);
    fclose(fp);
    return written == bytes.size();
}

}

int main(int argc, char** argv) {
    const char* socket_path = "/tmp/frame_service.sock";
    const char* command = nullptr;
    const char* out_path = nullptr;

    for (int i = 1; i < argc; ++i) {
        if (strcmp(argv[i], "--socket") == 0 && i + 1 < argc) {
            socket_path = argv[++i];
        } else if (strcmp(argv[i], "--out") == 0 && i + 1 < argc) {
            out_path = argv[++i];
        } else if (!command) {
            command = argv[i];
        } else {
            usage(argv[0]);
            return 2;
        }
    }

    if (!command) {
        usage(argv[0]);
        return 2;
    }

    aico::FrameServiceClient client(socket_path);
    if (strcmp(command, "health") == 0) {
        aico::HealthResult health;
        aico::FrameServiceStatus status = client.health(&health);
        if (status != aico::FrameServiceStatus::OK) {
            fprintf(stderr, "health failed: %s\n", aico::frame_service_status_to_string(status));
            return 1;
        }
        printf("state=%s capture_mode=%s latest_seq=%llu frame_age_ms=%llu ring=%u/%u consecutive_failures=%u last_error=%s last_recovery_ts=%llu avg_frame_serve_latency_ms=%.3f avg_capture_copy_latency_ms=%.3f\n",
               health.state.c_str(),
               health.capture_mode.empty() ? "-" : health.capture_mode.c_str(),
               static_cast<unsigned long long>(health.latest_seq),
               static_cast<unsigned long long>(health.frame_age_ms),
               health.ring_buffer_used,
               health.ring_buffer_size,
               health.consecutive_failures,
               health.last_error.empty() ? "-" : health.last_error.c_str(),
               static_cast<unsigned long long>(health.last_recovery_ts),
               health.avg_frame_serve_latency_ms,
               health.avg_capture_copy_latency_ms);
        return 0;
    }

    if (strcmp(command, "latest-frame") == 0 || strcmp(command, "screenshot") == 0) {
        aico::FrameResult frame;
        aico::FrameServiceStatus status = client.latest_frame(0, 0, &frame);
        if (status != aico::FrameServiceStatus::OK) {
            fprintf(stderr, "%s failed: %s\n", command, aico::frame_service_status_to_string(status));
            return 1;
        }
        if (strcmp(command, "latest-frame") == 0) {
            if (out_path && !write_file(out_path, frame.data)) {
                fprintf(stderr, "failed to write %s\n", out_path);
                return 1;
            }
        } else {
            std::vector<uint8_t> bmp;
            if (!aico::encode_frame_to_bmp(frame.metadata, frame.data, &bmp)) {
                fprintf(stderr, "failed to encode screenshot\n");
                return 1;
            }
            if (out_path && !write_file(out_path, bmp)) {
                fprintf(stderr, "failed to write %s\n", out_path);
                return 1;
            }
        }
        printf("seq=%llu width=%u height=%u pixel_format=%s bytes=%llu stale=%s\n",
               static_cast<unsigned long long>(frame.metadata.seq),
               frame.metadata.width,
               frame.metadata.height,
               frame.metadata.pixel_format.c_str(),
               static_cast<unsigned long long>(frame.metadata.bytes),
               frame.metadata.stale ? "true" : "false");
        return 0;
    }

    if (strcmp(command, "list-frames") == 0) {
        aico::FrameListResult list;
        aico::FrameServiceStatus status = client.list_frames(8, &list);
        if (status != aico::FrameServiceStatus::OK) {
            fprintf(stderr, "list-frames failed: %s\n", aico::frame_service_status_to_string(status));
            return 1;
        }
        for (size_t i = 0; i < list.frames.size(); ++i) {
            printf("seq=%llu width=%u height=%u bytes=%llu stale=%s\n",
                   static_cast<unsigned long long>(list.frames[i].seq),
                   list.frames[i].width,
                   list.frames[i].height,
                   static_cast<unsigned long long>(list.frames[i].bytes),
                   list.frames[i].stale ? "true" : "false");
        }
        return 0;
    }

    if (strcmp(command, "restart") == 0) {
        aico::FrameServiceStatus status = client.restart();
        if (status != aico::FrameServiceStatus::OK) {
            fprintf(stderr, "restart failed: %s\n", aico::frame_service_status_to_string(status));
            return 1;
        }
        printf("ok\n");
        return 0;
    }

    usage(argv[0]);
    return 2;
}
