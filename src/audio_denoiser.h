#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

namespace aico {

// One stateful DTLN hop: 128 normalized mono samples at 16 kHz.
// Implementations throw on failure; no exception escapes AudioDenoiser.
class DenoiseHop {
public:
    virtual ~DenoiseHop() {}
    virtual void process(const float* input, float* output) = 0;
    virtual void reset() = 0;
};

std::unique_ptr<DenoiseHop> create_dtln_denoiser(const std::string& directory,
                                               std::string* error);

// Accepts arbitrary PCM16LE chunk boundaries. Removes the 384-sample startup
// padding and flushes the delayed tail at EOF, preserving the input length.
// On inference failure, buffered original samples are returned and subsequent
// input bypasses the model. State belongs to one recording session only.
class AudioDenoiser {
public:
    explicit AudioDenoiser(std::unique_ptr<DenoiseHop> model);
    std::vector<uint8_t> process(const std::vector<uint8_t>& pcm);
    std::vector<uint8_t> finish();
    void reset();
    bool active() const { return model_.get() != nullptr; }
    const std::string& error() const { return error_; }

private:
    void filter_pending_samples(float* input, size_t count);
    void hop(const float* input, std::vector<uint8_t>* out);
    void bypass(std::vector<uint8_t>* out);
    std::unique_ptr<DenoiseHop> model_;
    std::vector<uint8_t> pending_;
    size_t processed_ = 0;
    size_t warmup_ = 384;
    float previous_input_ = 0;
    float previous_output_ = 0;
    std::string error_;
};

}  // namespace aico
