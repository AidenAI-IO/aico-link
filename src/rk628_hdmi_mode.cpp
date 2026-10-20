#include "rk628_hdmi_mode.h"
#include "aiden_log.h"
#include <array>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <linux/v4l2-subdev.h>
#include <sys/ioctl.h>

namespace aiden {

static int mode_ioctl(int fd, unsigned long request, void* data) {
    int ret;
    do {
        ret = ioctl(fd, request, data);
    } while (ret < 0 && errno == EINTR);
    return ret;
}

bool prepare_rk628_hdmi_mode(
    int fd, const std::string& mode_file, const std::string& edid_directory,
    std::string* selected_edid,
    int (*ioctl_fn)(int, unsigned long, void*)) {
    std::string mode = "1080p60";
    FILE* file = fopen(mode_file.c_str(), "r");
    if (file) {
        char selected[32] = {};
        char extra;
        const int fields = fscanf(file, "%31s %c", selected, &extra);
        const bool read_failed = ferror(file);
        fclose(file);
        if (read_failed || fields != 1 ||
            (strcmp(selected, "1080p30") && strcmp(selected, "1080p60"))) {
            AIDEN_LOG_ERROR("hdmi", "mode_invalid", "path=%s", mode_file.c_str());
            errno = EINVAL;
            return false;
        }
        mode = selected;
    } else if (errno != ENOENT) {
        AIDEN_LOG_ERROR("hdmi", "mode_read_failed", "path=%s error=%s",
                        mode_file.c_str(), strerror(errno));
        return false;
    }

    const std::string path = edid_directory + "/hdmi_" + mode + "_cta.hex";
    file = fopen(path.c_str(), "r");
    if (!file) {
        AIDEN_LOG_ERROR("hdmi", "mode_edid_open_failed", "path=%s error=%s",
                        path.c_str(), strerror(errno));
        return false;
    }
    std::array<unsigned char, 256> desired = {};
    bool valid = true;
    for (size_t i = 0; i < desired.size(); ++i) {
        unsigned value;
        if (fscanf(file, "%x", &value) != 1 || value > 255) {
            valid = false;
            break;
        }
        desired[i] = static_cast<unsigned char>(value);
    }
    char extra;
    if (fscanf(file, " %c", &extra) != EOF || ferror(file)) {
        valid = false;
    }
    fclose(file);
    const unsigned char header[] = {0, 255, 255, 255, 255, 255, 255, 0};
    valid = valid && !memcmp(desired.data(), header, sizeof(header)) && desired[126] == 1;
    for (size_t offset = 0; offset < desired.size(); offset += 128) {
        unsigned sum = 0;
        for (size_t i = offset; i < offset + 128; ++i) {
            sum += desired[i];
        }
        valid = valid && sum % 256 == 0;
    }
    if (!valid) {
        AIDEN_LOG_ERROR("hdmi", "mode_edid_invalid", "path=%s", path.c_str());
        errno = EINVAL;
        return false;
    }

    if (!ioctl_fn) {
        ioctl_fn = mode_ioctl;
    }
    std::array<unsigned char, 256> current = {};
    struct v4l2_edid edid = {};
    edid.blocks = 2;
    edid.edid = current.data();
    const int ret = ioctl_fn(fd, VIDIOC_SUBDEV_G_EDID, &edid);
    if (ret < 0 && errno != ENODATA) {
        // Do not turn a transient read failure into a destructive HPD retry.
        AIDEN_LOG_WARN("hdmi", "mode_edid_read_pending", "error=%s", strerror(errno));
        return false;
    }
    *selected_edid = path;
    if (ret == 0 && edid.blocks == 2 && current == desired) {
        AIDEN_LOG_INFO("hdmi", "mode_ready", "mode=%s changed=0", mode.c_str());
        return true;
    }
    edid = {};
    edid.blocks = 2;
    edid.edid = desired.data();
    if (ioctl_fn(fd, VIDIOC_SUBDEV_S_EDID, &edid) < 0) {
        AIDEN_LOG_WARN("hdmi", "mode_edid_write_pending", "mode=%s error=%s",
                       mode.c_str(), strerror(errno));
        return false;
    }
    AIDEN_LOG_INFO("hdmi", "mode_ready", "mode=%s changed=1", mode.c_str());
    return true;
}

}  // namespace aiden
