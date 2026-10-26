#include "doctest.h"
#include "frame_service_protocol.h"
#include "cJSON/cJSON.h"

#include <cstdint>
#include <fstream>
#include <iterator>
#include <string>
#include <vector>

namespace {

std::string fixture_path(const char* name) {
    return std::string(AICO_SOURCE_DIR) + "/tests/contracts/" + name;
}

std::vector<uint8_t> read_fixture(const char* name) {
    std::ifstream stream(fixture_path(name), std::ios::binary);
    REQUIRE(stream.good());
    return std::vector<uint8_t>((std::istreambuf_iterator<char>(stream)),
                                std::istreambuf_iterator<char>());
}

std::string read_text_fixture(const char* name) {
    std::ifstream stream(fixture_path(name));
    REQUIRE(stream.good());
    return std::string((std::istreambuf_iterator<char>(stream)),
                       std::istreambuf_iterator<char>());
}

}  // namespace

TEST_CASE("shared UDS fixtures match the documented envelope") {
    const std::string request = read_text_fixture("uds-health-request.json");
    cJSON* request_json = cJSON_Parse(request.c_str());
    REQUIRE(request_json != nullptr);
    cJSON* type = cJSON_GetObjectItem(request_json, "type");
    cJSON* method = cJSON_GetObjectItem(request_json, "method");
    REQUIRE(type != nullptr);
    REQUIRE(method != nullptr);
    REQUIRE((type->type & 0xff) == cJSON_String);
    REQUIRE((method->type & 0xff) == cJSON_String);
    CHECK(std::string(type->valuestring) == "request");
    CHECK(std::string(method->valuestring) == "health");
    cJSON_Delete(request_json);

    const std::vector<uint8_t> encoded = read_fixture("uds-frame-response.bin");
    REQUIRE(encoded.size() >= aico::kFrameWirePrefixSize);
    aico::FrameWirePrefix prefix;
    REQUIRE(aico::decode_frame_wire_prefix(encoded.data(),
                                             aico::kFrameWirePrefixSize,
                                             &prefix));
    REQUIRE(encoded.size() == aico::kFrameWirePrefixSize + prefix.header_len +
                                prefix.payload_len);
    const std::string header(
        reinterpret_cast<const char*>(encoded.data() + aico::kFrameWirePrefixSize),
        prefix.header_len);
    cJSON* response_json = cJSON_Parse(header.c_str());
    REQUIRE(response_json != nullptr);
    cJSON* response_type = cJSON_GetObjectItem(response_json, "type");
    cJSON* response_method = cJSON_GetObjectItem(response_json, "method");
    cJSON* sequence = cJSON_GetObjectItem(response_json, "seq");
    REQUIRE(response_type != nullptr);
    REQUIRE(response_method != nullptr);
    REQUIRE(sequence != nullptr);
    REQUIRE((response_type->type & 0xff) == cJSON_String);
    REQUIRE((response_method->type & 0xff) == cJSON_String);
    REQUIRE((sequence->type & 0xff) == cJSON_String);
    CHECK(std::string(response_type->valuestring) == "response");
    CHECK(std::string(response_method->valuestring) == "latest_frame");
    CHECK(std::string(sequence->valuestring) == "7");
    cJSON_Delete(response_json);

    const size_t payload_offset = aico::kFrameWirePrefixSize + prefix.header_len;
    REQUIRE(prefix.payload_len == 4);
    CHECK(encoded[payload_offset + 0] == 0);
    CHECK(encoded[payload_offset + 1] == 1);
    CHECK(encoded[payload_offset + 2] == 2);
    CHECK(encoded[payload_offset + 3] == 3);
}

TEST_CASE("shared Config Web and OTA fixtures preserve required fields") {
    const std::string config = read_text_fixture("config-wire.json");
    cJSON* config_json = cJSON_Parse(config.c_str());
    REQUIRE(config_json != nullptr);
    cJSON* model = cJSON_GetObjectItem(config_json, "model");
    cJSON* search = cJSON_GetObjectItem(config_json, "search");
    cJSON* device = cJSON_GetObjectItem(config_json, "device");
    REQUIRE(model != nullptr);
    REQUIRE(search != nullptr);
    REQUIRE(device != nullptr);
    REQUIRE((model->type & 0xff) == cJSON_Object);
    REQUIRE((search->type & 0xff) == cJSON_Object);
    REQUIRE((device->type & 0xff) == cJSON_Object);
    CHECK(std::string(cJSON_GetObjectItem(model, "provider")->valuestring) == "openai");
    CHECK(std::string(cJSON_GetObjectItem(search, "provider")->valuestring) == "duckduckgo");
    CHECK(std::string(cJSON_GetObjectItem(device, "device_type")->valuestring) == "iOS");
    cJSON_Delete(config_json);

    const std::string manifest = read_text_fixture("ota-manifest.json");
    cJSON* manifest_json = cJSON_Parse(manifest.c_str());
    REQUIRE(manifest_json != nullptr);
    cJSON* schema = cJSON_GetObjectItem(manifest_json, "schema_version");
    cJSON* parts = cJSON_GetObjectItem(manifest_json, "parts");
    cJSON* signature = cJSON_GetObjectItem(manifest_json, "signature");
    REQUIRE(schema != nullptr);
    REQUIRE(parts != nullptr);
    REQUIRE(signature != nullptr);
    REQUIRE((schema->type & 0xff) == cJSON_Number);
    REQUIRE((parts->type & 0xff) == cJSON_Array);
    REQUIRE(cJSON_GetArraySize(parts) == 2);
    REQUIRE((signature->type & 0xff) == cJSON_Object);
    CHECK(schema->valueint == 2);
    cJSON_Delete(manifest_json);
}
