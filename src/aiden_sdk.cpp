#include "aiden_sdk.h"
#include "aiden_log.h"
#include "frame_layout.h"
#include "rockit_system.h"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstring>
#include <limits>
#include <fcntl.h>
#include <linux/v4l2-subdev.h>
#include <linux/videodev2.h>
#include <poll.h>
#include <pthread.h>
#include <stdio.h>
#include <string>
#include <sys/mman.h>
#include <sys/ioctl.h>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

extern "C" {
#include "rk_debug.h"
#include "rk_mpi_ai.h"
#include "rk_mpi_amix.h"
#include "rk_mpi_ao.h"
#include "rk_mpi_mb.h"
#include "rk_mpi_sys.h"
#include "rk_mpi_vi.h"
}

namespace aiden {

static const char* kAudioVqeConfigPath = "/usr/share/aiden/audio/config_aivqe.json";

static bool ensure_sys_init() {
    return acquire_rockit_system();
}

static void maybe_sys_deinit() {
    release_rockit_system();
}

static AUDIO_BIT_WIDTH_E to_bit_width(int bits) {
    switch (bits) {
    case 8:  return AUDIO_BIT_WIDTH_8;
    case 24: return AUDIO_BIT_WIDTH_24;
    default: return AUDIO_BIT_WIDTH_16;
    }
}

static uint32_t audio_sound_mode_channels(AUDIO_SOUND_MODE_E mode) {
    switch (mode) {
    case AUDIO_SOUND_MODE_MONO:   return 1;
    case AUDIO_SOUND_MODE_STEREO: return 2;
    case AUDIO_SOUND_MODE_4_CHN:  return 4;
    case AUDIO_SOUND_MODE_6_CHN:  return 6;
    case AUDIO_SOUND_MODE_8_CHN:  return 8;
    default:                      return 0;
    }
}

static uint32_t audio_frame_channels(AUDIO_SOUND_MODE_E mode, int configured_channels) {
    uint32_t channels = audio_sound_mode_channels(mode);
    if (channels != 0) {
        return channels;
    }
    return configured_channels > 0 ? static_cast<uint32_t>(configured_channels) : 1;
}

static bool configure_ao_volume_curve(AUDIO_DEV dev_id) {
    AUDIO_VOLUME_CURVE_S volume_curve;
    memset(&volume_curve, 0, sizeof(volume_curve));
    volume_curve.enCurveType = AUDIO_CURVE_LOGARITHM;
    volume_curve.s32Resolution = 101;
    volume_curve.fMinDB = -51.0f;
    volume_curve.fMaxDB = 0.0f;
    volume_curve.pCurveTable = RK_NULL;
    return RK_MPI_AO_SetVolumeCurve(dev_id, &volume_curve) == RK_SUCCESS;
}

static bool set_ao_mute(AUDIO_DEV dev_id, bool mute) {
    AUDIO_FADE_S fade;
    memset(&fade, 0, sizeof(fade));
    fade.bFade = RK_FALSE;
    return RK_MPI_AO_SetMute(dev_id, mute ? RK_TRUE : RK_FALSE, &fade) == RK_SUCCESS;
}

static const uint8_t kDefaultHdmiEdid1080p30[] = {
    0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00,
    0x31, 0xd8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x05, 0x16, 0x01, 0x03, 0x80, 0x32, 0x1c, 0x78,
    0xea, 0x5e, 0xc0, 0xa4, 0x59, 0x4a, 0x98, 0x25,
    0x20, 0x50, 0x54, 0x00, 0x00, 0x00, 0x01, 0x01,
    0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01,
    0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x1d,
    0x80, 0x18, 0x71, 0x38, 0x2d, 0x40, 0x58, 0x2c,
    0x45, 0x00, 0xf4, 0x19, 0x11, 0x00, 0x00, 0x1e,
    0x00, 0x00, 0x00, 0xff, 0x00, 0x4c, 0x69, 0x6e,
    0x75, 0x78, 0x20, 0x23, 0x30, 0x0a, 0x20, 0x20,
    0x20, 0x20, 0x00, 0x00, 0x00, 0xfd, 0x00, 0x1d,
    0x1f, 0x20, 0x22, 0x08, 0x00, 0x0a, 0x20, 0x20,
    0x20, 0x20, 0x20, 0x20, 0x00, 0x00, 0x00, 0xfc,
    0x00, 0x4c, 0x69, 0x6e, 0x75, 0x78, 0x20, 0x46,
    0x48, 0x44, 0x33, 0x30, 0x0a, 0x20, 0x01, 0x02,
    0x02, 0x03, 0x0f, 0x00, 0x41, 0xa2, 0x65, 0x03,
    0x0c, 0x00, 0x10, 0x00, 0xe2, 0x00, 0x2b, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x78,
};

static_assert(sizeof(kDefaultHdmiEdid1080p30) % 128 == 0,
              "Built-in HDMI EDID must be a multiple of 128 bytes");

static int xioctl(int fd, unsigned long request, void* arg) {
    int ret;

    do {
        ret = ioctl(fd, request, arg);
    } while (ret < 0 && errno == EINTR);

    return ret;
}

static int traced_xioctl(int fd,
                         unsigned long request,
                         void* arg,
                         const char* operation,
                         const char* device,
                         bool suppress_fast_success_log = false) {
    AIDEN_LOG_INFO("v4l2", "ioctl_started",
                   "operation=%s device=%s fd=%d request=%#lx",
                   operation ? operation : "unknown",
                   device ? device : "", fd, request);
    const std::chrono::steady_clock::time_point started =
        std::chrono::steady_clock::now();
    errno = 0;
    const int ret = xioctl(fd, request, arg);
    const int operation_errno = ret < 0 ? errno : 0;
    const uint64_t elapsed_ms = static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now() - started).count());
    if (ret < 0) {
        AIDEN_LOG_WARN("v4l2", "ioctl_completed",
                       "operation=%s device=%s fd=%d request=%#lx ret=%d errno=%d error=%s elapsed_ms=%llu",
                       operation ? operation : "unknown",
                       device ? device : "", fd, request, ret,
                       operation_errno, strerror(operation_errno),
                       static_cast<unsigned long long>(elapsed_ms));
    } else if (elapsed_ms >= 100) {
        AIDEN_LOG_WARN("v4l2", "ioctl_completed",
                       "operation=%s device=%s fd=%d request=%#lx ret=%d errno=0 slow=1 elapsed_ms=%llu",
                       operation ? operation : "unknown",
                       device ? device : "", fd, request, ret,
                       static_cast<unsigned long long>(elapsed_ms));
    } else if (!suppress_fast_success_log) {
        AIDEN_LOG_INFO("v4l2", "ioctl_completed",
                       "operation=%s device=%s fd=%d request=%#lx ret=%d errno=0 elapsed_ms=%llu",
                       operation ? operation : "unknown",
                       device ? device : "", fd, request, ret,
                       static_cast<unsigned long long>(elapsed_ms));
    }
    errno = operation_errno;
    return ret;
}

static const char* v4l2_format_name(uint32_t format, char text[5]) {
    text[0] = static_cast<char>(format & 0xff);
    text[1] = static_cast<char>((format >> 8) & 0xff);
    text[2] = static_cast<char>((format >> 16) & 0xff);
    text[3] = static_cast<char>((format >> 24) & 0xff);
    text[4] = '\0';
    return text;
}

static uint32_t frame_size_bytes(const char* pixel_format, uint32_t width, uint32_t height) {
    if (!pixel_format || strcmp(pixel_format, "nv12") == 0) {
        return width * height * 3 / 2;
    }
    return width * height * 2;
}

static bool timings_match_request(const CameraConfig& config,
                                  const struct v4l2_dv_timings& timings) {
    if (!config.require_exact_resolution) {
        return true;
    }

    return timings.bt.width == static_cast<uint32_t>(config.width) &&
           timings.bt.height == static_cast<uint32_t>(config.height);
}

static bool is_uniform_packed_frame(const uint8_t* data, size_t size) {
    if (!data || size < 4 || (size % 2) != 0) {
        return false;
    }

    const uint8_t first0 = data[0];
    const uint8_t first1 = data[1];
    size_t step = size / 4096;

    if (step < 2) {
        step = 2;
    }
    if (step & 1) {
        ++step;
    }

    for (size_t i = 0; i + 1 < size; i += step) {
        if (data[i] != first0 || data[i + 1] != first1) {
            return false;
        }
    }

    return data[size - 2] == first0 && data[size - 1] == first1;
}

static bool frame_looks_invalid(const CameraConfig& config,
                                const uint8_t* data,
                                size_t size) {
    if (!config.reject_uniform_frames || !config.pixel_format) {
        return false;
    }

    return (strcmp(config.pixel_format, "uyvy") == 0 ||
            strcmp(config.pixel_format, "yuyv") == 0) &&
           is_uniform_packed_frame(data, size);
}

static int write_all(int fd, const void* data, size_t length) {
    const uint8_t* bytes = static_cast<const uint8_t*>(data);

    while (length > 0) {
        ssize_t written = write(fd, bytes, length);
        if (written < 0) {
            if (errno == EINTR) {
                continue;
            }
            return -1;
        }
        bytes += written;
        length -= static_cast<size_t>(written);
    }

    return 0;
}

static int load_edid_hex_file(const char* path, uint8_t** data_out, uint32_t* blocks_out) {
    FILE* file = fopen(path, "r");
    uint8_t* buffer = nullptr;
    size_t used = 0;
    size_t capacity = 0;
    unsigned value;

    if (!file) {
        AIDEN_LOG_ERROR("hdmi", "edid_open_failed", "path=%s error=%s", path,
                        strerror(errno));
        return -1;
    }

    while (fscanf(file, "%x", &value) == 1) {
        if (value > 0xff) {
            AIDEN_LOG_ERROR("hdmi", "edid_byte_invalid", "path=%s value=0x%x", path,
                            value);
            free(buffer);
            fclose(file);
            errno = EINVAL;
            return -1;
        }

        if (used == capacity) {
            size_t new_capacity = capacity ? capacity * 2 : 256;
            uint8_t* new_buffer = static_cast<uint8_t*>(realloc(buffer, new_capacity));
            if (!new_buffer) {
                AIDEN_LOG_ERROR("hdmi", "edid_allocation_failed", "path=%s capacity=%zu",
                                path, new_capacity);
                free(buffer);
                fclose(file);
                errno = ENOMEM;
                return -1;
            }
            buffer = new_buffer;
            capacity = new_capacity;
        }

        buffer[used++] = static_cast<uint8_t>(value);
    }

    fclose(file);

    if (used == 0 || (used % 128) != 0) {
        AIDEN_LOG_ERROR("hdmi", "edid_size_invalid", "path=%s bytes=%zu", path, used);
        free(buffer);
        errno = EINVAL;
        return -1;
    }

    *data_out = buffer;
    *blocks_out = static_cast<uint32_t>(used / 128);
    return 0;
}

static int write_edid_hex_file(const char* path, const uint8_t* data, size_t size) {
    int fd = open(path, O_CREAT | O_TRUNC | O_WRONLY, 0644);
    if (fd < 0) {
        AIDEN_LOG_ERROR("hdmi", "edid_temp_create_failed", "path=%s error=%s", path,
                        strerror(errno));
        return -1;
    }

    std::string text;
    text.reserve(size * 3);
    for (size_t i = 0; i < size; ++i) {
        char byte_text[4];
        snprintf(byte_text, sizeof(byte_text), "%02x%s", data[i], ((i + 1) % 16) ? " " : "\n");
        text += byte_text;
    }
    if ((size % 16) != 0) {
        text += '\n';
    }

    int ret = write_all(fd, text.data(), text.size());
    close(fd);
    return ret;
}

static void normalize_edid_checksums(uint8_t* data, uint32_t blocks) {
    for (uint32_t block = 0; block < blocks; ++block) {
        const size_t offset = static_cast<size_t>(block) * 128;
        uint8_t checksum = 0;
        for (size_t i = 0; i < 127; ++i) {
            checksum = static_cast<uint8_t>(checksum + data[offset + i]);
        }
        data[offset + 127] = static_cast<uint8_t>(0 - checksum);
    }
}

static int push_edid_with_v4l2ctl(const CameraConfig& config,
                                  const uint8_t* data,
                                  uint32_t blocks) {
    char temp_path[64];
    snprintf(temp_path, sizeof(temp_path), "/tmp/libaiden_edid_%d.hex", getpid());

    if (write_edid_hex_file(temp_path, data, static_cast<size_t>(blocks) * 128) < 0) {
        return -1;
    }

    const char* subdev = config.subdev_device ? config.subdev_device : "/dev/v4l-subdev2";
    std::string edid_arg = std::string("pad=0,file=") + temp_path;
    pid_t child = fork();
    int status = -1;
    int wait_ret = -1;

    if (child == 0) {
        execlp("v4l2-ctl",
               "v4l2-ctl",
               "-d",
               subdev,
               "--set-edid",
               edid_arg.c_str(),
               static_cast<char*>(nullptr));
        _exit(127);
    }

    if (child > 0) {
        do {
            wait_ret = waitpid(child, &status, 0);
        } while (wait_ret < 0 && errno == EINTR);
    }

    unlink(temp_path);

    if (child < 0 || wait_ret < 0) {
        AIDEN_LOG_ERROR("hdmi", "edid_fallback_exec_failed", "error=%s", strerror(errno));
        return -1;
    }

    if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
        AIDEN_LOG_ERROR("hdmi", "edid_fallback_failed", "status=%d",
                        WIFEXITED(status) ? WEXITSTATUS(status) : status);
        errno = EIO;
        return -1;
    }

    return 0;
}

static int push_edid(int subdev_fd, const CameraConfig& config) {
    struct v4l2_edid edid;
    uint8_t* heap_edid = nullptr;
    const uint8_t* data = kDefaultHdmiEdid1080p30;
    uint32_t blocks = sizeof(kDefaultHdmiEdid1080p30) / 128;

    if (config.edid_path) {
        if (load_edid_hex_file(config.edid_path, &heap_edid, &blocks) < 0) {
            return -1;
        }
        data = heap_edid;
    }

    std::vector<uint8_t> normalized_data(
        data, data + static_cast<size_t>(blocks) * 128);
    normalize_edid_checksums(normalized_data.data(), blocks);

    memset(&edid, 0, sizeof(edid));
    edid.pad = 0;
    edid.start_block = 0;
    edid.blocks = blocks;
    edid.edid = normalized_data.data();

    // Program the bridge in-process first. VIDIOC_SUBDEV_S_EDID needs no
    // external binary and is what actually sets the EDID on current images;
    // running v4l2-ctl first meant forking a child on every push and, whenever
    // the installed v4l-utils disagreed about the command line, logging
    // edid_fallback_failed on a push that then succeeded through the ioctl
    // anyway. Keep v4l2-ctl for kernels or bridges that reject the ioctl.
    if (traced_xioctl(subdev_fd, VIDIOC_SUBDEV_S_EDID, &edid,
                      "subdev_set_edid",
                      config.subdev_device ? config.subdev_device : "") < 0) {
        AIDEN_LOG_WARN("hdmi", "edid_ioctl_failed",
                       "device=%s error=%s trying=v4l2-ctl",
                       config.subdev_device ? config.subdev_device : "",
                       strerror(errno));
        if (push_edid_with_v4l2ctl(config, normalized_data.data(), blocks) < 0) {
            free(heap_edid);
            return -1;
        }
    }

    free(heap_edid);
    return 0;
}

static int query_and_set_timings(int subdev_fd,
                                 const char* subdev_device,
                                 struct v4l2_dv_timings* timings) {
    memset(timings, 0, sizeof(*timings));

    if (traced_xioctl(subdev_fd, VIDIOC_SUBDEV_QUERY_DV_TIMINGS, timings,
                      "subdev_query_dv_timings", subdev_device) < 0) {
        return -1;
    }
    AIDEN_LOG_INFO("hdmi", "timings_detected",
                   "device=%s width=%u height=%u pixelclock=%llu",
                   subdev_device ? subdev_device : "",
                   timings->bt.width, timings->bt.height,
                   static_cast<unsigned long long>(timings->bt.pixelclock));
    if (traced_xioctl(subdev_fd, VIDIOC_SUBDEV_S_DV_TIMINGS, timings,
                      "subdev_set_dv_timings", subdev_device) < 0) {
        return -2;
    }

    return 0;
}

static bool sync_hdmi_input(const CameraConfig& config, uint32_t* width, uint32_t* height) {
    if (!config.enable_hdmi_sync) {
        AIDEN_LOG_INFO("hdmi", "sync_skipped",
                       "reason=disabled width=%d height=%d",
                       config.width, config.height);
        *width = static_cast<uint32_t>(config.width);
        *height = static_cast<uint32_t>(config.height);
        return true;
    }

    const char* subdev_device = config.subdev_device ? config.subdev_device : "/dev/v4l-subdev2";
    const std::chrono::steady_clock::time_point sync_started =
        std::chrono::steady_clock::now();
    AIDEN_LOG_INFO("hdmi", "sync_started",
                   "device=%s force_trigger=%d allow_edid_fallback=%d trigger_retries=%d trigger_delay_ms=%d expected_width=%d expected_height=%d",
                   subdev_device, config.force_trigger ? 1 : 0,
                   config.allow_edid_fallback ? 1 : 0, config.trigger_retries,
                   config.trigger_delay_ms, config.width, config.height);
    AIDEN_LOG_INFO("hdmi", "subdevice_open_started",
                   "device=%s", subdev_device);
    errno = 0;
    int subdev_fd = open(subdev_device, O_RDWR);
    if (subdev_fd < 0) {
        const int open_errno = errno;
        const uint64_t elapsed_ms = static_cast<uint64_t>(
            std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::steady_clock::now() - sync_started).count());
        AIDEN_LOG_ERROR("hdmi", "subdevice_open_failed", "device=%s error=%s",
                        subdev_device, strerror(open_errno));
        AIDEN_LOG_WARN("hdmi", "sync_completed",
                       "device=%s ok=0 phase=subdevice_open errno=%d elapsed_ms=%llu",
                       subdev_device, open_errno,
                       static_cast<unsigned long long>(elapsed_ms));
        errno = open_errno;
        return false;
    }
    AIDEN_LOG_INFO("hdmi", "subdevice_open_completed",
                   "device=%s fd=%d", subdev_device, subdev_fd);

    struct v4l2_dv_timings timings;
    const int trigger_attempts = config.trigger_retries;

    if (!config.force_trigger) {
        int ret = query_and_set_timings(subdev_fd, subdev_device, &timings);
        if (ret == 0) {
            if (!timings_match_request(config, timings)) {
                AIDEN_LOG_WARN("hdmi", "timing_mismatch",
                               "device=%s detected_width=%u detected_height=%u expected_width=%d expected_height=%d",
                               subdev_device, timings.bt.width, timings.bt.height,
                               config.width, config.height);
                errno = ERANGE;
            } else {
                *width = timings.bt.width;
                *height = timings.bt.height;
                close(subdev_fd);
                const uint64_t elapsed_ms = static_cast<uint64_t>(
                    std::chrono::duration_cast<std::chrono::milliseconds>(
                        std::chrono::steady_clock::now() - sync_started).count());
                AIDEN_LOG_INFO("hdmi", "sync_completed",
                               "device=%s ok=1 phase=timings width=%u height=%u elapsed_ms=%llu",
                               subdev_device, *width, *height,
                               static_cast<unsigned long long>(elapsed_ms));
                return true;
            }
        } else if (ret == -2) {
            AIDEN_LOG_WARN("hdmi", "timing_apply_failed", "device=%s error=%s",
                           subdev_device, strerror(errno));
        }

        if (!config.allow_edid_fallback) {
            // A normal recovery probe can be read-only with respect to the
            // HDMI bridge.  In particular, do not fall through to EDID/HPD
            // writes when there is no source or the bridge has transient I2C
            // errors.  The auto-subdevice capture source schedules bounded
            // force-trigger attempts separately.
            AIDEN_LOG_WARN("hdmi", "timing_probe_pending", "device=%s error=%s",
                           subdev_device, strerror(errno));
            const int probe_errno = errno;
            close(subdev_fd);
            const uint64_t elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - sync_started).count());
            AIDEN_LOG_WARN("hdmi", "sync_completed",
                           "device=%s ok=0 phase=timing_probe errno=%d elapsed_ms=%llu",
                           subdev_device, probe_errno,
                           static_cast<unsigned long long>(elapsed_ms));
            errno = probe_errno;
            return false;
        }
    }

    for (int attempt = 0; attempt <= trigger_attempts; ++attempt) {
        if (push_edid(subdev_fd, config) < 0) {
            const int edid_errno = errno;
            close(subdev_fd);
            const uint64_t elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - sync_started).count());
            AIDEN_LOG_WARN("hdmi", "sync_completed",
                           "device=%s ok=0 phase=edid errno=%d elapsed_ms=%llu trigger_attempt=%d",
                           subdev_device, edid_errno,
                           static_cast<unsigned long long>(elapsed_ms), attempt + 1);
            errno = edid_errno;
            return false;
        }
        if (config.trigger_delay_ms > 0) {
            usleep(static_cast<useconds_t>(config.trigger_delay_ms) * 1000);
        }

        int ret = query_and_set_timings(subdev_fd, subdev_device, &timings);
        if (ret == 0) {
            if (!timings_match_request(config, timings)) {
                AIDEN_LOG_WARN("hdmi", "timing_mismatch",
                               "device=%s detected_width=%u detected_height=%u expected_width=%d expected_height=%d",
                               subdev_device, timings.bt.width, timings.bt.height,
                               config.width, config.height);
                errno = ERANGE;
                continue;
            }

            *width = timings.bt.width;
            *height = timings.bt.height;
            close(subdev_fd);
            const uint64_t elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - sync_started).count());
            AIDEN_LOG_INFO("hdmi", "sync_completed",
                           "device=%s ok=1 phase=triggered_timings width=%u height=%u elapsed_ms=%llu trigger_attempt=%d",
                           subdev_device, *width, *height,
                           static_cast<unsigned long long>(elapsed_ms), attempt + 1);
            return true;
        }

        if (ret == -2) {
            AIDEN_LOG_WARN("hdmi", "timing_apply_failed", "device=%s error=%s",
                           subdev_device, strerror(errno));
        }

        if (attempt == trigger_attempts) {
            break;
        }
    }

    AIDEN_LOG_ERROR("hdmi", "timing_sync_failed", "device=%s error=%s",
                    subdev_device, strerror(errno));
    const int sync_errno = errno;
    close(subdev_fd);
    const uint64_t elapsed_ms = static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now() - sync_started).count());
    AIDEN_LOG_WARN("hdmi", "sync_completed",
                   "device=%s ok=0 phase=timing_sync errno=%d elapsed_ms=%llu",
                   subdev_device, sync_errno,
                   static_cast<unsigned long long>(elapsed_ms));
    errno = sync_errno;
    return false;
}

// --- WakeupListener ---

class WakeupListenerImpl {
public:
    std::atomic<bool> running{false};
    pthread_t thread{};
    int gpio_pin = 0;
    WakeupCallback callback;

    static void* thread_func(void* arg) {
        auto* self = static_cast<WakeupListenerImpl*>(arg);
        self->run();
        return nullptr;
    }

    void run() {
        char gpio_path[64];
        snprintf(gpio_path, sizeof(gpio_path), "/sys/class/gpio/gpio%d/value", gpio_pin);

        int fd = open(gpio_path, O_RDONLY);
        if (fd < 0) {
            AIDEN_LOG_ERROR("gpio", "value_open_failed", "gpio=%d path=%s error=%s",
                            gpio_pin, gpio_path, strerror(errno));
            running = false;
            return;
        }

        struct pollfd pfd;
        pfd.fd = fd;
        pfd.events = POLLPRI | POLLERR;

        char buf[64];
        while (running) {
            lseek(fd, 0, SEEK_SET);
            read(fd, buf, sizeof(buf));

            int ret = poll(&pfd, 1, 500);
            if (ret > 0 && (pfd.revents & POLLPRI)) {
                if (callback) {
                    callback();
                }
                usleep(150000);
            }
        }

        close(fd);
    }
};

WakeupListener::WakeupListener() : impl_(new WakeupListenerImpl()) {}
WakeupListener::~WakeupListener() { stop(); }

bool WakeupListener::start(int gpio_pin, WakeupCallback callback) {
    if (impl_->running) return false;

    // Export GPIO
    int fd = open("/sys/class/gpio/export", O_WRONLY);
    if (fd != -1) {
        char pin_str[8];
        snprintf(pin_str, sizeof(pin_str), "%d", gpio_pin);
        write(fd, pin_str, strlen(pin_str));
        close(fd);
    }

    // Set direction to input
    char path[64];
    snprintf(path, sizeof(path), "/sys/class/gpio/gpio%d/direction", gpio_pin);
    fd = open(path, O_WRONLY);
    if (fd != -1) {
        write(fd, "in", 2);
        close(fd);
    }

    // Set edge to falling
    snprintf(path, sizeof(path), "/sys/class/gpio/gpio%d/edge", gpio_pin);
    fd = open(path, O_WRONLY);
    if (fd != -1) {
        write(fd, "falling", 7);
        close(fd);
    }

    impl_->gpio_pin = gpio_pin;
    impl_->callback = callback;
    impl_->running = true;

    pthread_create(&impl_->thread, nullptr, WakeupListenerImpl::thread_func, impl_.get());
    return true;
}

void WakeupListener::stop() {
    if (impl_->running) {
        impl_->running = false;
        pthread_join(impl_->thread, nullptr);
    }
}

bool WakeupListener::is_running() const {
    return impl_->running;
}

// --- AudioCapture ---

class AudioCaptureImpl {
public:
    std::atomic<bool> running{false};
    bool initialized = false;
    bool vqe_enabled = false;
    pthread_t thread{};
    AudioConfig config;
    AudioStreamCallback callback;

    AUDIO_DEV dev_id = 0;
    AI_CHN chn_id = 0;
    AIO_ATTR_S attr{};
    AUDIO_FRAME_S frame{};

    static void* thread_func(void* arg) {
        auto* self = static_cast<AudioCaptureImpl*>(arg);
        self->run();
        return nullptr;
    }

    void run() {
        while (running) {
            RK_S32 ret = RK_MPI_AI_GetFrame(dev_id, chn_id, &frame, nullptr, 500);
            if (ret == RK_SUCCESS) {
                void* data = RK_MPI_MB_Handle2VirAddr(frame.pMbBlk);
                if (callback && data) {
                    AudioFrame af;
                    af.data = data;
                    af.length = frame.u32Len;
                    af.timestamp = frame.u64TimeStamp;
                    af.channels = audio_frame_channels(frame.enSoundMode, config.channels);
                    af.sample_rate = frame.s32SampleRate > 0
                                         ? static_cast<uint32_t>(frame.s32SampleRate)
                                         : static_cast<uint32_t>(config.sample_rate);
                    callback(af);
                }
                RK_MPI_AI_ReleaseFrame(dev_id, chn_id, &frame, nullptr);
            }
        }
    }
};

AudioCapture::AudioCapture() : impl_(new AudioCaptureImpl()) {}
AudioCapture::~AudioCapture() { stop(); }

bool AudioCapture::init(const AudioConfig& config) {
    if (impl_->initialized) {
        stop();
    }
    if (!ensure_sys_init()) {
        return false;
    }

    impl_->config = config;
    impl_->dev_id = 0;
    impl_->chn_id = 0;
    impl_->vqe_enabled = false;

    if (config.bit_width != 16 ||
        (config.sample_rate != 8000 && config.sample_rate != 16000 && config.sample_rate != 48000)) {
        AIDEN_LOG_ERROR("recording", "vqe_format_unsupported",
                        "sample_rate=%d bit_width=%d", config.sample_rate, config.bit_width);
        maybe_sys_deinit();
        return false;
    }

    memset(&impl_->attr, 0, sizeof(AIO_ATTR_S));

    if (config.device_name) {
        strncpy((char*)impl_->attr.u8CardName, config.device_name, sizeof(impl_->attr.u8CardName) - 1);
    }

    // Hardware always opens with 2 channels (rv1106-acodec minimum)
    impl_->attr.soundCard.channels = 2;
    impl_->attr.soundCard.sampleRate = config.sample_rate;
    impl_->attr.soundCard.bitWidth = to_bit_width(config.bit_width);
    impl_->attr.enBitwidth = to_bit_width(config.bit_width);
    impl_->attr.enSamplerate = (AUDIO_SAMPLE_RATE_E)config.sample_rate;
    // VQE consumes the physical two-channel stream (mic + AO loopback) and
    // exposes a single processed microphone channel to callers.
    impl_->attr.enSoundmode = AUDIO_SOUND_MODE_MONO;
    impl_->attr.u32PtNumPerFrm = 1024;
    impl_->attr.u32FrmNum = 4;
    impl_->attr.u32EXFlag = 0;
    impl_->attr.u32ChnCnt = 2;

    RK_S32 ret = RK_MPI_AI_SetPubAttr(impl_->dev_id, &impl_->attr);
    if (ret != RK_SUCCESS) {
        maybe_sys_deinit();
        return false;
    }

    // Mode2 places the microphone on channel bit 0 and the digital AO
    // reference on channel bit 1, which is the layout expected below.
    ret = RK_MPI_AMIX_SetControl(impl_->dev_id, "I2STDM Digital Loopback Mode", (char*)"Mode2");
    if (ret != RK_SUCCESS) {
        AIDEN_LOG_ERROR("recording", "loopback_enable_failed", "ret=%#x", ret);
        maybe_sys_deinit();
        return false;
    }
    RK_MPI_AMIX_SetControl(impl_->dev_id, "ADC ALC Left Volume", (char*)"22");
    RK_MPI_AMIX_SetControl(impl_->dev_id, "ADC ALC Right Volume", (char*)"22");

    ret = RK_MPI_AI_Enable(impl_->dev_id);
    if (ret != RK_SUCCESS) {
        RK_MPI_AMIX_SetControl(impl_->dev_id, "I2STDM Digital Loopback Mode", (char*)"Disabled");
        maybe_sys_deinit();
        return false;
    }

    // Set channel param — s32UsrFrmDepth must be > 0 for GetFrame to work
    AI_CHN_PARAM_S chnParam;
    memset(&chnParam, 0, sizeof(AI_CHN_PARAM_S));
    chnParam.s32UsrFrmDepth = 4;
    ret = RK_MPI_AI_SetChnParam(impl_->dev_id, impl_->chn_id, &chnParam);
    if (ret != RK_SUCCESS) {
        AIDEN_LOG_ERROR("recording", "channel_config_failed", "ret=%#x", ret);
        RK_MPI_AI_Disable(impl_->dev_id);
        RK_MPI_AMIX_SetControl(impl_->dev_id, "I2STDM Digital Loopback Mode", (char*)"Disabled");
        maybe_sys_deinit();
        return false;
    }

    AI_VQE_CONFIG_S vqeConfig;
    memset(&vqeConfig, 0, sizeof(vqeConfig));
    if (access(kAudioVqeConfigPath, R_OK) != 0) {
        AIDEN_LOG_ERROR("recording", "vqe_config_missing", "path=%s", kAudioVqeConfigPath);
        RK_MPI_AI_Disable(impl_->dev_id);
        RK_MPI_AMIX_SetControl(impl_->dev_id, "I2STDM Digital Loopback Mode", (char*)"Disabled");
        maybe_sys_deinit();
        return false;
    }
    vqeConfig.enCfgMode = AIO_VQE_CONFIG_LOAD_FILE;
    snprintf(vqeConfig.aCfgFile, sizeof(vqeConfig.aCfgFile), "%s", kAudioVqeConfigPath);
    vqeConfig.s32WorkSampleRate = config.sample_rate;
    vqeConfig.s32FrameSample = config.sample_rate * 16 / 1000;
    vqeConfig.s64RecChannelType = 0x1;
    vqeConfig.s64RefChannelType = 0x2;
    vqeConfig.s64ChannelLayoutType = 0x3;
    ret = RK_MPI_AI_SetVqeAttr(impl_->dev_id, impl_->chn_id, 0, 0, &vqeConfig);
    if (ret != RK_SUCCESS) {
        AIDEN_LOG_ERROR("recording", "vqe_config_failed", "ret=%#x", ret);
        RK_MPI_AI_Disable(impl_->dev_id);
        RK_MPI_AMIX_SetControl(impl_->dev_id, "I2STDM Digital Loopback Mode", (char*)"Disabled");
        maybe_sys_deinit();
        return false;
    }

    ret = RK_MPI_AI_EnableVqe(impl_->dev_id, impl_->chn_id);
    if (ret != RK_SUCCESS) {
        AIDEN_LOG_ERROR("recording", "vqe_enable_failed", "ret=%#x", ret);
        RK_MPI_AI_Disable(impl_->dev_id);
        RK_MPI_AMIX_SetControl(impl_->dev_id, "I2STDM Digital Loopback Mode", (char*)"Disabled");
        maybe_sys_deinit();
        return false;
    }
    impl_->vqe_enabled = true;

    ret = RK_MPI_AI_EnableChn(impl_->dev_id, impl_->chn_id);
    if (ret != RK_SUCCESS) {
        RK_MPI_AI_DisableVqe(impl_->dev_id, impl_->chn_id);
        impl_->vqe_enabled = false;
        RK_MPI_AI_Disable(impl_->dev_id);
        RK_MPI_AMIX_SetControl(impl_->dev_id, "I2STDM Digital Loopback Mode", (char*)"Disabled");
        maybe_sys_deinit();
        return false;
    }

    RK_MPI_AI_SetVolume(impl_->dev_id, 100);
    RK_MPI_AI_SetTrackMode(impl_->dev_id, AUDIO_TRACK_NORMAL);

    impl_->initialized = true;
    AIDEN_LOG_INFO("recording", "vqe_enabled",
                   "sample_rate=%d frame_samples=%d rec_layout=0x1 ref_layout=0x2",
                   config.sample_rate, vqeConfig.s32FrameSample);
    return true;
}

bool AudioCapture::start(AudioStreamCallback callback) {
    if (impl_->running) return false;

    impl_->callback = callback;
    impl_->running = true;
    pthread_create(&impl_->thread, nullptr, AudioCaptureImpl::thread_func, impl_.get());
    return true;
}

void AudioCapture::stop() {
    if (!impl_->initialized) return;

    if (impl_->running) {
        impl_->running = false;
        pthread_join(impl_->thread, nullptr);
    }

    if (impl_->vqe_enabled) {
        RK_MPI_AI_DisableVqe(impl_->dev_id, impl_->chn_id);
        impl_->vqe_enabled = false;
    }
    RK_MPI_AI_DisableChn(impl_->dev_id, impl_->chn_id);
    RK_MPI_AI_Disable(impl_->dev_id);
    RK_MPI_AMIX_SetControl(impl_->dev_id, "I2STDM Digital Loopback Mode", (char*)"Disabled");

    impl_->initialized = false;
    maybe_sys_deinit();
}

bool AudioCapture::get_frame(AudioFrame& frame) {
    RK_S32 ret = RK_MPI_AI_GetFrame(impl_->dev_id, impl_->chn_id, &impl_->frame, nullptr, 500);
    if (ret == RK_SUCCESS) {
        void* data = RK_MPI_MB_Handle2VirAddr(impl_->frame.pMbBlk);
        frame.data = data;
        frame.length = impl_->frame.u32Len;
        frame.timestamp = impl_->frame.u64TimeStamp;
        frame.channels = audio_frame_channels(impl_->frame.enSoundMode, impl_->config.channels);
        frame.sample_rate = impl_->frame.s32SampleRate > 0
                                ? static_cast<uint32_t>(impl_->frame.s32SampleRate)
                                : static_cast<uint32_t>(impl_->config.sample_rate);
        return true;
    }
    return false;
}

void AudioCapture::release_frame() {
    RK_MPI_AI_ReleaseFrame(impl_->dev_id, impl_->chn_id, &impl_->frame, nullptr);
}

bool AudioCapture::is_running() const {
    return impl_->running;
}

// --- AudioPlayer ---

class AudioPlayerImpl {
public:
    AudioConfig config;
    bool initialized = false;
    int logical_volume = 100;

    AUDIO_DEV dev_id = 0;
    AO_CHN chn_id = 0;
    AIO_ATTR_S attr{};
};

AudioPlayer::AudioPlayer() : impl_(new AudioPlayerImpl()) {}
AudioPlayer::~AudioPlayer() { stop(); }

bool AudioPlayer::init(const AudioConfig& config) {
    if (impl_->initialized) {
        stop();
    }

    if (!ensure_sys_init()) {
        return false;
    }

    impl_->config = config;
    impl_->initialized = false;
    impl_->logical_volume = 0;
    impl_->dev_id = 0;
    impl_->chn_id = 0;

    bool ao_enabled = false;
    bool chn_enabled = false;
    bool resmp_enabled = false;
    auto rollback_init = [&]() {
        if (resmp_enabled) {
            RK_MPI_AO_DisableReSmp(impl_->dev_id, impl_->chn_id);
        }
        if (chn_enabled) {
            RK_MPI_AO_DisableChn(impl_->dev_id, impl_->chn_id);
        }
        if (ao_enabled) {
            RK_MPI_AO_Disable(impl_->dev_id);
        }
        impl_->initialized = false;
        impl_->logical_volume = 0;
        maybe_sys_deinit();
    };

    memset(&impl_->attr, 0, sizeof(AIO_ATTR_S));

    if (config.device_name) {
        strncpy((char*)impl_->attr.u8CardName, config.device_name, sizeof(impl_->attr.u8CardName) - 1);
    }

    // Hardware always opens with 2 channels (rv1106-acodec minimum)
    impl_->attr.soundCard.channels = 2;
    impl_->attr.soundCard.sampleRate = config.sample_rate;
    impl_->attr.soundCard.bitWidth = to_bit_width(config.bit_width);
    impl_->attr.enBitwidth = to_bit_width(config.bit_width);
    impl_->attr.enSamplerate = (AUDIO_SAMPLE_RATE_E)config.sample_rate;
    impl_->attr.enSoundmode = (config.channels == 1) ? AUDIO_SOUND_MODE_MONO : AUDIO_SOUND_MODE_STEREO;
    impl_->attr.u32PtNumPerFrm = 1024;
    impl_->attr.u32FrmNum = 4;
    impl_->attr.u32EXFlag = 0;
    impl_->attr.u32ChnCnt = 2;

    RK_S32 ret = RK_MPI_AO_SetPubAttr(impl_->dev_id, &impl_->attr);
    if (ret != RK_SUCCESS) {
        rollback_init();
        return false;
    }

    ret = RK_MPI_AO_Enable(impl_->dev_id);
    if (ret != RK_SUCCESS) {
        rollback_init();
        return false;
    }
    ao_enabled = true;

    AO_CHN_PARAM_S chnParam;
    memset(&chnParam, 0, sizeof(AO_CHN_PARAM_S));
    chnParam.enLoopbackMode = AUDIO_LOOPBACK_NONE;
    RK_MPI_AO_SetChnParams(impl_->dev_id, impl_->chn_id, &chnParam);

    if (config.channels == 1)
        RK_MPI_AO_SetTrackMode(impl_->dev_id, AUDIO_TRACK_OUT_STEREO);
    else
        RK_MPI_AO_SetTrackMode(impl_->dev_id, AUDIO_TRACK_NORMAL);

    ret = RK_MPI_AO_EnableChn(impl_->dev_id, impl_->chn_id);
    if (ret != RK_SUCCESS) {
        rollback_init();
        return false;
    }
    chn_enabled = true;

    ret = RK_MPI_AO_EnableReSmp(impl_->dev_id, impl_->chn_id,
                                (AUDIO_SAMPLE_RATE_E)config.sample_rate);
    if (ret != RK_SUCCESS) {
        rollback_init();
        return false;
    }
    resmp_enabled = true;

    if (!configure_ao_volume_curve(impl_->dev_id)) {
        rollback_init();
        return false;
    }
    if (!set_ao_mute(impl_->dev_id, false)) {
        rollback_init();
        return false;
    }
    if (RK_MPI_AO_SetVolume(impl_->dev_id, 100) != RK_SUCCESS) {
        rollback_init();
        return false;
    }
    impl_->logical_volume = 100;

    impl_->initialized = true;
    return true;
}

bool AudioPlayer::play(const void* data, uint32_t length) {
    if (!impl_->initialized) return false;

    AUDIO_FRAME_S frame{};
    frame.u32Len = length;
    frame.enBitWidth = to_bit_width(impl_->config.bit_width);
    frame.enSoundMode = (impl_->config.channels == 1) ? AUDIO_SOUND_MODE_MONO : AUDIO_SOUND_MODE_STEREO;
    frame.bBypassMbBlk = RK_FALSE;

    MB_EXT_CONFIG_S extConfig;
    memset(&extConfig, 0, sizeof(MB_EXT_CONFIG_S));
    extConfig.pOpaque = const_cast<void*>(data);
    extConfig.pu8VirAddr = (RK_U8*)data;
    extConfig.u64Size = length;

    RK_S32 ret = RK_MPI_SYS_CreateMB(&frame.pMbBlk, &extConfig);
    if (ret != RK_SUCCESS) return false;

    ret = RK_MPI_AO_SendFrame(impl_->dev_id, impl_->chn_id, &frame, 100);

    RK_MPI_MB_ReleaseMB(frame.pMbBlk);
    return ret == RK_SUCCESS;
}

bool AudioPlayer::play(const AudioFrame& frame) {
    return play(frame.data, frame.length);
}

void AudioPlayer::stop() {
    if (!impl_->initialized) return;

    RK_MPI_AO_ClearChnBuf(impl_->dev_id, impl_->chn_id);
    RK_MPI_AO_DisableReSmp(impl_->dev_id, impl_->chn_id);
    RK_MPI_AO_DisableChn(impl_->dev_id, impl_->chn_id);
    RK_MPI_AO_Disable(impl_->dev_id);

    impl_->initialized = false;
    maybe_sys_deinit();
}

void AudioPlayer::pause() {
    if (impl_->initialized)
        RK_MPI_AO_PauseChn(impl_->dev_id, impl_->chn_id);
}

void AudioPlayer::resume() {
    if (impl_->initialized)
        RK_MPI_AO_ResumeChn(impl_->dev_id, impl_->chn_id);
}

bool AudioPlayer::set_volume(int volume) {
    if (!impl_->initialized) return false;
    if (volume < 0) volume = 0;
    if (volume > 100) volume = 100;

    if (volume == 0) {
        if (!set_ao_mute(impl_->dev_id, true)) return false;
        impl_->logical_volume = 0;
        return true;
    }

    if (!set_ao_mute(impl_->dev_id, false)) return false;
    if (RK_MPI_AO_SetVolume(impl_->dev_id, volume) != RK_SUCCESS) return false;
    impl_->logical_volume = volume;
    return true;
}

int AudioPlayer::get_volume() const {
    if (!impl_->initialized) return 0;
    return impl_->logical_volume;
}

bool AudioPlayer::is_initialized() const {
    return impl_->initialized;
}

// --- CameraCapture ---

static uint32_t to_v4l2_pixel_format(const char* fmt) {
    if (!fmt || strcmp(fmt, "nv12") == 0) return V4L2_PIX_FMT_NV12;
    if (strcmp(fmt, "nv16") == 0) return V4L2_PIX_FMT_NV16;
    if (strcmp(fmt, "uyvy") == 0) return V4L2_PIX_FMT_UYVY;
    if (strcmp(fmt, "yuyv") == 0) return V4L2_PIX_FMT_YUYV;
    return V4L2_PIX_FMT_NV12;
}

struct CaptureBuffer {
    void* start = nullptr;
    size_t length = 0;
};

class CameraCaptureImpl {
public:
    std::atomic<bool> running{false};
    bool initialized = false;
    bool streaming = false;
    bool frame_held = false;
    pthread_t thread{};
    CameraConfig config;
    VideoStreamCallback callback;
    int skip_frames_remaining = 0;

    int video_fd = -1;
    bool is_mplane = false;
    enum v4l2_buf_type buf_type = V4L2_BUF_TYPE_VIDEO_CAPTURE;
    std::vector<CaptureBuffer> buffers;
    struct v4l2_buffer held_buffer{};
    struct v4l2_plane held_planes[VIDEO_MAX_PLANES]{};
    uint32_t held_bytes_used = 0;
    uint32_t held_data_offset = 0;
    uint32_t negotiated_stride = 0;
    uint32_t negotiated_size_image = 0;
    uint32_t negotiated_plane_count = 0;

    static void* thread_func(void* arg) {
        auto* self = static_cast<CameraCaptureImpl*>(arg);
        self->run();
        return nullptr;
    }

    void cleanup_buffers() {
        for (size_t i = 0; i < buffers.size(); ++i) {
            if (buffers[i].start) {
                munmap(buffers[i].start, buffers[i].length);
            }
        }
        buffers.clear();
    }

    bool queue_buffer(uint32_t index) {
        struct v4l2_buffer buf;
        struct v4l2_plane planes[VIDEO_MAX_PLANES];
        memset(&buf, 0, sizeof(buf));
        memset(planes, 0, sizeof(planes));

        buf.type = buf_type;
        buf.memory = V4L2_MEMORY_MMAP;
        buf.index = index;
        if (is_mplane) {
            buf.m.planes = planes;
            buf.length = VIDEO_MAX_PLANES;
        }
        return xioctl(video_fd, VIDIOC_QBUF, &buf) == 0;
    }

    bool start_streaming() {
        if (streaming) {
            return true;
        }
        const char* device = config.device_name ? config.device_name : "/dev/video0";
        const std::chrono::steady_clock::time_point queue_started =
            std::chrono::steady_clock::now();
        AIDEN_LOG_INFO("camera", "buffer_queue_batch_started",
                       "device=%s count=%zu", device, buffers.size());
        for (uint32_t i = 0; i < buffers.size(); ++i) {
            if (!queue_buffer(i)) {
                const int queue_errno = errno;
                const uint64_t elapsed_ms = static_cast<uint64_t>(
                    std::chrono::duration_cast<std::chrono::milliseconds>(
                        std::chrono::steady_clock::now() - queue_started).count());
                AIDEN_LOG_ERROR("camera", "buffer_requeue_failed",
                                "device=%s buffer_index=%u errno=%d error=%s elapsed_ms=%llu",
                                device, i, queue_errno, strerror(queue_errno),
                                static_cast<unsigned long long>(elapsed_ms));
                errno = queue_errno;
                return false;
            }
        }
        if (traced_xioctl(video_fd, VIDIOC_STREAMON, &buf_type,
                          "video_stream_on", device, true) < 0) {
            AIDEN_LOG_ERROR("camera", "stream_start_failed", "error=%s",
                            strerror(errno));
            return false;
        }
        skip_frames_remaining = config.skip_frames;
        streaming = true;
        return true;
    }

    bool stop_streaming() {
        if (!streaming) {
            return true;
        }
        const char* device = config.device_name ? config.device_name : "/dev/video0";
        if (traced_xioctl(video_fd, VIDIOC_STREAMOFF, &buf_type,
                          "video_stream_off", device, true) < 0) {
            AIDEN_LOG_ERROR("camera", "stream_stop_failed", "error=%s",
                            strerror(errno));
            return false;
        }
        streaming = false;
        skip_frames_remaining = config.skip_frames;
        return true;
    }

    void cleanup_device() {
        if (frame_held) {
            if (xioctl(video_fd, VIDIOC_QBUF, &held_buffer) < 0) {
                AIDEN_LOG_ERROR("camera", "held_frame_release_failed", "error=%s",
                                strerror(errno));
            }
            frame_held = false;
            held_bytes_used = 0;
            held_data_offset = 0;
        }

        stop_streaming();

        cleanup_buffers();

        if (video_fd >= 0) {
            close(video_fd);
            video_fd = -1;
        }
    }

    bool wait_for_ready(int timeout_ms) const {
        struct pollfd pfd;
        pfd.fd = video_fd;
        pfd.events = POLLIN | POLLERR;
        pfd.revents = 0;

        while (true) {
            int ret = poll(&pfd, 1, timeout_ms);
            if (ret > 0) {
                return true;
            }
            if (ret == 0) {
                errno = ETIMEDOUT;
                return false;
            }
            if (errno != EINTR) {
                return false;
            }
        }
    }

    void fill_video_frame(VideoFrame& frame_info, const struct v4l2_buffer& raw_buffer) const {
        const CaptureBuffer& capture_buffer = buffers[raw_buffer.index];
        frame_info.data = static_cast<uint8_t*>(capture_buffer.start) + held_data_offset;
        frame_info.width = static_cast<uint32_t>(config.width);
        frame_info.height = static_cast<uint32_t>(config.height);
        const uint32_t reported_length = held_bytes_used
            ? held_bytes_used
            : frame_size_bytes(config.pixel_format,
                               static_cast<uint32_t>(config.width),
                               static_cast<uint32_t>(config.height));
        const size_t buffer_capacity = capture_buffer.length - held_data_offset;
        frame_info.length = reported_length > buffer_capacity
            ? static_cast<uint32_t>(buffer_capacity)
            : reported_length;
        frame_info.stride = negotiated_stride
            ? negotiated_stride
            : (strcmp(config.pixel_format, "nv12") == 0
                   ? static_cast<uint32_t>(config.width)
                   : static_cast<uint32_t>(config.width) * 2U);
        frame_info.size_image = negotiated_size_image >= held_data_offset
            ? negotiated_size_image - held_data_offset
            : frame_info.length;
        frame_info.plane_count = negotiated_plane_count ? negotiated_plane_count : 1;
        frame_info.buffer_capacity = buffer_capacity;
        frame_info.timestamp =
            static_cast<uint64_t>(raw_buffer.timestamp.tv_sec) * 1000000ULL +
            static_cast<uint64_t>(raw_buffer.timestamp.tv_usec);
        frame_info.sequence = raw_buffer.sequence;
    }

    bool acquire_frame(VideoFrame* frame_info, int timeout_ms) {
        if (!streaming) {
            errno = EPIPE;
            return false;
        }
        while (true) {
            if (!wait_for_ready(timeout_ms)) {
                return false;
            }

            struct v4l2_buffer raw_buffer;
            struct v4l2_plane raw_planes[VIDEO_MAX_PLANES];
            memset(&raw_buffer, 0, sizeof(raw_buffer));
            memset(raw_planes, 0, sizeof(raw_planes));

            raw_buffer.type = buf_type;
            raw_buffer.memory = V4L2_MEMORY_MMAP;
            if (is_mplane) {
                raw_buffer.m.planes = raw_planes;
                raw_buffer.length = VIDEO_MAX_PLANES;
            }

            if (xioctl(video_fd, VIDIOC_DQBUF, &raw_buffer) < 0) {
                if (errno == EAGAIN) {
                    continue;
                }
                return false;
            }

            if (raw_buffer.index >= buffers.size()) {
                errno = EIO;
                return false;
            }

            const uint32_t data_offset = is_mplane ? raw_planes[0].data_offset : 0;
            const uint32_t raw_bytes_used = is_mplane ? raw_planes[0].bytesused
                                                      : raw_buffer.bytesused;
            CapturePayloadBounds payload_bounds;
            if (!capture_payload_bounds(buffers[raw_buffer.index].length,
                                        raw_bytes_used,
                                        data_offset,
                                        negotiated_size_image,
                                        &payload_bounds) ||
                payload_bounds.payload_length > std::numeric_limits<uint32_t>::max()) {
                if (xioctl(video_fd, VIDIOC_QBUF, &raw_buffer) < 0) {
                    return false;
                }
                errno = EIO;
                return false;
            }

            if (skip_frames_remaining > 0) {
                --skip_frames_remaining;
                if (xioctl(video_fd, VIDIOC_QBUF, &raw_buffer) < 0) {
                    return false;
                }
                continue;
            }

            memset(&held_buffer, 0, sizeof(held_buffer));
            held_bytes_used = static_cast<uint32_t>(payload_bounds.payload_length);
            held_data_offset = static_cast<uint32_t>(payload_bounds.data_offset);
            if (is_mplane) {
                memcpy(held_planes, raw_planes, sizeof(raw_planes));
            }
            held_buffer = raw_buffer;
            if (is_mplane) {
                held_buffer.m.planes = held_planes;
            }
            frame_held = true;

            if (frame_info) {
                fill_video_frame(*frame_info, held_buffer);
            }
            return true;
        }
    }

    void run() {
        while (running) {
            VideoFrame frame_info{};

            if (!acquire_frame(&frame_info, 500)) {
                if (errno == ETIMEDOUT || errno == EAGAIN) {
                    continue;
                }
                usleep(1000);
                continue;
            }

            if (callback) {
                callback(frame_info);
            }

            if (frame_held && xioctl(video_fd, VIDIOC_QBUF, &held_buffer) < 0) {
                AIDEN_LOG_ERROR("camera", "buffer_requeue_failed", "error=%s",
                                strerror(errno));
            }
            frame_held = false;
            held_bytes_used = 0;
            held_data_offset = 0;
        }
    }
};

CameraCapture::CameraCapture() : impl_(new CameraCaptureImpl()) {}
CameraCapture::~CameraCapture() { stop(); }

bool CameraCapture::init(const CameraConfig& config) {
    if (impl_->initialized) {
        stop();
    }

    impl_->config = config;
    impl_->skip_frames_remaining = config.skip_frames;
    impl_->frame_held = false;
    impl_->held_bytes_used = 0;
    impl_->held_data_offset = 0;
    impl_->negotiated_stride = 0;
    impl_->negotiated_size_image = 0;
    impl_->negotiated_plane_count = 0;
    impl_->streaming = false;
    impl_->video_fd = -1;
    impl_->buffers.clear();

    const char* device = config.device_name ? config.device_name : "/dev/video0";
    const char* subdev = config.subdev_device ? config.subdev_device : "/dev/v4l-subdev2";
    const std::chrono::steady_clock::time_point initialization_started =
        std::chrono::steady_clock::now();
    const char* initialization_stage = "configuration";
    AIDEN_LOG_INFO("camera", "initialization_started",
                   "device=%s subdev=%s pixel_format=%s width=%d height=%d hdmi_sync=%d force_trigger=%d",
                   device, subdev, config.pixel_format ? config.pixel_format : "",
                   config.width, config.height, config.enable_hdmi_sync ? 1 : 0,
                   config.force_trigger ? 1 : 0);

    auto fail = [this, &initialization_stage, &initialization_started, device, subdev]() {
        const int failure_errno = errno;
        const uint64_t elapsed_ms = static_cast<uint64_t>(
            std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::steady_clock::now() - initialization_started).count());
        AIDEN_LOG_WARN("camera", "initialization_completed",
                       "device=%s subdev=%s ok=0 stage=%s errno=%d error=%s elapsed_ms=%llu",
                       device, subdev, initialization_stage, failure_errno,
                       failure_errno == 0 ? "not_set" : strerror(failure_errno),
                       static_cast<unsigned long long>(elapsed_ms));
        impl_->cleanup_device();
        impl_->initialized = false;
        errno = failure_errno;
        return false;
    };

    uint32_t synced_width = static_cast<uint32_t>(config.width);
    uint32_t synced_height = static_cast<uint32_t>(config.height);
    initialization_stage = "hdmi_sync";
    if (!sync_hdmi_input(config, &synced_width, &synced_height)) {
        return fail();
    }
    impl_->config.width = static_cast<int>(synced_width);
    impl_->config.height = static_cast<int>(synced_height);

    initialization_stage = "video_open";
    AIDEN_LOG_INFO("camera", "device_open_started",
                   "device=%s", device);
    errno = 0;
    impl_->video_fd = open(device, O_RDWR | O_NONBLOCK);
    if (impl_->video_fd < 0) {
        AIDEN_LOG_ERROR("camera", "device_open_failed", "device=%s error=%s", device,
                        strerror(errno));
        return fail();
    }
    AIDEN_LOG_INFO("camera", "device_open_completed",
                   "device=%s fd=%d", device, impl_->video_fd);

    struct v4l2_capability cap;
    memset(&cap, 0, sizeof(cap));
    initialization_stage = "video_query_capabilities";
    if (traced_xioctl(impl_->video_fd, VIDIOC_QUERYCAP, &cap,
                      "video_query_capabilities", device) < 0) {
        AIDEN_LOG_ERROR("camera", "capability_query_failed", "device=%s error=%s", device,
                        strerror(errno));
        return fail();
    }

    uint32_t capabilities = (cap.capabilities & V4L2_CAP_DEVICE_CAPS)
        ? cap.device_caps
        : cap.capabilities;

    if (capabilities & V4L2_CAP_VIDEO_CAPTURE_MPLANE) {
        impl_->is_mplane = true;
        impl_->buf_type = V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE;
    } else if (capabilities & V4L2_CAP_VIDEO_CAPTURE) {
        impl_->is_mplane = false;
        impl_->buf_type = V4L2_BUF_TYPE_VIDEO_CAPTURE;
    } else {
        AIDEN_LOG_ERROR("camera", "capture_capability_missing", "device=%s", device);
        errno = ENODEV;
        return fail();
    }

    struct v4l2_format fmt;
    memset(&fmt, 0, sizeof(fmt));
    fmt.type = impl_->buf_type;
    if (impl_->is_mplane) {
        fmt.fmt.pix_mp.width = synced_width;
        fmt.fmt.pix_mp.height = synced_height;
        fmt.fmt.pix_mp.pixelformat = to_v4l2_pixel_format(config.pixel_format);
        fmt.fmt.pix_mp.field = V4L2_FIELD_NONE;
    } else {
        fmt.fmt.pix.width = synced_width;
        fmt.fmt.pix.height = synced_height;
        fmt.fmt.pix.pixelformat = to_v4l2_pixel_format(config.pixel_format);
        fmt.fmt.pix.field = V4L2_FIELD_NONE;
    }

    initialization_stage = "video_set_format";
    if (traced_xioctl(impl_->video_fd, VIDIOC_S_FMT, &fmt,
                      "video_set_format", device) < 0) {
        AIDEN_LOG_ERROR("camera", "format_set_failed", "device=%s error=%s", device,
                        strerror(errno));
        return fail();
    }

    if (impl_->is_mplane) {
        impl_->negotiated_plane_count = fmt.fmt.pix_mp.num_planes;
        impl_->negotiated_stride = fmt.fmt.pix_mp.plane_fmt[0].bytesperline;
        impl_->negotiated_size_image = fmt.fmt.pix_mp.plane_fmt[0].sizeimage;
    } else {
        impl_->negotiated_plane_count = 1;
        impl_->negotiated_stride = fmt.fmt.pix.bytesperline;
        impl_->negotiated_size_image = fmt.fmt.pix.sizeimage;
    }
    if (impl_->negotiated_plane_count != 1 || impl_->negotiated_stride == 0 ||
        impl_->negotiated_size_image == 0) {
        AIDEN_LOG_ERROR("camera", "unsupported_capture_layout",
                        "device=%s planes=%u stride=%u size_image=%u",
                        device, impl_->negotiated_plane_count,
                        impl_->negotiated_stride, impl_->negotiated_size_image);
        errno = EINVAL;
        return fail();
    }

    const uint32_t requested_format = to_v4l2_pixel_format(config.pixel_format);
    const uint32_t actual_format = impl_->is_mplane
        ? fmt.fmt.pix_mp.pixelformat
        : fmt.fmt.pix.pixelformat;
    if (actual_format != requested_format) {
        char requested_text[5];
        char actual_text[5];
        AIDEN_LOG_WARN("camera", "pixel_format_mismatch",
                       "device=%s actual_format=%s requested_format=%s", device,
                       v4l2_format_name(actual_format, actual_text),
                       v4l2_format_name(requested_format, requested_text));
        errno = EINVAL;
        return fail();
    }

    if (impl_->is_mplane) {
        impl_->config.width = static_cast<int>(fmt.fmt.pix_mp.width);
        impl_->config.height = static_cast<int>(fmt.fmt.pix_mp.height);
    } else {
        impl_->config.width = static_cast<int>(fmt.fmt.pix.width);
        impl_->config.height = static_cast<int>(fmt.fmt.pix.height);
    }

    if (config.require_exact_resolution &&
        (impl_->config.width != static_cast<int>(synced_width) ||
         impl_->config.height != static_cast<int>(synced_height))) {
        AIDEN_LOG_ERROR("camera", "resolution_mismatch",
                        "device=%s actual_width=%d actual_height=%d expected_width=%u expected_height=%u",
                        device, impl_->config.width, impl_->config.height,
                        synced_width, synced_height);
        errno = EINVAL;
        return fail();
    }

    struct v4l2_requestbuffers req;
    memset(&req, 0, sizeof(req));
    req.count = 4;
    req.type = impl_->buf_type;
    req.memory = V4L2_MEMORY_MMAP;
    initialization_stage = "video_request_buffers";
    if (traced_xioctl(impl_->video_fd, VIDIOC_REQBUFS, &req,
                      "video_request_buffers", device) < 0) {
        AIDEN_LOG_ERROR("camera", "buffer_request_failed", "device=%s error=%s", device,
                        strerror(errno));
        return fail();
    }
    if (req.count == 0) {
        AIDEN_LOG_ERROR("camera", "capture_buffers_missing", "device=%s", device);
        errno = ENOMEM;
        return fail();
    }

    impl_->buffers.resize(req.count);
    for (uint32_t i = 0; i < req.count; ++i) {
        struct v4l2_buffer buf;
        struct v4l2_plane planes[VIDEO_MAX_PLANES];
        memset(&buf, 0, sizeof(buf));
        memset(planes, 0, sizeof(planes));

        buf.type = impl_->buf_type;
        buf.memory = V4L2_MEMORY_MMAP;
        buf.index = i;
        if (impl_->is_mplane) {
            buf.m.planes = planes;
            buf.length = VIDEO_MAX_PLANES;
        }

        initialization_stage = "video_query_buffer";
        if (traced_xioctl(impl_->video_fd, VIDIOC_QUERYBUF, &buf,
                          "video_query_buffer", device) < 0) {
            AIDEN_LOG_ERROR("camera", "buffer_query_failed",
                            "device=%s buffer_index=%u error=%s", device, i,
                            strerror(errno));
            return fail();
        }

        size_t length = impl_->is_mplane ? planes[0].length : buf.length;
        off_t offset = impl_->is_mplane ? planes[0].m.mem_offset : buf.m.offset;
        void* start = mmap(nullptr, length, PROT_READ | PROT_WRITE, MAP_SHARED, impl_->video_fd, offset);
        if (start == MAP_FAILED) {
            AIDEN_LOG_ERROR("camera", "buffer_map_failed",
                            "device=%s buffer_index=%u error=%s", device, i,
                            strerror(errno));
            return fail();
        }

        impl_->buffers[i].start = start;
        impl_->buffers[i].length = length;

    }

    initialization_stage = "video_stream_on";
    if (!impl_->start_streaming()) {
        AIDEN_LOG_ERROR("camera", "stream_start_failed", "device=%s error=%s",
                        device, strerror(errno));
        return fail();
    }

    impl_->initialized = true;
    const uint64_t elapsed_ms = static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now() - initialization_started).count());
    AIDEN_LOG_INFO("camera", "initialization_completed",
                   "device=%s subdev=%s ok=1 stage=ready width=%d height=%d buffers=%zu elapsed_ms=%llu",
                   device, subdev, impl_->config.width, impl_->config.height,
                   impl_->buffers.size(),
                   static_cast<unsigned long long>(elapsed_ms));
    return true;
}

bool CameraCapture::start(VideoStreamCallback callback) {
    if (!impl_->initialized || impl_->running || impl_->frame_held) return false;

    impl_->callback = callback;
    impl_->running = true;
    pthread_create(&impl_->thread, nullptr, CameraCaptureImpl::thread_func, impl_.get());
    return true;
}

void CameraCapture::stop() {
    if (!impl_->initialized) return;

    if (impl_->running) {
        impl_->running = false;
        pthread_join(impl_->thread, nullptr);
    }

    impl_->cleanup_device();
    impl_->initialized = false;
}

bool CameraCapture::pause() {
    if (!impl_->initialized || impl_->running || impl_->frame_held) {
        return false;
    }
    return impl_->stop_streaming();
}

bool CameraCapture::resume() {
    if (!impl_->initialized || impl_->running || impl_->frame_held) {
        return false;
    }
    return impl_->start_streaming();
}

bool CameraCapture::get_frame(VideoFrame& frame) {
    if (!impl_->initialized || impl_->running || impl_->frame_held) {
        return false;
    }

    return impl_->acquire_frame(&frame, -1);
}

bool CameraCapture::capture_frame(VideoFrame& frame, std::vector<uint8_t>& buffer) {
    return capture_frame_timeout(frame, buffer, -1);
}

bool CameraCapture::capture_frame_timeout(VideoFrame& frame, std::vector<uint8_t>& buffer, int timeout_ms) {
    if (!impl_->initialized || impl_->running) {
        return false;
    }

    CameraConfig retry_config = impl_->config;
    const int max_attempts = retry_config.capture_retries + 1;

    for (int attempt = 0; attempt < max_attempts; ++attempt) {
        if (!impl_->initialized) {
            if (!init(retry_config)) {
                AIDEN_LOG_WARN("camera", "capture_init_retry_failed",
                               "attempt=%d max_attempts=%d error=%s", attempt + 1,
                               max_attempts, strerror(errno));
                continue;
            }
        }

        bool uniform_reject = false;
        if (!impl_->acquire_frame(&frame, timeout_ms)) {
            AIDEN_LOG_WARN("camera", "frame_acquire_failed",
                           "attempt=%d max_attempts=%d error=%s", attempt + 1,
                           max_attempts, strerror(errno));
        } else {
            if (retry_config.pixel_format &&
                strcmp(retry_config.pixel_format, "nv12") == 0) {
                std::vector<uint8_t> compact;
                const uint32_t stride = frame.stride ? frame.stride : frame.width;
                const size_t readable = std::min<size_t>(
                    frame.buffer_capacity, static_cast<size_t>(frame.length));
                if (!compact_nv12(static_cast<const uint8_t*>(frame.data), readable,
                                  frame.width, frame.height, stride, &compact)) {
                    AIDEN_LOG_WARN("camera", "nv12_compact_failed",
                                   "width=%u height=%u stride=%u bytes_used=%u size_image=%u mapped=%llu",
                                   frame.width, frame.height, stride, frame.length,
                                   frame.size_image,
                                   static_cast<unsigned long long>(frame.buffer_capacity));
                    release_frame();
                    continue;
                }
                release_frame();
                buffer.swap(compact);
                frame.length = static_cast<uint32_t>(buffer.size());
                frame.stride = frame.width;
                frame.size_image = frame.length;
                frame.plane_count = 1;
                frame.buffer_capacity = frame.length;
            } else {
                if (frame.length > frame.buffer_capacity) {
                    AIDEN_LOG_WARN("camera", "frame_length_invalid",
                                   "length=%u mapped=%llu", frame.length,
                                   static_cast<unsigned long long>(frame.buffer_capacity));
                    release_frame();
                    continue;
                }
                buffer.resize(frame.length);
                memcpy(buffer.data(), frame.data, frame.length);
                release_frame();
            }

            if (!frame_looks_invalid(retry_config, buffer.data(), buffer.size())) {
                frame.data = buffer.data();
                return true;
            }

            uniform_reject = true;
            AIDEN_LOG_WARN("camera", "uniform_frame_rejected",
                           "attempt=%d max_attempts=%d", attempt + 1, max_attempts);
        }

        if (impl_->frame_held) {
            release_frame();
        }
        if (attempt + 1 >= max_attempts) {
            break;
        }

        if (!uniform_reject) {
            stop();
        }
    }

    return false;
}

bool CameraCapture::discard_frame_timeout(int timeout_ms) {
    if (!impl_->initialized || impl_->running) {
        return false;
    }

    CameraConfig retry_config = impl_->config;
    const int max_attempts = retry_config.capture_retries + 1;

    for (int attempt = 0; attempt < max_attempts; ++attempt) {
        if (!impl_->initialized) {
            if (!init(retry_config)) {
                AIDEN_LOG_WARN("camera", "discard_init_retry_failed",
                               "attempt=%d max_attempts=%d error=%s", attempt + 1,
                               max_attempts, strerror(errno));
                continue;
            }
        }

        VideoFrame frame{};
        if (!impl_->acquire_frame(&frame, timeout_ms)) {
            AIDEN_LOG_WARN("camera", "frame_discard_failed",
                           "attempt=%d max_attempts=%d error=%s", attempt + 1,
                           max_attempts, strerror(errno));
        } else {
            release_frame();
            return true;
        }

        if (impl_->frame_held) {
            release_frame();
        }
        if (attempt + 1 >= max_attempts) {
            break;
        }

        stop();
    }

    return false;
}

bool CameraCapture::capture_once(const CameraConfig& config,
                                 VideoFrame& frame,
                                 std::vector<uint8_t>& buffer) {
    const int max_attempts = config.capture_retries + 1;

    stop();
    for (int attempt = 0; attempt < max_attempts; ++attempt) {
        if (!init(config)) {
            AIDEN_LOG_WARN("camera", "init_retry_failed",
                           "attempt=%d max_attempts=%d error=%s", attempt + 1,
                           max_attempts, strerror(errno));
        } else if (capture_frame(frame, buffer)) {
            stop();
            return true;
        }

        stop();
    }

    return false;
}

void CameraCapture::release_frame() {
    if (!impl_->frame_held) {
        return;
    }

    if (xioctl(impl_->video_fd, VIDIOC_QBUF, &impl_->held_buffer) < 0) {
        AIDEN_LOG_ERROR("camera", "frame_release_failed", "error=%s", strerror(errno));
    }
    impl_->frame_held = false;
    impl_->held_bytes_used = 0;
    impl_->held_data_offset = 0;
}

bool CameraCapture::is_running() const {
    return impl_->running;
}

}  // namespace aiden
