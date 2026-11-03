#pragma once

#include <cstddef>
#include <cstdint>

namespace aico {

static const size_t kDefaultFrameServiceRingSize = 3;
static const double kDefaultFrameServiceFps = 3.0;
static const uint32_t kDefaultScreenshotMaxEdge = 960;
static const int kDefaultPersistentStreamWarmupFrames = 6;
static const int kDefaultPausedStreamWarmupFrames = 0;

inline int default_frame_service_warmup_frames(bool keep_streamon) {
    return keep_streamon ? kDefaultPersistentStreamWarmupFrames
                         : kDefaultPausedStreamWarmupFrames;
}

}
