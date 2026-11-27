---
sidebar_position: 1
---

# C++ SDK 参考

Aico Link C++ 硬件 SDK 使用 `aico` CMake 目标和以下公共头文件，构建产物为
`libaico.a`：

```cpp
#include "aico_sdk.h"
```

SDK 实现使用 `aico` 命名空间。使用方应基于重命名后的头文件和库重新构建。参见
[产品名称与技术标识符](../02-architecture/naming.md)。

主要能力：

1. GPIO 唤醒事件；
2. 音频采集；
3. 音频播放；
4. HDMI / 摄像头帧采集；
5. USB HID 辅助能力。

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

`frame_service` 会覆盖上面两个时序标志，并要求协商出的分辨率为 1920x1080。RK628D
路径不会强制重新触发 EDID/HPD，并保留驱动提供的 EDID。TC358743 路径在启动时执行一次
EDID/HPD 重新协商，然后查询稳定后的时序。

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

GPIO 33/GPIO 32 通常用于按键或唤醒信号，下降沿触发。需要同时监听两个引脚时，创建两个 `WakeupListener` 实例并绑定到同一个回调。

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

为保持兼容，`CameraCapture` 保留了历史直接采集的默认格式 UYVY。frame_service
可执行程序会显式选择 NV12。使用 NV12 时，`capture_frame*` 复制辅助函数会去除 V4L2
行填充，返回紧凑的 Y + 交错 UV 数据。返回的 `VideoFrame::stride` 为可见宽度，
`size_image`/`length` 为紧凑数据的大小。

流式采集：

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

暂停和恢复流式采集，同时保持设备打开和 MMAP 缓冲区：

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

`frame_service` 使用这一生命周期实现按请求截图。恢复时会重置 `skip_frames`，随后服务在复制
响应帧之前再执行若干次零拷贝预热出队。部署默认值为：每次请求重新开启流时额外预热 0 帧，
`keep_streamon` 保持流常开时额外预热 6 帧。

单次采集并复制到调用方持有的缓冲区：

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

## 依赖

- Rockchip MPI / Rockit / MPP / RGA / IVE 库；
- opencv-mobile；
- pthread、m 等系统库；
- 相应的头文件和库路径在 `CMakeLists.txt` 中配置。

## 注意事项

- SDK 在首次使用时初始化 `RK_MPI_SYS_Init()`；
- 阻塞操作支持按服务逻辑优雅退出；
- 使用 `frame_service` 时，应用通常不应直接打开 `/dev/video0`。
