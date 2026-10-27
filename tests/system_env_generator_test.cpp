#include "system_env_generator.h"

#include <doctest.h>

#include <string>

TEST_CASE("system environment generator normalizes proxy variables") {
    aico::GeneratedSystemEnv generated;
    REQUIRE(aico::generate_systemd_environment(
        "HTTPS_PROXY=http://proxy.example:7890\nOPENAI_API_KEY='key with spaces'\n",
        &generated));
    REQUIRE(generated.valid);
    CHECK(generated.content.find("HTTPS_PROXY=\"http://proxy.example:7890\"") !=
          std::string::npos);
    CHECK(generated.content.find("https_proxy=\"http://proxy.example:7890\"") !=
          std::string::npos);
    CHECK(generated.content.find("NO_PROXY=\"localhost,127.0.0.1,::1") !=
          std::string::npos);
    CHECK(generated.content.find("OPENAI_API_KEY=\"key with spaces\"") !=
          std::string::npos);
}

TEST_CASE("system environment generator drops execution boundary variables") {
    aico::GeneratedSystemEnv generated;
    REQUIRE(aico::generate_systemd_environment("LD_PRELOAD=/tmp/inject.so\n", &generated));
    CHECK(generated.valid);
    CHECK(generated.content.find("LD_PRELOAD") == std::string::npos);
    REQUIRE(generated.dropped_keys.size() == 1);
    CHECK(generated.dropped_keys[0] == "LD_PRELOAD");
}

TEST_CASE("system environment generator keeps approved keys beside an unapproved one") {
    aico::GeneratedSystemEnv generated;
    REQUIRE(aico::generate_systemd_environment(
        "OPENAI_API_KEY=live\nPYTHONPATH=/tmp/inject\n", &generated));
    CHECK(generated.valid);
    CHECK(generated.content.find("OPENAI_API_KEY=\"live\"") != std::string::npos);
    CHECK(generated.content.find("PYTHONPATH") == std::string::npos);
    REQUIRE(generated.dropped_keys.size() == 1);
    CHECK(generated.dropped_keys[0] == "PYTHONPATH");
}

TEST_CASE("system environment generator approves operator-named provider credentials") {
    // agent.toml references provider credentials as `api_key = "$NAME"` with a
    // name the operator chooses, so these must survive on Debian too.
    aico::GeneratedSystemEnv generated;
    REQUIRE(aico::generate_systemd_environment(
        "DASHSCOPE_API_KEY=qwen\nSPEKO_KEY=speko\nVERTEX_TOKEN=vertex\n"
        "AICO_STT_LIVE_TEST_API_KEY=stt\n",
        &generated));
    CHECK(generated.valid);
    CHECK(generated.dropped_keys.empty());
    CHECK(generated.content.find("DASHSCOPE_API_KEY=\"qwen\"") != std::string::npos);
    CHECK(generated.content.find("SPEKO_KEY=\"speko\"") != std::string::npos);
    CHECK(generated.content.find("VERTEX_TOKEN=\"vertex\"") != std::string::npos);
    CHECK(generated.content.find("AICO_STT_LIVE_TEST_API_KEY=\"stt\"") !=
          std::string::npos);
}

TEST_CASE("system environment generator still refuses loader and interpreter names") {
    const char* const hostile[] = {
        "LD_PRELOAD=/tmp/inject.so\n", "LD_AUDIT=/tmp/audit.so\n",
        "LD_LIBRARY_PATH=/tmp\n",     "PATH=/tmp\n",
        "PYTHONPATH=/tmp\n",          "BASH_ENV=/tmp/rc\n",
        "GLIBC_TUNABLES=x\n",         "IFS=,\n",
    };
    for (size_t i = 0; i < sizeof(hostile) / sizeof(hostile[0]); ++i) {
        aico::GeneratedSystemEnv generated;
        REQUIRE(aico::generate_systemd_environment(hostile[i], &generated));
        REQUIRE(generated.dropped_keys.size() == 1);
        CHECK(generated.content.find(generated.dropped_keys[0]) == std::string::npos);
    }
}

TEST_CASE("system environment generator reports no drops once the file is fatal") {
    aico::GeneratedSystemEnv generated;
    REQUIRE(aico::generate_systemd_environment(
        "LD_PRELOAD=/tmp/inject.so\nHTTP_PROXY=http://http://proxy.example:7890\n",
        &generated));
    CHECK_FALSE(generated.valid);
    CHECK(generated.dropped_keys.empty());
    CHECK(generated.content.find("LD_PRELOAD") == std::string::npos);
    CHECK(generated.content.find("AICO_EXTERNAL_ACCESS_DISABLED=\"1\"") !=
          std::string::npos);
}

TEST_CASE("system environment generator rejects shell statements atomically") {
    aico::GeneratedSystemEnv generated;
    REQUIRE(aico::generate_systemd_environment(
        "OPENAI_API_KEY=valid\ntouch /tmp/pwn\n", &generated));
    CHECK_FALSE(generated.valid);
    CHECK(generated.content.find("OPENAI_API_KEY") == std::string::npos);
    CHECK(generated.error.find("unsupported system env statement") != std::string::npos);
    CHECK(generated.content.find("AICO_EXTERNAL_ACCESS_DISABLED=\"1\"") !=
          std::string::npos);
}

TEST_CASE("system environment generator rejects malformed proxy URLs") {
    aico::GeneratedSystemEnv generated;
    REQUIRE(aico::generate_systemd_environment(
        "HTTP_PROXY=http://http://proxy.example:7890\n", &generated));
    CHECK_FALSE(generated.valid);
    CHECK(generated.error.find("duplicate scheme") != std::string::npos);
}
