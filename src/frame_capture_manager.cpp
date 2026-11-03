#include "frame_capture_manager.h"
#include "aico_log.h"
#include "frame_jpeg_encoder.h"
#include <chrono>
#include <memory>
#include <thread>

namespace aico {

FrameCaptureManager::FrameCaptureManager(FrameCaptureSource* source,
                                         FrameServiceServer* server,
                                         const FrameCaptureManagerOptions& options)
    : source_(source),
      server_(server),
      options_(options),
      running_(false),
      restart_requested_(false),
      jpeg_warmup_running_(false),
      received_requests_(0),
      recovery_count_(0),
      have_jpeg_warmup_key_(false),
      jpeg_warmup_width_(0),
      jpeg_warmup_height_(0),
      requested_generation_(0),
      completed_generation_(0),
      completed_status_(FrameServiceStatus::SERVICE_RECOVERING) {}

FrameCaptureManager::~FrameCaptureManager() {
    stop();
}

bool FrameCaptureManager::start() {
    if (!source_ || !server_ || running_) {
        return false;
    }
    running_ = true;
    server_->set_state("STARTING");
    AICO_LOG_INFO("capture", "manager_started",
                   "keep_streamon=%d warmup_frames=%d request_timeout_ms=%d recovery_initial_backoff_ms=%d recovery_max_backoff_ms=%d recovery_idle_max_backoff_ms=%d",
                   options_.keep_streamon ? 1 : 0, options_.warmup_frames,
                   options_.request_timeout_ms, options_.recovery_initial_backoff_ms,
                   options_.recovery_max_backoff_ms,
                   options_.recovery_idle_max_backoff_ms);
    thread_ = std::thread(&FrameCaptureManager::run, this);
    return true;
}

void FrameCaptureManager::stop() {
    if (!running_ && !thread_.joinable() && !jpeg_warmup_thread_.joinable()) {
        return;
    }
    {
        // Publish the stop through mutex_ before notifying. run()'s inner loop
        // evaluates its wait predicate while holding mutex_, and only releases
        // it atomically once it is enqueued on work_cv_. Without this barrier a
        // notify_all() can land after the predicate read but before the
        // enqueue, where it is dropped -- leaving the worker asleep forever and
        // the join() below blocked for good. FrameServiceServer::stop()
        // publishes running_ the same way.
        std::lock_guard<std::mutex> lock(mutex_);
        running_ = false;
    }
    work_cv_.notify_all();
    capture_cv_.notify_all();
    if (thread_.joinable()) {
        thread_.join();
    }
    if (source_) {
        source_->close();
    }
    if (jpeg_warmup_thread_.joinable()) {
        jpeg_warmup_thread_.join();
    }
    uint64_t requested_generation = 0;
    uint64_t completed_generation = 0;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        requested_generation = requested_generation_;
        completed_generation = completed_generation_;
    }
    AICO_LOG_INFO("capture", "manager_stopped",
                   "requested_generation=%llu completed_generation=%llu",
                   static_cast<unsigned long long>(requested_generation),
                   static_cast<unsigned long long>(completed_generation));
}

bool FrameCaptureManager::is_running() const {
    return running_;
}

void FrameCaptureManager::request_restart() {
    restart_requested_ = true;
    work_cv_.notify_all();
}

void FrameCaptureManager::maybe_start_jpeg_warmup(const CapturedFrame& frame) {
    if (frame.metadata.pixel_format != "nv12" ||
        (have_jpeg_warmup_key_ && frame.metadata.width == jpeg_warmup_width_ &&
         frame.metadata.height == jpeg_warmup_height_)) {
        return;
    }
    if (jpeg_warmup_thread_.joinable()) {
        if (jpeg_warmup_running_.load()) {
            return;
        }
        jpeg_warmup_thread_.join();
    }
    if (!prepare_jpeg_encoder_warmup(frame.metadata)) {
        return;
    }

    std::shared_ptr<CapturedFrame> warmup_frame(new CapturedFrame(frame));
    jpeg_warmup_running_ = true;
    try {
        jpeg_warmup_thread_ = std::thread([this, warmup_frame]() {
            warmup_jpeg_encoder(warmup_frame->metadata, warmup_frame->data);
            jpeg_warmup_running_ = false;
        });
        have_jpeg_warmup_key_ = true;
        jpeg_warmup_width_ = frame.metadata.width;
        jpeg_warmup_height_ = frame.metadata.height;
    } catch (...) {
        jpeg_warmup_running_ = false;
        cancel_jpeg_encoder_warmup();
        AICO_LOG_ERROR("jpeg", "venc_warmup_thread_failed",
                        "width=%u height=%u", frame.metadata.width,
                        frame.metadata.height);
    }
}

FrameServiceStatus FrameCaptureManager::capture(uint32_t timeout_ms, CapturedFrame* frame) {
    if (!frame) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }

    const std::chrono::steady_clock::time_point request_started =
        std::chrono::steady_clock::now();

    // Only one caller registers and consumes a result at a time. The worker
    // coalesces timed-out generations so repeated short polls cannot build an
    // unbounded backlog of captures.
    std::unique_lock<std::mutex> request_lock(request_mutex_);
    const uint64_t request_number = ++received_requests_;
    const uint64_t serialization_wait_ms = static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now() - request_started).count());
    uint64_t generation = 0;
    bool manager_running = false;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        manager_running = running_;
        if (manager_running) {
            generation = ++requested_generation_;
        }
    }
    if (!manager_running) {
        AICO_LOG_WARN("capture", "request_rejected",
                       "request_number=%llu reason=manager_not_running serialization_wait_ms=%llu",
                       static_cast<unsigned long long>(request_number),
                       static_cast<unsigned long long>(serialization_wait_ms));
        return FrameServiceStatus::SERVICE_RECOVERING;
    }
    AICO_LOG_INFO("capture", "request_queued",
                   "request_number=%llu generation=%llu client_timeout_ms=%u serialization_wait_ms=%llu",
                   static_cast<unsigned long long>(request_number),
                   static_cast<unsigned long long>(generation), timeout_ms,
                   static_cast<unsigned long long>(serialization_wait_ms));
    work_cv_.notify_all();

    const int configured_timeout = options_.request_timeout_ms > 0
        ? options_.request_timeout_ms
        : 4000;
    const uint32_t wait_ms = timeout_ms > 0
        ? timeout_ms
        : static_cast<uint32_t>(configured_timeout);

    std::unique_lock<std::mutex> lock(mutex_);
    if (!capture_cv_.wait_for(lock, std::chrono::milliseconds(wait_ms), [&]() {
            return !running_ || completed_generation_ >= generation;
        })) {
        const uint64_t completed_generation = completed_generation_;
        lock.unlock();
        const uint64_t elapsed_ms = static_cast<uint64_t>(
            std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::steady_clock::now() - request_started).count());
        AICO_LOG_WARN("capture", "request_timed_out",
                       "request_number=%llu generation=%llu wait_ms=%u elapsed_ms=%llu completed_generation=%llu",
                       static_cast<unsigned long long>(request_number),
                       static_cast<unsigned long long>(generation), wait_ms,
                       static_cast<unsigned long long>(elapsed_ms),
                       static_cast<unsigned long long>(completed_generation));
        return FrameServiceStatus::TIMEOUT;
    }
    if (completed_generation_ < generation) {
        const uint64_t completed_generation = completed_generation_;
        const bool still_running = running_;
        lock.unlock();
        AICO_LOG_WARN("capture", "request_interrupted",
                       "request_number=%llu generation=%llu running=%d completed_generation=%llu",
                       static_cast<unsigned long long>(request_number),
                       static_cast<unsigned long long>(generation),
                       still_running ? 1 : 0,
                       static_cast<unsigned long long>(completed_generation));
        return FrameServiceStatus::SERVICE_RECOVERING;
    }
    const FrameServiceStatus status = completed_status_;
    if (completed_status_ == FrameServiceStatus::OK) {
        *frame = std::move(completed_frame_);
    }
    lock.unlock();
    if (status != FrameServiceStatus::OK) {
        const uint64_t elapsed_ms = static_cast<uint64_t>(
            std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::steady_clock::now() - request_started).count());
        AICO_LOG_WARN("capture", "request_completed",
                       "request_number=%llu generation=%llu status=%s elapsed_ms=%llu",
                       static_cast<unsigned long long>(request_number),
                       static_cast<unsigned long long>(generation),
                       frame_service_status_to_string(status),
                       static_cast<unsigned long long>(elapsed_ms));
    }
    return status;
}

void FrameCaptureManager::recover(int* backoff_ms,
                                  int max_backoff_ms,
                                  const char* error,
                                  bool count_failure) {
    const uint64_t recovery_id = ++recovery_count_;
    uint64_t requested_generation = 0;
    uint64_t completed_generation = 0;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        requested_generation = requested_generation_;
        completed_generation = completed_generation_;
    }
    AICO_LOG_WARN("capture", "recovery_started",
                   "recovery_id=%llu reason=%s count_failure=%d backoff_ms=%d max_backoff_ms=%d requested_generation=%llu completed_generation=%llu",
                   static_cast<unsigned long long>(recovery_id),
                   error ? error : "", count_failure ? 1 : 0, *backoff_ms,
                   max_backoff_ms,
                   static_cast<unsigned long long>(requested_generation),
                   static_cast<unsigned long long>(completed_generation));
    server_->record_recovery(error ? error : "", count_failure);
    source_->close();
    bool interrupted = false;
    {
        std::unique_lock<std::mutex> lock(mutex_);
        interrupted = work_cv_.wait_for(lock, std::chrono::milliseconds(*backoff_ms), [&]() {
            return !running_ || restart_requested_.load();
        });
    }
    if (restart_requested_.exchange(false)) {
        *backoff_ms = initial_backoff_ms();
        AICO_LOG_INFO("capture", "recovery_backoff_completed",
                       "recovery_id=%llu interrupted=1 reason=restart_requested next_backoff_ms=%d running=%d",
                       static_cast<unsigned long long>(recovery_id), *backoff_ms,
                       running_ ? 1 : 0);
        return;
    }
    if (*backoff_ms < max_backoff_ms) {
        *backoff_ms *= 2;
        if (*backoff_ms > max_backoff_ms) {
            *backoff_ms = max_backoff_ms;
        }
    }
    AICO_LOG_INFO("capture", "recovery_backoff_completed",
                   "recovery_id=%llu interrupted=%d next_backoff_ms=%d running=%d",
                   static_cast<unsigned long long>(recovery_id),
                   interrupted ? 1 : 0, *backoff_ms, running_ ? 1 : 0);
}

void FrameCaptureManager::run() {
    int backoff_ms = initial_backoff_ms();
    uint64_t open_attempt = 0;
    if (options_.recovery_max_backoff_ms <= 0) options_.recovery_max_backoff_ms = backoff_ms;
    if (options_.recovery_idle_max_backoff_ms < options_.recovery_max_backoff_ms) {
        options_.recovery_idle_max_backoff_ms = options_.recovery_max_backoff_ms;
    }

    while (running_) {
        bool captured_since_open = false;
        ++open_attempt;
        const std::chrono::steady_clock::time_point open_started =
            std::chrono::steady_clock::now();
        AICO_LOG_INFO("capture", "source_open_started",
                       "open_attempt=%llu keep_streamon=%d",
                       static_cast<unsigned long long>(open_attempt),
                       options_.keep_streamon ? 1 : 0);
        const bool source_opened = source_->open();
        const uint64_t open_elapsed_ms = static_cast<uint64_t>(
            std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::steady_clock::now() - open_started).count());
        if (!source_opened) {
            AICO_LOG_WARN("capture", "source_open_completed",
                           "open_attempt=%llu ok=0 elapsed_ms=%llu",
                           static_cast<unsigned long long>(open_attempt),
                           static_cast<unsigned long long>(open_elapsed_ms));
            recover(&backoff_ms,
                    options_.recovery_idle_max_backoff_ms,
                    "open failed",
                    true);
            continue;
        }
        AICO_LOG_INFO("capture", "source_open_completed",
                       "open_attempt=%llu ok=1 elapsed_ms=%llu",
                       static_cast<unsigned long long>(open_attempt),
                       static_cast<unsigned long long>(open_elapsed_ms));
        if (!options_.keep_streamon) {
            const std::chrono::steady_clock::time_point pause_started =
                std::chrono::steady_clock::now();
            AICO_LOG_INFO("capture", "initial_pause_started",
                           "open_attempt=%llu",
                           static_cast<unsigned long long>(open_attempt));
            const bool initially_paused = source_->pause();
            const uint64_t pause_elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - pause_started).count());
            if (!initially_paused) {
                AICO_LOG_WARN("capture", "initial_pause_completed",
                               "open_attempt=%llu ok=0 elapsed_ms=%llu",
                               static_cast<unsigned long long>(open_attempt),
                               static_cast<unsigned long long>(pause_elapsed_ms));
                recover(&backoff_ms,
                        options_.recovery_idle_max_backoff_ms,
                        "initial pause failed",
                        true);
                continue;
            }
            AICO_LOG_INFO("capture", "initial_pause_completed",
                           "open_attempt=%llu ok=1 elapsed_ms=%llu",
                           static_cast<unsigned long long>(open_attempt),
                           static_cast<unsigned long long>(pause_elapsed_ms));
        }

        // Publish readiness as soon as the source is open. On-demand clients
        // gate their first capture on this state, so leaving it at STARTING or
        // RECOVERING until a capture succeeds deadlocks them: no request is
        // sent, so no capture can succeed, so the state never advances. open()
        // already synced HDMI timings and ran the warmup, so the pipeline is
        // genuinely ready here. Losing the link still fails open() and keeps
        // the service in RECOVERING on the idle backoff.
        server_->set_state("RUNNING");
        uint64_t idle_requested_generation = 0;
        uint64_t idle_completed_generation = 0;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            idle_requested_generation = requested_generation_;
            idle_completed_generation = completed_generation_;
        }
        AICO_LOG_INFO("capture", "worker_idle",
                       "open_attempt=%llu stream_active=%d requested_generation=%llu completed_generation=%llu",
                       static_cast<unsigned long long>(open_attempt),
                       options_.keep_streamon ? 1 : 0,
                       static_cast<unsigned long long>(idle_requested_generation),
                       static_cast<unsigned long long>(idle_completed_generation));

        while (running_) {
            uint64_t generation = 0;
            {
                std::unique_lock<std::mutex> lock(mutex_);
                work_cv_.wait(lock, [&]() {
                    return !running_ || restart_requested_ ||
                           completed_generation_ < requested_generation_;
                });
                if (!running_) {
                    break;
                }
                if (!restart_requested_) {
                    generation = requested_generation_;
                }
            }

            const int max_backoff_ms = captured_since_open
                ? options_.recovery_max_backoff_ms
                : options_.recovery_idle_max_backoff_ms;

            if (restart_requested_.load()) {
                backoff_ms = initial_backoff_ms();
                recover(&backoff_ms,
                        options_.recovery_max_backoff_ms,
                        "restart requested",
                        false);
                break;
            }

            CapturedFrame frame;
            const std::chrono::steady_clock::time_point cycle_started =
                std::chrono::steady_clock::now();
            AICO_LOG_INFO("capture", "cycle_started",
                           "generation=%llu keep_streamon=%d warmup_frames=%d",
                           static_cast<unsigned long long>(generation),
                           options_.keep_streamon ? 1 : 0, options_.warmup_frames);
            uint64_t resume_elapsed_ms = 0;
            uint64_t warmup_elapsed_ms = 0;
            uint64_t frame_elapsed_ms = 0;
            uint64_t pause_elapsed_ms = 0;
            const std::chrono::steady_clock::time_point resume_started =
                std::chrono::steady_clock::now();
            const bool resumed = options_.keep_streamon || source_->resume();
            resume_elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - resume_started).count());

            bool warmed_up = resumed;
            const std::chrono::steady_clock::time_point warmup_started =
                std::chrono::steady_clock::now();
            if (warmed_up && options_.warmup_frames > 0) {
                AICO_LOG_INFO("capture", "warmup_started",
                               "generation=%llu frames=%d",
                               static_cast<unsigned long long>(generation),
                               options_.warmup_frames);
            }
            for (int i = 0; warmed_up && i < options_.warmup_frames; ++i) {
                warmed_up = source_->discard();
            }
            warmup_elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - warmup_started).count());
            bool captured = false;
            if (warmed_up) {
                AICO_LOG_INFO("capture", "frame_read_started",
                               "generation=%llu",
                               static_cast<unsigned long long>(generation));
                const std::chrono::steady_clock::time_point frame_started =
                    std::chrono::steady_clock::now();
                captured = source_->capture(&frame);
                frame_elapsed_ms = static_cast<uint64_t>(
                    std::chrono::duration_cast<std::chrono::milliseconds>(
                        std::chrono::steady_clock::now() - frame_started).count());
            }
            const std::chrono::steady_clock::time_point pause_started =
                std::chrono::steady_clock::now();
            const bool paused = options_.keep_streamon || source_->pause();
            pause_elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - pause_started).count());
            const bool ok = resumed && warmed_up && captured && paused;
            const uint64_t cycle_elapsed_ms = static_cast<uint64_t>(
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now() - cycle_started).count());
            if (ok) {
                AICO_LOG_INFO("capture", "cycle_completed",
                               "generation=%llu ok=1 elapsed_ms=%llu resume_ms=%llu warmup_ms=%llu frame_read_ms=%llu pause_ms=%llu bytes=%zu width=%u height=%u format=%s",
                               static_cast<unsigned long long>(generation),
                               static_cast<unsigned long long>(cycle_elapsed_ms),
                               static_cast<unsigned long long>(resume_elapsed_ms),
                               static_cast<unsigned long long>(warmup_elapsed_ms),
                               static_cast<unsigned long long>(frame_elapsed_ms),
                               static_cast<unsigned long long>(pause_elapsed_ms),
                               frame.data.size(), frame.metadata.width,
                               frame.metadata.height,
                               frame.metadata.pixel_format.c_str());
            } else {
                AICO_LOG_WARN("capture", "cycle_completed",
                               "generation=%llu ok=0 elapsed_ms=%llu resume_ms=%llu warmup_ms=%llu frame_read_ms=%llu pause_ms=%llu resumed=%d warmed_up=%d captured=%d paused=%d",
                               static_cast<unsigned long long>(generation),
                               static_cast<unsigned long long>(cycle_elapsed_ms),
                               static_cast<unsigned long long>(resume_elapsed_ms),
                               static_cast<unsigned long long>(warmup_elapsed_ms),
                               static_cast<unsigned long long>(frame_elapsed_ms),
                               static_cast<unsigned long long>(pause_elapsed_ms),
                               resumed ? 1 : 0, warmed_up ? 1 : 0,
                               captured ? 1 : 0, paused ? 1 : 0);
            }

            if (ok) {
                maybe_start_jpeg_warmup(frame);
            }

            {
                std::lock_guard<std::mutex> lock(mutex_);
                completed_generation_ = generation;
                completed_status_ = ok ? FrameServiceStatus::OK
                                       : FrameServiceStatus::SERVICE_RECOVERING;
                if (ok) {
                    completed_frame_ = std::move(frame);
                } else {
                    completed_frame_ = CapturedFrame();
                }
            }
            capture_cv_.notify_all();

            if (!ok) {
                const char* error = !resumed ? "resume failed"
                                  : !warmed_up ? "warmup failed"
                                  : !captured ? "capture failed"
                                              : "pause failed";
                recover(&backoff_ms, max_backoff_ms, error, true);
                break;
            }
            captured_since_open = true;
            backoff_ms = initial_backoff_ms();
            server_->set_state("RUNNING");
            AICO_LOG_INFO("capture", "worker_idle",
                           "open_attempt=%llu stream_active=%d requested_generation=%llu completed_generation=%llu",
                           static_cast<unsigned long long>(open_attempt),
                           options_.keep_streamon ? 1 : 0,
                           static_cast<unsigned long long>(generation),
                           static_cast<unsigned long long>(generation));
        }
    }
}

}  // namespace aico
