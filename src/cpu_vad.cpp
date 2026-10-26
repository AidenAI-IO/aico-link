#include "aico_log.h"
#include "vad_common.h"

#include <algorithm>
#include <cstdlib>
#include <iostream>
#include <string>
#include <vector>

namespace {

constexpr const char* kDefaultWeightsPath = "/usr/lib/aico/models/silero_vad_6_2_lstm_decoder_weights.bin";

struct Args {
    std::string weights_path = kDefaultWeightsPath;
    bool self_test = false;
    bool benchmark = false;
    int benchmark_frames = 1000;
    int benchmark_warmup = 20;
};

bool parse_positive_int(const std::string& raw, int* value) {
    char* end = nullptr;
    long parsed = std::strtol(raw.c_str(), &end, 10);
    if (end == raw.c_str() || *end != '\0' || parsed <= 0 || parsed > 10000000) {
        return false;
    }
    *value = static_cast<int>(parsed);
    return true;
}

bool is_integer_token(const char* raw) {
    if (raw == nullptr || *raw == '\0') {
        return false;
    }
    const char* p = raw;
    if (*p == '+' || *p == '-') {
        ++p;
    }
    if (*p == '\0') {
        return false;
    }
    for (; *p != '\0'; ++p) {
        if (*p < '0' || *p > '9') {
            return false;
        }
    }
    return true;
}

Args parse_args(int argc, char** argv) {
    Args args;
    const auto parse_or_exit = [](const char* flag, const std::string& raw, int* value) {
        if (!parse_positive_int(raw, value)) {
            AICO_LOG_ERROR("arguments", "invalid_value", "flag=%s value=%s", flag,
                            raw.c_str());
            std::exit(2);
        }
    };

    for (int i = 1; i < argc; ++i) {
        const std::string arg(argv[i]);
        if ((arg == "--weights" || arg == "--model") && i + 1 < argc) {
            // --model is accepted for protocol compatibility with rknn_vad,
            // but CPU VAD only needs the combined weights blob.
            const std::string value(argv[++i]);
            if (arg == "--weights") {
                args.weights_path = value;
            }
        } else if (arg == "--self-test") {
            args.self_test = true;
        } else if (arg == "--benchmark") {
            args.benchmark = true;
            if (i + 1 < argc && (argv[i + 1][0] != '-' || is_integer_token(argv[i + 1]))) {
                const std::string value(argv[++i]);
                parse_or_exit("--benchmark", value, &args.benchmark_frames);
            }
        } else if (arg == "--benchmark-frames" && i + 1 < argc) {
            const std::string value(argv[++i]);
            parse_or_exit("--benchmark-frames", value, &args.benchmark_frames);
        } else if (arg == "--benchmark-warmup" && i + 1 < argc) {
            const std::string value(argv[++i]);
            parse_or_exit("--benchmark-warmup", value, &args.benchmark_warmup);
        }
    }
    return args;
}

class SileroCPUVAD {
public:
    bool init(const std::string& weights_path, std::string* err) {
        if (!weights_.load(weights_path, true, err)) {
            return false;
        }
        if (!encoder_.init(&weights_, err)) {
            return false;
        }
        if (!decoder_.init(&weights_.recurrent, err)) {
            return false;
        }
        reset();
        return true;
    }

    void reset() {
        feature_extractor_.reset();
        decoder_.reset();
    }

    bool infer(const int16_t* pcm, float* probability, std::string* err) {
        feature_extractor_.compute(pcm, features_);
        if (!encoder_.run(features_, encoder_out_, err)) {
            return false;
        }
        *probability = decoder_.infer(encoder_out_);
        feature_extractor_.update_context(pcm);
        return true;
    }

private:
    aico_vad::SileroWeights weights_;
    aico_vad::STFTFeatureExtractor feature_extractor_;
    aico_vad::SileroConvEncoder encoder_;
    aico_vad::SileroLSTMDecoder decoder_;
    float features_[aico_vad::kSTFTBins * aico_vad::kFeatureFrames];
    float encoder_out_[aico_vad::kHidden];
};

bool run_benchmark(SileroCPUVAD* vad, const Args& args, std::string* err) {
    std::vector<std::vector<int16_t> > frames = aico_vad::make_benchmark_frames();
    float probability = 0.0f;
    for (int i = 0; i < args.benchmark_warmup; ++i) {
        const std::vector<int16_t>& frame = frames[static_cast<std::size_t>(i) % frames.size()];
        if (!vad->infer(frame.data(), &probability, err)) {
            return false;
        }
    }
    vad->reset();

    const aico_vad::CPUUsage cpu_start = aico_vad::current_cpu_usage();
    const std::chrono::steady_clock::time_point wall_start = std::chrono::steady_clock::now();
    for (int i = 0; i < args.benchmark_frames; ++i) {
        const std::vector<int16_t>& frame = frames[static_cast<std::size_t>(i) % frames.size()];
        if (!vad->infer(frame.data(), &probability, err)) {
            return false;
        }
    }
    const std::chrono::steady_clock::time_point wall_end = std::chrono::steady_clock::now();
    const aico_vad::CPUUsage cpu_end = aico_vad::current_cpu_usage();
    const double wall_ms =
        std::chrono::duration_cast<std::chrono::duration<double, std::milli> >(wall_end - wall_start).count();
    aico_vad::print_benchmark_result("cpu", args.benchmark_frames, args.benchmark_warmup,
                                      wall_ms, cpu_start, cpu_end, probability);
    return true;
}

}  // namespace

int main(int argc, char** argv) {
    aico::set_log_service("cpu_vad");
    Args args = parse_args(argc, argv);
    std::string err;
    std::vector<int16_t> frame(aico_vad::kFrameSamples);

    SileroCPUVAD vad;
    if (!vad.init(args.weights_path, &err)) {
        aico_vad::protocol_error(err);
        return 1;
    }

    if (args.self_test) {
        std::fill(frame.begin(), frame.end(), 0);
        float probability = 0.0f;
        if (!vad.infer(frame.data(), &probability, &err)) {
            aico_vad::protocol_error(err);
            return 1;
        }
        std::cout << "P " << probability << std::endl;
        return 0;
    }

    if (args.benchmark) {
        if (!run_benchmark(&vad, args, &err)) {
            aico_vad::protocol_error(err);
            return 1;
        }
        return 0;
    }

    std::cout << "READY" << std::endl;
    while (true) {
        char command = 0;
        if (!aico_vad::read_exact(std::cin, &command, 1)) return 0;
        if (command == 'Q') return 0;
        if (command == 'R') {
            vad.reset();
            std::cout << "OK" << std::endl;
            continue;
        }
        if (command != 'F') {
            aico_vad::protocol_error("unknown command");
            continue;
        }
        if (!aico_vad::read_exact(std::cin, reinterpret_cast<char*>(frame.data()),
                                   frame.size() * sizeof(int16_t))) {
            return 0;
        }
        float probability = 0.0f;
        if (!vad.infer(frame.data(), &probability, &err)) {
            aico_vad::protocol_error(err);
            continue;
        }
        std::cout << "P " << probability << std::endl;
    }
}
