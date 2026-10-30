#include "audio_record_session.h"
#include "aico_log.h"
#include <chrono>
#include <cmath>
#include <stdio.h>
#include <stdint.h>
#include <unistd.h>
#include <cstdlib>
#include <cstring>
#include <pthread.h>
#include <sched.h>

namespace aico {

static int16_t mono_sample_at(const int16_t* src, size_t frame_index, int channels) {
    if (channels == 2) {
        const int32_t left = src[frame_index * 2];
        const int32_t right = src[frame_index * 2 + 1];
        return static_cast<int16_t>((left + right) / 2);
    }
    return src[frame_index];
}

static void write_i16_le(std::vector<uint8_t>* out, size_t sample_index, int16_t sample) {
    const uint16_t value = static_cast<uint16_t>(sample);
    const size_t offset = sample_index * sizeof(int16_t);
    (*out)[offset] = static_cast<uint8_t>(value & 0xffu);
    (*out)[offset + 1] = static_cast<uint8_t>((value >> 8) & 0xffu);
}

// Convert captured PCM to the requested mono PCM16 stream in one pass.
static std::vector<uint8_t> convert_to_target_mono_pcm16(const int16_t* src,
                                                         size_t total_samples,
                                                         int channels,
                                                         int src_rate,
                                                         int dst_rate) {
    if (!src || total_samples == 0 || src_rate <= 0 || dst_rate <= 0) return {};
    if (channels != 2) channels = 1;
    const size_t source_frames = (channels == 2 && total_samples >= 2)
                                     ? (total_samples / 2)
                                     : total_samples;
    if (source_frames == 0) return {};

    if (channels == 1 && src_rate == dst_rate) {
        const uint8_t* bytes = reinterpret_cast<const uint8_t*>(src);
        return std::vector<uint8_t>(bytes, bytes + source_frames * sizeof(int16_t));
    }

    if (channels == 2 && src_rate == dst_rate) {
        std::vector<uint8_t> out(source_frames * sizeof(int16_t));
        for (size_t i = 0; i < source_frames; ++i) {
            write_i16_le(&out, i, mono_sample_at(src, i, channels));
        }
        return out;
    }

    if (channels == 2 && src_rate == dst_rate * 2) {
        const size_t out_count = source_frames / 2;
        std::vector<uint8_t> out(out_count * sizeof(int16_t));
        for (size_t i = 0; i < out_count; ++i) {
            write_i16_le(&out, i, mono_sample_at(src, i * 2, channels));
        }
        return out;
    }

    const double ratio = static_cast<double>(src_rate) / static_cast<double>(dst_rate);
    size_t out_count = static_cast<size_t>(static_cast<double>(source_frames) / ratio);
    if (out_count == 0) out_count = 1;
    std::vector<uint8_t> out(out_count * sizeof(int16_t));
    for (size_t i = 0; i < out_count; ++i) {
        double pos = static_cast<double>(i) * ratio;
        size_t idx = static_cast<size_t>(pos);
        if (idx >= source_frames) idx = source_frames - 1;
        size_t idx_next = (idx + 1 < source_frames) ? (idx + 1) : idx;
        double frac = pos - static_cast<double>(idx);
        double a = static_cast<double>(mono_sample_at(src, idx, channels));
        double b = static_cast<double>(mono_sample_at(src, idx_next, channels));
        write_i16_le(&out, i, static_cast<int16_t>(a * (1.0 - frac) + b * frac));
    }
    return out;
}

static int snap_sample_rate(int observed_rate) {
    static const int kRates[] = {8000, 16000, 32000, 44100, 48000};
    int best = kRates[0];
    int best_diff = std::abs(observed_rate - best);
    for (size_t i = 1; i < sizeof(kRates) / sizeof(kRates[0]); ++i) {
        int diff = std::abs(observed_rate - kRates[i]);
        if (diff < best_diff) {
            best = kRates[i];
            best_diff = diff;
        }
    }
    return best;
}

AudioRecordSession::AudioRecordSession(uint64_t session_id, const AudioFormat& fmt)
    : session_id_(session_id), fmt_(fmt), stopped_(false) {}

AudioRecordSession::~AudioRecordSession() {
    stop();
    join();
}

void AudioRecordSession::join() {
    if (capture_thread_.joinable()) capture_thread_.join();
}

AudioConfig AudioRecordSession::hardware_capture_config() const {
    AudioConfig cfg;
    // RV1106 VQE accepts 8/16/48 kHz, while realtime providers such as xAI
    // require 24 kHz PCM. Capture at 16 kHz and use the existing conversion
    // path to emit 24 kHz while keeping full-duplex playback at 16 kHz.
    cfg.sample_rate = fmt_.sample_rate == 24000
                          ? 16000
                          : static_cast<int>(fmt_.sample_rate);
    cfg.channels    = static_cast<int>(fmt_.channels);
    cfg.bit_width   = static_cast<int>(fmt_.bit_width);
    return cfg;
}

bool AudioRecordSession::start() {
    const char* enabled = std::getenv("AUDIO_SERVICE_DENOISE");
    if (!enabled || std::strcmp(enabled, "0") != 0) {
        if (fmt_.channels == 1 && fmt_.bit_width == 16 &&
            (fmt_.sample_rate == 16000 || fmt_.sample_rate == 24000)) {
            const char* directory = std::getenv("AUDIO_SERVICE_DTLN_DIR");
            std::string error;
            auto model = create_dtln_denoiser(directory && *directory ? directory :
                                             "/usr/lib/aico/models/dtln", &error);
            if (model) {
                denoiser_.reset(new AudioDenoiser(std::move(model)));
                AICO_LOG_INFO("recording", "dtln_enabled", "rate=16000 hop=128 delay_samples=384");
            } else {
                AICO_LOG_WARN("recording", "dtln_unavailable", "fallback=vqe error=%s", error.c_str());
            }
        } else {
            AICO_LOG_WARN("recording", "dtln_format_bypassed", "rate=%u channels=%u bits=%u",
                          fmt_.sample_rate, fmt_.channels, fmt_.bit_width);
        }
    }
    AudioConfig cfg = hardware_capture_config();
    if (cfg.sample_rate != static_cast<int>(fmt_.sample_rate)) {
        AICO_LOG_INFO("recording", "capture_rate_resampled",
                       "hardware_rate=%d target_rate=%u",
                       cfg.sample_rate, fmt_.sample_rate);
    }
    if (!capture_.init(cfg)) {
        AICO_LOG_ERROR("recording", "capture_init_failed", "session_id=%llu",
                        static_cast<unsigned long long>(session_id_));
        return false;
    }

    // RV1106 capture path typically delivers stereo even in mono mode.
    hw_sample_rate_ = cfg.sample_rate;
    hw_channels_    = 2;
    prev_timestamp_us_ = 0;

    capture_done_.store(false);
    capture_thread_ = std::thread(&AudioRecordSession::capture_loop, this);
    return true;
}

void AudioRecordSession::stop() {
    if (stopped_.exchange(true)) return;
    // Do not call capture_.stop() here: the capture thread owns the device and
    // will exit its loop on the next get_frame() timeout once stopped_ is set.
    cv_.notify_all();
}

AicoServiceStatus AudioRecordSession::pop_chunk(uint32_t timeout_ms, AudioChunkResult* out) {
    std::unique_lock<std::mutex> lock(mutex_);
    auto deadline = std::chrono::steady_clock::now() +
                    std::chrono::milliseconds(timeout_ms);

    while (queue_.empty()) {
        if (stopped_.load() && capture_done_.load()) {
            out->pcm.clear();
            out->end_of_stream = true;
            return AicoServiceStatus::OK;
        }
        if (cv_.wait_until(lock, deadline) == std::cv_status::timeout) {
            return AicoServiceStatus::TIMEOUT;
        }
    }

    out->pcm = std::move(queue_.front());
    queue_.pop();
    out->end_of_stream = false;
    return AicoServiceStatus::OK;
}

void AudioRecordSession::maybe_update_hw_sample_rate(uint64_t timestamp_us,
                                                     size_t frame_samples_per_channel) {
    if (prev_timestamp_us_ == 0 || timestamp_us <= prev_timestamp_us_) {
        prev_timestamp_us_ = timestamp_us;
        return;
    }
    uint64_t delta_us = timestamp_us - prev_timestamp_us_;
    prev_timestamp_us_ = timestamp_us;
    if (delta_us == 0 || frame_samples_per_channel == 0) return;

    int observed = static_cast<int>((frame_samples_per_channel * 1000000ULL) / delta_us);
    int snapped = snap_sample_rate(observed);
    int diff = std::abs(snapped - hw_sample_rate_);
    if (diff > hw_sample_rate_ / 10) {
        AICO_LOG_WARN("recording", "sample_rate_adjusted",
                       "previous_rate=%d sample_rate=%d observed_rate=%d frame_samples=%zu delta_us=%llu",
                       hw_sample_rate_, snapped, observed, frame_samples_per_channel,
                       static_cast<unsigned long long>(delta_us));
        hw_sample_rate_ = snapped;
    }
}

void AudioRecordSession::enqueue_pcm(std::vector<uint8_t> chunk) {
    if (denoiser_ && !denoiser_->active() && !denoise_failure_logged_) {
        AICO_LOG_WARN("recording", "dtln_failed", "fallback=vqe error=%s",
                      denoiser_->error().c_str());
        denoise_failure_logged_ = true;
    }
    if (chunk.empty()) return;
    if (denoiser_ && fmt_.sample_rate != 16000) {
        // DTLN runs before the existing 16 -> 24 kHz provider conversion.
        std::vector<int16_t> samples(chunk.size() / 2);
        std::memcpy(samples.data(), chunk.data(), samples.size() * sizeof(int16_t));
        chunk = convert_to_target_mono_pcm16(samples.data(), samples.size(), 1,
                                             16000, static_cast<int>(fmt_.sample_rate));
    }
    std::lock_guard<std::mutex> lock(mutex_);
    if (queue_.size() < kMaxQueueChunks) {
        queue_.push(std::move(chunk));
        cv_.notify_one();
    }
}

void AudioRecordSession::capture_loop() {
    if (denoiser_) {
        const char* setting = std::getenv("AUDIO_SERVICE_CAPTURE_RT_PRIORITY");
        const int priority = setting ? std::atoi(setting) : 10;
        if (priority > 0 && priority <= 99) {
            sched_param param = {};
            param.sched_priority = priority;
            const int rc = pthread_setschedparam(pthread_self(), SCHED_FIFO, &param);
            if (rc != 0) AICO_LOG_WARN("recording", "capture_rt_unavailable", "error=%d", rc);
        }
    }
    int consecutive_failures = 0;

    while (!stopped_.load()) {
        AudioFrame frame;
        if (!capture_.get_frame(frame)) {
            consecutive_failures++;
            if (consecutive_failures >= 5) {
                AICO_LOG_WARN("recording", "capture_restart_requested",
                               "session_id=%llu consecutive_failures=%d",
                               static_cast<unsigned long long>(session_id_),
                               consecutive_failures);
                capture_.stop();
                if (denoiser_) enqueue_pcm(denoiser_->finish());
                AudioConfig cfg = hardware_capture_config();
                if (!capture_.init(cfg)) {
                    AICO_LOG_ERROR("recording", "capture_reinit_failed", "session_id=%llu",
                                    static_cast<unsigned long long>(session_id_));
                    usleep(20000);
                } else {
                    consecutive_failures = 0;
                    prev_timestamp_us_ = 0;
                }
            } else {
                usleep(5000);
            }
            continue;
        }
        consecutive_failures = 0;

        if (frame.data && frame.length > 0) {
            const int16_t* src = reinterpret_cast<const int16_t*>(frame.data);
            size_t total_samples = frame.length / sizeof(int16_t);
            if (total_samples == 0) {
                capture_.release_frame();
                continue;
            }
            if (frame.channels == 1 || frame.channels == 2) {
                hw_channels_ = static_cast<int>(frame.channels);
            } else if ((total_samples % 2) != 0) {
                // Odd sample count cannot be stereo-interleaved.
                hw_channels_ = 1;
            }
            if (frame.sample_rate > 0) {
                hw_sample_rate_ = static_cast<int>(frame.sample_rate);
            }

            size_t frame_samples_per_channel =
                (hw_channels_ == 2 && total_samples >= 2) ? (total_samples / 2) : total_samples;
            maybe_update_hw_sample_rate(frame.timestamp, frame_samples_per_channel);

            std::vector<uint8_t> chunk = convert_to_target_mono_pcm16(
                src, total_samples, hw_channels_, hw_sample_rate_,
                denoiser_ ? 16000 : static_cast<int>(fmt_.sample_rate));
            // Release the hardware buffer before running four NPU graphs.
            capture_.release_frame();
            if (denoiser_) {
                chunk = denoiser_->process(chunk);
            }
            enqueue_pcm(std::move(chunk));
            continue;
        }
        capture_.release_frame();
    }

    capture_.stop();
    if (denoiser_) enqueue_pcm(denoiser_->finish());
    // EOF is visible only after the delayed DTLN tail has been queued.
    {
        std::lock_guard<std::mutex> lock(mutex_);
        capture_done_.store(true);
    }
    cv_.notify_all();
}

}  // namespace aico
