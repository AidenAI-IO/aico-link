#include "aico_log.h"

#include <errno.h>
#include <stdio.h>

#include <string>

#include "doctest.h"

TEST_CASE("Aico Link logger writes one normalized UTC event line") {
    FILE* output = tmpfile();
    REQUIRE(output != NULL);

    aico::set_log_service("Frame-Service");
    aico::set_log_output(output);
    errno = EBUSY;
    AICO_LOG_ERROR("Camera Capture", "Device Open Failed",
                    "device=%s error=%s\nretrying", "/dev/video0", "busy");
    CHECK(errno == EBUSY);
    fflush(output);
    REQUIRE(fseek(output, 0, SEEK_SET) == 0);

    char buffer[1024];
    const size_t read = fread(buffer, 1, sizeof(buffer) - 1, output);
    buffer[read] = '\0';
    const std::string line(buffer);

    CHECK(line.size() > 21);
    CHECK(line[4] == '-');
    CHECK(line[7] == '-');
    CHECK(line[10] == 'T');
    CHECK(line.substr(20).find(" [ERROR][frame_service][camera_capture] device_open_failed ") == 0);
    CHECK(line.find(" monotonic_ms=") != std::string::npos);
    CHECK(line.find(" pid=") != std::string::npos);
    CHECK(line.find(" tid=") != std::string::npos);
    CHECK(line.find("message=\"device=/dev/video0 error=busy\\nretrying\"") != std::string::npos);
    CHECK(line[line.size() - 1] == '\n');
    CHECK(line.find('\n') == line.size() - 1);

    aico::set_log_output(NULL);
    aico::set_log_service("unknown");
    fclose(output);
}
