#pragma once

#include <string>
#include <vector>

namespace aico {

struct EnvAssignment {
    std::string key;
    std::string value;
};

struct SystemEnvProxy {
    std::string http_proxy;
    std::string https_proxy;
    std::string all_proxy;
    std::string no_proxy;
};

std::string validate_system_proxy_url(const std::string& url);

// Keys that may reach the systemd services through /run/aico/system.env.
// The file is loaded as root by every Aico Link service, and its source is
// writable through the config portal, so names that steer the dynamic
// loader or an interpreter must never be approved.
bool is_allowed_system_env_key(const std::string& key);

bool parse_system_env_content(const std::string& content,
                              std::vector<EnvAssignment>* assignments,
                              SystemEnvProxy* proxy,
                              std::string* error);

bool render_system_env_without_proxy_assignments(const std::string& content,
                                                 const std::string& managed_proxy_comment,
                                                 std::string* rendered,
                                                 std::string* error);

}  // namespace aico
