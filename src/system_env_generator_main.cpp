#include "system_env_generator.h"

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#include <fstream>
#include <iostream>
#include <sstream>
#include <string>
#include <vector>

namespace {

bool read_input(const std::string& path, std::string* content, std::string* error) {
    std::ifstream input(path.c_str(), std::ios::binary);
    if (!input) {
        if (errno == ENOENT) {
            content->clear();
            return true;
        }
        *error = "cannot open " + path + ": " + strerror(errno);
        return false;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    if (!input.good() && !input.eof()) {
        *error = "cannot read " + path;
        return false;
    }
    *content = buffer.str();
    return true;
}

std::string parent_dir(const std::string& path) {
    size_t slash = path.rfind('/');
    if (slash == std::string::npos) return ".";
    if (slash == 0) return "/";
    return path.substr(0, slash);
}

bool fsync_dir(const std::string& path, std::string* error) {
    int fd = open(parent_dir(path).c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (fd < 0) {
        *error = "cannot open parent directory for " + path + ": " + strerror(errno);
        return false;
    }
    bool ok = fsync(fd) == 0;
    if (!ok) *error = "cannot fsync parent directory for " + path + ": " + strerror(errno);
    close(fd);
    return ok;
}

bool atomic_write(const std::string& path,
                  const std::string& content,
                  mode_t mode,
                  std::string* error) {
    std::string pattern = path + ".tmp.XXXXXX";
    if (pattern.size() >= PATH_MAX) {
        *error = "temporary path is too long: " + path;
        return false;
    }
    char temp[PATH_MAX];
    memcpy(temp, pattern.c_str(), pattern.size() + 1);
    int fd = mkstemp(temp);
    if (fd < 0) {
        *error = "cannot create temporary file for " + path + ": " + strerror(errno);
        return false;
    }

    bool ok = fchmod(fd, mode) == 0;
    size_t offset = 0;
    while (ok && offset < content.size()) {
        ssize_t written = write(fd, content.data() + offset, content.size() - offset);
        if (written < 0) {
            if (errno == EINTR) continue;
            ok = false;
            break;
        }
        offset += static_cast<size_t>(written);
    }
    if (ok) ok = fsync(fd) == 0;
    int saved_errno = errno;
    if (close(fd) != 0 && ok) {
        ok = false;
        saved_errno = errno;
    }
    if (ok && rename(temp, path.c_str()) != 0) {
        ok = false;
        saved_errno = errno;
    }
    if (!ok) {
        unlink(temp);
        *error = "cannot atomically write " + path + ": " + strerror(saved_errno);
        return false;
    }
    return fsync_dir(path, error);
}

std::string join_keys(const std::vector<std::string>& keys) {
    std::string joined;
    for (size_t i = 0; i < keys.size(); ++i) {
        if (i) joined += ", ";
        joined += keys[i];
    }
    return joined;
}

void usage() {
    std::cerr << "Usage: aico-environment [--input PATH] [--output PATH] "
                 "[--invalid-marker PATH]\n";
}

}  // namespace

int main(int argc, char** argv) {
    std::string input_path = "/userdata/system/env";
    std::string output_path = "/run/aico/system.env";
    std::string invalid_marker = "/run/aico/environment.invalid";
    for (int i = 1; i < argc; ++i) {
        std::string arg(argv[i]);
        if (arg == "--help" || arg == "-h") {
            usage();
            return 0;
        }
        const char* prefixes[] = {"--input=", "--output=", "--invalid-marker="};
        std::string* values[] = {&input_path, &output_path, &invalid_marker};
        bool consumed = false;
        for (size_t j = 0; j < 3; ++j) {
            if (arg.compare(0, strlen(prefixes[j]), prefixes[j]) == 0) {
                *values[j] = arg.substr(strlen(prefixes[j]));
                consumed = true;
                break;
            }
        }
        if (!consumed) {
            usage();
            return 64;
        }
    }

    std::string input;
    std::string error;
    if (!read_input(input_path, &input, &error)) {
        std::cerr << error << "\n";
        return 1;
    }
    aico::GeneratedSystemEnv generated;
    if (!aico::generate_systemd_environment(input, &generated)) {
        std::cerr << "failed to generate system environment\n";
        return 1;
    }
    if (!atomic_write(output_path, generated.content, 0600, &error)) {
        std::cerr << error << "\n";
        return 1;
    }
    if (generated.valid && generated.dropped_keys.empty()) {
        if (unlink(invalid_marker.c_str()) != 0 && errno != ENOENT) {
            std::cerr << "cannot remove " << invalid_marker << ": " << strerror(errno) << "\n";
            return 1;
        }
        if (!fsync_dir(invalid_marker, &error)) {
            std::cerr << error << "\n";
            return 1;
        }
        return 0;
    }

    // A dropped key leaves the rest of the environment usable, so this reports a
    // degraded environment rather than an unusable one. Both land in the marker
    // because it is the only place an operator can see why a key went missing.
    std::string report;
    if (generated.valid) {
        report = "ignored unapproved environment variables: " +
                 join_keys(generated.dropped_keys);
    } else {
        report = "invalid system environment: " + generated.error;
    }
    if (!atomic_write(invalid_marker, report + "\n", 0600, &error)) {
        std::cerr << error << "\n";
        return 1;
    }
    std::cerr << report << "\n";
    return 0;
}
