#include "doctest.h"

#include "vad_common.h"

#include <cstdint>
#include <cstdio>
#include <fstream>
#include <string>
#include <unistd.h>
#include <vector>

namespace {

std::string make_temp_path(const char* leaf) {
    char tmpl[] = "/tmp/aico_vad_common_XXXXXX";
    int fd = mkstemp(tmpl);
    REQUIRE(fd >= 0);
    close(fd);
    std::remove(tmpl);
    std::string base = tmpl;
    return base + "_" + leaf;
}

void write_floats(std::ofstream& out, std::size_t count) {
    std::vector<float> values(count, 0.0f);
    out.write(reinterpret_cast<const char*>(values.data()),
              static_cast<std::streamsize>(values.size() * sizeof(float)));
}

void write_u32(std::ofstream& out, uint32_t value) {
    out.write(reinterpret_cast<const char*>(&value), sizeof(value));
}

std::string write_recurrent_weights(uint32_t version) {
    std::string path = make_temp_path("weights.bin");
    std::ofstream out(path.c_str(), std::ios::binary);
    REQUIRE(out);

    out.write("SVLW", 4);
    write_u32(out, version);
    write_u32(out, aico_vad::kHidden);
    write_u32(out, aico_vad::kHidden);
    write_floats(out, aico_vad::kGates * aico_vad::kHidden);
    write_floats(out, aico_vad::kGates * aico_vad::kHidden);
    write_floats(out, 2 * aico_vad::kGates);
    write_floats(out, aico_vad::kHidden);
    write_floats(out, 1);
    REQUIRE(out.good());
    return path;
}

std::string write_combined_weights(uint32_t version) {
    std::string path = write_recurrent_weights(version);
    std::ofstream out(path.c_str(), std::ios::binary | std::ios::app);
    REQUIRE(out);

    const uint32_t layers[4][5] = {
        {aico_vad::kSTFTBins, 128, 3, 1, 1},
        {128, 64, 3, 2, 1},
        {64, 64, 3, 2, 1},
        {64, aico_vad::kHidden, 3, 1, 1},
    };

    out.write("SVCE", 4);
    write_u32(out, 1);
    write_u32(out, 4);
    for (int i = 0; i < 4; ++i) {
        for (int field = 0; field < 5; ++field) {
            write_u32(out, layers[i][field]);
        }
        write_floats(out, layers[i][0] * layers[i][1] * layers[i][2]);
        write_floats(out, layers[i][1]);
    }
    REQUIRE(out.good());
    return path;
}

}  // namespace

TEST_CASE("SileroWeights accepts current SVLW version 2 combined weights") {
    const std::string path = write_combined_weights(2);

    aico_vad::SileroWeights weights;
    std::string err;
    CHECK(weights.load(path, true, &err));
    CHECK(err.empty());
    CHECK(weights.has_encoder);

    std::remove(path.c_str());
}

TEST_CASE("SileroWeights rejects unknown SVLW versions with the version number") {
    const std::string path = write_recurrent_weights(3);

    aico_vad::SileroWeights weights;
    std::string err;
    CHECK_FALSE(weights.load(path, false, &err));
    CHECK(err == "unsupported weights version: 3");

    std::remove(path.c_str());
}

TEST_CASE("SileroLSTMDecoder uses PyTorch LSTMCell gate order") {
    aico_vad::RecurrentWeights weights;
    weights.lstm_B[0] = 10.0f;                         // input gate
    weights.lstm_B[aico_vad::kHidden] = -10.0f;        // forget gate
    weights.lstm_B[2 * aico_vad::kHidden] = 1.0f;      // cell gate
    weights.lstm_B[3 * aico_vad::kHidden] = 10.0f;     // output gate
    weights.dec_weight[0] = 10.0f;
    weights.dec_bias = -5.0f;

    aico_vad::SileroLSTMDecoder decoder;
    std::string err;
    REQUIRE(decoder.init(&weights, &err));

    std::vector<float> encoder_out(aico_vad::kHidden, 0.0f);
    const float probability = decoder.infer(encoder_out.data());

    CHECK(probability > 0.5f);
}
