#include "audio_denoiser.h"

#include <algorithm>
#include <cmath>
#include <stdexcept>

namespace aico {

AudioDenoiser::AudioDenoiser(std::unique_ptr<DenoiseHop> model)
    : model_(std::move(model)) {}

void AudioDenoiser::reset() {
    pending_.clear();
    processed_ = 0;
    warmup_ = 384;
    previous_input_ = previous_output_ = 0;
    if (model_) model_->reset();
}

void AudioDenoiser::bypass(std::vector<uint8_t>* out) {
    out->insert(out->end(), pending_.begin(), pending_.end());
    pending_.clear();
    processed_ = 0;
    model_.reset();
}

void AudioDenoiser::filter_pending_samples(float* input, size_t count) {
    for (size_t i = 0; i < count; ++i) {
        const size_t offset = (processed_ + i) * 2;
        const int16_t sample = static_cast<int16_t>(
            uint16_t(pending_[offset]) | (uint16_t(pending_[offset + 1]) << 8));
        const float current = sample / 32768.f;
        // Same DC high-pass used in the validated board recordings.
        input[i] = current - previous_input_ + 0.995f * previous_output_;
        previous_input_ = current;
        previous_output_ = input[i];
    }
}

void AudioDenoiser::hop(const float* input, std::vector<uint8_t>* out) {
    float enhanced[128];
    model_->process(input, enhanced);
    for (float sample : enhanced) {
        if (!std::isfinite(sample)) throw std::runtime_error("nonfinite DTLN output");
    }
    if (warmup_) {
        warmup_ -= 128;
        return;
    }
    const size_t count = std::min(size_t(128), pending_.size() / 2);
    for (size_t i = 0; i < count; ++i) {
        const float value = std::max(-32768.f, std::min(32767.f, enhanced[i] * 32768.f));
        const uint16_t sample = static_cast<uint16_t>(static_cast<int16_t>(std::lround(value)));
        out->push_back(static_cast<uint8_t>(sample));
        out->push_back(static_cast<uint8_t>(sample >> 8));
    }
    pending_.erase(pending_.begin(), pending_.begin() + count * 2);
    processed_ -= std::min(processed_, count);
}

std::vector<uint8_t> AudioDenoiser::process(const std::vector<uint8_t>& pcm) {
    if (!model_) return pcm;
    pending_.insert(pending_.end(), pcm.begin(), pcm.end());
    std::vector<uint8_t> out;
    try {
        while (pending_.size() / 2 - processed_ >= 128) {
            float input[128];
            filter_pending_samples(input, 128);
            processed_ += 128;
            hop(input, &out);
        }
    } catch (const std::exception& e) {
        error_ = e.what();
        bypass(&out);
    }
    return out;
}

std::vector<uint8_t> AudioDenoiser::finish() {
    if (!model_) return {};
    std::vector<uint8_t> out;
    try {
        // At most one partial hop plus three delay hops remain.
        while (!pending_.empty()) {
            float input[128] = {};
            const size_t remaining = pending_.size() / 2 - processed_;
            filter_pending_samples(input, remaining);
            if (pending_.size() % 2) throw std::runtime_error("incomplete PCM16 sample");
            processed_ += remaining;
            hop(input, &out);
        }
    } catch (const std::exception& e) {
        error_ = e.what();
        bypass(&out);
    }
    reset();
    return out;
}

}  // namespace aico
