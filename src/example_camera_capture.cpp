#include "aico_sdk.h"
#include "camera_frame_utils.h"

#include <errno.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <vector>

static volatile sig_atomic_t g_quit = 0;

struct Options {
    aico::CameraConfig camera;
    const char* output_path = "/mnt/tmp/frame.ppm";
    int frame_limit = 1;
};

static void signal_handler(int sig) {
    fprintf(stderr, "Received signal %d, exiting...\n", sig);
    g_quit = 1;
}

static void usage(const char* prog) {
    fprintf(stderr,
            "Usage: %s [device_path] [width] [height] [pixfmt] [skip_frames] [options]\n"
            "Options:\n"
            "  --device PATH             V4L2 capture device (default: /dev/video0)\n"
            "  --entity PATH             Deprecated alias for --device\n"
            "  --width N                 Requested width before HDMI sync (default: 1920)\n"
            "  --height N                Requested height before HDMI sync (default: 1080)\n"
            "  --pixfmt FORMAT           Capture pixel format (default: uyvy)\n"
            "  --skip N                  Drop the first N frames after stream-on (default: 1)\n"
            "  --count N                 Capture N frames, 0 means loop until Ctrl+C (default: 1)\n"
            "  --output PATH             Save first frame; .ppm writes viewable RGB, others keep raw bytes\n"
            "                           (default: /mnt/tmp/frame.ppm)\n"
            "  --no-output               Capture without saving a file\n"
            "  --subdev PATH             HDMI bridge subdev (default: /dev/v4l-subdev2)\n"
            "  --edid PATH               Use a custom EDID hex file instead of built-in 1080p60-only CTA\n"
            "  --trigger-retries N       Additional EDID retrigger attempts after the first trigger (default: 0)\n"
            "  --trigger-delay-ms N      Delay after each trigger (default: 1000)\n"
            "  --capture-retries N       Full init/capture recovery retries (default: 2)\n"
            "  --force-trigger           Push EDID before the first timing query\n"
            "  --no-force-trigger        Query timings first and only push on failure\n"
            "  --no-hdmi-sync            Skip HDMI EDID/timing sync and use VI directly\n"
            "  --require-exact-resolution\n"
            "                            Fail if negotiated mode differs from requested width/height\n"
            "  --allow-resolution-mismatch\n"
            "                            Accept negotiated width/height instead of requiring an exact match\n"
            "  --allow-uniform-frames    Keep all-same HDMI frames instead of retrying\n"
            "  --help                    Show this help\n",
            prog);
}

static int parse_int(const char* text, int* value) {
    char* end = nullptr;
    long parsed;

    if (!text || !*text) {
        return -1;
    }

    errno = 0;
    parsed = strtol(text, &end, 10);
    if (errno != 0 || *end != '\0') {
        return -1;
    }

    *value = static_cast<int>(parsed);
    return 0;
}

static int write_raw_file(const char* path, const std::vector<uint8_t>& buffer) {
    FILE* file = fopen(path, "wb");
    if (!file) {
        fprintf(stderr, "Failed to open %s: %s\n", path, strerror(errno));
        return -1;
    }

    if (!buffer.empty() && fwrite(buffer.data(), 1, buffer.size(), file) != buffer.size()) {
        fprintf(stderr, "Failed to write %s: %s\n", path, strerror(errno));
        fclose(file);
        return -1;
    }

    if (fclose(file) != 0) {
        fprintf(stderr, "Failed to flush %s: %s\n", path, strerror(errno));
        return -1;
    }

    return 0;
}

static bool has_suffix(const char* text, const char* suffix) {
    if (!text || !suffix) {
        return false;
    }

    const size_t text_len = strlen(text);
    const size_t suffix_len = strlen(suffix);
    return text_len >= suffix_len &&
           strcmp(text + text_len - suffix_len, suffix) == 0;
}

static int write_ppm_file(const char* path,
                          const aico::VideoFrame& frame,
                          const char* pixel_format,
                          const std::vector<uint8_t>& buffer) {
    std::vector<uint8_t> rgb;
    if (!aico_demo::convert_frame_to_rgb(frame, pixel_format, buffer, &rgb)) {
        fprintf(stderr, "PPM export does not support pixel format %s or buffer is incomplete\n",
                pixel_format ? pixel_format : "(null)");
        errno = EINVAL;
        return -1;
    }

    FILE* file = fopen(path, "wb");
    if (!file) {
        fprintf(stderr, "Failed to open %s: %s\n", path, strerror(errno));
        return -1;
    }

    if (fprintf(file, "P6\n%u %u\n255\n", frame.width, frame.height) < 0) {
        fprintf(stderr, "Failed to write PPM header to %s: %s\n", path, strerror(errno));
        fclose(file);
        return -1;
    }

    if (!rgb.empty() && fwrite(rgb.data(), 1, rgb.size(), file) != rgb.size()) {
        fprintf(stderr, "Failed to write %s: %s\n", path, strerror(errno));
        fclose(file);
        return -1;
    }

    if (fclose(file) != 0) {
        fprintf(stderr, "Failed to flush %s: %s\n", path, strerror(errno));
        return -1;
    }

    return 0;
}

static int save_frame(const Options& opts,
                      const aico::VideoFrame& frame,
                      const std::vector<uint8_t>& buffer) {
    if (!opts.output_path) {
        return 0;
    }

    if (has_suffix(opts.output_path, ".ppm")) {
        return write_ppm_file(opts.output_path, frame, opts.camera.pixel_format, buffer);
    }

    return write_raw_file(opts.output_path, buffer);
}

static int parse_options(int argc, char* argv[], Options* opts) {
    int positional = 0;

    aico_demo::set_default_camera_config(&opts->camera);
    opts->output_path = "/mnt/tmp/frame.ppm";

    for (int i = 1; i < argc; ++i) {
        const char* arg = argv[i];

        if (strcmp(arg, "--help") == 0) {
            usage(argv[0]);
            return 1;
        } else if (strcmp(arg, "--device") == 0 && i + 1 < argc) {
            opts->camera.device_name = argv[++i];
        } else if (strcmp(arg, "--entity") == 0 && i + 1 < argc) {
            opts->camera.device_name = argv[++i];
        } else if (strcmp(arg, "--width") == 0 && i + 1 < argc) {
            if (parse_int(argv[++i], &opts->camera.width) < 0) {
                fprintf(stderr, "Invalid width: %s\n", argv[i]);
                return -1;
            }
        } else if (strcmp(arg, "--height") == 0 && i + 1 < argc) {
            if (parse_int(argv[++i], &opts->camera.height) < 0) {
                fprintf(stderr, "Invalid height: %s\n", argv[i]);
                return -1;
            }
        } else if (strcmp(arg, "--pixfmt") == 0 && i + 1 < argc) {
            opts->camera.pixel_format = argv[++i];
        } else if (strcmp(arg, "--skip") == 0 && i + 1 < argc) {
            if (parse_int(argv[++i], &opts->camera.skip_frames) < 0) {
                fprintf(stderr, "Invalid skip count: %s\n", argv[i]);
                return -1;
            }
        } else if (strcmp(arg, "--count") == 0 && i + 1 < argc) {
            if (parse_int(argv[++i], &opts->frame_limit) < 0) {
                fprintf(stderr, "Invalid frame count: %s\n", argv[i]);
                return -1;
            }
        } else if (strcmp(arg, "--output") == 0 && i + 1 < argc) {
            opts->output_path = argv[++i];
        } else if (strcmp(arg, "--no-output") == 0) {
            opts->output_path = nullptr;
        } else if (strcmp(arg, "--subdev") == 0 && i + 1 < argc) {
            opts->camera.subdev_device = argv[++i];
        } else if (strcmp(arg, "--edid") == 0 && i + 1 < argc) {
            opts->camera.edid_path = argv[++i];
        } else if (strcmp(arg, "--trigger-retries") == 0 && i + 1 < argc) {
            if (parse_int(argv[++i], &opts->camera.trigger_retries) < 0) {
                fprintf(stderr, "Invalid trigger retry count: %s\n", argv[i]);
                return -1;
            }
        } else if (strcmp(arg, "--trigger-delay-ms") == 0 && i + 1 < argc) {
            if (parse_int(argv[++i], &opts->camera.trigger_delay_ms) < 0) {
                fprintf(stderr, "Invalid trigger delay: %s\n", argv[i]);
                return -1;
            }
        } else if (strcmp(arg, "--capture-retries") == 0 && i + 1 < argc) {
            if (parse_int(argv[++i], &opts->camera.capture_retries) < 0) {
                fprintf(stderr, "Invalid capture retry count: %s\n", argv[i]);
                return -1;
            }
        } else if (strcmp(arg, "--force-trigger") == 0) {
            opts->camera.force_trigger = true;
        } else if (strcmp(arg, "--no-force-trigger") == 0) {
            opts->camera.force_trigger = false;
        } else if (strcmp(arg, "--no-hdmi-sync") == 0) {
            opts->camera.enable_hdmi_sync = false;
        } else if (strcmp(arg, "--require-exact-resolution") == 0) {
            opts->camera.require_exact_resolution = true;
        } else if (strcmp(arg, "--allow-resolution-mismatch") == 0) {
            opts->camera.require_exact_resolution = false;
        } else if (strcmp(arg, "--allow-uniform-frames") == 0) {
            opts->camera.reject_uniform_frames = false;
        } else if (arg[0] == '-') {
            fprintf(stderr, "Unknown option: %s\n", arg);
            usage(argv[0]);
            return -1;
        } else {
            switch (positional++) {
                case 0:
                    opts->camera.device_name = arg;
                    break;
                case 1:
                    if (parse_int(arg, &opts->camera.width) < 0) {
                        fprintf(stderr, "Invalid width: %s\n", arg);
                        return -1;
                    }
                    break;
                case 2:
                    if (parse_int(arg, &opts->camera.height) < 0) {
                        fprintf(stderr, "Invalid height: %s\n", arg);
                        return -1;
                    }
                    break;
                case 3:
                    opts->camera.pixel_format = arg;
                    break;
                case 4:
                    if (parse_int(arg, &opts->camera.skip_frames) < 0) {
                        fprintf(stderr, "Invalid skip count: %s\n", arg);
                        return -1;
                    }
                    break;
                default:
                    fprintf(stderr, "Too many positional arguments\n");
                    usage(argv[0]);
                    return -1;
            }
        }
    }

    if (opts->camera.width <= 0 || opts->camera.height <= 0 ||
        opts->camera.skip_frames < 0 || opts->frame_limit < 0 ||
        opts->camera.trigger_retries < 0 || opts->camera.trigger_delay_ms < 0 ||
        opts->camera.capture_retries < 0) {
        fprintf(stderr, "Negative sizes, counts, or delays are not allowed\n");
        return -1;
    }

    return 0;
}

int main(int argc, char* argv[]) {
    Options opts;
    aico::CameraCapture camera;
    std::vector<uint8_t> frame_buffer;
    int captured = 0;

    signal(SIGINT, signal_handler);
    signal(SIGTERM, signal_handler);

    int parse_ret = parse_options(argc, argv, &opts);
    if (parse_ret != 0) {
        return parse_ret > 0 ? 0 : 1;
    }

    printf("Initializing camera capture...\n");
    printf("Capture device: %s\n", opts.camera.device_name);
    printf("Requested format: %dx%d %s\n",
           opts.camera.width, opts.camera.height, opts.camera.pixel_format);
    printf("Initial frame skip: %d\n", opts.camera.skip_frames);
    printf("Capture retries: %d\n", opts.camera.capture_retries);
    printf("Exact resolution required: %s\n",
           opts.camera.require_exact_resolution ? "yes" : "no");
    printf("Reject uniform frames: %s\n",
           opts.camera.reject_uniform_frames ? "yes" : "no");
    printf("Output: %s\n", opts.output_path ? opts.output_path : "(disabled)");
    if (opts.camera.enable_hdmi_sync) {
        printf("HDMI sync: enabled, subdev=%s, EDID=%s, force_trigger=%s, retries=%d\n",
               opts.camera.subdev_device,
               opts.camera.edid_path ? opts.camera.edid_path : "built-in 1080p60-only CTA",
               opts.camera.force_trigger ? "yes" : "no",
               opts.camera.trigger_retries);
    } else {
        printf("HDMI sync: disabled\n");
    }

    if (opts.frame_limit == 1) {
        aico::VideoFrame frame{};
        if (!camera.capture_once(opts.camera, frame, frame_buffer)) {
            fprintf(stderr, "Failed to capture frame\n");
            return 1;
        }

        printf("Captured frame #%d: %ux%u, %u bytes, seq=%u, ts=%llu\n",
               1,
               frame.width,
               frame.height,
               frame.length,
               frame.sequence,
               (unsigned long long)frame.timestamp);

        if (opts.output_path) {
            if (save_frame(opts, frame, frame_buffer) < 0) {
                return 1;
            }
            printf("Saved frame to %s (%zu bytes source payload)\n",
                   opts.output_path, frame_buffer.size());
        }

        printf("Captured 1 frame. Stopped.\n");
        return 0;
    }

    if (!camera.init(opts.camera)) {
        fprintf(stderr, "Failed to initialize camera capture\n");
        return 1;
    }

    while (!g_quit && (opts.frame_limit == 0 || captured < opts.frame_limit)) {
        aico::VideoFrame frame{};
        if (!camera.capture_frame(frame, frame_buffer)) {
            fprintf(stderr, "Failed to capture frame\n");
            camera.stop();
            return 1;
        }

        ++captured;
        printf("Captured frame #%d: %ux%u, %u bytes, seq=%u, ts=%llu\n",
               captured,
               frame.width,
               frame.height,
               frame.length,
               frame.sequence,
               (unsigned long long)frame.timestamp);

        if (captured == 1 && opts.output_path) {
            if (save_frame(opts, frame, frame_buffer) < 0) {
                camera.stop();
                return 1;
            }
            printf("Saved first captured frame to %s (%zu bytes source payload)\n",
                   opts.output_path, frame_buffer.size());
        }
    }

    camera.stop();
    printf("Captured %d frame(s). Stopped.\n", captured);
    return captured > 0 ? 0 : 1;
}
