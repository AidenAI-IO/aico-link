#pragma once

#include <string>
#include <vector>

namespace aico {

struct GeneratedSystemEnv {
    bool valid;
    std::string content;
    std::string error;
    // Unapproved keys that were skipped. The rest of the environment is still
    // generated, so one unknown name cannot strip every approved credential.
    std::vector<std::string> dropped_keys;
};

bool generate_systemd_environment(const std::string& input,
                                  GeneratedSystemEnv* generated);

}  // namespace aico
