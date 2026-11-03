#ifndef AICO_SDK_H
#define AICO_SDK_H

#include <functional>
#include <memory>
#include <vector>
#include <cstdint>

namespace aico {

// Audio configuration
struct AudioConfig {
    const char* device_name = "hw:0,0";
    int sample_rate = 16000;
    int channels = 1;
    int bit_width = 16;  // 8, 16, or 24
};

// Audio frame data
struct AudioFrame {
    void* data;
    uint32_t length;
    uint64_t timestamp;
    uint32_t channels;
    uint32_t sample_rate;
};

// Camera configuration
struct CameraConfig {
    const char* device_name = "/dev/video0";  // V4L2 capture device
    int width = 1920;
    int height = 1080;
    int camera_id = 0;
    const char* pixel_format = "uyvy";  // nv12, nv16, uyvy, yuyv
    const char* subdev_device = "/dev/v4l-subdev2";
    const char* edid_path = nullptr;    // Defaults to built-in 1080p60-only CTA EDID
    int skip_frames = 1;                // Drop transitional frames after stream-on
    int trigger_retries = 0;            // Additional EDID retrigger attempts after the first trigger
    int trigger_delay_ms = 1000;
    int capture_retries = 2;            // Full init/capture recovery retries
    bool enable_hdmi_sync = true;
    bool force_trigger = false;
    // When false, a failed timing probe is reported to the caller instead of
    // falling back to an EDID write.  Recovery-oriented services use this to
    // keep no-signal probes read-only.
    bool allow_edid_fallback = true;
    bool require_exact_resolution = false;
    bool reject_uniform_frames = true;  // Reject known-bad all-same HDMI frames
};

// Video frame data
struct VideoFrame {
    void* data;
    uint32_t width;
    uint32_t height;
    uint32_t length;
    uint64_t timestamp;
    uint32_t sequence;
    uint32_t stride;
    uint32_t size_image;
    uint32_t plane_count;
    uint64_t buffer_capacity;
};

// Wakeup event callback
using WakeupCallback = std::function<void()>;

// Audio stream callback
using AudioStreamCallback = std::function<void(const AudioFrame& frame)>;

// Video stream callback
using VideoStreamCallback = std::function<void(const VideoFrame& frame)>;

// Forward declarations
class WakeupListenerImpl;
class AudioCaptureImpl;
class AudioPlayerImpl;
class CameraCaptureImpl;

// Wakeup event listener
class WakeupListener {
public:
    WakeupListener();
    ~WakeupListener();

    // Start listening for wakeup events on GPIO pin
    bool start(int gpio_pin, WakeupCallback callback);

    // Stop listening
    void stop();

    // Check if listener is running
    bool is_running() const;

private:
    std::unique_ptr<WakeupListenerImpl> impl_;
};

// Audio capture (recording)
class AudioCapture {
public:
    AudioCapture();
    ~AudioCapture();

    // Initialize audio capture with config
    bool init(const AudioConfig& config);

    // Start capturing audio with callback
    bool start(AudioStreamCallback callback);

    // Stop capturing
    void stop();

    // Get a single frame (blocking)
    bool get_frame(AudioFrame& frame);

    // Release frame after processing
    void release_frame();

    // Check if capture is running
    bool is_running() const;

private:
    std::unique_ptr<AudioCaptureImpl> impl_;
};

// Audio playback
class AudioPlayer {
public:
    AudioPlayer();
    ~AudioPlayer();

    // Initialize audio player with config
    bool init(const AudioConfig& config);

    // Play audio data (blocking until queued)
    bool play(const void* data, uint32_t length);

    // Play audio frame
    bool play(const AudioFrame& frame);

    // Stop playback and clear buffer
    void stop();

    // Pause playback
    void pause();

    // Resume playback
    void resume();

    // Set playback volume in the logical range 0..100.
    // 0 mutes playback; 1..100 map onto the configured AO volume curve.
    bool set_volume(int volume);

    // Get current logical volume in the range 0..100.
    int get_volume() const;

    // Check if player is initialized
    bool is_initialized() const;

private:
    std::unique_ptr<AudioPlayerImpl> impl_;
};

// Camera capture
class CameraCapture {
public:
    CameraCapture();
    ~CameraCapture();

    // Initialize camera with config
    bool init(const CameraConfig& config);

    // Start capturing video with callback
    bool start(VideoStreamCallback callback);

    // Stop capturing
    void stop();

    // Stop V4L2 streaming while keeping the device and MMAP buffers open.
    bool pause();

    // Queue the mapped buffers and restart V4L2 streaming.
    bool resume();

    // Get a single frame (blocking)
    bool get_frame(VideoFrame& frame);

    // Get a single frame and copy it into a caller-owned buffer.
    bool capture_frame(VideoFrame& frame, std::vector<uint8_t>& buffer);

    // Get a single frame with timeout and copy it into a caller-owned buffer.
    bool capture_frame_timeout(VideoFrame& frame, std::vector<uint8_t>& buffer, int timeout_ms);

    // Dequeue and release a single frame without copying it.
    bool discard_frame_timeout(int timeout_ms);

    // One-shot still capture helper: init, capture, and stop internally.
    bool capture_once(const CameraConfig& config,
                      VideoFrame& frame,
                      std::vector<uint8_t>& buffer);

    // Release frame after processing
    void release_frame();

    // Check if capture is running
    bool is_running() const;

private:
    std::unique_ptr<CameraCaptureImpl> impl_;
};

}  // namespace aico

#endif  // AICO_SDK_H
