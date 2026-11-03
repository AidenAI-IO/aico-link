#include "system_env_parser.h"

#include <arpa/inet.h>
#include <ctype.h>

#include <algorithm>
#include <map>
#include <set>
#include <sstream>
#include <string>

namespace aico {
namespace {

std::string trim_copy(const std::string& input) {
    size_t start = 0;
    while (start < input.size() && isspace(static_cast<unsigned char>(input[start]))) {
        ++start;
    }

    size_t end = input.size();
    while (end > start && isspace(static_cast<unsigned char>(input[end - 1]))) {
        --end;
    }
    return input.substr(start, end - start);
}

bool starts_with(const std::string& text, const std::string& prefix) {
    return text.compare(0, prefix.size(), prefix) == 0;
}

std::string lower_copy(const std::string& text) {
    std::string out;
    out.reserve(text.size());
    for (size_t i = 0; i < text.size(); ++i) {
        out.push_back(static_cast<char>(tolower(static_cast<unsigned char>(text[i]))));
    }
    return out;
}

bool is_env_name_start(char c) {
    unsigned char u = static_cast<unsigned char>(c);
    return c == '_' || isalpha(u);
}

bool is_env_name_char(char c) {
    unsigned char u = static_cast<unsigned char>(c);
    return c == '_' || isalnum(u);
}

bool is_env_name(const std::string& name) {
    if (name.empty() || !is_env_name_start(name[0])) {
        return false;
    }
    for (size_t i = 1; i < name.size(); ++i) {
        if (!is_env_name_char(name[i])) {
            return false;
        }
    }
    return true;
}

bool is_ascii_space(char c) {
    return isspace(static_cast<unsigned char>(c)) != 0;
}

bool is_unsafe_unquoted_value_char(char c) {
    switch (c) {
    case ';':
    case '&':
    case '|':
    case '<':
    case '>':
    case '(':
    case ')':
    case '`':
    case '$':
    case '\\':
    case '\'':
    case '"':
        return true;
    default:
        return false;
    }
}

bool is_safe_unquoted_value(const std::string& value) {
    if (value.empty()) {
        return false;
    }
    for (size_t i = 0; i < value.size(); ++i) {
        unsigned char c = static_cast<unsigned char>(value[i]);
        if (c >= 0x80 || isspace(c) || is_unsafe_unquoted_value_char(static_cast<char>(c))) {
            return false;
        }
    }
    return true;
}

bool is_system_proxy_env_key(const std::string& key) {
    return key == "http_proxy" || key == "HTTP_PROXY" ||
           key == "https_proxy" || key == "HTTPS_PROXY" ||
           key == "all_proxy" || key == "ALL_PROXY" ||
           key == "no_proxy" || key == "NO_PROXY";
}

bool quote_env_value(const std::string& value, std::string* quoted, std::string* error) {
    if (is_safe_unquoted_value(value)) {
        *quoted = value;
        return true;
    }
    if (value.find('\'') == std::string::npos) {
        *quoted = "'" + value + "'";
        return true;
    }
    if (value.find('"') == std::string::npos &&
        value.find('$') == std::string::npos &&
        value.find('`') == std::string::npos &&
        value.find('\\') == std::string::npos) {
        *quoted = "\"" + value + "\"";
        return true;
    }
    if (error) {
        *error = "cannot render assignment value without unsupported shell syntax";
    }
    return false;
}

bool append_env_assignment(std::ostringstream& out,
                           const EnvAssignment& assignment,
                           std::string* error) {
    std::string quoted;
    if (!quote_env_value(assignment.value, &quoted, error)) {
        return false;
    }
    out << assignment.key << "=" << quoted << "\n";
    return true;
}

bool parse_value(const std::string& line,
                 size_t line_number,
                 size_t* pos,
                 std::string* value,
                 std::string* error) {
    if (*pos >= line.size()) {
        value->clear();
        return true;
    }

    char quote = line[*pos];
    if (quote == '\'' || quote == '"') {
        ++(*pos);
        std::string out;
        while (*pos < line.size() && line[*pos] != quote) {
            char c = line[*pos];
            if (quote == '"' && (c == '$' || c == '`' || c == '\\')) {
                if (error) {
                    *error = "line " + std::to_string(line_number) + ": unsupported shell expansion in quoted value";
                }
                return false;
            }
            out.push_back(c);
            ++(*pos);
        }
        if (*pos >= line.size()) {
            if (error) *error = "line " + std::to_string(line_number) + ": unterminated quoted value";
            return false;
        }
        ++(*pos);
        *value = out;
        return true;
    }

    std::string out;
    while (*pos < line.size() && !is_ascii_space(line[*pos])) {
        char c = line[*pos];
        if (is_unsafe_unquoted_value_char(c)) {
            if (error) {
                *error = "line " + std::to_string(line_number) + ": unsupported shell syntax in assignment value";
            }
            return false;
        }
        out.push_back(c);
        ++(*pos);
    }
    *value = out;
    return true;
}

void skip_spaces(const std::string& line, size_t* pos) {
    while (*pos < line.size() && is_ascii_space(line[*pos])) {
        ++(*pos);
    }
}

bool parse_assignment_line(const std::string& raw_line,
                           size_t line_number,
                           std::vector<EnvAssignment>* assignments,
                           std::map<std::string, std::string>* values_by_key,
                           std::string* error) {
    std::string line = trim_copy(raw_line);
    if (line.empty() || line[0] == '#') {
        return true;
    }

    bool saw_export = false;
    if (line == "export") {
        if (error) *error = "line " + std::to_string(line_number) + ": export requires assignments";
        return false;
    }
    if (starts_with(line, "export ") || starts_with(line, "export\t")) {
        saw_export = true;
    }

    size_t pos = saw_export ? 6 : 0;
    skip_spaces(line, &pos);
    bool saw_assignment = false;
    while (pos < line.size()) {
        if (line[pos] == '#') {
            break;
        }

        size_t key_start = pos;
        if (!is_env_name_start(line[pos])) {
            if (error) *error = "line " + std::to_string(line_number) + ": unsupported system env statement";
            return false;
        }
        ++pos;
        while (pos < line.size() && is_env_name_char(line[pos])) {
            ++pos;
        }
        std::string key = line.substr(key_start, pos - key_start);
        if (pos >= line.size() || line[pos] != '=') {
            if (error) *error = "line " + std::to_string(line_number) + ": unsupported system env statement";
            return false;
        }
        ++pos;

        std::string value;
        if (!parse_value(line, line_number, &pos, &value, error)) {
            return false;
        }
        if (!is_env_name(key)) {
            if (error) *error = "line " + std::to_string(line_number) + ": invalid environment variable name";
            return false;
        }

        if (assignments) {
            EnvAssignment assignment;
            assignment.key = key;
            assignment.value = value;
            assignments->push_back(assignment);
        }
        (*values_by_key)[key] = value;
        saw_assignment = true;

        if (pos < line.size() && !is_ascii_space(line[pos])) {
            if (error) {
                *error = "line " + std::to_string(line_number) + ": expected whitespace after assignment";
            }
            return false;
        }
        skip_spaces(line, &pos);
    }

    if (!saw_assignment) {
        if (error) *error = "line " + std::to_string(line_number) + ": unsupported system env statement";
        return false;
    }
    return true;
}

std::string first_upper_or_lower(const std::map<std::string, std::string>& values,
                                 const char* upper,
                                 const char* lower) {
    std::map<std::string, std::string>::const_iterator it = values.find(upper);
    if (it != values.end() && !it->second.empty()) {
        return it->second;
    }
    it = values.find(lower);
    if (it != values.end()) {
        return it->second;
    }
    return "";
}

bool has_duplicate_proxy_scheme(const std::string& url) {
    std::string trimmed = trim_copy(url);
    size_t first = trimmed.find("://");
    if (first == std::string::npos) {
        return false;
    }
    return trimmed.find("://", first + 3) != std::string::npos;
}

bool is_hex_digit(char c) {
    unsigned char u = static_cast<unsigned char>(c);
    return isxdigit(u) != 0;
}

int hex_value(char c) {
    if (c >= '0' && c <= '9') {
        return c - '0';
    }
    if (c >= 'a' && c <= 'f') {
        return c - 'a' + 10;
    }
    if (c >= 'A' && c <= 'F') {
        return c - 'A' + 10;
    }
    return -1;
}

std::string invalid_percent_escape(const std::string& value) {
    for (size_t i = 0; i < value.size(); ++i) {
        if (value[i] != '%') {
            continue;
        }
        if (i + 2 >= value.size() || !is_hex_digit(value[i + 1]) || !is_hex_digit(value[i + 2])) {
            return value.substr(i, std::min<size_t>(3, value.size() - i));
        }
        i += 2;
    }
    return "";
}

std::string invalid_host_percent_escape(const std::string& host) {
    for (size_t i = 0; i < host.size(); ++i) {
        if (host[i] != '%') {
            continue;
        }
        int escaped = hex_value(host[i + 1]) * 16 + hex_value(host[i + 2]);
        if (host.substr(i, 3) != "%25" && escaped < 0x80) {
            return host.substr(i, 3);
        }
        i += 2;
    }
    return "";
}

bool has_proxy_whitespace(const std::string& url) {
    for (unsigned char c : url) {
        if (c >= 0x80 || isspace(c)) {
            return true;
        }
    }
    return false;
}

bool has_embedded_proxy_assignment(const std::string& url) {
    const char* fragments[] = {
        "http_proxy=", "https_proxy=", "all_proxy=",
        "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
        NULL,
    };
    std::string lower = lower_copy(url);
    for (int i = 0; fragments[i]; ++i) {
        size_t pos = lower.find(lower_copy(fragments[i]));
        if (pos != std::string::npos && pos > 0) {
            return true;
        }
    }
    return false;
}

bool has_ascii_control(const std::string& value) {
    for (unsigned char c : value) {
        if (c < 0x20 || c == 0x7f) {
            return true;
        }
    }
    return false;
}

bool is_valid_optional_port(const std::string& tail) {
    if (tail.empty()) {
        return true;
    }
    if (tail[0] != ':') {
        return false;
    }
    for (size_t i = 1; i < tail.size(); ++i) {
        if (!isdigit(static_cast<unsigned char>(tail[i]))) {
            return false;
        }
    }
    return true;
}

bool is_valid_bracketed_ipv6_literal(const std::string& literal) {
    if (literal.empty()) {
        return false;
    }

    std::string address = literal;
    size_t zone = literal.find("%25");
    if (zone != std::string::npos) {
        if (zone == 0 || zone + 3 >= literal.size()) {
            return false;
        }
        address = literal.substr(0, zone);
    }
    if (address.find('%') != std::string::npos) {
        return false;
    }

    struct in6_addr parsed;
    return inet_pton(AF_INET6, address.c_str(), &parsed) == 1;
}

bool is_valid_proxy_host_char(char c) {
    unsigned char u = static_cast<unsigned char>(c);
    if (isalnum(u)) {
        return true;
    }
    switch (c) {
    case '-':
    case '.':
    case '_':
    case '~':
    case '%':
    case '!':
    case '$':
    case '&':
    case '\'':
    case '(':
    case ')':
    case '*':
    case '+':
    case ',':
    case ';':
    case '=':
        return true;
    default:
        return false;
    }
}

bool is_valid_proxy_host(const std::string& host) {
    if (host.empty()) {
        return false;
    }
    for (size_t i = 0; i < host.size(); ++i) {
        if (!is_valid_proxy_host_char(host[i])) {
            return false;
        }
    }
    return true;
}

std::string validate_proxy_host(const std::string& host) {
    if (!is_valid_proxy_host(host)) {
        return "invalid proxy URL host";
    }
    std::string bad_escape = invalid_host_percent_escape(host);
    if (!bad_escape.empty()) {
        return "invalid URL escape \"" + bad_escape + "\"";
    }
    return "";
}

std::string validate_proxy_authority(const std::string& trimmed, size_t scheme_end) {
    size_t host_start = scheme_end + 3;
    size_t host_end = trimmed.find_first_of("/?#", host_start);
    std::string authority = trimmed.substr(
        host_start,
        host_end == std::string::npos ? std::string::npos : host_end - host_start);
    size_t at = authority.rfind('@');
    std::string host_port = at == std::string::npos ? authority : authority.substr(at + 1);
    if (host_port.empty()) {
        return "expected absolute proxy URL, for example http://127.0.0.1:7890";
    }

    if (host_port[0] == '[') {
        size_t close = host_port.find(']');
        if (close == std::string::npos || close == 1) {
            return "invalid proxy URL authority";
        }
        if (!is_valid_bracketed_ipv6_literal(host_port.substr(1, close - 1))) {
            return "invalid proxy URL host";
        }
        if (!is_valid_optional_port(host_port.substr(close + 1))) {
            return "invalid proxy URL port";
        }
        return "";
    }

    if (host_port.find(']') != std::string::npos || host_port.find('[') != std::string::npos) {
        return "invalid proxy URL authority";
    }

    size_t colon = host_port.find(':');
    std::string host = colon == std::string::npos ? host_port : host_port.substr(0, colon);
    if (host.empty()) {
        return "expected absolute proxy URL, for example http://127.0.0.1:7890";
    }
    std::string host_error = validate_proxy_host(host);
    if (!host_error.empty()) {
        return host_error;
    }
    if (colon != std::string::npos && !is_valid_optional_port(host_port.substr(colon))) {
        return "invalid proxy URL port";
    }
    if (colon != std::string::npos && host_port.find(':', colon + 1) != std::string::npos) {
        return "invalid proxy URL authority";
    }
    return "";
}

}  // namespace

bool is_allowed_system_env_key(const std::string& key) {
    static const std::set<std::string> allowed = {
        "http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY",
        "all_proxy", "ALL_PROXY", "no_proxy", "NO_PROXY",
        "ANTHROPIC_BASE_URL", "ANDROID_SERIAL", "AICO_ADB_SERIAL",
        "AICO_POINTER_MODE", "AICO_WIFI_PROXY_ENABLED",
    };
    if (allowed.find(key) != allowed.end()) {
        return true;
    }

    // Provider credentials reach the agent as `api_key = "$NAME"`, where the
    // operator names the variable per provider record, so an exact list cannot
    // cover them and goes stale every time a provider is added. Approve
    // credential-shaped names instead: the names that matter for the loader or
    // an interpreter (LD_*, PATH, PYTHONPATH, BASH_ENV, GLIBC_TUNABLES) do not
    // end in any of these suffixes. Only api_key supports this syntax, so no
    // other field needs widening here.
    static const std::string suffixes[] = {"_KEY", "_TOKEN", "_SECRET"};
    for (size_t i = 0; i < sizeof(suffixes) / sizeof(suffixes[0]); ++i) {
        const std::string& suffix = suffixes[i];
        if (key.size() > suffix.size() &&
            key.compare(key.size() - suffix.size(), suffix.size(), suffix) == 0) {
            return true;
        }
    }
    return false;
}

std::string validate_system_proxy_url(const std::string& url) {
    if (url.empty()) {
        return "";
    }
    if (has_proxy_whitespace(url)) {
        return "proxy URL contains whitespace; use one export assignment per line";
    }
    std::string trimmed = trim_copy(url);
    if (trimmed.empty()) {
        return "expected absolute proxy URL, for example http://127.0.0.1:7890";
    }
    if (has_embedded_proxy_assignment(trimmed)) {
        return "proxy URL contains another proxy assignment; use one export assignment per line";
    }
    if (has_ascii_control(trimmed)) {
        return "invalid control character in proxy URL";
    }
    if (has_duplicate_proxy_scheme(trimmed)) {
        return "duplicate scheme in proxy URL";
    }
    std::string bad_escape = invalid_percent_escape(trimmed);
    if (!bad_escape.empty()) {
        return "invalid URL escape \"" + bad_escape + "\"";
    }

    size_t scheme_end = trimmed.find("://");
    if (scheme_end == std::string::npos || scheme_end == 0) {
        return "expected absolute proxy URL, for example http://127.0.0.1:7890";
    }
    std::string authority_error = validate_proxy_authority(trimmed, scheme_end);
    if (!authority_error.empty()) {
        return authority_error;
    }

    std::string scheme = lower_copy(trimmed.substr(0, scheme_end));
    if (scheme == "http" || scheme == "https" || scheme == "socks5") {
        return "";
    }
    return "unsupported proxy URL scheme \"" + trimmed.substr(0, scheme_end) + "\"";
}

bool parse_system_env_content(const std::string& content,
                              std::vector<EnvAssignment>* assignments,
                              SystemEnvProxy* proxy,
                              std::string* error) {
    if (assignments) {
        assignments->clear();
    }
    if (proxy) {
        *proxy = SystemEnvProxy();
    }

    std::map<std::string, std::string> values_by_key;
    std::istringstream input(content);
    std::string line;
    size_t line_number = 1;
    while (std::getline(input, line)) {
        if (!line.empty() && line[line.size() - 1] == '\r') {
            line.resize(line.size() - 1);
        }
        if (!parse_assignment_line(line, line_number, assignments, &values_by_key, error)) {
            return false;
        }
        ++line_number;
    }

    if (proxy) {
        proxy->http_proxy = first_upper_or_lower(values_by_key, "HTTP_PROXY", "http_proxy");
        proxy->https_proxy = first_upper_or_lower(values_by_key, "HTTPS_PROXY", "https_proxy");
        proxy->all_proxy = first_upper_or_lower(values_by_key, "ALL_PROXY", "all_proxy");
        proxy->no_proxy = first_upper_or_lower(values_by_key, "NO_PROXY", "no_proxy");
    }
    return true;
}

bool render_system_env_without_proxy_assignments(const std::string& content,
                                                 const std::string& managed_proxy_comment,
                                                 std::string* rendered,
                                                 std::string* error) {
    if (!rendered) {
        if (error) *error = "rendered output is null";
        return false;
    }
    rendered->clear();

    std::ostringstream out;
    std::istringstream input(content);
    std::string line;
    size_t line_number = 1;
    while (std::getline(input, line)) {
        if (!line.empty() && line[line.size() - 1] == '\r') {
            line.resize(line.size() - 1);
        }

        std::string trimmed = trim_copy(line);
        if (!managed_proxy_comment.empty() && trimmed == managed_proxy_comment) {
            ++line_number;
            continue;
        }
        if (trimmed.empty() || trimmed[0] == '#') {
            out << line << "\n";
            ++line_number;
            continue;
        }

        std::vector<EnvAssignment> line_assignments;
        std::map<std::string, std::string> values_by_key;
        if (!parse_assignment_line(line, line_number, &line_assignments, &values_by_key, error)) {
            return false;
        }

        bool has_proxy_assignment = false;
        for (size_t i = 0; i < line_assignments.size(); ++i) {
            if (is_system_proxy_env_key(line_assignments[i].key)) {
                has_proxy_assignment = true;
                break;
            }
        }
        if (!has_proxy_assignment) {
            out << line << "\n";
            ++line_number;
            continue;
        }
        for (size_t i = 0; i < line_assignments.size(); ++i) {
            if (!is_system_proxy_env_key(line_assignments[i].key) &&
                !append_env_assignment(out, line_assignments[i], error)) {
                return false;
            }
        }
        ++line_number;
    }

    *rendered = out.str();
    return true;
}

}  // namespace aico
