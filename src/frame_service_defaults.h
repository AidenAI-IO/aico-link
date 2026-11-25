#pragma once

#include <cstddef>
#include <cstdint>
#include <string>

namespace aico {

static const size_t kDefaultFrameServiceRingSize = 3;
static const double kDefaultFrameServiceFps = 3.0;
static const uint32_t kDefaultScreenshotMaxEdge = 960;
static const int kDefaultPersistentStreamWarmupFrames = 6;
static const int kDefaultPausedStreamWarmupFrames = 0;

inline std::string frame_service_capture_mode(const std::string& requested,
                                              const std::string& bridge_name) {
    if (requested != "auto") {
        return requested;
    }
    return bridge_name.find("rk628-csi") != std::string::npos ? "nv16-to-nv12" : "nv12";
}

inline int default_frame_service_warmup_frames(bool keep_streamon) {
    return keep_streamon ? kDefaultPersistentStreamWarmupFrames
                         : kDefaultPausedStreamWarmupFrames;
}

}
