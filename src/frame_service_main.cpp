#include "camera_frame_utils.h"
#include "aico_log.h"
#include "frame_camera_capture_source.h"
#include "frame_capture_manager.h"
#include "frame_service_defaults.h"
#include "frame_service_server.h"
#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <string>
#include <thread>
#include <unistd.h>

namespace {

volatile sig_atomic_t g_quit = 0;

void signal_handler(int) {
    g_quit = 1;
}

struct Options {
    Options() : socket_path("/tmp/frame_service.sock"),
                ring_size(aico::kDefaultFrameServiceRingSize),
                fps(aico::kDefaultFrameServiceFps),
                warmup_frames(-1),
                keep_streamon(false),
                auto_subdev(false) {
        aico_demo::set_default_camera_config(&camera);
        // A uniformly black or single-colour phone screen is a valid
        // screenshot. The camera layer's initial frame skip handles
        // transitional output after STREAMON without rejecting valid content.
        camera.reject_uniform_frames = false;
        device_name = camera.device_name;
        // C1 keeps the general SDK/direct-capture default compatible while
        // making the frame service's capture boundary native NV12.
        pixel_format = "nv12";
        subdev_device = camera.subdev_device;
        sync();
    }

    void sync() {
        camera.device_name = device_name.c_str();
        camera.pixel_format = pixel_format.c_str();
        camera.subdev_device = auto_subdev ? nullptr : subdev_device.c_str();
        camera.edid_path = edid_path.empty() ? nullptr : edid_path.c_str();
    }

    std::string socket_path;
    std::string device_name;
    std::string pixel_format;
    std::string subdev_device;
    std::string edid_path;
    size_t ring_size;
    double fps;
    int warmup_frames;
    bool keep_streamon;
    bool auto_subdev;
    aico::CameraConfig camera;
};

void usage(const char* program) {
    fprintf(stderr,
            "Usage: %s [--socket PATH] [--device PATH] [--width N] [--height N] "
            "[--pixel-format FMT] [--subdev PATH|--auto-subdev] [--edid PATH] [--ring-size N] "
            "[--fps N] [--no-hdmi-sync] [--force-trigger|--no-force-trigger] "
            "[--warmup-frames N] "
            "[--keep-streamon|--pause-between-captures] "
            "[--allow-uniform-frames|--reject-uniform-frames] "
            "[--require-exact-resolution|--allow-resolution-mismatch]\n"
            "  --ring-size and --fps are accepted for compatibility but ignored in on-demand mode.\n"
            "  --warmup-frames defaults to 6 with --keep-streamon and 0 with "
            "--pause-between-captures.\n",
            program);
}

bool parse_int_arg(const char* text, int* out) {
    if (!text || !out) return false;
    char* end = nullptr;
    long value = strtol(text, &end, 10);
    if (!end || *end != '\0') return false;
    *out = static_cast<int>(value);
    return true;
}

bool parse_double_arg(const char* text, double* out) {
    if (!text || !out) return false;
    char* end = nullptr;
    double value = strtod(text, &end);
    if (!end || *end != '\0') return false;
    *out = value;
    return true;
}

bool parse_options(int argc, char** argv, Options* options) {
    const char* env_socket = getenv("FRAME_SERVICE_SOCKET");
    if (env_socket && env_socket[0] != '\0') {
        options->socket_path = env_socket;
    }

    for (int i = 1; i < argc; ++i) {
        std::string arg = argv[i];
        if (arg == "--socket" && i + 1 < argc) {
            options->socket_path = argv[++i];
        } else if (arg == "--device" && i + 1 < argc) {
            options->device_name = argv[++i];
        } else if (arg == "--width" && i + 1 < argc) {
            if (!parse_int_arg(argv[++i], &options->camera.width)) return false;
        } else if (arg == "--height" && i + 1 < argc) {
            if (!parse_int_arg(argv[++i], &options->camera.height)) return false;
        } else if (arg == "--pixel-format" && i + 1 < argc) {
            options->pixel_format = argv[++i];
        } else if (arg == "--subdev" && i + 1 < argc) {
            options->subdev_device = argv[++i];
            options->auto_subdev = false;
        } else if (arg == "--auto-subdev") {
            options->subdev_device.clear();
            options->auto_subdev = true;
        } else if (arg == "--edid" && i + 1 < argc) {
            options->edid_path = argv[++i];
        } else if (arg == "--ring-size" && i + 1 < argc) {
            int value = 0;
            if (!parse_int_arg(argv[++i], &value) || value <= 0) return false;
            options->ring_size = static_cast<size_t>(value);
        } else if (arg == "--fps" && i + 1 < argc) {
            if (!parse_double_arg(argv[++i], &options->fps) || options->fps < 0.0) return false;
        } else if (arg == "--warmup-frames" && i + 1 < argc) {
            if (!parse_int_arg(argv[++i], &options->warmup_frames) ||
                options->warmup_frames < 0) return false;
        } else if (arg == "--keep-streamon") {
            options->keep_streamon = true;
        } else if (arg == "--pause-between-captures") {
            options->keep_streamon = false;
        } else if (arg == "--allow-uniform-frames") {
            options->camera.reject_uniform_frames = false;
        } else if (arg == "--reject-uniform-frames") {
            options->camera.reject_uniform_frames = true;
        } else if (arg == "--no-hdmi-sync") {
            options->camera.enable_hdmi_sync = false;
        } else if (arg == "--force-trigger") {
            options->camera.force_trigger = true;
        } else if (arg == "--no-force-trigger") {
            options->camera.force_trigger = false;
        } else if (arg == "--require-exact-resolution") {
            options->camera.require_exact_resolution = true;
        } else if (arg == "--allow-resolution-mismatch") {
            options->camera.require_exact_resolution = false;
        } else if (arg == "--help") {
            usage(argv[0]);
            exit(0);
        } else {
            return false;
        }
    }
    if (options->warmup_frames < 0) {
        options->warmup_frames =
            aico::default_frame_service_warmup_frames(options->keep_streamon);
    }
    options->sync();
    return true;
}

}

int main(int argc, char** argv) {
    Options options;
    if (!parse_options(argc, argv, &options)) {
        usage(argv[0]);
        return 2;
    }

    aico::set_log_service("frame_service");

    signal(SIGINT, signal_handler);
    signal(SIGTERM, signal_handler);

    aico::FrameServiceServer server(options.socket_path.c_str(), options.ring_size);
    if (server.start() != aico::FrameServiceStatus::OK) {
        AICO_LOG_ERROR("server", "socket_start_failed",
                        "socket_path=%s", options.socket_path.c_str());
        return 1;
    }

    aico::FrameCameraCaptureSource source(options.camera);
    aico::FrameCaptureManagerOptions manager_options;
    manager_options.warmup_frames = options.warmup_frames;
    manager_options.keep_streamon = options.keep_streamon;
    // Keep hot-plug recovery responsive while bounding retries and log volume
    // when a board boots without an HDMI source.
    manager_options.recovery_initial_backoff_ms = 1000;
    manager_options.recovery_max_backoff_ms = 5000;
    manager_options.recovery_idle_max_backoff_ms = 10000;
    aico::FrameCaptureManager manager(&source, &server, manager_options);
    server.set_capture_handler(
        [&manager](uint32_t timeout_ms,
                   aico::FrameMetadata* metadata,
                   std::vector<uint8_t>* data) {
            if (!metadata || !data) {
                return aico::FrameServiceStatus::INTERNAL_ERROR;
            }
            aico::CapturedFrame frame;
            const aico::FrameServiceStatus status = manager.capture(timeout_ms, &frame);
            if (status == aico::FrameServiceStatus::OK) {
                *metadata = frame.metadata;
                *data = std::move(frame.data);
            }
            return status;
        });
    server.set_restart_handler([&manager]() { manager.request_restart(); });
    if (!manager.start()) {
        AICO_LOG_ERROR("capture", "manager_start_failed", "device=%s",
                        options.camera.device_name);
        server.stop();
        return 1;
    }

    AICO_LOG_INFO("server", "listening",
                   "socket_path=%s capture_mode=on_demand warmup_frames=%d keep_streamon=%d reject_uniform_frames=%d deprecated_fps=%.3f",
                   options.socket_path.c_str(), options.warmup_frames,
                   options.keep_streamon ? 1 : 0,
                   options.camera.reject_uniform_frames ? 1 : 0, options.fps);
    while (!g_quit) {
        std::this_thread::sleep_for(std::chrono::milliseconds(200));
    }

    manager.stop();
    server.stop();
    AICO_LOG_INFO("server", "stopped", "socket_path=%s", options.socket_path.c_str());
    return 0;
}
