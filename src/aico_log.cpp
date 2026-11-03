#include "aico_log.h"

#include <errno.h>
#include <stdarg.h>
#include <stdint.h>
#include <string.h>
#include <time.h>

#include <cctype>
#include <functional>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#if defined(_WIN32)
#include <windows.h>
#else
#include <unistd.h>
#endif

#if defined(__linux__)
#include <sys/syscall.h>
#endif

#if defined(__APPLE__)
#include <pthread.h>
#endif

namespace aico {
namespace {

std::mutex g_log_mutex;
std::string g_log_service("unknown");
FILE* g_log_output = stderr;

const char* level_name(LogLevel level) {
    switch (level) {
        case LogLevel::DEBUG: return "DEBUG";
        case LogLevel::INFO: return "INFO";
        case LogLevel::WARN: return "WARN";
        case LogLevel::ERROR: return "ERROR";
    }
    return "INFO";
}

std::string normalize_identifier(const char* raw, const char* fallback) {
    std::string result;
    bool underscore = false;
    if (raw) {
        for (const unsigned char* p = reinterpret_cast<const unsigned char*>(raw); *p; ++p) {
            const unsigned char c = *p;
            if ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
                result.push_back(static_cast<char>(c));
                underscore = false;
            } else if (c >= 'A' && c <= 'Z') {
                result.push_back(static_cast<char>(std::tolower(c)));
                underscore = false;
            } else if (!result.empty() && !underscore) {
                result.push_back('_');
                underscore = true;
            }
        }
    }
    while (!result.empty() && result[result.size() - 1] == '_') {
        result.resize(result.size() - 1);
    }
    if (!result.empty()) {
        return result;
    }
    return fallback && fallback[0] ? fallback : "unknown";
}

std::string format_message(const char* format, va_list args) {
    if (!format || !format[0]) {
        return std::string();
    }
    va_list size_args;
    va_copy(size_args, args);
    const int required = vsnprintf(NULL, 0, format, size_args);
    va_end(size_args);
    if (required < 0) {
        return "format_error";
    }
    std::vector<char> buffer(static_cast<size_t>(required) + 1);
    va_list write_args;
    va_copy(write_args, args);
    vsnprintf(buffer.data(), buffer.size(), format, write_args);
    va_end(write_args);
    return std::string(buffer.data(), static_cast<size_t>(required));
}

void append_quoted(std::string* out, const std::string& value) {
    out->push_back('"');
    for (size_t i = 0; i < value.size(); ++i) {
        const unsigned char c = static_cast<unsigned char>(value[i]);
        switch (c) {
            case '"': out->append("\\\""); break;
            case '\\': out->append("\\\\"); break;
            case '\n': out->append("\\n"); break;
            case '\r': out->append("\\r"); break;
            case '\t': out->append("\\t"); break;
            default:
                if (c < 0x20) {
                    char escaped[7];
                    snprintf(escaped, sizeof(escaped), "\\u%04x", c);
                    out->append(escaped);
                } else {
                    out->push_back(static_cast<char>(c));
                }
                break;
        }
    }
    out->push_back('"');
}

std::string utc_timestamp() {
    const time_t now = time(NULL);
    struct tm utc;
    memset(&utc, 0, sizeof(utc));
#if defined(_WIN32)
    gmtime_s(&utc, &now);
#else
    gmtime_r(&now, &utc);
#endif
    char buffer[32];
    if (strftime(buffer, sizeof(buffer), "%Y-%m-%dT%H:%M:%SZ", &utc) == 0) {
        return "1970-01-01T00:00:00Z";
    }
    return buffer;
}

uint64_t monotonic_millis() {
#if defined(CLOCK_MONOTONIC)
    struct timespec now;
    memset(&now, 0, sizeof(now));
    if (clock_gettime(CLOCK_MONOTONIC, &now) == 0) {
        return static_cast<uint64_t>(now.tv_sec) * 1000ULL +
               static_cast<uint64_t>(now.tv_nsec) / 1000000ULL;
    }
#endif
    return 0;
}

uint64_t process_id() {
#if defined(_WIN32)
    return static_cast<uint64_t>(GetCurrentProcessId());
#else
    return static_cast<uint64_t>(getpid());
#endif
}

uint64_t native_thread_id() {
#if defined(_WIN32)
    return static_cast<uint64_t>(GetCurrentThreadId());
#elif defined(__linux__) && defined(SYS_gettid)
    return static_cast<uint64_t>(syscall(SYS_gettid));
#elif defined(__APPLE__)
    uint64_t thread_id = 0;
    if (pthread_threadid_np(NULL, &thread_id) == 0) {
        return thread_id;
    }
    return 0;
#else
    return static_cast<uint64_t>(
        std::hash<std::thread::id>()(std::this_thread::get_id()));
#endif
}

}  // namespace

void set_log_service(const char* service) {
    std::lock_guard<std::mutex> lock(g_log_mutex);
    g_log_service = normalize_identifier(service, "unknown");
}

void set_log_output(FILE* output) {
    std::lock_guard<std::mutex> lock(g_log_mutex);
    g_log_output = output ? output : stderr;
}

void log_event(LogLevel level,
               const char* component,
               const char* event,
               const char* format,
               ...) {
    const int saved_errno = errno;
    va_list args;
    va_start(args, format);
    const std::string message = format_message(format, args);
    va_end(args);

    std::lock_guard<std::mutex> lock(g_log_mutex);
    std::string line;
    line.reserve(message.size() + 128);
    line.append(utc_timestamp());
    line.append(" [");
    line.append(level_name(level));
    line.append("][");
    line.append(g_log_service);
    line.append("][");
    line.append(normalize_identifier(component, "runtime"));
    line.append("] ");
    line.append(normalize_identifier(event, "log_message"));
    line.append(" monotonic_ms=");
    line.append(std::to_string(monotonic_millis()));
    line.append(" pid=");
    line.append(std::to_string(process_id()));
    line.append(" tid=");
    line.append(std::to_string(native_thread_id()));
    if (!message.empty()) {
        line.append(" message=");
        append_quoted(&line, message);
    }
    line.push_back('\n');

    FILE* output = g_log_output ? g_log_output : stderr;
    fwrite(line.data(), 1, line.size(), output);
    fflush(output);
    errno = saved_errno;
}

}  // namespace aico
