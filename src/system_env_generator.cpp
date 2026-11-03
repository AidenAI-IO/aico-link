#include "system_env_generator.h"

#include "system_env_parser.h"

#include <map>
#include <sstream>
#include <string>
#include <vector>

namespace aico {
namespace {

const char kDefaultNoProxy[] =
    "localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16";

void set_pair(std::map<std::string, std::string>* values,
              const char* lower,
              const char* upper,
              const std::string& value) {
    values->erase(lower);
    values->erase(upper);
    if (!value.empty()) {
        (*values)[lower] = value;
        (*values)[upper] = value;
    }
}

std::string quote_systemd_value(const std::string& value) {
    std::string quoted = "\"";
    for (size_t i = 0; i < value.size(); ++i) {
        if (value[i] == '\\' || value[i] == '"') {
            quoted.push_back('\\');
        }
        quoted.push_back(value[i]);
    }
    quoted.push_back('"');
    return quoted;
}

std::string render_minimal_environment() {
    std::ostringstream out;
    out << "AICO_ENVIRONMENT_VALID=\"0\"\n";
    out << "AICO_EXTERNAL_ACCESS_DISABLED=\"1\"\n";
    out << "NO_PROXY=" << quote_systemd_value(kDefaultNoProxy) << "\n";
    out << "no_proxy=" << quote_systemd_value(kDefaultNoProxy) << "\n";
    return out.str();
}

bool invalid(GeneratedSystemEnv* generated, const std::string& error) {
    generated->valid = false;
    generated->content = render_minimal_environment();
    generated->error = error;
    generated->dropped_keys.clear();
    return true;
}

}  // namespace

bool generate_systemd_environment(const std::string& input,
                                  GeneratedSystemEnv* generated) {
    if (!generated) {
        return false;
    }
    generated->valid = false;
    generated->content.clear();
    generated->error.clear();
    generated->dropped_keys.clear();

    if (input.size() > 64 * 1024) {
        return invalid(generated, "system environment input exceeds 64 KiB");
    }

    std::vector<EnvAssignment> assignments;
    SystemEnvProxy proxy;
    std::string error;
    if (!parse_system_env_content(input, &assignments, &proxy, &error)) {
        return invalid(generated, error);
    }

    std::map<std::string, std::string> values;
    for (size_t i = 0; i < assignments.size(); ++i) {
        const EnvAssignment& assignment = assignments[i];
        if (!is_allowed_system_env_key(assignment.key)) {
            // Dropping just this key keeps every approved credential in place.
            // Discarding the whole file here used to strip them all silently.
            generated->dropped_keys.push_back(assignment.key);
            continue;
        }
        if (assignment.value.size() > 8192) {
            return invalid(generated,
                           "environment value exceeds 8192 bytes: " + assignment.key);
        }
        values[assignment.key] = assignment.value;
    }

    const struct {
        const char* name;
        const std::string* value;
    } proxy_values[] = {
        {"HTTP_PROXY", &proxy.http_proxy},
        {"HTTPS_PROXY", &proxy.https_proxy},
        {"ALL_PROXY", &proxy.all_proxy},
    };
    for (size_t i = 0; i < sizeof(proxy_values) / sizeof(proxy_values[0]); ++i) {
        error = validate_system_proxy_url(*proxy_values[i].value);
        if (!error.empty()) {
            return invalid(generated,
                           std::string(proxy_values[i].name) + ": " + error);
        }
    }

    set_pair(&values, "http_proxy", "HTTP_PROXY", proxy.http_proxy);
    set_pair(&values, "https_proxy", "HTTPS_PROXY", proxy.https_proxy);
    set_pair(&values, "all_proxy", "ALL_PROXY", proxy.all_proxy);
    const bool has_proxy = !proxy.http_proxy.empty() || !proxy.https_proxy.empty() ||
                           !proxy.all_proxy.empty();
    std::string no_proxy = proxy.no_proxy;
    if (has_proxy && no_proxy.empty()) {
        no_proxy = kDefaultNoProxy;
    }
    set_pair(&values, "no_proxy", "NO_PROXY", no_proxy);

    std::ostringstream out;
    out << "AICO_ENVIRONMENT_VALID=\"1\"\n";
    for (std::map<std::string, std::string>::const_iterator it = values.begin();
         it != values.end(); ++it) {
        out << it->first << "=" << quote_systemd_value(it->second) << "\n";
    }

    generated->valid = true;
    generated->content = out.str();
    return true;
}

}  // namespace aico
