---
sidebar_position: 1
---

# C++ SDK Reference

The Aico Link C++ hardware SDK uses the `aico` CMake target and the following
public header and produces `libaico.a`:

```cpp
#include "aico_sdk.h"
```

The SDK implementation uses the `aico` namespace. Consumers should rebuild
against the renamed headers and library. See
[Product Names and Technical Identifiers](../02-architecture/naming.md).

Key capabilities:

1. GPIO wakeup event;
2. Audio capture;
3. Audio playback;
4. HDMI / camera frame capture;
5. USB HID auxiliary capabilities.

## AudioConfig

```cpp
struct AudioConfig {
    const char* device_name = "hw:0,0";
    int sample_rate = 16000;
    int channels = 1;
    int bit_width = 16;
};
```

## CameraConfig

```cpp
struct CameraConfig {
    const char* device_name = "/dev/video0";
    int width = 1920;
    int height = 1080;
    int camera_id = 0;
    const char* pixel_format = "uyvy";
    const char* subdev_device = "/dev/v4l-subdev2";
    const char* edid_path = nullptr;
    int skip_frames = 1;
    int trigger_retries = 0;
    int trigger_delay_ms = 1000;
    int capture_retries = 2;
    bool enable_hdmi_sync = true;
    bool force_trigger = false;
    bool require_exact_resolution = false;
    bool reject_uniform_frames = true;
};
```

`frame_service` overrides the two timing flags above and requires the negotiated
1920x1080 resolution. The RK628D path avoids forced EDID/HPD retriggering and
retains the driver-provided EDID. The TC358743 path performs one startup
EDID/HPD renegotiation, then queries the settled timings.

## WakeupListener

```cpp
#include "aico_sdk.h"

void on_wakeup() {
    printf("Wakeup detected!\n");
}

aico::WakeupListener listener;
listener.start(33, on_wakeup);
// ...
listener.stop();
```

GPIO 33/GPIO 32 are typically used for button or wakeup signals, triggered on falling edge. When you need to listen on both pins simultaneously, create two `WakeupListener` instances and bind them to the same callback.

## AudioCapture

```cpp
aico::AudioConfig config;
config.sample_rate = 16000;
config.channels = 1;
config.bit_width = 16;

aico::AudioCapture capture;
capture.init(config);
capture.start([](const aico::AudioFrame& frame) {
    printf("Got %u bytes\n", frame.length);
});
// ...
capture.stop();
```

## AudioPlayer

```cpp
aico::AudioConfig config;
config.sample_rate = 16000;
config.channels = 1;
config.bit_width = 16;

aico::AudioPlayer player;
player.init(config);
player.set_volume(80); // 0..100
player.play(samples, sizeof(samples));
player.stop();
```

## CameraCapture

`CameraCapture` preserves the historical direct-capture default of UYVY for
compatibility. The frame_service executable selects NV12 explicitly. With
NV12, the `capture_frame*` copy helpers remove V4L2 row padding and return a
tight Y + interleaved UV payload. The returned `VideoFrame::stride` is the
visible width and `size_image`/`length` are the compact payload size.

Streaming capture:

```cpp
aico::CameraConfig config;
config.width = 1920;
config.height = 1080;
config.pixel_format = "uyvy";

aico::CameraCapture camera;
camera.init(config);
camera.start([](const aico::VideoFrame& frame) {
    printf("Frame: %ux%u, %u bytes, stride=%u\n",
           frame.width, frame.height, frame.length, frame.stride);
});
// ...
camera.stop();
```

Pause and resume streaming while retaining the open device and MMAP buffers:

```cpp
aico::CameraCapture camera;
camera.init(config);
camera.pause();  // VIDIOC_STREAMOFF; device and mappings remain owned

// Later, when a frame is needed:
camera.resume(); // requeue buffers and VIDIOC_STREAMON
aico::VideoFrame frame;
std::vector<uint8_t> bytes;
camera.capture_frame(frame, bytes);
camera.pause();
```

`frame_service` uses this lifecycle for request-driven screenshots. Resume
resets `skip_frames`, then the service performs additional zero-copy warm-up
dequeues before the response frame is copied. The deployed default is zero
additional warm-up frames when streaming restarts per request and six when
`keep_streamon` keeps the stream active.

One-shot capture and copy to caller-owned buffer:

```cpp
aico::CameraConfig config;
config.edid_path = nullptr; // Use built-in 1080p60-only CTA EDID

aico::CameraCapture camera;
aico::VideoFrame frame;
std::vector<uint8_t> bytes;

if (camera.capture_once(config, frame, bytes)) {
    printf("Frame: %ux%u, %u bytes\n", frame.width, frame.height, frame.length);
}
```

## Dependencies

- Rockchip MPI / Rockit / MPP / RGA / IVE libraries;
- opencv-mobile;
- System libraries such as pthread, m;
- Corresponding header and library paths are configured in `CMakeLists.txt`.

## Notes

- The SDK initializes `RK_MPI_SYS_Init()` on first use;
- Blocking operations support graceful shutdown according to service logic;
- When using `frame_service`, applications should typically not open `/dev/video0` directly.
