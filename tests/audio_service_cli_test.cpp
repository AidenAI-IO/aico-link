#include "doctest.h"
#include "uds_server.h"
#include "uds_message.h"
#include <atomic>
#include <cstdio>
#include <spawn.h>
#include <string>
#include <sys/wait.h>
#include <unistd.h>

extern char** environ;

TEST_CASE("record-stream drains delayed PCM after stop and handles drain errors") {
    bool fail_drain = false;
    SUBCASE("delayed tail and PCM on EOF") {}
    SUBCASE("read failure after stop") { fail_drain = true; }

    char socket_path[] = "/tmp/aico_cli_XXXXXX";
    int fd = mkstemp(socket_path);
    REQUIRE(fd >= 0);
    close(fd);
    unlink(socket_path);
    std::atomic<bool> stopped(false);
    std::atomic<int> reads(0);
    aico::UdsServer server(socket_path, [&](const aico::UdsMessage& req, int client_fd) {
        if (req.header_json.find("start_recording") != std::string::npos) {
            aico::write_uds_message(client_fd, R"({"status":"OK","session_id":"42"})", {});
        } else if (req.header_json.find("stop_recording") != std::string::npos) {
            stopped = true;
            aico::write_uds_message(client_fd, R"({"status":"OK"})", {});
        } else if (!stopped || fail_drain) {
            aico::write_uds_message(client_fd, R"({"status":"INTERNAL_ERROR"})", {});
        } else {
            switch (++reads) {
            case 1: // stop returns before the capture worker finishes flushing.
                aico::write_uds_message(client_fd, R"({"status":"TIMEOUT"})", {});
                break;
            case 2:
                aico::write_uds_message(client_fd, R"({"status":"OK"})", {1, 0, 2, 0});
                break;
            default:
                aico::write_uds_message(client_fd, R"({"status":"OK","end_of_stream":true})", {3, 0});
            }
        }
    });
    REQUIRE(server.start() == aico::AicoServiceStatus::OK);
    FILE* output = tmpfile();
    REQUIRE(output != nullptr);
    posix_spawn_file_actions_t actions;
    REQUIRE(posix_spawn_file_actions_init(&actions) == 0);
    REQUIRE(posix_spawn_file_actions_adddup2(&actions, fileno(output), STDOUT_FILENO) == 0);
    const char* args[] = {AICO_AUDIO_CLI_PATH, "--socket", socket_path,
                          "record-stream", "--seconds", "0", nullptr};
    pid_t pid;
    int result = posix_spawn(&pid, args[0], &actions, nullptr,
                            const_cast<char**>(args), environ);
    posix_spawn_file_actions_destroy(&actions);
    REQUIRE(result == 0);
    int status = 0;
    REQUIRE(waitpid(pid, &status, 0) == pid);
    REQUIRE(WIFEXITED(status));
    CHECK(WEXITSTATUS(status) == (fail_drain ? 1 : 0));
    CHECK(stopped.load());
    rewind(output);
    unsigned char pcm[16] = {};
    size_t size = fread(pcm, 1, sizeof(pcm), output);
    if (!fail_drain) {
        CHECK(reads.load() == 3);
        REQUIRE(size == 6);
        CHECK(pcm[0] == 1);
        CHECK(pcm[2] == 2);
        CHECK(pcm[4] == 3);
    } else {
        CHECK(size == 0);
    }
    fclose(output);
    server.stop();
    unlink(socket_path);
}
