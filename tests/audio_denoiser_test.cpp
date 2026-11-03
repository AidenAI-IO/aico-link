#include "audio_denoiser.h"
#include "doctest.h"

#include <algorithm>
#include <cmath>
#include <deque>
#include <limits>
#include <stdexcept>

namespace {
class DelayedIdentity : public aico::DenoiseHop {
public:
    explicit DelayedIdentity(int fail_at = -1, bool nonfinite = false)
        : fail_at_(fail_at), nonfinite_(nonfinite) { reset(); }
    void reset() override { delay_.assign(384, 0); calls_ = 0; }
    void process(const float* input, float* output) override {
        if (calls_++ == fail_at_) {
            if (!nonfinite_) throw std::runtime_error("injected RKNN failure");
            std::fill(output, output + 128, std::numeric_limits<float>::quiet_NaN());
            return;
        }
        delay_.insert(delay_.end(), input, input + 128);
        for (size_t i = 0; i < 128; ++i) { output[i] = delay_.front(); delay_.pop_front(); }
    }
private:
    int fail_at_, calls_ = 0;
    bool nonfinite_;
    std::deque<float> delay_;
};

std::vector<uint8_t> signal(size_t samples) {
    std::vector<uint8_t> bytes;
    for (size_t i = 0; i < samples; ++i) {
        const uint16_t value = static_cast<uint16_t>(static_cast<int16_t>(10000 * std::sin(i * .07)));
        bytes.push_back(value & 255);
        bytes.push_back(value >> 8);
    }
    return bytes;
}

std::vector<uint8_t> filtered(const std::vector<uint8_t>& input) {
    std::vector<uint8_t> output;
    float previous = 0, value = 0;
    for (size_t i = 0; i < input.size(); i += 2) {
        const float current = static_cast<int16_t>(uint16_t(input[i]) | uint16_t(input[i+1]) << 8) / 32768.f;
        value = current - previous + .995f * value;
        previous = current;
        const uint16_t sample = static_cast<uint16_t>(static_cast<int16_t>(std::lround(value * 32768.f)));
        output.push_back(sample & 255);
        output.push_back(sample >> 8);
    }
    return output;
}

void append(std::vector<uint8_t>* output, const std::vector<uint8_t>& chunk) {
    output->insert(output->end(), chunk.begin(), chunk.end());
}
}  // namespace

TEST_CASE("DTLN stream preserves sample order and length across chunk boundaries and EOF") {
    for (size_t length : {size_t(0), size_t(1), size_t(127), size_t(128), size_t(129),
                          size_t(511), size_t(512), size_t(513), size_t(16001)}) {
        for (size_t chunk_size : {size_t(1), size_t(254), size_t(256), size_t(1030)}) {
            aico::AudioDenoiser stream(std::unique_ptr<aico::DenoiseHop>(new DelayedIdentity));
            const auto input = signal(length);
            std::vector<uint8_t> output;
            for (size_t offset = 0; offset < input.size(); offset += chunk_size) {
                const size_t end = std::min(offset + chunk_size, input.size());
                append(&output, stream.process({input.begin() + offset, input.begin() + end}));
            }
            append(&output, stream.finish());
            CHECK(output == filtered(input));
            CHECK(stream.active());
            CHECK(stream.finish().empty());
            auto repeated = stream.process(input);
            append(&repeated, stream.finish());
            CHECK(repeated == output);
        }
    }
}

TEST_CASE("DTLN failures return buffered original audio and permanently bypass the model") {
    for (int fail_at : {0, 2, 4, 7}) {
        for (bool nonfinite : {false, true}) {
            aico::AudioDenoiser stream(std::unique_ptr<aico::DenoiseHop>(new DelayedIdentity(fail_at, nonfinite)));
            const auto input = signal(2048);
            auto output = stream.process(input);
            append(&output, stream.finish());
            const size_t enhanced_bytes = fail_at > 3 ? size_t(fail_at - 3) * 256 : 0;
            auto expected = filtered(input);
            std::copy(input.begin() + enhanced_bytes, input.end(), expected.begin() + enhanced_bytes);
            CHECK(output == expected);
            CHECK_FALSE(stream.active());
            CHECK_FALSE(stream.error().empty());
            CHECK(stream.process(input) == input);
        }
    }
}

TEST_CASE("DTLN failure during tail flush preserves all remaining samples") {
    aico::AudioDenoiser stream(std::unique_ptr<aico::DenoiseHop>(new DelayedIdentity(2)));
    const auto input = signal(129);
    auto output = stream.process(input);
    CHECK(output.empty());
    append(&output, stream.finish());
    CHECK(output == input);
    CHECK_FALSE(stream.active());
}

TEST_CASE("DTLN reset discards recurrent and partial-hop state") {
    aico::AudioDenoiser stream(std::unique_ptr<aico::DenoiseHop>(new DelayedIdentity));
    stream.process(signal(333));
    stream.reset();
    const auto input = signal(513);
    auto output = stream.process(input);
    append(&output, stream.finish());
    CHECK(output == filtered(input));
}

TEST_CASE("Missing DTLN runtime reports a recoverable initialization error") {
    std::string error;
    CHECK_FALSE(aico::create_dtln_denoiser("/nonexistent/aico-dtln-test", &error));
    CHECK_FALSE(error.empty());
}
