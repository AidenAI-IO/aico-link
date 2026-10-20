#include "frame_camera_capture_source.h"
#include "aiden_log.h"
#include "rk628_hdmi_mode.h"
#include <chrono>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <limits>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/file.h>
#include <unistd.h>

namespace aiden {

static std::string subdev_sysfs_device_path(const std::string& subdev_device) {
    const char* basename = strrchr(subdev_device.c_str(), '/');
    basename = basename ? basename + 1 : subdev_device.c_str();
    if (basename[0] == '\0') {
        return std::string();
    }

    char link_path[PATH_MAX];
    snprintf(link_path, sizeof(link_path), "/sys/class/video4linux/%s/device", basename);
    char resolved[PATH_MAX];
    if (!realpath(link_path, resolved)) {
        return std::string();
    }
    return resolved;
}

FrameCameraCaptureSource::FrameCameraCaptureSource(const CameraConfig& config)
    : config_(config),
      device_name_(config.device_name ? config.device_name : "/dev/video0"),
      pixel_format_(config.pixel_format ? config.pixel_format : "nv12"),
      subdev_device_(config.subdev_device && strcmp(config.subdev_device, "auto") != 0
                         ? config.subdev_device : ""),
      edid_path_(config.edid_path ? config.edid_path : ""),
      auto_subdev_(!config.subdev_device || strcmp(config.subdev_device, "auto") == 0),
      auto_edid_(!config.edid_path),
      auto_force_trigger_pending_(auto_subdev_),
      // Automatic discovery may need an occasional bounded HPD/EDID retry
      // while a hot-plugged source is settling.  An explicit --force-trigger
      // keeps its historical one-shot behaviour instead of silently turning
      // into a periodic EDID writer.
      periodic_force_enabled_(auto_subdev_),
      tc_open_attempts_(0),
      open_attempts_(0),
      force_trigger_pending_(config.force_trigger),
      lock_fd_(-1) {
    sync_config_strings();
}

std::string FrameCameraCaptureSource::detect_hdmi_subdev(std::string* name) {
    if (name) {
        name->clear();
    }
    DIR* directory = opendir("/sys/class/video4linux");
    if (!directory) {
        return std::string();
    }

    std::string result;
    struct dirent* entry = nullptr;
    while ((entry = readdir(directory)) != nullptr) {
        if (strncmp(entry->d_name, "v4l-subdev", 10) != 0) {
            continue;
        }
        char name_path[256];
        snprintf(name_path, sizeof(name_path), "/sys/class/video4linux/%s/name", entry->d_name);
        FILE* file = fopen(name_path, "r");
        if (!file) {
            continue;
        }
        char buffer[256] = {};
        if (!fgets(buffer, sizeof(buffer), file)) {
            fclose(file);
            continue;
        }
        fclose(file);
        buffer[strcspn(buffer, "\r\n")] = '\0';
        if (!strstr(buffer, "rk628-csi") && !strstr(buffer, "tc358743")) {
            continue;
        }
        result = std::string("/dev/") + entry->d_name;
        if (name) {
            *name = buffer;
        }
        break;
    }
    closedir(directory);
    return result;
}

void FrameCameraCaptureSource::sync_config_strings() {
    config_.device_name = device_name_.c_str();
    config_.pixel_format = pixel_format_.c_str();
    config_.subdev_device = auto_subdev_ && subdev_device_.empty()
        ? nullptr : subdev_device_.c_str();
    config_.edid_path = edid_path_.empty() ? nullptr : edid_path_.c_str();
}

bool FrameCameraCaptureSource::open() {
    const uint64_t open_attempt = ++open_attempts_;
    const std::chrono::steady_clock::time_point open_started =
        std::chrono::steady_clock::now();
    AIDEN_LOG_INFO("camera_source", "open_started",
                   "attempt=%llu video_device=%s configured_subdev=%s auto_subdev=%d pixel_format=%s width=%d height=%d",
                   static_cast<unsigned long long>(open_attempt),
                   device_name_.c_str(), subdev_device_.c_str(),
                   auto_subdev_ ? 1 : 0, pixel_format_.c_str(),
                   config_.width, config_.height);
    std::string bridge_name;
    config_.allow_edid_fallback = !periodic_force_enabled_;
    if (auto_subdev_) {
        subdev_device_ = detect_hdmi_subdev(&bridge_name);
        if (subdev_device_.empty()) {
            prepared_mode_subdev_.clear();
            AIDEN_LOG_WARN("hdmi", "bridge_not_ready",
                           "attempt=%llu frame capture will retry until an HDMI bridge appears",
                           static_cast<unsigned long long>(open_attempt));
            const uint64_t elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - open_started).count());
            AIDEN_LOG_WARN("camera_source", "open_completed",
                           "attempt=%llu ok=0 phase=bridge_discovery elapsed_ms=%llu",
                           static_cast<unsigned long long>(open_attempt),
                           static_cast<unsigned long long>(elapsed_ms));
            return false;
        }

        // Keep the bridge-aware policy when the kernel registers the subdevice
        // after frame_service has already started.  RK628D uses its driver
        // EDID, while TC358743 needs the HPD/EDID renegotiation path.
    } else {
        char name_path[256];
        snprintf(name_path, sizeof(name_path), "/sys/class/video4linux/%s/name",
                 strrchr(subdev_device_.c_str(), '/')
                     ? strrchr(subdev_device_.c_str(), '/') + 1 : subdev_device_.c_str());
        FILE* file = fopen(name_path, "r");
        if (file) {
            char buffer[256] = {};
            if (fgets(buffer, sizeof(buffer), file)) {
                buffer[strcspn(buffer, "\r\n")] = '\0';
                bridge_name = buffer;
            }
            fclose(file);
        }
    }
    const std::string bridge_sysfs_device =
        subdev_sysfs_device_path(subdev_device_);
    AIDEN_LOG_INFO("camera_source", "bridge_selected",
                   "attempt=%llu subdev=%s bridge_name=%s sysfs_device=%s",
                   static_cast<unsigned long long>(open_attempt),
                   subdev_device_.c_str(),
                   bridge_name.empty() ? "unknown" : bridge_name.c_str(),
                   bridge_sysfs_device.empty() ? "unknown"
                                               : bridge_sysfs_device.c_str());

    if (bridge_name.find("tc358743") != std::string::npos) {
        ++tc_open_attempts_;
        // Try HPD/EDID immediately and then at a bounded cadence.  This
        // handles a TC358743 that becomes usable only after the source is
        // plugged in, without writing the bridge on every recovery loop.
        const bool first_force = auto_force_trigger_pending_;
        auto_force_trigger_pending_ = false;
        const bool periodic_force = periodic_force_enabled_ &&
            (tc_open_attempts_ % 6U) == 1U;
        config_.force_trigger = force_trigger_pending_ || first_force || periodic_force;
        // A failed TC358743 timing probe must not silently fall through to an
        // EDID write on every recovery loop.  Only the bounded force attempt
        // below is allowed to perform that renegotiation.
        config_.allow_edid_fallback = false;
        if (edid_path_.empty()) {
            const char* default_edid = "/usr/share/aiden/edid/hdmi_1080p30_cta.hex";
            if (access(default_edid, R_OK) == 0) {
                edid_path_ = default_edid;
            }
        }
    } else if (auto_subdev_) {
        auto_force_trigger_pending_ = false;
        config_.force_trigger = force_trigger_pending_;
        config_.allow_edid_fallback = false;
        if (auto_edid_ && bridge_name.find("rk628-csi") == std::string::npos) {
            edid_path_.clear();
        }
    } else {
        config_.force_trigger = force_trigger_pending_;
        config_.allow_edid_fallback = true;
    }
    if (config_.force_trigger) {
        config_.allow_edid_fallback = true;
    }
    sync_config_strings();

    const bool force_attempt = config_.force_trigger;
    AIDEN_LOG_INFO("camera_source", "policy_resolved",
                   "attempt=%llu bridge_name=%s force_trigger=%d allow_edid_fallback=%d edid_path=%s",
                   static_cast<unsigned long long>(open_attempt),
                   bridge_name.empty() ? "unknown" : bridge_name.c_str(),
                   config_.force_trigger ? 1 : 0,
                   config_.allow_edid_fallback ? 1 : 0,
                   edid_path_.empty() ? "" : edid_path_.c_str());

    // Keep device ownership inside the recoverable capture source.  If
    // /dev/video0 is temporarily absent or held by a diagnostic process, the
    // IPC service remains alive and the manager retries instead of terminating
    // the entire systemd service.
    if (lock_fd_ < 0) {
        AIDEN_LOG_INFO("camera", "device_lock_open_started",
                       "attempt=%llu device=%s",
                       static_cast<unsigned long long>(open_attempt),
                       device_name_.c_str());
        const std::chrono::steady_clock::time_point lock_open_started =
            std::chrono::steady_clock::now();
        lock_fd_ = ::open(device_name_.c_str(), O_RDONLY | O_CLOEXEC);
        if (lock_fd_ < 0) {
            const int open_errno = errno;
            AIDEN_LOG_WARN("camera", "device_lock_open_pending",
                           "attempt=%llu device=%s errno=%d error=%s",
                           static_cast<unsigned long long>(open_attempt),
                           device_name_.c_str(), open_errno, strerror(open_errno));
            const uint64_t elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - open_started).count());
            AIDEN_LOG_WARN("camera_source", "open_completed",
                           "attempt=%llu ok=0 phase=device_lock_open elapsed_ms=%llu errno=%d",
                           static_cast<unsigned long long>(open_attempt),
                           static_cast<unsigned long long>(elapsed_ms), open_errno);
            errno = open_errno;
            return false;
        }
        const uint64_t lock_open_elapsed_ms = static_cast<uint64_t>(
            std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::steady_clock::now() - lock_open_started).count());
        AIDEN_LOG_INFO("camera", "device_lock_open_completed",
                       "attempt=%llu device=%s fd=%d elapsed_ms=%llu",
                       static_cast<unsigned long long>(open_attempt),
                       device_name_.c_str(), lock_fd_,
                       static_cast<unsigned long long>(lock_open_elapsed_ms));
        if (flock(lock_fd_, LOCK_EX | LOCK_NB) < 0) {
            const int lock_errno = errno;
            AIDEN_LOG_WARN("camera", "device_lock_pending",
                           "attempt=%llu device=%s errno=%d error=%s",
                           static_cast<unsigned long long>(open_attempt),
                           device_name_.c_str(), lock_errno, strerror(lock_errno));
            ::close(lock_fd_);
            lock_fd_ = -1;
            const uint64_t elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - open_started).count());
            AIDEN_LOG_WARN("camera_source", "open_completed",
                           "attempt=%llu ok=0 phase=device_lock elapsed_ms=%llu errno=%d",
                           static_cast<unsigned long long>(open_attempt),
                           static_cast<unsigned long long>(elapsed_ms), lock_errno);
            errno = lock_errno;
            return false;
        }
    }

    // Apply only after discovery and device ownership, including a bridge
    // registered after service startup. Recovery keeps the accepted EDID;
    // it must not repeatedly interrupt the source's negotiation.
    if (auto_edid_ && bridge_name.find("rk628-csi") != std::string::npos &&
        prepared_mode_subdev_ != subdev_device_) {
        const int fd = ::open(subdev_device_.c_str(), O_RDWR | O_CLOEXEC);
        const bool prepared = fd >= 0 && prepare_rk628_hdmi_mode(
            fd, "/userdata/system/hdmi-mode", "/usr/share/aiden/hdmi", &edid_path_);
        const int prepare_errno = errno;
        if (fd >= 0) {
            ::close(fd);
        }
        if (!prepared) {
            ::close(lock_fd_);
            lock_fd_ = -1;
            errno = prepare_errno;
            return false;
        }
        prepared_mode_subdev_ = subdev_device_;
        sync_config_strings();
    }

    AIDEN_LOG_INFO("camera_source", "camera_init_started",
                   "attempt=%llu video_device=%s subdev=%s",
                   static_cast<unsigned long long>(open_attempt),
                   device_name_.c_str(), subdev_device_.c_str());
    errno = 0;
    const bool opened = camera_.init(config_);
    const int init_errno = opened ? 0 : errno;
    const uint64_t elapsed_ms = static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now() - open_started).count());
    if (force_attempt) {
        // A failed force-trigger is recoverable.  Do not repeat HPD/EDID
        // writes on every retry; auto-subdevice periodically schedules another
        // bounded attempt for TC358743 when needed.
        force_trigger_pending_ = false;
    }
    if (opened) {
        force_trigger_pending_ = false;
        if (auto_subdev_) {
            auto_force_trigger_pending_ = false;
        }
    } else {
        ::close(lock_fd_);
        lock_fd_ = -1;
    }
    if (opened) {
        AIDEN_LOG_INFO("camera_source", "open_completed",
                       "attempt=%llu ok=1 phase=ready elapsed_ms=%llu lock_fd=%d",
                       static_cast<unsigned long long>(open_attempt),
                       static_cast<unsigned long long>(elapsed_ms), lock_fd_);
    } else {
        AIDEN_LOG_WARN("camera_source", "open_completed",
                       "attempt=%llu ok=0 phase=camera_init elapsed_ms=%llu errno=%d error=%s",
                       static_cast<unsigned long long>(open_attempt),
                       static_cast<unsigned long long>(elapsed_ms), init_errno,
                       init_errno == 0 ? "not_set" : strerror(init_errno));
        errno = init_errno;
    }
    return opened;
}

bool FrameCameraCaptureSource::pause() {
    return camera_.pause();
}

bool FrameCameraCaptureSource::resume() {
    return camera_.resume();
}

bool FrameCameraCaptureSource::capture(CapturedFrame* frame) {
    if (!frame) {
        return false;
    }

    VideoFrame video_frame{};
    std::vector<uint8_t> buffer;
    if (!camera_.capture_frame_timeout(video_frame, buffer, 500)) {
        return false;
    }

    frame->metadata.capture_ts_ns = video_frame.timestamp * 1000ULL;
    frame->metadata.width = video_frame.width;
    frame->metadata.height = video_frame.height;
    frame->metadata.pixel_format = pixel_format_;
    frame->metadata.bytes = buffer.size();
    if (pixel_format_ == "nv12") {
        frame->metadata.stride = video_frame.width;
    } else if (video_frame.stride > 0) {
        frame->metadata.stride = video_frame.stride;
    } else if (pixel_format_ == "uyvy" || pixel_format_ == "yuyv" || pixel_format_ == "nv16") {
        frame->metadata.stride = video_frame.width * 2U;
    } else {
        frame->metadata.stride = video_frame.width;
    }
    frame->metadata.planes.clear();
    if (pixel_format_ == "nv12" || pixel_format_ == "nv16") {
        const uint64_t y_bytes_u64 = static_cast<uint64_t>(video_frame.width) *
            video_frame.height;
        const uint64_t uv_bytes_u64 = pixel_format_ == "nv12"
            ? y_bytes_u64 / 2U : y_bytes_u64;
        if (y_bytes_u64 > std::numeric_limits<uint32_t>::max() ||
            uv_bytes_u64 > std::numeric_limits<uint32_t>::max() ||
            y_bytes_u64 + uv_bytes_u64 > buffer.size()) {
            AIDEN_LOG_ERROR("camera", "frame_layout_incomplete",
                            "format=%s width=%u height=%u bytes=%zu expected=%llu",
                            pixel_format_.c_str(), video_frame.width, video_frame.height,
                            buffer.size(),
                            static_cast<unsigned long long>(y_bytes_u64 + uv_bytes_u64));
            return false;
        }
        const uint32_t y_bytes = static_cast<uint32_t>(y_bytes_u64);
        const uint32_t uv_bytes = static_cast<uint32_t>(uv_bytes_u64);
        FramePlaneMetadata y_plane;
        y_plane.offset = 0;
        y_plane.stride = video_frame.width;
        y_plane.bytes = y_bytes;
        frame->metadata.planes.push_back(y_plane);
        FramePlaneMetadata uv_plane;
        uv_plane.offset = y_bytes;
        uv_plane.stride = video_frame.width;
        uv_plane.bytes = uv_bytes;
        frame->metadata.planes.push_back(uv_plane);
    }
    frame->data.swap(buffer);
    return true;
}

bool FrameCameraCaptureSource::discard() {
    return camera_.discard_frame_timeout(500);
}

void FrameCameraCaptureSource::close() {
    const std::chrono::steady_clock::time_point close_started =
        std::chrono::steady_clock::now();
    AIDEN_LOG_INFO("camera_source", "close_started",
                   "open_attempts=%llu lock_fd=%d",
                   static_cast<unsigned long long>(open_attempts_), lock_fd_);
    camera_.stop();
    if (lock_fd_ >= 0) {
        ::close(lock_fd_);
        lock_fd_ = -1;
    }
    const uint64_t elapsed_ms = static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now() - close_started).count());
    AIDEN_LOG_INFO("camera_source", "close_completed",
                   "elapsed_ms=%llu",
                   static_cast<unsigned long long>(elapsed_ms));
}

}  // namespace aiden
