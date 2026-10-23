#pragma once

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <sstream>
#include <string>
#include <sys/resource.h>
#include <vector>

#if defined(__ARM_NEON) || defined(__ARM_NEON__)
#include <arm_neon.h>
#endif

namespace aico_vad {

constexpr int kSampleRate = 16000;
constexpr int kFrameSamples = 512;
constexpr int kSTFTSize = 256;
constexpr int kSTFTHop = 128;
constexpr int kSTFTBins = 129;
constexpr int kSileroContextSamples = 64;
constexpr int kFeatureFrames = 4;
constexpr int kHidden = 128;
constexpr int kGates = 4 * kHidden;

inline bool read_exact(std::istream& in, char* dst, std::size_t bytes) {
    in.read(dst, static_cast<std::streamsize>(bytes));
    return static_cast<std::size_t>(in.gcount()) == bytes;
}

inline void protocol_error(const std::string& message) {
    std::cout << "ERR " << message << std::endl;
}

inline double timeval_ms(const timeval& value) {
    return static_cast<double>(value.tv_sec) * 1000.0 +
           static_cast<double>(value.tv_usec) / 1000.0;
}

struct CPUUsage {
    double user_ms = 0.0;
    double system_ms = 0.0;
};

inline CPUUsage current_cpu_usage() {
    rusage usage;
    std::memset(&usage, 0, sizeof(usage));
    getrusage(RUSAGE_SELF, &usage);
    CPUUsage out;
    out.user_ms = timeval_ms(usage.ru_utime);
    out.system_ms = timeval_ms(usage.ru_stime);
    return out;
}

inline double cpu_total_ms(const CPUUsage& usage) {
    return usage.user_ms + usage.system_ms;
}

inline float dot_product(const float* a, const float* b, int n) {
#if defined(__ARM_NEON) || defined(__ARM_NEON__)
    float32x4_t sum = vdupq_n_f32(0.0f);
    int i = 0;
    for (; i + 4 <= n; i += 4) {
        sum = vmlaq_f32(sum, vld1q_f32(a + i), vld1q_f32(b + i));
    }
    float32x2_t pair = vadd_f32(vget_low_f32(sum), vget_high_f32(sum));
    pair = vpadd_f32(pair, pair);
    float out = vget_lane_f32(pair, 0);
    for (; i < n; ++i) {
        out += a[i] * b[i];
    }
    return out;
#else
    float out = 0.0f;
    for (int i = 0; i < n; ++i) {
        out += a[i] * b[i];
    }
    return out;
#endif
}

inline void dual_dot_product(const float* samples,
                             const float* real_basis,
                             const float* imag_basis,
                             int n,
                             float* real,
                             float* imag) {
#if defined(__ARM_NEON) || defined(__ARM_NEON__)
    float32x4_t real_sum = vdupq_n_f32(0.0f);
    float32x4_t imag_sum = vdupq_n_f32(0.0f);
    int i = 0;
    for (; i + 4 <= n; i += 4) {
        const float32x4_t sample = vld1q_f32(samples + i);
        real_sum = vmlaq_f32(real_sum, sample, vld1q_f32(real_basis + i));
        imag_sum = vmlaq_f32(imag_sum, sample, vld1q_f32(imag_basis + i));
    }
    float32x2_t real_pair = vadd_f32(vget_low_f32(real_sum), vget_high_f32(real_sum));
    float32x2_t imag_pair = vadd_f32(vget_low_f32(imag_sum), vget_high_f32(imag_sum));
    real_pair = vpadd_f32(real_pair, real_pair);
    imag_pair = vpadd_f32(imag_pair, imag_pair);
    float real_out = vget_lane_f32(real_pair, 0);
    float imag_out = vget_lane_f32(imag_pair, 0);
    for (; i < n; ++i) {
        const float sample = samples[i];
        real_out += sample * real_basis[i];
        imag_out += sample * imag_basis[i];
    }
    *real = real_out;
    *imag = imag_out;
#else
    float real_out = 0.0f;
    float imag_out = 0.0f;
    for (int i = 0; i < n; ++i) {
        const float sample = samples[i];
        real_out += sample * real_basis[i];
        imag_out += sample * imag_basis[i];
    }
    *real = real_out;
    *imag = imag_out;
#endif
}

inline void dual_dot_product2(const float* samples,
                              const float* real_basis0,
                              const float* imag_basis0,
                              const float* real_basis1,
                              const float* imag_basis1,
                              int n,
                              float* real0,
                              float* imag0,
                              float* real1,
                              float* imag1) {
#if defined(__ARM_NEON) || defined(__ARM_NEON__)
    float32x4_t r0 = vdupq_n_f32(0.0f);
    float32x4_t i0 = vdupq_n_f32(0.0f);
    float32x4_t r1 = vdupq_n_f32(0.0f);
    float32x4_t i1 = vdupq_n_f32(0.0f);
    int i = 0;
    for (; i + 4 <= n; i += 4) {
        const float32x4_t sample = vld1q_f32(samples + i);
        r0 = vmlaq_f32(r0, sample, vld1q_f32(real_basis0 + i));
        i0 = vmlaq_f32(i0, sample, vld1q_f32(imag_basis0 + i));
        r1 = vmlaq_f32(r1, sample, vld1q_f32(real_basis1 + i));
        i1 = vmlaq_f32(i1, sample, vld1q_f32(imag_basis1 + i));
    }
    float32x2_t p0 = vadd_f32(vget_low_f32(r0), vget_high_f32(r0));
    float32x2_t p1 = vadd_f32(vget_low_f32(i0), vget_high_f32(i0));
    float32x2_t p2 = vadd_f32(vget_low_f32(r1), vget_high_f32(r1));
    float32x2_t p3 = vadd_f32(vget_low_f32(i1), vget_high_f32(i1));
    p0 = vpadd_f32(p0, p0);
    p1 = vpadd_f32(p1, p1);
    p2 = vpadd_f32(p2, p2);
    p3 = vpadd_f32(p3, p3);
    float out_r0 = vget_lane_f32(p0, 0);
    float out_i0 = vget_lane_f32(p1, 0);
    float out_r1 = vget_lane_f32(p2, 0);
    float out_i1 = vget_lane_f32(p3, 0);
    for (; i < n; ++i) {
        const float sample = samples[i];
        out_r0 += sample * real_basis0[i];
        out_i0 += sample * imag_basis0[i];
        out_r1 += sample * real_basis1[i];
        out_i1 += sample * imag_basis1[i];
    }
    *real0 = out_r0;
    *imag0 = out_i0;
    *real1 = out_r1;
    *imag1 = out_i1;
#else
    float out_r0 = 0.0f;
    float out_i0 = 0.0f;
    float out_r1 = 0.0f;
    float out_i1 = 0.0f;
    for (int i = 0; i < n; ++i) {
        const float sample = samples[i];
        out_r0 += sample * real_basis0[i];
        out_i0 += sample * imag_basis0[i];
        out_r1 += sample * real_basis1[i];
        out_i1 += sample * imag_basis1[i];
    }
    *real0 = out_r0;
    *imag0 = out_i0;
    *real1 = out_r1;
    *imag1 = out_i1;
#endif
}

inline float sigmoid(float x) {
    return 1.0f / (1.0f + std::exp(-x));
}

template <typename T>
bool read_pod(std::ifstream& file, T* value, const char* label, std::string* err) {
    file.read(reinterpret_cast<char*>(value), sizeof(T));
    if (!file) {
        if (err) *err = std::string("weights file truncated while reading ") + label;
        return false;
    }
    return true;
}

inline bool read_float_array(std::ifstream& file,
                             std::vector<float>* values,
                             const char* label,
                             std::string* err) {
    if (values->empty()) {
        return true;
    }
    file.read(reinterpret_cast<char*>(values->data()),
              static_cast<std::streamsize>(values->size() * sizeof(float)));
    if (!file) {
        if (err) *err = std::string("weights file truncated while reading ") + label;
        return false;
    }
    return true;
}

class STFTFeatureExtractor {
public:
    STFTFeatureExtractor()
        : context_(kSileroContextSamples, 0.0f),
          stft_real_(kSTFTBins * kSTFTSize, 0.0f),
          stft_imag_(kSTFTBins * kSTFTSize, 0.0f) {
        prepare_basis();
    }

    void reset() {
        std::fill(context_.begin(), context_.end(), 0.0f);
    }

    void compute(const int16_t* pcm, float* features) const {
        const int audio_samples = kFrameSamples + kSileroContextSamples;
        float padded[audio_samples + kSTFTSize / 4];
        int write_index = 0;
        for (int i = 0; i < kSileroContextSamples; ++i) {
            padded[write_index++] = context_[i];
        }
        for (int i = 0; i < kFrameSamples; ++i) {
            padded[write_index++] = static_cast<float>(pcm[i]) / 32768.0f;
        }
        for (int i = 0; i < kSTFTSize / 4; ++i) {
            padded[write_index + i] = padded[audio_samples - 2 - i];
        }

        for (int frame = 0; frame < kFeatureFrames; ++frame) {
            const int offset = frame * kSTFTHop;
            int freq = 0;
            for (; freq + 1 < kSTFTBins; freq += 2) {
                float real0 = 0.0f;
                float imag0 = 0.0f;
                float real1 = 0.0f;
                float imag1 = 0.0f;
                const float* real_basis0 = &stft_real_[freq * kSTFTSize];
                const float* imag_basis0 = &stft_imag_[freq * kSTFTSize];
                const float* real_basis1 = &stft_real_[(freq + 1) * kSTFTSize];
                const float* imag_basis1 = &stft_imag_[(freq + 1) * kSTFTSize];
                dual_dot_product2(padded + offset,
                                  real_basis0, imag_basis0,
                                  real_basis1, imag_basis1,
                                  kSTFTSize,
                                  &real0, &imag0, &real1, &imag1);
                features[freq * kFeatureFrames + frame] = std::sqrt(real0 * real0 + imag0 * imag0);
                features[(freq + 1) * kFeatureFrames + frame] = std::sqrt(real1 * real1 + imag1 * imag1);
            }
            for (; freq < kSTFTBins; ++freq) {
                float real = 0.0f;
                float imag = 0.0f;
                const float* real_basis = &stft_real_[freq * kSTFTSize];
                const float* imag_basis = &stft_imag_[freq * kSTFTSize];
                dual_dot_product(padded + offset, real_basis, imag_basis, kSTFTSize, &real, &imag);
                features[freq * kFeatureFrames + frame] = std::sqrt(real * real + imag * imag);
            }
        }
    }

    void update_context(const int16_t* pcm) {
        const int start = kFrameSamples - kSileroContextSamples;
        for (int i = 0; i < kSileroContextSamples; ++i) {
            context_[i] = static_cast<float>(pcm[start + i]) / 32768.0f;
        }
    }

private:
    void prepare_basis() {
        const float pi = 3.14159265358979323846f;
        for (int freq = 0; freq < kSTFTBins; ++freq) {
            for (int n = 0; n < kSTFTSize; ++n) {
                const float window = 0.5f - 0.5f * std::cos((2.0f * pi * n) / kSTFTSize);
                const float angle = (2.0f * pi * freq * n) / kSTFTSize;
                stft_real_[freq * kSTFTSize + n] = window * std::cos(angle);
                stft_imag_[freq * kSTFTSize + n] = window * -std::sin(angle);
            }
        }
    }

    std::vector<float> context_;
    std::vector<float> stft_real_;
    std::vector<float> stft_imag_;
};

struct RecurrentWeights {
    RecurrentWeights()
        : lstm_W(kGates * kHidden, 0.0f),
          lstm_R(kGates * kHidden, 0.0f),
          lstm_B(2 * kGates, 0.0f),
          dec_weight(kHidden, 0.0f) {}

    std::vector<float> lstm_W;
    std::vector<float> lstm_R;
    std::vector<float> lstm_B;
    std::vector<float> dec_weight;
    float dec_bias = 0.0f;
};

struct Conv1dLayerWeights {
    int in_channels = 0;
    int out_channels = 0;
    int kernel_size = 0;
    int stride = 0;
    int padding = 0;
    std::vector<float> weight;
    std::vector<float> packed_weight;
    std::vector<float> bias;

    int output_length(int input_length) const {
        return ((input_length + 2 * padding - kernel_size) / stride) + 1;
    }

    bool set_weights(std::vector<float>* raw_weight, std::vector<float>* raw_bias, std::string* err) {
        const std::size_t expected_weight =
            static_cast<std::size_t>(out_channels) * in_channels * kernel_size;
        if (raw_weight->size() != expected_weight ||
            raw_bias->size() != static_cast<std::size_t>(out_channels)) {
            if (err) *err = "conv encoder weight size mismatch";
            return false;
        }
        weight.swap(*raw_weight);
        bias.swap(*raw_bias);
        packed_weight.assign(expected_weight, 0.0f);
        for (int out = 0; out < out_channels; ++out) {
            for (int k = 0; k < kernel_size; ++k) {
                for (int in = 0; in < in_channels; ++in) {
                    packed_weight[(out * kernel_size + k) * in_channels + in] =
                        weight[(out * in_channels + in) * kernel_size + k];
                }
            }
        }
        return true;
    }

    void run(const float* input, int input_length, std::vector<float>* output) const {
        const int out_length = output_length(input_length);
        output->assign(static_cast<std::size_t>(out_length) * out_channels, 0.0f);
        for (int t = 0; t < out_length; ++t) {
            for (int out = 0; out < out_channels; ++out) {
                float sum = bias[out];
                for (int k = 0; k < kernel_size; ++k) {
                    const int input_pos = t * stride + k - padding;
                    if (input_pos < 0 || input_pos >= input_length) {
                        continue;
                    }
                    const float* input_row = input + input_pos * in_channels;
                    const float* weight_row = &packed_weight[(out * kernel_size + k) * in_channels];
                    sum += dot_product(input_row, weight_row, in_channels);
                }
                (*output)[t * out_channels + out] = sum > 0.0f ? sum : 0.0f;
            }
        }
    }
};

struct SileroWeights {
    RecurrentWeights recurrent;
    std::vector<Conv1dLayerWeights> encoder;
    bool has_encoder = false;

    bool load(const std::string& path, bool require_encoder, std::string* err) {
        encoder.clear();
        has_encoder = false;

        std::ifstream file(path.c_str(), std::ios::binary);
        if (!file) {
            if (err) *err = "cannot open weights: " + path;
            return false;
        }

        char magic[4];
        file.read(magic, sizeof(magic));
        if (!file || std::memcmp(magic, "SVLW", 4) != 0) {
            if (err) *err = "invalid weights magic";
            return false;
        }

        uint32_t version = 0;
        uint32_t hidden = 0;
        uint32_t input_size = 0;
        if (!read_pod(file, &version, "version", err) ||
            !read_pod(file, &hidden, "hidden", err) ||
            !read_pod(file, &input_size, "input size", err)) {
            return false;
        }
        if (version != 1 && version != 2) {
            if (err) *err = "unsupported weights version: " + std::to_string(version);
            return false;
        }
        if (hidden != kHidden || input_size != kHidden) {
            if (err) *err = "weights size mismatch";
            return false;
        }

        if (!read_float_array(file, &recurrent.lstm_W, "lstm_W", err) ||
            !read_float_array(file, &recurrent.lstm_R, "lstm_R", err) ||
            !read_float_array(file, &recurrent.lstm_B, "lstm_B", err) ||
            !read_float_array(file, &recurrent.dec_weight, "decoder weight", err) ||
            !read_pod(file, &recurrent.dec_bias, "decoder bias", err)) {
            return false;
        }

        char extension_magic[4];
        file.read(extension_magic, sizeof(extension_magic));
        if (file.gcount() == 0) {
            if (require_encoder && err) *err = "weights file does not contain CPU encoder weights";
            return !require_encoder;
        }
        if (file.gcount() != static_cast<std::streamsize>(sizeof(extension_magic))) {
            if (err) *err = "weights file has truncated extension magic";
            return false;
        }
        if (std::memcmp(extension_magic, "SVCE", 4) != 0) {
            if (err) *err = "unknown weights extension magic";
            return false;
        }
        return load_encoder_extension(file, require_encoder, err);
    }

private:
    bool load_encoder_extension(std::ifstream& file, bool require_encoder, std::string* err) {
        uint32_t version = 0;
        uint32_t layer_count = 0;
        if (!read_pod(file, &version, "encoder extension version", err) ||
            !read_pod(file, &layer_count, "encoder layer count", err)) {
            return false;
        }
        if (version != 1 || layer_count != 4) {
            if (err) *err = "unsupported CPU encoder weights extension";
            return false;
        }

        encoder.assign(layer_count, Conv1dLayerWeights());
        for (uint32_t i = 0; i < layer_count; ++i) {
            uint32_t in_channels = 0;
            uint32_t out_channels = 0;
            uint32_t kernel_size = 0;
            uint32_t stride = 0;
            uint32_t padding = 0;
            if (!read_pod(file, &in_channels, "conv in_channels", err) ||
                !read_pod(file, &out_channels, "conv out_channels", err) ||
                !read_pod(file, &kernel_size, "conv kernel_size", err) ||
                !read_pod(file, &stride, "conv stride", err) ||
                !read_pod(file, &padding, "conv padding", err)) {
                return false;
            }
            Conv1dLayerWeights layer;
            layer.in_channels = static_cast<int>(in_channels);
            layer.out_channels = static_cast<int>(out_channels);
            layer.kernel_size = static_cast<int>(kernel_size);
            layer.stride = static_cast<int>(stride);
            layer.padding = static_cast<int>(padding);
            const std::size_t weight_count =
                static_cast<std::size_t>(layer.out_channels) *
                layer.in_channels *
                layer.kernel_size;
            std::vector<float> raw_weight(weight_count, 0.0f);
            std::vector<float> raw_bias(layer.out_channels, 0.0f);
            if (!read_float_array(file, &raw_weight, "conv weight", err) ||
                !read_float_array(file, &raw_bias, "conv bias", err) ||
                !layer.set_weights(&raw_weight, &raw_bias, err)) {
                return false;
            }
            encoder[i] = layer;
        }

        has_encoder = true;
        if (require_encoder && !validate_encoder_layout(err)) {
            return false;
        }
        return true;
    }

    bool validate_encoder_layout(std::string* err) const {
        const int expected[4][5] = {
            {kSTFTBins, 128, 3, 1, 1},
            {128, 64, 3, 2, 1},
            {64, 64, 3, 2, 1},
            {64, kHidden, 3, 1, 1},
        };
        if (encoder.size() != 4) {
            if (err) *err = "CPU encoder layer count mismatch";
            return false;
        }
        for (std::size_t i = 0; i < encoder.size(); ++i) {
            const Conv1dLayerWeights& layer = encoder[i];
            if (layer.in_channels != expected[i][0] ||
                layer.out_channels != expected[i][1] ||
                layer.kernel_size != expected[i][2] ||
                layer.stride != expected[i][3] ||
                layer.padding != expected[i][4]) {
                if (err) *err = "CPU encoder layer layout mismatch at layer " + std::to_string(i);
                return false;
            }
        }
        return true;
    }
};

class SileroLSTMDecoder {
public:
    bool init(const RecurrentWeights* weights, std::string* err) {
        if (weights == nullptr) {
            if (err) *err = "nil recurrent weights";
            return false;
        }
        weights_ = weights;
        reset();
        return true;
    }

    void reset() {
        std::fill(h_.begin(), h_.end(), 0.0f);
        std::fill(c_.begin(), c_.end(), 0.0f);
        std::fill(gates_.begin(), gates_.end(), 0.0f);
        std::fill(relu_h_.begin(), relu_h_.end(), 0.0f);
    }

    float infer(const float* encoder_out) {
        for (int g = 0; g < kGates; ++g) {
            float sum = weights_->lstm_B[g] + weights_->lstm_B[kGates + g];
            sum += dot_product(&weights_->lstm_W[g * kHidden], encoder_out, kHidden);
            sum += dot_product(&weights_->lstm_R[g * kHidden], h_.data(), kHidden);
            gates_[g] = sum;
        }
        for (int i = 0; i < kHidden; ++i) {
            const float ig = sigmoid(gates_[i]);
            const float fg = sigmoid(gates_[kHidden + i]);
            const float cg = std::tanh(gates_[2 * kHidden + i]);
            const float og = sigmoid(gates_[3 * kHidden + i]);
            c_[i] = fg * c_[i] + ig * cg;
            h_[i] = og * std::tanh(c_[i]);
            relu_h_[i] = h_[i] > 0.0f ? h_[i] : 0.0f;
        }
        const float logit = weights_->dec_bias +
                            dot_product(weights_->dec_weight.data(), relu_h_.data(), kHidden);
        return sigmoid(logit);
    }

private:
    const RecurrentWeights* weights_ = nullptr;
    std::vector<float> h_ = std::vector<float>(kHidden, 0.0f);
    std::vector<float> c_ = std::vector<float>(kHidden, 0.0f);
    std::vector<float> gates_ = std::vector<float>(kGates, 0.0f);
    std::vector<float> relu_h_ = std::vector<float>(kHidden, 0.0f);
};

class SileroConvEncoder {
public:
    bool init(const SileroWeights* weights, std::string* err) {
        if (weights == nullptr || !weights->has_encoder) {
            if (err) *err = "CPU encoder weights are missing";
            return false;
        }
        layers_ = weights->encoder;
        return true;
    }

    bool run(const float* features, float* encoder_out, std::string* err) {
        if (layers_.size() != 4) {
            if (err) *err = "CPU encoder is not initialized";
            return false;
        }

        time_major_.assign(kFeatureFrames * kSTFTBins, 0.0f);
        for (int t = 0; t < kFeatureFrames; ++t) {
            for (int c = 0; c < kSTFTBins; ++c) {
                time_major_[t * kSTFTBins + c] = features[c * kFeatureFrames + t];
            }
        }

        const float* input = time_major_.data();
        int input_length = kFeatureFrames;
        buffer_a_.clear();
        buffer_b_.clear();
        for (std::size_t i = 0; i < layers_.size(); ++i) {
            std::vector<float>* output = (i % 2 == 0) ? &buffer_a_ : &buffer_b_;
            layers_[i].run(input, input_length, output);
            input = output->data();
            input_length = layers_[i].output_length(input_length);
        }

        if (input_length != 1) {
            if (err) *err = "unexpected CPU encoder output length";
            return false;
        }
        for (int i = 0; i < kHidden; ++i) {
            encoder_out[i] = input[i];
        }
        return true;
    }

private:
    std::vector<Conv1dLayerWeights> layers_;
    std::vector<float> time_major_;
    std::vector<float> buffer_a_;
    std::vector<float> buffer_b_;
};

inline void fill_benchmark_frame(int index, std::vector<int16_t>* frame) {
    frame->assign(kFrameSamples, 0);
    const float pi = 3.14159265358979323846f;
    const float frequency = 180.0f + static_cast<float>((index % 7) * 35);
    const float amplitude = (index % 5 == 0) ? 0.0f : 0.12f;
    for (int i = 0; i < kFrameSamples; ++i) {
        const float t = static_cast<float>(index * kFrameSamples + i) /
                        static_cast<float>(kSampleRate);
        const float sample = amplitude * std::sin(2.0f * pi * frequency * t);
        (*frame)[i] = static_cast<int16_t>(sample * 32767.0f);
    }
}

inline std::vector<std::vector<int16_t> > make_benchmark_frames() {
    std::vector<std::vector<int16_t> > frames(8);
    for (std::size_t i = 0; i < frames.size(); ++i) {
        fill_benchmark_frame(static_cast<int>(i), &frames[i]);
    }
    return frames;
}

inline void print_benchmark_result(const std::string& label,
                                   int frames,
                                   int warmup,
                                   double wall_ms,
                                   const CPUUsage& cpu_start,
                                   const CPUUsage& cpu_end,
                                   float last_probability) {
    const double user_ms = cpu_end.user_ms - cpu_start.user_ms;
    const double system_ms = cpu_end.system_ms - cpu_start.system_ms;
    const double total_cpu_ms = user_ms + system_ms;
    const double cpu_percent = wall_ms > 0.0 ? (total_cpu_ms / wall_ms) * 100.0 : 0.0;
    const double avg_ms = frames > 0 ? wall_ms / static_cast<double>(frames) : 0.0;
    const double fps = wall_ms > 0.0 ? static_cast<double>(frames) * 1000.0 / wall_ms : 0.0;

    std::cout << std::fixed << std::setprecision(3)
              << "BENCHMARK backend=" << label
              << " frames=" << frames
              << " warmup=" << warmup
              << " wall_ms=" << wall_ms
              << " avg_ms=" << avg_ms
              << " fps=" << fps
              << " cpu_user_ms=" << user_ms
              << " cpu_sys_ms=" << system_ms
              << " cpu_total_ms=" << total_cpu_ms
              << " cpu_usage_percent=" << cpu_percent
              << " last_probability=" << last_probability
              << std::endl;
}

}  // namespace aico_vad
