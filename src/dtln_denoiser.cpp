// Streaming adaptation of the validated RNN-Noise hybrid DTLN runner.
// DTLN weights: Copyright (c) 2020 Nils L. Westhausen, MIT; bundled LICENSE.
#include "audio_denoiser.h"
#include "dtln_weight_offsets.h"
#include "../third_party/rknpu2/v2.5.0/include/rknn_api.h"
#include <algorithm>
#include <cmath>
#include <cstring>
#include <dlfcn.h>
#include <fstream>
#include <mutex>
#include <stdexcept>
#if defined(__ARM_NEON)
#include <arm_neon.h>
#endif

namespace aico {
namespace {
constexpr int BLOCK = 512;
constexpr int HOP = 128;
constexpr int DELAY = BLOCK - HOP;

void check(int rc, const char* operation) {
    if (rc < 0) throw std::runtime_error(std::string(operation) + ": " + std::to_string(rc));
}

std::vector<char> read_file(const std::string& path, size_t limit) {
    std::ifstream file(path, std::ios::binary | std::ios::ate);
    const std::streamoff size = file.tellg();
    if (!file || size <= 0 || static_cast<uint64_t>(size) > limit)
        throw std::runtime_error("invalid DTLN asset: " + path);
    std::vector<char> data(static_cast<size_t>(size));
    file.seekg(0);
    if (!file.read(data.data(), size)) throw std::runtime_error("short read: " + path);
    return data;
}

// The model was compiled with 2.5.0. Load its private runtime by absolute path;
// rknn_vad continues to use the platform's separately pinned static runtime.
struct Runtime {
    void* library = nullptr;
    decltype(&rknn_init) init = nullptr;
    decltype(&rknn_query) query = nullptr;
    decltype(&rknn_create_mem) create_mem = nullptr;
    decltype(&rknn_destroy_mem) destroy_mem = nullptr;
    decltype(&rknn_set_io_mem) set_io_mem = nullptr;
    decltype(&rknn_run) run = nullptr;
    decltype(&rknn_destroy) destroy = nullptr;

    explicit Runtime(const std::string& path) {
        library = dlopen(path.c_str(), RTLD_NOW | RTLD_LOCAL);
        if (!library) throw std::runtime_error(dlerror());
        try {
#define LOAD(name) name = reinterpret_cast<decltype(name)>(dlsym(library, "rknn_" #name)); \
    if (!name) throw std::runtime_error("missing RKNN symbol: " #name)
            LOAD(init); LOAD(query); LOAD(create_mem); LOAD(destroy_mem);
            LOAD(set_io_mem); LOAD(run); LOAD(destroy);
#undef LOAD
        } catch (...) { dlclose(library); library = nullptr; throw; }
    }
    ~Runtime() { if (library) dlclose(library); }
    Runtime(const Runtime&) = delete;
    Runtime& operator=(const Runtime&) = delete;
};

struct Model {
    Runtime& api;
    rknn_context ctx = 0;
    rknn_tensor_attr in[2] = {}, out[4] = {};
    rknn_tensor_mem* im[2] = {};
    rknn_tensor_mem* om[4] = {};
    float decoded[4][128] = {};
    explicit Model(Runtime& runtime) : api(runtime) {}
    ~Model() {
        if (!ctx) return;
        for (auto memory : im) if (memory) api.destroy_mem(ctx, memory);
        for (auto memory : om) if (memory) api.destroy_mem(ctx, memory);
        api.destroy(ctx);
    }
    Model(const Model&) = delete;
    Model& operator=(const Model&) = delete;

    void bind(rknn_tensor_attr& attr, rknn_tensor_mem*& memory,
              unsigned index, unsigned elements, bool input) {
        attr.index = index;
        check(api.query(ctx, input ? RKNN_QUERY_INPUT_ATTR : RKNN_QUERY_NATIVE_NHWC_OUTPUT_ATTR,
                        &attr, sizeof(attr)), "query tensor");
        // These pinned gate models are 1x1x1xC INT8. Reject incompatible
        // shapes/layouts before indexing fixed state or DMA buffers.
        if (attr.n_dims != 4 || attr.dims[0] != 1 || attr.dims[1] != 1 ||
            attr.dims[2] != 1 || attr.dims[3] != elements || attr.n_elems != elements ||
            attr.fmt != RKNN_TENSOR_NHWC || attr.type != RKNN_TENSOR_INT8 ||
            attr.qnt_type != RKNN_TENSOR_QNT_AFFINE_ASYMMETRIC ||
            !std::isfinite(attr.scale) || attr.scale <= 0 || attr.size < elements ||
            attr.zp < -128 || attr.zp > 127)
            throw std::runtime_error("incompatible DTLN gate tensor");
        const uint32_t bytes = attr.size_with_stride ? attr.size_with_stride : attr.size;
        if (bytes < elements || bytes > 1024 * 1024)
            throw std::runtime_error("invalid DTLN tensor allocation");
        memory = api.create_mem(ctx, bytes);
        if (!memory || !memory->virt_addr || memory->size < bytes)
            throw std::runtime_error("DTLN tensor allocation failed");
        if (input) attr.pass_through = 1;
        check(api.set_io_mem(ctx, memory, &attr), "bind tensor");
    }

    void open(const std::string& path, unsigned features) {
        auto data = read_file(path, 8 * 1024 * 1024);
        check(api.init(&ctx, data.data(), static_cast<uint32_t>(data.size()), 0, nullptr), "rknn_init");
        rknn_sdk_version version = {};
        check(api.query(ctx, RKNN_QUERY_SDK_VERSION, &version, sizeof(version)), "query runtime");
        if (std::strncmp(version.api_version, "2.5.0", 5) != 0)
            throw std::runtime_error("DTLN requires RKNN runtime 2.5.0");
        rknn_input_output_num counts = {};
        check(api.query(ctx, RKNN_QUERY_IN_OUT_NUM, &counts, sizeof(counts)), "query IO count");
        if (counts.n_input != 2 || counts.n_output != 4)
            throw std::runtime_error("invalid DTLN gate interface");
        bind(in[0], im[0], 0, features, true);
        bind(in[1], im[1], 1, 128, true);
        for (unsigned i = 0; i < 4; ++i) bind(out[i], om[i], i, 128, false);
    }
};

static void set_input(Model* model, unsigned index, const float* data) {
    const auto& attr = model->in[index];
    auto* memory = model->im[index];
    auto* bytes = static_cast<int8_t*>(memory->virt_addr);
    std::memset(bytes, static_cast<unsigned char>(attr.zp), memory->size);
    for (unsigned i = 0; i < attr.n_elems; ++i) {
        if (!std::isfinite(data[i])) throw std::runtime_error("nonfinite DTLN input");
        const double q = std::round(data[i] / attr.scale + attr.zp);
        bytes[i] = static_cast<int8_t>(std::max(-128.0, std::min(127.0, q)));
    }
}
static void run(Model* model) { check(model->api.run(model->ctx, nullptr), "rknn_run"); }
static float* output(Model* model, unsigned index) {
    const auto& attr = model->out[index];
    const auto* bytes = static_cast<const int8_t*>(model->om[index]->virt_addr);
    for (unsigned i = 0; i < 128; ++i)
        model->decoded[index][i] = (bytes[i] - attr.zp) * attr.scale;
    return model->decoded[index];
}

// Tables are initialized once before any session starts processing.
static float tw_re[BLOCK / 2], tw_im[BLOCK / 2];
static void fft_init() {
    for (int i = 0; i < BLOCK / 2; ++i) {
        tw_re[i] = std::cos(2 * M_PI * i / BLOCK);
        tw_im[i] = -std::sin(2 * M_PI * i / BLOCK);
    }
}

static void fft(float* re, float* im, bool inverse) {
    for (unsigned i = 1, j = 0; i < BLOCK; ++i) {
        unsigned bit = BLOCK >> 1;
        for (; j & bit; bit >>= 1) j ^= bit;
        j ^= bit;
        if (i < j) {
            std::swap(re[i], re[j]);
            std::swap(im[i], im[j]);
        }
    }
    for (unsigned len = 2; len <= BLOCK; len *= 2) {
        const unsigned half = len / 2, step = BLOCK / len;
        for (unsigned start = 0; start < BLOCK; start += len) {
            for (unsigned j = 0; j < half; ++j) {
                const unsigned a = start + j, b = a + half, k = j * step;
                const float wr = tw_re[k], wi = inverse ? -tw_im[k] : tw_im[k];
                const float vr = re[b] * wr - im[b] * wi, vi = re[b] * wi + im[b] * wr;
                re[b] = re[a] - vr;
                im[b] = im[a] - vi;
                re[a] += vr;
                im[a] += vi;
            }
        }
    }
    if (inverse) {
        for (int i = 0; i < BLOCK; ++i) { re[i] /= BLOCK; im[i] /= BLOCK; }
    }
}

constexpr int ACTIVATION_GRID = 128;
constexpr int ACTIVATION_LIMIT = 12;
static float sigmoid_table[2 * ACTIVATION_LIMIT * ACTIVATION_GRID + 1];
static void activation_init() {
    for (int i = 0; i <= 2 * ACTIVATION_LIMIT * ACTIVATION_GRID; ++i) {
        const float x = (i - ACTIVATION_LIMIT * ACTIVATION_GRID) / float(ACTIVATION_GRID);
        sigmoid_table[i] = 1.f / (1.f + std::exp(-x));
    }
}

static float sigmoidf_fast(float x) {
    if (std::isnan(x)) throw std::runtime_error("nonfinite DTLN activation");
    if (x <= -ACTIVATION_LIMIT) return 0;
    if (x >= ACTIVATION_LIMIT) return 1;
    const float index = (x + ACTIVATION_LIMIT) * ACTIVATION_GRID;
    const int i = std::min(static_cast<int>(index), 2 * ACTIVATION_LIMIT * ACTIVATION_GRID - 1);
    const float fraction = index - i;
    return sigmoid_table[i] + fraction * (sigmoid_table[i + 1] - sigmoid_table[i]);
}
static float tanhf_fast(float x) { return 2.f * sigmoidf_fast(2.f * x) - 1.f; }

static float dot(const float* a, const float* b, int n) {
#if defined(__ARM_NEON)
    float32x4_t v0 = vdupq_n_f32(0), v1 = v0, v2 = v0, v3 = v0;
    int i = 0;
    for (; i + 15 < n; i += 16) {
        v0 = vmlaq_f32(v0, vld1q_f32(a + i), vld1q_f32(b + i));
        v1 = vmlaq_f32(v1, vld1q_f32(a + i + 4), vld1q_f32(b + i + 4));
        v2 = vmlaq_f32(v2, vld1q_f32(a + i + 8), vld1q_f32(b + i + 8));
        v3 = vmlaq_f32(v3, vld1q_f32(a + i + 12), vld1q_f32(b + i + 12));
    }
    const float32x4_t v = vaddq_f32(vaddq_f32(v0, v1), vaddq_f32(v2, v3));
    float32x2_t pair = vadd_f32(vget_low_f32(v), vget_high_f32(v));
    pair = vpadd_f32(pair, pair);
    float sum = vget_lane_f32(pair, 0);
#else
    float sum0 = 0, sum1 = 0, sum2 = 0, sum3 = 0;
    int i = 0;
    for (; i + 3 < n; i += 4) {
        sum0 += a[i] * b[i];
        sum1 += a[i + 1] * b[i + 1];
        sum2 += a[i + 2] * b[i + 2];
        sum3 += a[i + 3] * b[i + 3];
    }
    float sum = (sum0 + sum1) + (sum2 + sum3);
#endif
    for (; i < n; ++i) sum += a[i] * b[i];
    return sum;
}

static void matmul_cols(const float* x, int input_size, const float* w,
                        int output_size, float* dest) {
    std::memset(dest, 0, output_size * sizeof(float));
    for (int i = 0; i < input_size; ++i) {
        const float v = x[i];
        const float* row = w + size_t(i) * output_size;
        int j = 0;
#if defined(__ARM_NEON)
        for (; j + 15 < output_size; j += 16) {
            vst1q_f32(dest + j, vmlaq_n_f32(vld1q_f32(dest + j), vld1q_f32(row + j), v));
            vst1q_f32(dest + j + 4, vmlaq_n_f32(vld1q_f32(dest + j + 4), vld1q_f32(row + j + 4), v));
            vst1q_f32(dest + j + 8, vmlaq_n_f32(vld1q_f32(dest + j + 8), vld1q_f32(row + j + 8), v));
            vst1q_f32(dest + j + 12, vmlaq_n_f32(vld1q_f32(dest + j + 12), vld1q_f32(row + j + 12), v));
        }
#endif
        for (; j < output_size; ++j) dest[j] += v * row[j];
    }
}

static void hybrid_lstm(Model* model, const float* x, float* h, float* c, bool onnx_order) {
    set_input(model, 0, x);
    set_input(model, 1, h);
    run(model);
    const float* g0 = output(model, 0);
    const float* g1 = output(model, 1);
    const float* g2 = output(model, 2);
    const float* g3 = output(model, 3);
    // Stage 1 uses ONNX i/o/f/c gate order; stage 2 uses explicit i/f/c/o.
    for (int j = 0; j < 128; ++j) {
        const float input = sigmoidf_fast(g0[j]);
        const float forget = sigmoidf_fast(onnx_order ? g2[j] : g1[j]);
        const float candidate = tanhf_fast(onnx_order ? g3[j] : g2[j]);
        const float output_gate = sigmoidf_fast(onnx_order ? g1[j] : g3[j]);
        c[j] = forget * c[j] + input * candidate;
        h[j] = output_gate * tanhf_fast(c[j]);
    }
}

static void cpu_stage1(const float* weights, const float* mag, float* state,
                       float* mask, Model* g0, Model* g1) {
    float* h0 = state;
    float* c0 = state + 128;
    float* h1 = state + 256;
    float* c1 = state + 384;
    hybrid_lstm(g0, mag, h0, c0, true);
    hybrid_lstm(g1, h0, h1, c1, true);
    const float* matrix = weights + DTLN_S1_6;
    const float* bias = weights + DTLN_S1_7;
    float logits[257];
    matmul_cols(h1, 128, matrix, 257, logits);
    for (int j = 0; j < 257; ++j) mask[j] = sigmoidf_fast(logits[j] + bias[j]);
}

static void cpu_stage2(const float* weights, const float* block, float* state,
                       float* out, Model* g0, Model* g1) {
    const float* w = weights + DTLN_S2_0;
    const float* gamma = weights + DTLN_S2_1;
    const float* beta = weights + DTLN_S2_2;
    float latent[256], normalized[256], gate[256], decoded[256];
    for (int j = 0; j < 256; ++j) latent[j] = dot(w + j * 512, block, 512);
    float mean = 0, variance = 0;
    for (int j = 0; j < 256; ++j) mean += latent[j];
    mean /= 256;
    for (int j = 0; j < 256; ++j) {
        const float d = latent[j] - mean;
        variance += d * d;
    }
    const float inv = 1.f / std::sqrt(variance / 256 + 1e-7f);
    for (int j = 0; j < 256; ++j) normalized[j] = (latent[j] - mean) * inv * gamma[j] + beta[j];
    float* h0 = state;
    float* c0 = state + 128;
    float* h1 = state + 256;
    float* c1 = state + 384;
    hybrid_lstm(g0, normalized, h0, c0, false);
    hybrid_lstm(g1, h0, h1, c1, false);
    matmul_cols(h1, 128, weights + DTLN_S2_9, 256, gate);
    for (int j = 0; j < 256; ++j) decoded[j] = latent[j] * sigmoidf_fast(gate[j] + weights[DTLN_S2_10 + j]);
    w = weights + DTLN_S2_11;
    for (int j = 0; j < 512; ++j) out[j] = dot(w + j * 256, decoded, 256);
}

class DtlnDenoiser : public DenoiseHop {
public:
    explicit DtlnDenoiser(const std::string& directory)
        : runtime_(directory + "/librknnmrt.so"),
          g0_(runtime_), g1_(runtime_), g2_(runtime_), g3_(runtime_) {
        const auto data = read_file(directory + "/weights.f32", DTLN_WEIGHT_COUNT * sizeof(float));
        if (data.size() != DTLN_WEIGHT_COUNT * sizeof(float))
            throw std::runtime_error("invalid DTLN weight count");
        weights_.resize(DTLN_WEIGHT_COUNT);
        std::memcpy(weights_.data(), data.data(), data.size());
        for (float value : weights_) if (!std::isfinite(value))
            throw std::runtime_error("nonfinite DTLN weights");
        g0_.open(directory + "/s1l0.rknn", 257);
        g1_.open(directory + "/s1l1.rknn", 128);
        g2_.open(directory + "/s2l0.rknn", 256);
        g3_.open(directory + "/s2l1.rknn", 128);
        static std::once_flag tables;
        std::call_once(tables, [] { fft_init(); activation_init(); });
    }

    void reset() override {
        std::memset(inbuf_, 0, sizeof(inbuf_));
        std::memset(outbuf_, 0, sizeof(outbuf_));
        std::memset(s1_, 0, sizeof(s1_));
        std::memset(s2_, 0, sizeof(s2_));
    }

    void process(const float* input, float* result) override {
        float re[BLOCK], im[BLOCK] = {}, mag[257], mask[257], decoded[BLOCK];
        std::memmove(inbuf_, inbuf_ + HOP, DELAY * sizeof(float));
        std::memcpy(inbuf_ + DELAY, input, HOP * sizeof(float));
        std::memcpy(re, inbuf_, sizeof(re));
        fft(re, im, 0);
        for (int i = 0; i <= BLOCK / 2; ++i) mag[i] = std::hypot(re[i], im[i]);
        cpu_stage1(weights_.data(), mag, s1_, mask, &g0_, &g1_);
        for (int i = 0; i <= BLOCK / 2; ++i) { re[i] *= mask[i]; im[i] *= mask[i]; }
        for (int i = 1; i < BLOCK / 2; ++i) { re[BLOCK-i] = re[i]; im[BLOCK-i] = -im[i]; }
        fft(re, im, 1);
        cpu_stage2(weights_.data(), re, s2_, decoded, &g2_, &g3_);
        std::memmove(outbuf_, outbuf_ + HOP, DELAY * sizeof(float));
        std::memset(outbuf_ + DELAY, 0, HOP * sizeof(float));
        for (int i = 0; i < BLOCK; ++i) outbuf_[i] += decoded[i];
        std::memcpy(result, outbuf_, HOP * sizeof(float));
    }

private:
    Runtime runtime_;
    Model g0_, g1_, g2_, g3_;
    std::vector<float> weights_;
    float inbuf_[BLOCK] = {}, outbuf_[BLOCK] = {}, s1_[512] = {}, s2_[512] = {};
};
}  // namespace

std::unique_ptr<DenoiseHop> create_dtln_denoiser(const std::string& directory, std::string* error) {
    try {
        return std::unique_ptr<DenoiseHop>(new DtlnDenoiser(directory));
    } catch (const std::exception& e) {
        if (error) *error = e.what();
        return nullptr;
    }
}
}  // namespace aico
