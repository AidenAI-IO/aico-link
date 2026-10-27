#include "doctest.h"
#include "aico_log.h"
#include "frame_capture_manager.h"
#include "frame_service_client.h"
#include "frame_service_server.h"
#include <chrono>
#include <mutex>
#include <string>
#include <thread>
#include <unistd.h>
#include <vector>

using aico::CapturedFrame;
using aico::FrameCaptureManager;
using aico::FrameCaptureManagerOptions;
using aico::FrameCaptureSource;
using aico::FrameMetadata;
using aico::FrameResult;
using aico::FrameServiceClient;
using aico::FrameServiceServer;
using aico::FrameServiceStatus;
using aico::HealthResult;

namespace aico {
void reset_jpeg_encoder_stub();
int jpeg_encoder_stub_prepare_count();
int jpeg_encoder_stub_warmup_count();
int jpeg_encoder_stub_cancel_count();
uint32_t jpeg_encoder_stub_last_warmup_width();
uint32_t jpeg_encoder_stub_last_warmup_height();
}

namespace {

struct TempSocketPath {
    std::string path;
    TempSocketPath() {
        char tmpl[] = "/tmp/aico_frame_capture_XXXXXX";
        int fd = ::mkstemp(tmpl);
        REQUIRE(fd >= 0);
        ::close(fd);
        ::unlink(tmpl);
        path = tmpl;
    }
    ~TempSocketPath() { ::unlink(path.c_str()); }
};

FrameMetadata metadata(uint64_t ts) {
    FrameMetadata meta;
    meta.capture_ts_ns = ts;
    meta.width = 1;
    meta.height = 1;
    meta.pixel_format = "uyvy";
    meta.stride = 2;
    return meta;
}

CapturedFrame nv12_frame(uint64_t ts, uint32_t width, uint32_t height) {
    CapturedFrame frame;
    frame.metadata.capture_ts_ns = ts;
    frame.metadata.width = width;
    frame.metadata.height = height;
    frame.metadata.pixel_format = "nv12";
    frame.metadata.stride = width;
    frame.metadata.bytes = static_cast<uint64_t>(width) * height * 3U / 2U;
    frame.data.assign(static_cast<size_t>(frame.metadata.bytes), 0x80);
    return frame;
}

class FakeCaptureSource : public FrameCaptureSource {
public:
    std::vector<CapturedFrame> frames;
    int open_failures_remaining = 0;
    int fail_after = -1;
    bool repeat_last = false;
    int open_count = 0;
    int close_count = 0;
    int pause_count = 0;
    int resume_count = 0;
    int discard_count = 0;
    int capture_delay_ms = 0;
    bool pause_failure = false;
    bool pause_always_fails = false;
    bool resume_failure = false;

    bool open() override {
        std::lock_guard<std::mutex> lock(mutex_);
        ++open_count;
        if (open_failures_remaining > 0) {
            --open_failures_remaining;
            return false;
        }
        return true;
    }

    bool pause() override {
        std::lock_guard<std::mutex> lock(mutex_);
        ++pause_count;
        if (pause_failure || pause_always_fails) {
            pause_failure = false;
            return false;
        }
        return true;
    }

    bool resume() override {
        std::lock_guard<std::mutex> lock(mutex_);
        ++resume_count;
        if (resume_failure) {
            resume_failure = false;
            return false;
        }
        return true;
    }

    bool capture(CapturedFrame* frame) override {
        if (capture_delay_ms > 0) {
            std::this_thread::sleep_for(std::chrono::milliseconds(capture_delay_ms));
        }
        std::lock_guard<std::mutex> lock(mutex_);
        if (fail_after >= 0 && captures_ == fail_after) {
            fail_after = -1;
            return false;
        }
        if (frames.empty()) {
            if (repeat_last && has_last_) {
                *frame = last_;
                ++captures_;
                return true;
            }
            return false;
        }
        *frame = frames.front();
        last_ = *frame;
        has_last_ = true;
        frames.erase(frames.begin());
        ++captures_;
        return true;
    }

    bool discard() override {
        std::lock_guard<std::mutex> lock(mutex_);
        ++discard_count;
        return true;
    }

    void close() override {
        std::lock_guard<std::mutex> lock(mutex_);
        ++close_count;
    }

    int capture_count() {
        std::lock_guard<std::mutex> lock(mutex_);
        return captures_;
    }

    int opened_count() {
        std::lock_guard<std::mutex> lock(mutex_);
        return open_count;
    }

    int paused_count() {
        std::lock_guard<std::mutex> lock(mutex_);
        return pause_count;
    }

    int resumed_count() {
        std::lock_guard<std::mutex> lock(mutex_);
        return resume_count;
    }

    int discarded_count() {
        std::lock_guard<std::mutex> lock(mutex_);
        return discard_count;
    }

private:
    int captures_ = 0;
    bool has_last_ = false;
    CapturedFrame last_;
    std::mutex mutex_;
};

bool wait_for_latest(FrameServiceClient* client, uint64_t since_seq, FrameResult* out) {
    for (int i = 0; i < 50; ++i) {
        if (client->latest_frame(since_seq, 0, out) == FrameServiceStatus::OK) {
            return true;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
    return false;
}

bool wait_for_source_ready(FakeCaptureSource* source) {
    for (int i = 0; i < 50; ++i) {
        if (source->opened_count() > 0 && source->paused_count() > 0) {
            return true;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
    return false;
}

bool wait_for_source_open(FakeCaptureSource* source) {
    for (int i = 0; i < 50; ++i) {
        if (source->opened_count() > 0) {
            return true;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
    return false;
}

void connect_on_demand_capture(FrameServiceServer* server, FrameCaptureManager* manager) {
    server->set_capture_handler(
        [manager](uint32_t timeout_ms, FrameMetadata* meta, std::vector<uint8_t>* data) {
            CapturedFrame frame;
            FrameServiceStatus status = manager->capture(timeout_ms, &frame);
            if (status == FrameServiceStatus::OK) {
                *meta = frame.metadata;
                *data = std::move(frame.data);
            }
            return status;
        });
}

bool wait_for_latest_ts(FrameServiceClient* client, uint64_t capture_ts_ns, FrameResult* out) {
    for (int i = 0; i < 50; ++i) {
        if (client->latest_frame(0, 0, out) == FrameServiceStatus::OK &&
            out->metadata.capture_ts_ns == capture_ts_ns) {
            return true;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
    return false;
}

bool wait_for_health_failure(FrameServiceClient* client, HealthResult* out) {
    for (int i = 0; i < 50; ++i) {
        if (client->health(out) == FrameServiceStatus::OK && out->consecutive_failures > 0) {
            return true;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
    return false;
}

bool wait_for_warmup_count(int expected) {
    for (int i = 0; i < 100; ++i) {
        if (aico::jpeg_encoder_stub_warmup_count() >= expected) {
            return true;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(2));
    }
    return false;
}

}

TEST_CASE("FrameCaptureManager logs request and capture operation boundaries") {
    FILE* output = tmpfile();
    REQUIRE(output != NULL);
    aico::set_log_service("frame_service");
    aico::set_log_output(output);

    FrameServiceServer server("/tmp/aico_frame_logging_unused.sock", 4);
    FakeCaptureSource source;
    source.frames.push_back(CapturedFrame{metadata(1), std::vector<uint8_t>{1, 2}});
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1;
    options.recovery_max_backoff_ms = 1;
    options.recovery_idle_max_backoff_ms = 1;
    FrameCaptureManager manager(&source, &server, options);

    REQUIRE(manager.start());
    REQUIRE(wait_for_source_ready(&source));
    CapturedFrame captured;
    CHECK(manager.capture(1000, &captured) == FrameServiceStatus::OK);
    manager.stop();

    fflush(output);
    REQUIRE(fseek(output, 0, SEEK_SET) == 0);
    std::string log;
    char buffer[4096];
    size_t bytes = 0;
    while ((bytes = fread(buffer, 1, sizeof(buffer), output)) > 0) {
        log.append(buffer, bytes);
    }

    CHECK(log.find("[frame_service][capture] source_open_started ") != std::string::npos);
    CHECK(log.find("[frame_service][capture] worker_idle ") != std::string::npos);
    CHECK(log.find("[frame_service][capture] request_queued ") != std::string::npos);
    CHECK(log.find("[frame_service][capture] frame_read_started ") != std::string::npos);
    CHECK(log.find("[frame_service][capture] cycle_completed ") != std::string::npos);
    CHECK(log.find("frame_read_ms=") != std::string::npos);
    CHECK(log.find("generation=1") != std::string::npos);

    aico::set_log_output(NULL);
    aico::set_log_service("unknown");
    fclose(output);
}

TEST_CASE("FrameCaptureManager warms NV12 once across same-layout recovery") {
    aico::reset_jpeg_encoder_stub();
    FrameServiceServer server("/tmp/aico_frame_warmup_unused.sock", 4);
    FakeCaptureSource source;
    source.repeat_last = true;
    source.capture_delay_ms = 2;
    source.frames.push_back(nv12_frame(1, 2, 2));
    source.frames.push_back(nv12_frame(2, 2, 2));
    source.fail_after = 1;
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1;
    options.recovery_max_backoff_ms = 1;
    FrameCaptureManager manager(&source, &server, options);
    REQUIRE(manager.start());
    CapturedFrame captured;
    REQUIRE(manager.capture(1000, &captured) == FrameServiceStatus::OK);
    REQUIRE(wait_for_warmup_count(1));
    CHECK(manager.capture(1000, &captured) == FrameServiceStatus::SERVICE_RECOVERING);
    bool recovered = false;
    for (int i = 0; i < 20 && !recovered; ++i) {
        recovered = manager.capture(1000, &captured) == FrameServiceStatus::OK;
    }
    CHECK(recovered);
    manager.stop();

    CHECK(aico::jpeg_encoder_stub_prepare_count() == 1);
    CHECK(aico::jpeg_encoder_stub_warmup_count() == 1);
    CHECK(aico::jpeg_encoder_stub_cancel_count() == 0);
    CHECK(aico::jpeg_encoder_stub_last_warmup_width() == 2);
    CHECK(aico::jpeg_encoder_stub_last_warmup_height() == 2);
}

TEST_CASE("FrameCaptureManager skips JPEG warmup for non-NV12 frames") {
    aico::reset_jpeg_encoder_stub();
    FrameServiceServer server("/tmp/aico_frame_warmup_uyvy_unused.sock", 4);
    FakeCaptureSource source;
    source.repeat_last = true;
    source.capture_delay_ms = 2;
    source.frames.push_back(CapturedFrame{metadata(2), std::vector<uint8_t>{1, 2}});
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1;
    options.recovery_max_backoff_ms = 1;
    FrameCaptureManager manager(&source, &server, options);
    REQUIRE(manager.start());
    CapturedFrame captured;
    REQUIRE(manager.capture(1000, &captured) == FrameServiceStatus::OK);
    manager.stop();

    CHECK(aico::jpeg_encoder_stub_prepare_count() == 0);
    CHECK(aico::jpeg_encoder_stub_warmup_count() == 0);
}

TEST_CASE("FrameCaptureManager rewarms JPEG after NV12 resolution changes") {
    aico::reset_jpeg_encoder_stub();
    FrameServiceServer server("/tmp/aico_frame_rewarm_unused.sock", 4);
    FakeCaptureSource source;
    source.repeat_last = true;
    source.capture_delay_ms = 2;
    source.frames.push_back(nv12_frame(3, 2, 2));
    source.frames.push_back(nv12_frame(4, 4, 2));
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1;
    options.recovery_max_backoff_ms = 1;
    FrameCaptureManager manager(&source, &server, options);
    REQUIRE(manager.start());
    CapturedFrame captured;
    REQUIRE(manager.capture(1000, &captured) == FrameServiceStatus::OK);
    REQUIRE(wait_for_warmup_count(1));
    REQUIRE(manager.capture(1000, &captured) == FrameServiceStatus::OK);
    REQUIRE(wait_for_warmup_count(2));
    manager.stop();

    CHECK(aico::jpeg_encoder_stub_prepare_count() == 2);
    CHECK(aico::jpeg_encoder_stub_warmup_count() == 2);
    CHECK(aico::jpeg_encoder_stub_last_warmup_width() == 4);
    CHECK(aico::jpeg_encoder_stub_last_warmup_height() == 2);
}

TEST_CASE("FrameCaptureManager stays paused while idle and captures once per request") {
    TempSocketPath socket_path;
    FrameServiceServer server(socket_path.path.c_str(), 4);
    REQUIRE(server.start() == FrameServiceStatus::OK);

    FakeCaptureSource source;
    source.repeat_last = true;
    source.frames.push_back(CapturedFrame{metadata(10), std::vector<uint8_t>{1, 2}});
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1;
    options.recovery_max_backoff_ms = 1;
    options.warmup_frames = 3;
    FrameCaptureManager manager(&source, &server, options);
    connect_on_demand_capture(&server, &manager);
    REQUIRE(manager.start());
    REQUIRE(wait_for_source_ready(&source));

    std::this_thread::sleep_for(std::chrono::milliseconds(50));
    CHECK(source.capture_count() == 0);
    CHECK(source.resumed_count() == 0);

    FrameServiceClient client(socket_path.path.c_str());
    FrameResult frame;
    REQUIRE(client.latest_frame(0, 0, &frame) == FrameServiceStatus::OK);
    CHECK(frame.metadata.capture_ts_ns == 10);
    CHECK(frame.data == std::vector<uint8_t>{1, 2});
    const uint64_t first_seq = frame.metadata.seq;

    REQUIRE(client.latest_frame(first_seq, 0, &frame) == FrameServiceStatus::OK);
    CHECK(frame.metadata.seq > first_seq);
    CHECK(source.capture_count() == 2);
    CHECK(source.resumed_count() == 2);
    CHECK(source.discarded_count() == 6);
    CHECK(source.paused_count() >= 3);

    HealthResult health;
    REQUIRE(client.health(&health) == FrameServiceStatus::OK);
    CHECK(health.latest_seq == frame.metadata.seq);
    CHECK(health.ring_buffer_size == 0);
    CHECK(health.ring_buffer_used == 0);

    manager.stop();
    server.stop();
}

TEST_CASE("FrameCaptureManager reports RUNNING before the first capture request") {
    TempSocketPath socket_path;
    FrameServiceServer server(socket_path.path.c_str(), 4);
    REQUIRE(server.start() == FrameServiceStatus::OK);

    FakeCaptureSource source;
    source.repeat_last = true;
    source.open_failures_remaining = 1;
    source.frames.push_back(CapturedFrame{metadata(10), std::vector<uint8_t>{1, 2}});
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1;
    options.recovery_max_backoff_ms = 1;
    options.recovery_idle_max_backoff_ms = 1;
    FrameCaptureManager manager(&source, &server, options);
    connect_on_demand_capture(&server, &manager);
    REQUIRE(manager.start());
    REQUIRE(wait_for_source_ready(&source));

    // An on-demand client gates its first capture on this state. It has to
    // reach RUNNING from the open alone, with no request ever sent, otherwise
    // client and service deadlock.
    FrameServiceClient client(socket_path.path.c_str());
    HealthResult health;
    bool running = false;
    for (int i = 0; i < 50 && !running; ++i) {
        REQUIRE(client.health(&health) == FrameServiceStatus::OK);
        running = health.state == "RUNNING";
        if (!running) {
            std::this_thread::sleep_for(std::chrono::milliseconds(10));
        }
    }
    CHECK(running);
    CHECK(health.capture_mode == "on_demand");
    CHECK(health.latest_seq == 0);
    CHECK(source.capture_count() == 0);
    // The failed first open still counted, so RUNNING is published by the
    // reopen rather than by clearing the failure.
    CHECK(health.consecutive_failures == 1);

    FrameResult frame;
    REQUIRE(client.latest_frame(0, 0, &frame) == FrameServiceStatus::OK);
    CHECK(frame.data == std::vector<uint8_t>{1, 2});

    manager.stop();
    server.stop();
}

TEST_CASE("FrameCaptureManager reopens source after capture failure") {
    TempSocketPath socket_path;
    FrameServiceServer server(socket_path.path.c_str(), 4);
    REQUIRE(server.start() == FrameServiceStatus::OK);

    FakeCaptureSource source;
    source.frames.push_back(CapturedFrame{metadata(30), std::vector<uint8_t>{5, 6}});
    source.fail_after = 0;
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1;
    options.recovery_max_backoff_ms = 1;
    FrameCaptureManager manager(&source, &server, options);
    connect_on_demand_capture(&server, &manager);
    REQUIRE(manager.start());

    FrameServiceClient client(socket_path.path.c_str());
    FrameResult frame;
    CHECK(client.latest_frame(0, 0, &frame) == FrameServiceStatus::SERVICE_RECOVERING);
    REQUIRE(wait_for_latest_ts(&client, 30, &frame));
    CHECK(frame.metadata.capture_ts_ns == 30);
    CHECK(source.opened_count() >= 2);
    CHECK(source.close_count >= 1);

    manager.stop();
    server.stop();
}

TEST_CASE("FrameCaptureManager can keep STREAMON between requests") {
    TempSocketPath socket_path;
    FrameServiceServer server(socket_path.path.c_str(), 4);
    REQUIRE(server.start() == FrameServiceStatus::OK);

    FakeCaptureSource source;
    source.repeat_last = true;
    source.frames.push_back(CapturedFrame{metadata(20), std::vector<uint8_t>{3, 4}});
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1;
    options.recovery_max_backoff_ms = 1;
    options.warmup_frames = 2;
    options.keep_streamon = true;
    FrameCaptureManager manager(&source, &server, options);
    connect_on_demand_capture(&server, &manager);
    REQUIRE(manager.start());
    REQUIRE(wait_for_source_open(&source));

    FrameServiceClient client(socket_path.path.c_str());
    FrameResult frame;
    REQUIRE(client.latest_frame(0, 0, &frame) == FrameServiceStatus::OK);
    const uint64_t first_seq = frame.metadata.seq;
    REQUIRE(client.latest_frame(first_seq, 0, &frame) == FrameServiceStatus::OK);

    CHECK(source.capture_count() == 2);
    CHECK(source.discarded_count() == 4);
    CHECK(source.resumed_count() == 0);
    CHECK(source.paused_count() == 0);

    manager.stop();
    server.stop();
}

TEST_CASE("FrameCaptureManager stop interrupts recovery backoff promptly") {
    TempSocketPath socket_path;
    FrameServiceServer server(socket_path.path.c_str(), 4);
    REQUIRE(server.start() == FrameServiceStatus::OK);

    FakeCaptureSource source;
    source.open_failures_remaining = 100;
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1000;
    options.recovery_max_backoff_ms = 1000;
    FrameCaptureManager manager(&source, &server, options);
    REQUIRE(manager.start());
    std::this_thread::sleep_for(std::chrono::milliseconds(50));

    auto start = std::chrono::steady_clock::now();
    manager.stop();
    auto elapsed = std::chrono::duration_cast<std::chrono::milliseconds>(
        std::chrono::steady_clock::now() - start).count();

    CHECK(elapsed < 250);
    server.stop();
}

TEST_CASE("FrameCaptureManager slows retries before the first valid frame") {
    TempSocketPath socket_path;
    FrameServiceServer server(socket_path.path.c_str(), 4);
    REQUIRE(server.start() == FrameServiceStatus::OK);

    FakeCaptureSource source;
    source.pause_always_fails = true;
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 10;
    options.recovery_max_backoff_ms = 10;
    options.recovery_idle_max_backoff_ms = 160;
    FrameCaptureManager manager(&source, &server, options);
    REQUIRE(manager.start());

    std::this_thread::sleep_for(std::chrono::milliseconds(400));
    manager.stop();

    CHECK(source.opened_count() >= 2);
    CHECK(source.opened_count() <= 12);
    server.stop();
}

TEST_CASE("FrameCaptureManager restart interrupts the idle HDMI backoff") {
    TempSocketPath socket_path;
    FrameServiceServer server(socket_path.path.c_str(), 4);
    REQUIRE(server.start() == FrameServiceStatus::OK);

    FakeCaptureSource source;
    source.pause_always_fails = true;
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 5000;
    options.recovery_max_backoff_ms = 5000;
    options.recovery_idle_max_backoff_ms = 5000;
    FrameCaptureManager manager(&source, &server, options);
    REQUIRE(manager.start());

    int opens_before = 0;
    for (int i = 0; i < 100 && opens_before == 0; ++i) {
        opens_before = source.opened_count();
        if (opens_before == 0) {
            std::this_thread::sleep_for(std::chrono::milliseconds(10));
        }
    }
    REQUIRE(opens_before >= 1);

    manager.request_restart();
    bool reopened = false;
    for (int i = 0; i < 50 && !reopened; ++i) {
        reopened = source.opened_count() > opens_before;
        if (!reopened) {
            std::this_thread::sleep_for(std::chrono::milliseconds(10));
        }
    }
    CHECK(reopened);

    manager.stop();
    server.stop();
}

TEST_CASE("FrameCaptureManager publishes failure and copy latency health metrics") {
    TempSocketPath socket_path;
    FrameServiceServer server(socket_path.path.c_str(), 4);
    REQUIRE(server.start() == FrameServiceStatus::OK);

    FakeCaptureSource source;
    source.open_failures_remaining = 1;
    source.repeat_last = true;
    source.frames.push_back(CapturedFrame{metadata(40), std::vector<uint8_t>{7, 8}});
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 100;
    options.recovery_max_backoff_ms = 100;
    FrameCaptureManager manager(&source, &server, options);
    connect_on_demand_capture(&server, &manager);
    REQUIRE(manager.start());

    FrameServiceClient client(socket_path.path.c_str());
    HealthResult health;
    REQUIRE(wait_for_health_failure(&client, &health));
    CHECK(health.state == "RECOVERING");
    CHECK(health.consecutive_failures == 1);
    CHECK(health.last_error == "open failed");
    CHECK(health.last_recovery_ts > 0);

    FrameResult frame;
    REQUIRE(wait_for_latest_ts(&client, 40, &frame));
    manager.stop();
    REQUIRE(client.health(&health) == FrameServiceStatus::OK);
    CHECK(health.consecutive_failures == 0);
    CHECK(health.last_error == "open failed");
    CHECK(health.last_recovery_ts > 0);
    CHECK(health.ring_buffer_used == 0);

    server.stop();
}

TEST_CASE("FrameCaptureManager honors a bounded on-demand request timeout") {
    TempSocketPath socket_path;
    FrameServiceServer server(socket_path.path.c_str(), 4);
    REQUIRE(server.start() == FrameServiceStatus::OK);

    FakeCaptureSource source;
    source.repeat_last = true;
    source.capture_delay_ms = 150;
    source.frames.push_back(CapturedFrame{metadata(50), std::vector<uint8_t>{1, 2}});
    FrameCaptureManagerOptions options;
    options.recovery_initial_backoff_ms = 1;
    options.recovery_max_backoff_ms = 1;
    options.request_timeout_ms = 1000;
    FrameCaptureManager manager(&source, &server, options);
    connect_on_demand_capture(&server, &manager);
    REQUIRE(manager.start());
    REQUIRE(wait_for_source_ready(&source));

    FrameServiceClient client(socket_path.path.c_str());
    FrameResult frame;
    CHECK(client.latest_frame(0, 50, &frame) == FrameServiceStatus::TIMEOUT);
    REQUIRE(client.latest_frame(0, 1000, &frame) == FrameServiceStatus::OK);
    CHECK(frame.metadata.capture_ts_ns == 50);

    manager.stop();
    server.stop();
}
