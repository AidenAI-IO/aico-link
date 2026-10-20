#include "doctest.h"
#include "rk628_hdmi_mode.h"
#include <array>
#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <linux/v4l2-subdev.h>
#include <sys/ioctl.h>
#include <unistd.h>

namespace {
std::array<unsigned char, 256> installed;
int reads, writes, read_error, write_error;

int fake_ioctl(int, unsigned long request, void* data) {
    auto* edid = static_cast<v4l2_edid*>(data);
    CHECK(edid->pad == 0);
    CHECK(edid->blocks == 2);
    if (request == VIDIOC_SUBDEV_G_EDID) {
        ++reads;
        if (read_error) { errno = read_error; return -1; }
        memcpy(edid->edid, installed.data(), installed.size());
    } else {
        CHECK(request == VIDIOC_SUBDEV_S_EDID);
        ++writes;
        if (write_error) { errno = write_error; return -1; }
        memcpy(installed.data(), edid->edid, installed.size());
    }
    return 0;
}

struct ModeFixture {
    std::string root, mode_file, selected;
    ModeFixture() {
        char path[] = "/tmp/aiden-hdmi-mode-XXXXXX";
        const char* result = mkdtemp(path);
        REQUIRE(result != nullptr);
        root = result;
        mode_file = root + "/mode";
        reads = writes = read_error = write_error = 0;
        for (const char* mode : {"1080p30", "1080p60"}) {
            std::ifstream source(std::string(AIDEN_SOURCE_DIR) + "/edid/hdmi_" + mode + "_cta.hex");
            REQUIRE(source.good());
            std::ofstream target(root + "/hdmi_" + mode + "_cta.hex");
            target << source.rdbuf();
        }
        FILE* file = fopen((root + "/hdmi_1080p60_cta.hex").c_str(), "r");
        REQUIRE(file != nullptr);
        for (auto& byte : installed) {
            unsigned value;
            REQUIRE(fscanf(file, "%x", &value) == 1);
            byte = static_cast<unsigned char>(value);
        }
        fclose(file);
    }
    ~ModeFixture() {
        unlink(mode_file.c_str());
        unlink((root + "/hdmi_1080p30_cta.hex").c_str());
        unlink((root + "/hdmi_1080p60_cta.hex").c_str());
        rmdir(root.c_str());
    }
    void save(const char* mode) { std::ofstream(mode_file) << mode << '\n'; }
    bool apply() { return aiden::prepare_rk628_hdmi_mode(-1, mode_file, root, &selected, fake_ioctl); }
};
}

TEST_CASE("RK628 defaults to 60 Hz and avoids renegotiating an unchanged EDID") {
    ModeFixture fixture;
    CHECK(fixture.apply());
    CHECK(reads == 1);
    CHECK(writes == 0);
    CHECK(fixture.selected == fixture.root + "/hdmi_1080p60_cta.hex");
}

TEST_CASE("RK628 persisted selection switches both ways and survives a fresh preparation") {
    ModeFixture fixture;
    fixture.save("1080p30");
    REQUIRE(fixture.apply());
    CHECK(writes == 1);
    CHECK(fixture.selected == fixture.root + "/hdmi_1080p30_cta.hex");
    REQUIRE(fixture.apply());
    CHECK(writes == 1);
    fixture.save("1080p60");
    REQUIRE(fixture.apply());
    CHECK(writes == 2);
}

TEST_CASE("RK628 temporary read and write failures remain retryable") {
    ModeFixture fixture;
    fixture.save("1080p30");
    for (int error : {EAGAIN, EIO}) {
        read_error = error;
        CHECK_FALSE(fixture.apply());
        CHECK(errno == error);
        CHECK(writes == 0);
    }
    read_error = 0;
    write_error = EIO;
    CHECK_FALSE(fixture.apply());
    CHECK(errno == EIO);
    write_error = 0;
    CHECK(fixture.apply());
    CHECK(writes == 2);
}

TEST_CASE("RK628 empty receiver EDID can be populated") {
    ModeFixture fixture;
    read_error = ENODATA;
    CHECK(fixture.apply());
    CHECK(writes == 1);
}

TEST_CASE("RK628 invalid persisted state and damaged assets never touch hardware") {
    ModeFixture fixture;
    for (const char* invalid : {"", "1080p50", "1080p30 extra", "../../arbitrary"}) {
        fixture.save(invalid);
        CHECK_FALSE(fixture.apply());
        CHECK(errno == EINVAL);
    }
    fixture.save("1080p30");
    std::ofstream(fixture.root + "/hdmi_1080p30_cta.hex", std::ios::app) << "garbage";
    CHECK_FALSE(fixture.apply());
    CHECK(reads == 0);
    CHECK(writes == 0);
}
