#include "aico_sdk.h"
#include "camera_frame_utils.h"
#include "frame_processing.h"
#include "frame_service_client.h"
#include "hid_server.h"
#include "hid_server_html.h"
#include "image_process.h"

#include <arpa/inet.h>
#include <cerrno>
#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <iostream>
#include <netinet/in.h>
#include <sstream>
#include <string>
#include <time.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

static ScreenshotData g_screenshot;

namespace {

struct HttpRequest {
    std::string method;
    std::string path;
    std::string body;
};

struct ApiResponse {
    int status_code = 200;
    std::string status_text = "OK";
    std::string body = "{}";
};

struct CommandResult {
    int exit_code = -1;
    std::string output;
};

struct CaptureRequest {
    CaptureRequest()
        : device_name("/dev/video0"),
          pixel_format("uyvy"),
          subdev_device("/dev/v4l-subdev2") {
        aico_demo::set_default_camera_config(&config);
        // The legacy direct-capture HTTP path remains explicitly UYVY. The
        // frame_service path is the C1 NV12 owner and is configured separately.
        sync_strings();
    }

    void sync_strings() {
        config.device_name = device_name.c_str();
        config.pixel_format = pixel_format.c_str();
        config.subdev_device = subdev_device.c_str();
        config.edid_path = edid_path.empty() ? nullptr : edid_path.c_str();
    }

    aico::CameraConfig config;
    std::string device_name;
    std::string pixel_format;
    std::string subdev_device;
    std::string edid_path;
};

bool is_json_space(char ch) {
    return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n';
}

size_t skip_json_space(const std::string& text, size_t pos) {
    while (pos < text.size() && is_json_space(text[pos])) {
        ++pos;
    }
    return pos;
}

bool parse_json_string_at(const std::string& text, size_t* pos, std::string* out) {
    if (!pos || !out || *pos >= text.size() || text[*pos] != '"') {
        return false;
    }

    ++(*pos);
    out->clear();
    while (*pos < text.size()) {
        char ch = text[*pos];
        ++(*pos);
        if (ch == '"') {
            return true;
        }
        if (ch == '\\') {
            if (*pos >= text.size()) {
                return false;
            }
            char escaped = text[*pos];
            ++(*pos);
            switch (escaped) {
                case '"':
                case '\\':
                case '/':
                    out->push_back(escaped);
                    break;
                case 'b':
                    out->push_back('\b');
                    break;
                case 'f':
                    out->push_back('\f');
                    break;
                case 'n':
                    out->push_back('\n');
                    break;
                case 'r':
                    out->push_back('\r');
                    break;
                case 't':
                    out->push_back('\t');
                    break;
                default:
                    out->push_back(escaped);
                    break;
            }
            continue;
        }
        out->push_back(ch);
    }

    return false;
}

bool find_json_field_value(const std::string& json,
                           const std::string& field,
                           size_t* value_pos) {
    const std::string needle = "\"" + field + "\"";
    size_t search_pos = 0;

    while (true) {
        search_pos = json.find(needle, search_pos);
        if (search_pos == std::string::npos) {
            return false;
        }

        size_t pos = skip_json_space(json, search_pos + needle.size());
        if (pos >= json.size() || json[pos] != ':') {
            search_pos += needle.size();
            continue;
        }

        pos = skip_json_space(json, pos + 1);
        if (pos >= json.size()) {
            return false;
        }

        *value_pos = pos;
        return true;
    }
}

bool extract_json_string_field(const std::string& json,
                               const std::string& field,
                               std::string* out) {
    size_t pos = 0;
    if (!find_json_field_value(json, field, &pos)) {
        return false;
    }
    return parse_json_string_at(json, &pos, out);
}

bool extract_json_int_field(const std::string& json,
                            const std::string& field,
                            int* value) {
    size_t pos = 0;
    if (!value || !find_json_field_value(json, field, &pos)) {
        return false;
    }

    char* end = nullptr;
    long parsed = std::strtol(json.c_str() + pos, &end, 10);
    if (end == json.c_str() + pos) {
        return false;
    }

    *value = static_cast<int>(parsed);
    return true;
}

bool extract_json_double_field(const std::string& json,
                               const std::string& field,
                               double* value) {
    size_t pos = 0;
    if (!value || !find_json_field_value(json, field, &pos)) {
        return false;
    }

    char* end = nullptr;
    double parsed = std::strtod(json.c_str() + pos, &end);
    if (end == json.c_str() + pos) {
        return false;
    }

    *value = parsed;
    return true;
}

bool extract_json_bool_field(const std::string& json,
                             const std::string& field,
                             bool* value) {
    size_t pos = 0;
    if (!value || !find_json_field_value(json, field, &pos)) {
        return false;
    }

    if (json.compare(pos, 4, "true") == 0) {
        *value = true;
        return true;
    }
    if (json.compare(pos, 5, "false") == 0) {
        *value = false;
        return true;
    }

    return false;
}

bool extract_json_string_array_field(const std::string& json,
                                     const std::string& field,
                                     std::vector<std::string>* out) {
    size_t pos = 0;
    if (!out || !find_json_field_value(json, field, &pos) || pos >= json.size() ||
        json[pos] != '[') {
        return false;
    }

    out->clear();
    ++pos;
    while (pos < json.size()) {
        pos = skip_json_space(json, pos);
        if (pos >= json.size()) {
            return false;
        }
        if (json[pos] == ']') {
            return true;
        }

        std::string item;
        if (!parse_json_string_at(json, &pos, &item)) {
            return false;
        }
        out->push_back(item);

        pos = skip_json_space(json, pos);
        if (pos < json.size() && json[pos] == ',') {
            ++pos;
        }
    }

    return false;
}

std::string json_escape(const std::string& text) {
    static const char kHex[] = "0123456789abcdef";
    std::string out;
    out.reserve(text.size() + 16);

    for (size_t i = 0; i < text.size(); ++i) {
        unsigned char ch = static_cast<unsigned char>(text[i]);
        switch (ch) {
            case '\\':
                out += "\\\\";
                break;
            case '"':
                out += "\\\"";
                break;
            case '\b':
                out += "\\b";
                break;
            case '\f':
                out += "\\f";
                break;
            case '\n':
                out += "\\n";
                break;
            case '\r':
                out += "\\r";
                break;
            case '\t':
                out += "\\t";
                break;
            default:
                if (ch < 0x20) {
                    out += "\\u00";
                    out.push_back(kHex[(ch >> 4) & 0x0f]);
                    out.push_back(kHex[ch & 0x0f]);
                } else {
                    out.push_back(static_cast<char>(ch));
                }
                break;
        }
    }

    return out;
}

std::string trim_output(const std::string& text) {
    size_t end = text.size();
    while (end > 0 && (text[end - 1] == '\n' || text[end - 1] == '\r')) {
        --end;
    }
    return text.substr(0, end);
}

std::string read_until(int fd, const std::string& delimiter) {
    std::string result;
    char ch = '\0';
    while (true) {
        ssize_t n = read(fd, &ch, 1);
        if (n <= 0) {
            break;
        }
        result.push_back(ch);
        if (result.size() >= delimiter.size() &&
            result.compare(result.size() - delimiter.size(),
                           delimiter.size(),
                           delimiter) == 0) {
            break;
        }
    }
    return result;
}

bool read_exact(int fd, std::string* out, size_t length) {
    if (!out) {
        return false;
    }

    out->assign(length, '\0');
    size_t offset = 0;
    while (offset < length) {
        ssize_t n = read(fd, &(*out)[offset], length - offset);
        if (n < 0) {
            if (errno == EINTR) {
                continue;
            }
            return false;
        }
        if (n == 0) {
            return false;
        }
        offset += static_cast<size_t>(n);
    }
    return true;
}

bool write_all(int fd, const void* data, size_t size) {
    const char* bytes = static_cast<const char*>(data);
    size_t offset = 0;
    while (offset < size) {
        ssize_t n = write(fd, bytes + offset, size - offset);
        if (n < 0) {
            if (errno == EINTR) {
                continue;
            }
            return false;
        }
        offset += static_cast<size_t>(n);
    }
    return true;
}

HttpRequest parse_request(int client_fd) {
    HttpRequest req;
    std::string headers = read_until(client_fd, "\r\n\r\n");

    std::istringstream stream(headers);
    std::string line;
    if (std::getline(stream, line)) {
        std::istringstream line_stream(line);
        line_stream >> req.method >> req.path;
        size_t query_pos = req.path.find('?');
        if (query_pos != std::string::npos) {
            req.path = req.path.substr(0, query_pos);
        }
    }

    size_t content_length = 0;
    while (std::getline(stream, line)) {
        if (line.compare(0, 15, "Content-Length:") == 0) {
            content_length = static_cast<size_t>(std::strtoul(line.c_str() + 15, nullptr, 10));
        }
    }

    if (content_length > 0) {
        read_exact(client_fd, &req.body, content_length);
    }

    return req;
}

void send_response(int client_fd,
                   int status_code,
                   const std::string& status_text,
                   const std::string& content_type,
                   const std::string& body) {
    std::ostringstream header;
    header << "HTTP/1.1 " << status_code << " " << status_text << "\r\n";
    header << "Content-Type: " << content_type << "\r\n";
    header << "Content-Length: " << body.size() << "\r\n";
    header << "Connection: close\r\n";
    header << "\r\n";

    const std::string header_text = header.str();
    write_all(client_fd, header_text.data(), header_text.size());
    write_all(client_fd, body.data(), body.size());
}

void send_binary_response(int client_fd,
                          int status_code,
                          const std::string& status_text,
                          const std::string& content_type,
                          const std::vector<uint8_t>& body) {
    std::ostringstream header;
    header << "HTTP/1.1 " << status_code << " " << status_text << "\r\n";
    header << "Content-Type: " << content_type << "\r\n";
    header << "Content-Length: " << body.size() << "\r\n";
    header << "Connection: close\r\n";
    header << "\r\n";

    const std::string header_text = header.str();
    write_all(client_fd, header_text.data(), header_text.size());
    if (!body.empty()) {
        write_all(client_fd, body.data(), body.size());
    }
}

CommandResult execute_command(const std::vector<std::string>& command) {
    CommandResult result;
    if (command.empty()) {
        result.exit_code = 127;
        result.output = "missing command";
        return result;
    }

    int pipefd[2];
    if (pipe(pipefd) != 0) {
        result.exit_code = errno;
        result.output = std::string("pipe failed: ") + std::strerror(errno);
        return result;
    }

    pid_t pid = fork();
    if (pid < 0) {
        const int saved_errno = errno;
        close(pipefd[0]);
        close(pipefd[1]);
        result.exit_code = saved_errno;
        result.output = std::string("fork failed: ") + std::strerror(saved_errno);
        return result;
    }

    if (pid == 0) {
        close(pipefd[0]);
        dup2(pipefd[1], STDOUT_FILENO);
        dup2(pipefd[1], STDERR_FILENO);
        close(pipefd[1]);

        std::vector<char*> argv;
        argv.reserve(command.size() + 1);
        for (size_t i = 0; i < command.size(); ++i) {
            argv.push_back(const_cast<char*>(command[i].c_str()));
        }
        argv.push_back(nullptr);

        execv(argv[0], argv.data());

        char buf[256];
        int len = std::snprintf(buf,
                                sizeof(buf),
                                "execv failed for %s: %s\n",
                                command[0].c_str(),
                                std::strerror(errno));
        if (len > 0) {
            write(STDERR_FILENO, buf, static_cast<size_t>(len));
        }
        _exit(127);
    }

    close(pipefd[1]);
    char buf[4096];
    while (true) {
        ssize_t n = read(pipefd[0], buf, sizeof(buf));
        if (n < 0) {
            if (errno == EINTR) {
                continue;
            }
            break;
        }
        if (n == 0) {
            break;
        }
        result.output.append(buf, static_cast<size_t>(n));
    }
    close(pipefd[0]);

    int status = 0;
    while (waitpid(pid, &status, 0) < 0) {
        if (errno != EINTR) {
            status = -1;
            break;
        }
    }

    if (status == -1) {
        result.exit_code = 127;
    } else if (WIFEXITED(status)) {
        result.exit_code = WEXITSTATUS(status);
    } else if (WIFSIGNALED(status)) {
        result.exit_code = 128 + WTERMSIG(status);
    } else {
        result.exit_code = 127;
    }

    result.output = trim_output(result.output);
    return result;
}

CommandResult execute_hid_command(const std::vector<std::string>& hid_command_prefix,
                                  const std::vector<std::string>& args) {
    std::vector<std::string> command = hid_command_prefix;
    command.insert(command.end(), args.begin(), args.end());
    return execute_command(command);
}

ApiResponse make_json_error(int status_code, const std::string& message) {
    ApiResponse response;
    response.status_code = status_code;
    response.status_text = (status_code >= 500) ? "Internal Server Error" : "Bad Request";
    response.body = "{\"error\":\"" + json_escape(message) + "\"}";
    return response;
}

ApiResponse make_command_response(const CommandResult& result) {
    ApiResponse response;
    if (result.exit_code == 0) {
        const std::string output = result.output.empty() ? "ok" : result.output;
        response.body = "{\"result\":\"" + json_escape(output) + "\"}";
        return response;
    }

    response.status_code = 500;
    response.status_text = "Internal Server Error";
    response.body = "{\"error\":\"" +
                    json_escape(result.output.empty() ? "command failed" : result.output) +
                    "\",\"exit_code\":" + std::to_string(result.exit_code) + "}";
    return response;
}

bool parse_capture_request(const std::string& body,
                           CaptureRequest* request,
                           std::string* error) {
    if (!request) {
        if (error) {
            *error = "missing capture request";
        }
        return false;
    }

    extract_json_string_field(body, "device", &request->device_name);
    extract_json_string_field(body, "subdev", &request->subdev_device);
    extract_json_string_field(body, "edid", &request->edid_path);
    if (!extract_json_string_field(body, "pixfmt", &request->pixel_format)) {
        extract_json_string_field(body, "pixel_format", &request->pixel_format);
    }

    extract_json_int_field(body, "width", &request->config.width);
    extract_json_int_field(body, "height", &request->config.height);
    extract_json_int_field(body, "skip_frames", &request->config.skip_frames);
    extract_json_int_field(body, "trigger_retries", &request->config.trigger_retries);
    extract_json_int_field(body, "trigger_delay_ms", &request->config.trigger_delay_ms);
    extract_json_int_field(body, "capture_retries", &request->config.capture_retries);

    bool bool_value = false;
    if (extract_json_bool_field(body, "force", &bool_value)) {
        request->config.force_trigger = bool_value;
    }
    if (extract_json_bool_field(body, "enable_hdmi_sync", &bool_value)) {
        request->config.enable_hdmi_sync = bool_value;
    }
    if (extract_json_bool_field(body, "no_hdmi_sync", &bool_value)) {
        request->config.enable_hdmi_sync = !bool_value;
    }
    if (extract_json_bool_field(body, "require_exact_resolution", &bool_value)) {
        request->config.require_exact_resolution = bool_value;
    }
    if (extract_json_bool_field(body, "allow_resolution_mismatch", &bool_value)) {
        request->config.require_exact_resolution = !bool_value;
    }
    if (extract_json_bool_field(body, "reject_uniform_frames", &bool_value)) {
        request->config.reject_uniform_frames = bool_value;
    }
    if (extract_json_bool_field(body, "allow_uniform_frames", &bool_value)) {
        request->config.reject_uniform_frames = !bool_value;
    }

    if (request->config.width <= 0 || request->config.height <= 0) {
        if (error) {
            *error = "width and height must be positive";
        }
        return false;
    }
    if (request->config.skip_frames < 0 || request->config.trigger_retries < 0 ||
        request->config.trigger_delay_ms < 0 || request->config.capture_retries < 0) {
        if (error) {
            *error = "capture counts and delays must be non-negative";
        }
        return false;
    }
    if (request->pixel_format.empty()) {
        if (error) {
            *error = "pixel format must not be empty";
        }
        return false;
    }

    request->sync_strings();
    return true;
}

ApiResponse handle_capture_request(const std::string& body) {
    CaptureRequest request;
    std::string error;
    if (!parse_capture_request(body, &request, &error)) {
        g_screenshot = ScreenshotData{};
        return make_json_error(400, error);
    }

    const char* socket_path = std::getenv("FRAME_SERVICE_SOCKET");
    if (!socket_path || socket_path[0] == '\0') {
        socket_path = "/tmp/frame_service.sock";
    }

    aico::FrameServiceClient client(socket_path);
    uint64_t since_seq = 0;
    if (request.config.force_trigger) {
        aico::HealthResult health;
        if (client.health(&health) == aico::FrameServiceStatus::OK) {
            since_seq = health.latest_seq;
        }
        aico::FrameServiceStatus restart_status = client.restart();
        if (restart_status != aico::FrameServiceStatus::OK) {
            g_screenshot = ScreenshotData{};
            return make_json_error(500, std::string("frame service restart failed: ") +
                                         aico::frame_service_status_to_string(restart_status));
        }
    }
    aico::FrameResult frame;
    std::vector<uint8_t> rgb;

    aico::FrameServiceStatus status = aico::FrameServiceStatus::INTERNAL_ERROR;
    if (request.config.force_trigger) {
        const uint64_t start_ms = []() -> uint64_t {
            struct timespec ts;
            clock_gettime(CLOCK_MONOTONIC, &ts);
            return static_cast<uint64_t>(ts.tv_sec) * 1000ULL + static_cast<uint64_t>(ts.tv_nsec) / 1000000ULL;
        }();
        while (true) {
            struct timespec ts;
            clock_gettime(CLOCK_MONOTONIC, &ts);
            uint64_t now_ms = static_cast<uint64_t>(ts.tv_sec) * 1000ULL + static_cast<uint64_t>(ts.tv_nsec) / 1000000ULL;
            if (now_ms - start_ms >= 5000) {
                status = aico::FrameServiceStatus::TIMEOUT;
                break;
            }
            uint32_t remaining_ms = static_cast<uint32_t>(5000 - (now_ms - start_ms));
            status = client.latest_frame(since_seq, remaining_ms, &frame);
            if (status != aico::FrameServiceStatus::OK) {
                break;
            }
            if (!frame.metadata.stale && frame.metadata.seq > since_seq) {
                break;
            }
            usleep(50 * 1000);
        }
    } else {
        status = client.latest_frame(0, 0, &frame);
    }
    if (status != aico::FrameServiceStatus::OK) {
        g_screenshot = ScreenshotData{};
        return make_json_error(500, std::string("frame service capture failed: ") +
                                     aico::frame_service_status_to_string(status));
    }

    if (!aico::convert_frame_to_rgb(frame.metadata, frame.data, &rgb)) {
        g_screenshot = ScreenshotData{};
        return make_json_error(500, "unsupported pixel format or incomplete frame buffer");
    }

    const int w = static_cast<int>(frame.metadata.width);
    const int h = static_cast<int>(frame.metadata.height);
    if (rgb.size() < static_cast<size_t>(w) * static_cast<size_t>(h) * 3U) {
        g_screenshot = ScreenshotData{};
        return make_json_error(500, "incomplete rgb buffer");
    }

    cv::Mat rgb_mat(h, w, CV_8UC3, rgb.data());
    cv::Mat processed;
    const int crop_rc = aico_image::crop_black_bars(rgb_mat, processed);
    if (crop_rc != aico_image::kSuccess) {
        std::cerr << "Warning: crop_black_bars failed (" << crop_rc
                  << "); using uncropped frame" << std::endl;
        processed = rgb_mat;
    }

    std::vector<uint8_t> jpeg;
    if (aico_image::encode_to_jpg_fit(processed, jpeg) != aico_image::kSuccess) {
        g_screenshot = ScreenshotData{};
        return make_json_error(500, "jpeg encode failed");
    }

    g_screenshot.jpeg.swap(jpeg);
    g_screenshot.width = processed.cols;
    g_screenshot.height = processed.rows;

    ApiResponse response;
    response.body = "{\"ok\":true,\"width\":" + std::to_string(processed.cols) +
                    ",\"height\":" + std::to_string(processed.rows) +
                    ",\"pixel_format\":\"" + json_escape(frame.metadata.pixel_format) + "\"}";
    return response;
}

static uint64_t now_ms() {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return static_cast<uint64_t>(ts.tv_sec) * 1000ULL +
           static_cast<uint64_t>(ts.tv_nsec) / 1000000ULL;
}

// Compute mean per-channel absolute difference between two same-size RGB mats.
// Returns average difference in the range [0, 255].
static double frame_diff(const cv::Mat& a, const cv::Mat& b) {
    cv::Mat diff;
    cv::absdiff(a, b, diff);
    cv::Scalar mean_val = cv::mean(diff);
    return (mean_val[0] + mean_val[1] + mean_val[2]) / 3.0;
}

ApiResponse handle_wait_stable_request(const std::string& body) {
    int timeout_ms = 3000;
    int stable_ms = 500;
    double diff_threshold = 2.0;

    extract_json_int_field(body, "timeout_ms", &timeout_ms);
    extract_json_int_field(body, "stable_ms", &stable_ms);
    extract_json_double_field(body, "diff_threshold", &diff_threshold);

    if (timeout_ms <= 0) timeout_ms = 3000;
    if (stable_ms <= 0) stable_ms = 500;
    if (stable_ms > timeout_ms) stable_ms = timeout_ms;
    if (diff_threshold < 0.0) diff_threshold = 2.0;

    const char* socket_path = std::getenv("FRAME_SERVICE_SOCKET");
    if (!socket_path || socket_path[0] == '\0') {
        socket_path = "/tmp/frame_service.sock";
    }

    aico::FrameServiceClient client(socket_path);

    const uint64_t start_ms = now_ms();
    const uint64_t deadline_ms = start_ms + static_cast<uint64_t>(timeout_ms);

    aico::FrameResult prev_frame;
    uint64_t prev_seq = 0;
    {
        aico::FrameServiceStatus s = client.latest_frame(0, 0, &prev_frame);
        if (s != aico::FrameServiceStatus::OK) {
            return make_json_error(500, std::string("frame service error: ") +
                                        aico::frame_service_status_to_string(s));
        }
        prev_seq = prev_frame.metadata.seq;
    }

    std::vector<uint8_t> prev_rgb;
    if (!aico::convert_frame_to_rgb(prev_frame.metadata, prev_frame.data, &prev_rgb)) {
        return make_json_error(500, "unsupported pixel format");
    }

    int prev_w = static_cast<int>(prev_frame.metadata.width);
    int prev_h = static_cast<int>(prev_frame.metadata.height);
    if (prev_w <= 0 || prev_h <= 0 ||
        prev_rgb.size() < static_cast<size_t>(prev_w) * static_cast<size_t>(prev_h) * 3U) {
        return make_json_error(500, "incomplete rgb buffer");
    }
    cv::Mat prev_mat(prev_h, prev_w, CV_8UC3, prev_rgb.data());

    uint64_t stable_since_ms = now_ms();

    bool stable = false;
    while (true) {
        uint64_t cur_ms = now_ms();
        if (cur_ms >= deadline_ms) {
            stable = (cur_ms - stable_since_ms) >= static_cast<uint64_t>(stable_ms);
            break;
        }
        if ((cur_ms - stable_since_ms) >= static_cast<uint64_t>(stable_ms)) {
            stable = true;
            break;
        }

        uint32_t remaining = static_cast<uint32_t>(deadline_ms - cur_ms);
        uint32_t poll_ms = remaining < 200 ? remaining : 200;

        aico::FrameResult next_frame;
        aico::FrameServiceStatus s = client.latest_frame(prev_seq, poll_ms, &next_frame);

        if (s == aico::FrameServiceStatus::TIMEOUT) {
            continue;
        }
        if (s != aico::FrameServiceStatus::OK) {
            return make_json_error(500, std::string("frame service error: ") +
                                        aico::frame_service_status_to_string(s));
        }
        if (next_frame.metadata.stale || next_frame.metadata.seq <= prev_seq) {
            usleep(50 * 1000);
            continue;
        }

        std::vector<uint8_t> next_rgb;
        if (!aico::convert_frame_to_rgb(next_frame.metadata, next_frame.data, &next_rgb)) {
            prev_seq = next_frame.metadata.seq;
            stable_since_ms = now_ms();
            continue;
        }

        const int nw = static_cast<int>(next_frame.metadata.width);
        const int nh = static_cast<int>(next_frame.metadata.height);
        if (nw <= 0 || nh <= 0 ||
            next_rgb.size() < static_cast<size_t>(nw) * static_cast<size_t>(nh) * 3U) {
            prev_seq = next_frame.metadata.seq;
            stable_since_ms = now_ms();
            continue;
        }

        cv::Mat next_mat(nh, nw, CV_8UC3, next_rgb.data());

        double diff = 0.0;
        if (nw == prev_w && nh == prev_h) {
            diff = frame_diff(prev_mat, next_mat);
        } else {
            diff = diff_threshold + 1.0;
        }

        if (diff > diff_threshold) {
            stable_since_ms = now_ms();
        }

        prev_seq = next_frame.metadata.seq;
        prev_rgb = std::move(next_rgb);
        prev_w = nw;
        prev_h = nh;
        prev_mat = cv::Mat(nh, nw, CV_8UC3, prev_rgb.data());
    }

    uint64_t elapsed = now_ms() - start_ms;
    ApiResponse response;
    response.body = "{\"ok\":true,\"stable\":" + std::string(stable ? "true" : "false") +
                    ",\"elapsed_ms\":" + std::to_string(elapsed) + "}";
    return response;
}

ApiResponse handle_api_request(const std::string& path,
                               const std::string& body,
                               const std::vector<std::string>& hid_command_prefix) {
    if (path == "/api/keyboard/tap") {
        std::vector<std::string> keys;
        if (!extract_json_string_array_field(body, "keys", &keys) || keys.empty()) {
            return make_json_error(400, "missing keys");
        }

        std::vector<std::string> args;
        args.push_back("keyboard");
        args.push_back("tap");
        args.insert(args.end(), keys.begin(), keys.end());
        return make_command_response(execute_hid_command(hid_command_prefix, args));
    }

    if (path == "/api/keyboard/text") {
        std::string text;
        if (!extract_json_string_field(body, "text", &text) || text.empty()) {
            return make_json_error(400, "missing text");
        }

        std::vector<std::string> args;
        args.push_back("keyboard");
        args.push_back("text");
        args.push_back(text);
        return make_command_response(execute_hid_command(hid_command_prefix, args));
    }

    if (path == "/api/touch/move") {
        int x = 0;
        int y = 0;
        if (!extract_json_int_field(body, "x", &x) || !extract_json_int_field(body, "y", &y)) {
            return make_json_error(400, "missing x or y");
        }

        std::vector<std::string> args;
        args.push_back("touch");
        args.push_back("move");
        args.push_back(std::to_string(x));
        args.push_back(std::to_string(y));
        return make_command_response(execute_hid_command(hid_command_prefix, args));
    }

    if (path == "/api/touch/click") {
        std::string button = "left";
        extract_json_string_field(body, "button", &button);

        int x = 0;
        int y = 0;
        const bool has_x = extract_json_int_field(body, "x", &x);
        const bool has_y = extract_json_int_field(body, "y", &y);
        if (has_x != has_y) {
            return make_json_error(400, "x and y must be provided together");
        }
        if (has_x && has_y) {
            std::vector<std::string> move_args;
            move_args.push_back("touch");
            move_args.push_back("move");
            move_args.push_back(std::to_string(x));
            move_args.push_back(std::to_string(y));
            CommandResult move_result = execute_hid_command(hid_command_prefix, move_args);
            if (move_result.exit_code != 0) {
                return make_command_response(move_result);
            }
        }

        std::vector<std::string> args;
        args.push_back("touch");
        args.push_back("click");
        if (has_x && has_y) {
            args.push_back(std::to_string(x));
            args.push_back(std::to_string(y));
            args.push_back(button);
        } else {
            args.push_back(button);
        }
        return make_command_response(execute_hid_command(hid_command_prefix, args));
    }

    if (path == "/api/touch/scroll") {
        int amount = 0;
        if (!extract_json_int_field(body, "amount", &amount)) {
            return make_json_error(400, "missing amount");
        }

        std::vector<std::string> args;
        args.push_back("touch");
        args.push_back("scroll");
        args.push_back(std::to_string(amount));
        return make_command_response(execute_hid_command(hid_command_prefix, args));
    }

    if (path == "/api/capture") {
        return handle_capture_request(body);
    }

    if (path == "/api/wait_stable") {
        return handle_wait_stable_request(body);
    }

    return make_json_error(400, "unknown endpoint");
}

}  // namespace

void run_hid_server(int port, const std::vector<std::string>& hid_command_prefix) {
    int server_fd = socket(AF_INET, SOCK_STREAM, 0);
    if (server_fd < 0) {
        std::cerr << "Failed to create socket" << std::endl;
        return;
    }

    int opt = 1;
    setsockopt(server_fd, SOL_SOCKET, SO_REUSEADDR, &opt, sizeof(opt));

    sockaddr_in addr;
    std::memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = INADDR_ANY;
    addr.sin_port = htons(static_cast<uint16_t>(port));

    if (bind(server_fd, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)) < 0) {
        std::cerr << "Failed to bind to port " << port << ": "
                  << std::strerror(errno) << std::endl;
        close(server_fd);
        return;
    }

    if (listen(server_fd, 10) < 0) {
        std::cerr << "Failed to listen: " << std::strerror(errno) << std::endl;
        close(server_fd);
        return;
    }

    std::cout << "HID test server running on http://0.0.0.0:" << port << std::endl;
    std::cout << "Press Ctrl+C to stop" << std::endl;

    while (true) {
        sockaddr_in client_addr;
        socklen_t client_len = sizeof(client_addr);
        int client_fd =
            accept(server_fd, reinterpret_cast<sockaddr*>(&client_addr), &client_len);
        if (client_fd < 0) {
            continue;
        }

        HttpRequest req = parse_request(client_fd);

        if (req.method == "GET" && req.path == "/") {
            send_response(client_fd, 200, "OK", "text/html; charset=utf-8", HID_SERVER_HTML);
        } else if (req.method == "GET" &&
                   (req.path == "/screenshot.jpg" || req.path == "/screenshot.bmp")) {
            if (g_screenshot.jpeg.empty()) {
                send_response(client_fd, 404, "Not Found", "text/plain; charset=utf-8",
                              "No screenshot yet");
            } else {
                send_binary_response(client_fd, 200, "OK", "image/jpeg", g_screenshot.jpeg);
            }
        } else if (req.method == "POST" && req.path.compare(0, 5, "/api/") == 0) {
            ApiResponse response = handle_api_request(req.path, req.body, hid_command_prefix);
            send_response(client_fd,
                          response.status_code,
                          response.status_text,
                          "application/json; charset=utf-8",
                          response.body);
        } else {
            send_response(client_fd, 404, "Not Found", "text/plain; charset=utf-8", "Not Found");
        }

        close(client_fd);
    }

    close(server_fd);
}
