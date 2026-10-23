#ifndef AICO_LOG_H
#define AICO_LOG_H

#include <stdio.h>

namespace aico {

enum class LogLevel {
    DEBUG,
    INFO,
    WARN,
    ERROR,
};

// Sets the process-wide service name included in every subsequent log line.
// Names are normalized to lowercase snake_case.
void set_log_service(const char* service);

// Overrides the process-wide log destination. Passing NULL restores stderr.
// This hook is primarily intended for tests.
void set_log_output(FILE* output);

// Writes one event using the common format. Every line includes monotonic_ms,
// pid, and the native thread id so userspace operations can be correlated with
// monotonic kernel timestamps and per-thread /proc state. The formatted
// message is emitted as a quoted message= field with control characters
// escaped, so one call always produces exactly one physical line.
void log_event(LogLevel level,
               const char* component,
               const char* event,
               const char* format,
               ...)
#if defined(__GNUC__) || defined(__clang__)
               __attribute__((format(printf, 4, 5)))
#endif
               ;

}  // namespace aico

#define AICO_LOG_DEBUG(component, event, ...) \
    ::aico::log_event(::aico::LogLevel::DEBUG, component, event, __VA_ARGS__)
#define AICO_LOG_INFO(component, event, ...) \
    ::aico::log_event(::aico::LogLevel::INFO, component, event, __VA_ARGS__)
#define AICO_LOG_WARN(component, event, ...) \
    ::aico::log_event(::aico::LogLevel::WARN, component, event, __VA_ARGS__)
#define AICO_LOG_ERROR(component, event, ...) \
    ::aico::log_event(::aico::LogLevel::ERROR, component, event, __VA_ARGS__)

#endif  // AICO_LOG_H
