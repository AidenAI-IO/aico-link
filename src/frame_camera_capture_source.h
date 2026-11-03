#pragma once

#include "aico_sdk.h"
#include "frame_capture_manager.h"
#include <string>

namespace aico {

class FrameCameraCaptureSource : public FrameCaptureSource {
public:
    explicit FrameCameraCaptureSource(const CameraConfig& config);

    bool open() override;
    bool pause() override;
    bool resume() override;
    bool capture(CapturedFrame* frame) override;
    bool discard() override;
    void close() override;

private:
    static std::string detect_hdmi_subdev(std::string* name);
    void sync_config_strings();

    CameraConfig config_;
    std::string device_name_;
    std::string pixel_format_;
    std::string subdev_device_;
    std::string edid_path_;
    std::string prepared_mode_subdev_;
    bool auto_subdev_;
    bool auto_edid_;
    bool auto_force_trigger_pending_;
    bool periodic_force_enabled_;
    unsigned int tc_open_attempts_;
    uint64_t open_attempts_;
    bool force_trigger_pending_;
    int lock_fd_;
    CameraCapture camera_;
};

}  // namespace aico
