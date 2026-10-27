#include "doctest.h"
#include "audio_volume_state.h"

#include <fstream>
#include <sstream>
#include <string>
#include <cstdlib>
#include <sys/stat.h>
#include <unistd.h>

namespace {

struct TempDir {
    std::string path;

    TempDir() {
        char tmpl[] = "/tmp/aico_audio_volume_state_XXXXXX";
        char* p = ::mkdtemp(tmpl);
        REQUIRE(p != nullptr);
        path = p;
    }

    ~TempDir() {
        if (!path.empty()) {
            std::string cmd = "rm -rf '" + path + "'";
            (void)std::system(cmd.c_str());
        }
    }
};

std::string read_file(const std::string& path) {
    std::ifstream in(path);
    std::ostringstream out;
    out << in.rdbuf();
    return out.str();
}

void write_file(const std::string& path, const std::string& text) {
    std::ofstream out(path);
    REQUIRE(out.good());
    out << text;
}

}  // namespace

TEST_CASE("load_playback_volume_state reports missing file") {
    TempDir dir;
    std::string path = dir.path + "/audio_service/playback_volume";

    int volume = 7;
    std::string error = "unchanged";
    CHECK(aico::load_playback_volume_state(path.c_str(), &volume, &error) ==
          aico::AudioVolumeStateLoadStatus::MISSING);
    CHECK(volume == 7);
}

TEST_CASE("save_playback_volume_state writes integer newline and creates parent directory") {
    TempDir dir;
    std::string path = dir.path + "/audio_service/playback_volume";

    std::string error;
    REQUIRE(aico::save_playback_volume_state(path.c_str(), 70, &error));
    CHECK(read_file(path) == "70\n");

    struct stat st;
    REQUIRE(::stat(path.c_str(), &st) == 0);
    CHECK((st.st_mode & 0777) == 0644);

    int loaded = 0;
    CHECK(aico::load_playback_volume_state(path.c_str(), &loaded, &error) ==
          aico::AudioVolumeStateLoadStatus::LOADED);
    CHECK(loaded == 70);
}

TEST_CASE("save_playback_volume_state does not chmod existing parent directories") {
    TempDir dir;
    std::string parent = dir.path + "/audio_service";
    REQUIRE(::mkdir(parent.c_str(), 0700) == 0);
    REQUIRE(::chmod(parent.c_str(), 0700) == 0);

    std::string error;
    REQUIRE(aico::save_playback_volume_state((parent + "/playback_volume").c_str(), 33, &error));

    struct stat st;
    REQUIRE(::stat(parent.c_str(), &st) == 0);
    CHECK((st.st_mode & 0777) == 0700);
}

TEST_CASE("load_playback_volume_state rejects malformed or out-of-range values") {
    TempDir dir;
    std::string path = dir.path + "/playback_volume";

    const char* invalid_values[] = {"", "loud\n", "-1\n", "101\n", "70x\n", "70\nx\n"};
    for (size_t i = 0; i < sizeof(invalid_values) / sizeof(invalid_values[0]); ++i) {
        write_file(path, invalid_values[i]);
        int volume = 42;
        std::string error;
        CHECK(aico::load_playback_volume_state(path.c_str(), &volume, &error) ==
              aico::AudioVolumeStateLoadStatus::INVALID);
        CHECK(volume == 42);
        CHECK_FALSE(error.empty());
    }
}

TEST_CASE("save_playback_volume_state rejects out-of-range values") {
    TempDir dir;
    std::string path = dir.path + "/playback_volume";

    std::string error;
    CHECK_FALSE(aico::save_playback_volume_state(path.c_str(), 101, &error));
    CHECK_FALSE(error.empty());
    CHECK(::access(path.c_str(), F_OK) != 0);
}
