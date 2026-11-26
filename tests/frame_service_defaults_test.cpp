#include "doctest.h"
#include "camera_frame_utils.h"
#include "frame_service_defaults.h"
#include <string>

TEST_CASE("frame_service library defaults avoid unconditional EDID retriggering") {
    aico::CameraConfig camera;
    aico_demo::set_default_camera_config(&camera);

    CHECK(camera.width == 1920);
    CHECK(camera.height == 1080);
    CHECK(std::string(camera.pixel_format) == "uyvy");
    CHECK(camera.edid_path == nullptr);
    CHECK(camera.force_trigger == false);
    CHECK(camera.allow_edid_fallback == true);
    CHECK(camera.require_exact_resolution == true);
    CHECK(aico::kDefaultFrameServiceRingSize == 3);
    CHECK(aico::kDefaultFrameServiceFps == 3.0);
    CHECK(aico::kDefaultScreenshotMaxEdge == 960);
    CHECK(aico::default_frame_service_warmup_frames(false) == 0);
    CHECK(aico::default_frame_service_warmup_frames(true) == 6);
}

TEST_CASE("automatic capture format follows the detected bridge on each open") {
    using aico::frame_service_capture_mode;
    const std::string rk628 = "m00_b_rk628-csi 4-0050";
    const std::string tc358743 = "tc358743 4-000f";

    CHECK(frame_service_capture_mode("auto", rk628) == "nv16-to-nv12");
    CHECK(frame_service_capture_mode("auto", tc358743) == "nv12");
    CHECK(frame_service_capture_mode("auto", "") == "nv12");
    CHECK(frame_service_capture_mode("nv12", rk628) == "nv12");
    CHECK(frame_service_capture_mode("nv16-to-nv12", tc358743) == "nv16-to-nv12");
    CHECK(frame_service_capture_mode("uyvy", tc358743) == "uyvy");
}
