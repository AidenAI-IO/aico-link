#include "frame_service_server.h"
#include "aico_log.h"
#include "frame_jpeg_encoder.h"
#include "frame_processing.h"
#include "cJSON/cJSON.h"
#include "uds_message.h"
#include <chrono>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/types.h>

namespace aico {

FrameServiceServer::FrameServiceServer(const char* socket_path, size_t ring_capacity)
    : uds_server_(new UdsServer(socket_path ? socket_path : "",
                                [this](const UdsMessage& request, int fd) {
                                    handle_request(request, fd);
                                })),
      ring_(ring_capacity),
      // The IPC endpoint is ready before the capture source has a frame.  A
      // caller must not mistake that short (or long, when HDMI is absent)
      // period for a healthy capture stream.
      state_("STARTING"),
      running_(false),
      on_demand_latest_seq_(0),
      on_demand_capture_ts_ns_(0),
      avg_frame_serve_latency_ms_(0.0),
      serve_latency_samples_(0),
      avg_capture_copy_latency_ms_(0.0),
      capture_copy_latency_samples_(0),
      consecutive_failures_(0),
      last_recovery_ts_(0),
      capture_request_count_(0),
      active_payload_sends_(0),
      max_payload_sends_(1) {}

static uint64_t monotonic_ns() {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return static_cast<uint64_t>(ts.tv_sec) * 1000000000ULL + static_cast<uint64_t>(ts.tv_nsec);
}

struct PeerIdentity {
    PeerIdentity() : pid(-1), uid(-1), gid(-1), process_name("unknown") {}
    long pid;
    long uid;
    long gid;
    std::string process_name;
};

static PeerIdentity peer_identity(int fd) {
    PeerIdentity identity;
#if defined(__linux__) && defined(SO_PEERCRED)
    struct LinuxPeerCredentials {
        pid_t pid;
        uid_t uid;
        gid_t gid;
    } credentials;
    memset(&credentials, 0, sizeof(credentials));
    socklen_t length = sizeof(credentials);
    if (getsockopt(fd, SOL_SOCKET, SO_PEERCRED, &credentials, &length) == 0 &&
        length == sizeof(credentials)) {
        identity.pid = static_cast<long>(credentials.pid);
        identity.uid = static_cast<long>(credentials.uid);
        identity.gid = static_cast<long>(credentials.gid);

        char path[64];
        snprintf(path, sizeof(path), "/proc/%ld/comm", identity.pid);
        FILE* comm = fopen(path, "r");
        if (comm) {
            char name[128] = {};
            if (fgets(name, sizeof(name), comm)) {
                name[strcspn(name, "\r\n")] = '\0';
                if (name[0] != '\0') {
                    identity.process_name = name;
                }
            }
            fclose(comm);
        }
    }
#else
    (void)fd;
#endif
    return identity;
}

FrameServiceServer::~FrameServiceServer() {
    stop();
}

static std::string u64_json(uint64_t value) {
    char buf[32];
    snprintf(buf, sizeof(buf), "\"%llu\"", static_cast<unsigned long long>(value));
    return buf;
}

static std::string escape_json(const std::string& text) {
    std::string out;
    for (size_t i = 0; i < text.size(); ++i) {
        char c = text[i];
        switch (c) {
        case '\\':
            out += "\\\\";
            break;
        case '"':
            out += "\\\"";
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
            if (static_cast<unsigned char>(c) < 0x20) {
                char buf[8];
                snprintf(buf, sizeof(buf), "\\u%04x", static_cast<unsigned char>(c));
                out += buf;
            } else {
                out.push_back(c);
            }
        }
    }
    return out;
}

static std::string frame_metadata_json(const FrameMetadata& metadata) {
    std::string json = "{\"seq\":" + u64_json(metadata.seq) +
                       ",\"capture_ts_ns\":" + u64_json(metadata.capture_ts_ns) +
                       ",\"width\":" + std::to_string(metadata.width) +
                       ",\"height\":" + std::to_string(metadata.height) +
                       ",\"pixel_format\":\"" + escape_json(metadata.pixel_format) + "\"" +
                       ",\"stride\":" + std::to_string(metadata.stride) +
                       ",\"bytes\":" + u64_json(metadata.bytes) +
                       ",\"stale\":" + (metadata.stale ? "true" : "false");
    if (!metadata.planes.empty()) {
        json += ",\"planes\":[";
        for (size_t i = 0; i < metadata.planes.size(); ++i) {
            if (i > 0) json += ",";
            json += "{\"offset\":" + std::to_string(metadata.planes[i].offset) +
                    ",\"stride\":" + std::to_string(metadata.planes[i].stride) +
                    ",\"bytes\":" + std::to_string(metadata.planes[i].bytes) + "}";
        }
        json += "]";
    }
    if (metadata.source_width > 0 && metadata.source_height > 0) {
        json += ",\"source_width\":" + std::to_string(metadata.source_width) +
                ",\"source_height\":" + std::to_string(metadata.source_height);
    }
    if (metadata.crop_width > 0 && metadata.crop_height > 0) {
        json += ",\"crop_x\":" + std::to_string(metadata.crop_x) +
                ",\"crop_y\":" + std::to_string(metadata.crop_y) +
                ",\"crop_width\":" + std::to_string(metadata.crop_width) +
                ",\"crop_height\":" + std::to_string(metadata.crop_height);
    }
    json += "}";
    return json;
}

static uint64_t json_u64(cJSON* object, const char* key) {
    cJSON* item = cJSON_GetObjectItem(object, key);
    if (!item) return 0;
    if (item->type == cJSON_String && item->valuestring) {
        return strtoull(item->valuestring, nullptr, 10);
    }
    if (item->type == cJSON_Number) {
        return static_cast<uint64_t>(item->valuedouble);
    }
    return 0;
}

static uint32_t json_u32(cJSON* object, const char* key) {
    return static_cast<uint32_t>(json_u64(object, key));
}

static std::string json_string(cJSON* object, const char* key) {
    cJSON* item = cJSON_GetObjectItem(object, key);
    if (!item || item->type != cJSON_String || !item->valuestring) return std::string();
    return item->valuestring;
}

static bool json_bool(cJSON* object, const char* key) {
    cJSON* item = cJSON_GetObjectItem(object, key);
    return item && item->type == cJSON_True;
}

static std::string status_response(const char* method, FrameServiceStatus status) {
    return std::string("{\"type\":\"error\",\"method\":\"") + method +
           "\",\"status\":\"" + frame_service_status_to_string(status) + "\"}";
}

FrameServiceStatus FrameServiceServer::start() {
    std::lock_guard<std::mutex> lock(mutex_);
    if (running_) {
        return FrameServiceStatus::OK;
    }

    FrameServiceStatus status = uds_server_->start();
    if (status != FrameServiceStatus::OK) {
        return status;
    }
    running_ = true;
    return FrameServiceStatus::OK;
}

void FrameServiceServer::stop() {
    bool was_running = false;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        was_running = running_;
        running_ = false;
    }
    if (was_running) {
        frame_cv_.notify_all();
        payload_cv_.notify_all();
    }
    uds_server_->stop();
}

FrameServiceStatus FrameServiceServer::append_frame(const FrameMetadata& metadata,
                                                    const uint8_t* data,
                                                    size_t bytes,
                                                    uint64_t* seq_out) {
    uint64_t started_ns = monotonic_ns();
    FrameServiceStatus status = ring_.append_frame(metadata, data, bytes, seq_out);
    if (status == FrameServiceStatus::OK) {
        record_capture_copy_latency(started_ns);
        {
            std::lock_guard<std::mutex> lock(mutex_);
            state_ = "RUNNING";
            consecutive_failures_ = 0;
        }
        frame_cv_.notify_all();
    }
    return status;
}

void FrameServiceServer::set_state(const std::string& state) {
    {
        std::lock_guard<std::mutex> lock(mutex_);
        state_ = state;
    }
    frame_cv_.notify_all();
}

void FrameServiceServer::set_capture_handler(const CaptureHandler& handler) {
    std::lock_guard<std::mutex> lock(mutex_);
    capture_handler_ = handler;
}

void FrameServiceServer::set_restart_handler(const std::function<void()>& handler) {
    std::lock_guard<std::mutex> lock(mutex_);
    restart_handler_ = handler;
}

void FrameServiceServer::record_recovery(const std::string& error, bool count_failure) {
    {
        std::lock_guard<std::mutex> lock(mutex_);
        state_ = "RECOVERING";
        last_recovery_ts_ = monotonic_ns();
        if (count_failure) {
            ++consecutive_failures_;
            last_error_ = error;
        }
    }
    frame_cv_.notify_all();
}

bool FrameServiceServer::is_recovering() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return state_ == "RECOVERING";
}

uint64_t FrameServiceServer::frame_age_ms() const {
    uint64_t on_demand_capture_ts_ns = 0;
    bool on_demand = false;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        on_demand = static_cast<bool>(capture_handler_);
        on_demand_capture_ts_ns = on_demand_capture_ts_ns_;
    }
    if (on_demand) {
        if (on_demand_capture_ts_ns == 0) {
            return 0;
        }
        const uint64_t now = monotonic_ns();
        return now > on_demand_capture_ts_ns
            ? (now - on_demand_capture_ts_ns) / 1000000ULL
            : 0;
    }

    std::shared_ptr<const FrameBufferFrame> frame;
    if (ring_.latest_frame_ref(0, &frame) != FrameServiceStatus::OK || frame->metadata.capture_ts_ns == 0) {
        return 0;
    }
    uint64_t now = monotonic_ns();
    if (now <= frame->metadata.capture_ts_ns) {
        return 0;
    }
    return (now - frame->metadata.capture_ts_ns) / 1000000ULL;
}

void FrameServiceServer::record_serve_latency(uint64_t started_ns) {
    uint64_t elapsed_ns = monotonic_ns() - started_ns;
    double elapsed_ms = static_cast<double>(elapsed_ns) / 1000000.0;
    std::lock_guard<std::mutex> lock(mutex_);
    if (serve_latency_samples_ < 128) {
        avg_frame_serve_latency_ms_ =
            ((avg_frame_serve_latency_ms_ * serve_latency_samples_) + elapsed_ms) /
            static_cast<double>(serve_latency_samples_ + 1);
        ++serve_latency_samples_;
    } else {
        avg_frame_serve_latency_ms_ = (avg_frame_serve_latency_ms_ * 127.0 + elapsed_ms) / 128.0;
    }
}

void FrameServiceServer::record_capture_copy_latency(uint64_t started_ns) {
    uint64_t elapsed_ns = monotonic_ns() - started_ns;
    double elapsed_ms = static_cast<double>(elapsed_ns) / 1000000.0;
    std::lock_guard<std::mutex> lock(mutex_);
    if (capture_copy_latency_samples_ < 128) {
        avg_capture_copy_latency_ms_ =
            ((avg_capture_copy_latency_ms_ * capture_copy_latency_samples_) + elapsed_ms) /
            static_cast<double>(capture_copy_latency_samples_ + 1);
        ++capture_copy_latency_samples_;
    } else {
        avg_capture_copy_latency_ms_ = (avg_capture_copy_latency_ms_ * 127.0 + elapsed_ms) / 128.0;
    }
}

double FrameServiceServer::avg_frame_serve_latency_ms() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return avg_frame_serve_latency_ms_;
}

bool FrameServiceServer::acquire_payload_send_slot() {
    std::unique_lock<std::mutex> lock(mutex_);
    payload_cv_.wait(lock, [&]() {
        return !running_ || active_payload_sends_ < max_payload_sends_;
    });
    if (!running_) {
        return false;
    }
    ++active_payload_sends_;
    return true;
}

void FrameServiceServer::release_payload_send_slot() {
    {
        std::lock_guard<std::mutex> lock(mutex_);
        if (active_payload_sends_ > 0) {
            --active_payload_sends_;
        }
    }
    payload_cv_.notify_one();
}

FrameServiceStatus FrameServiceServer::write_payload_message(int fd,
                                                             const std::string& header,
                                                             const std::vector<uint8_t>& payload) {
    if (payload.empty()) {
        return write_uds_message(fd, header, payload);
    }
    if (!acquire_payload_send_slot()) {
        return FrameServiceStatus::TRANSPORT_ERROR;
    }
    FrameServiceStatus status = write_uds_message(fd, header, payload);
    release_payload_send_slot();
    return status;
}

void FrameServiceServer::handle_request(const UdsMessage& request, int fd) {
    cJSON* root = cJSON_Parse(request.header_json.c_str());
    if (!root) {
        write_uds_message(fd, status_response("unknown", FrameServiceStatus::TRANSPORT_ERROR), std::vector<uint8_t>());
        return;
    }

    std::string method = json_string(root, "method");
    if (method == "health") {
        std::string state;
        uint32_t consecutive_failures = 0;
        std::string last_error;
        uint64_t last_recovery_ts = 0;
        uint64_t latest_seq = 0;
        bool on_demand = false;
        double avg_capture_copy_latency_ms = 0.0;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            state = state_;
            consecutive_failures = consecutive_failures_;
            last_error = last_error_;
            last_recovery_ts = last_recovery_ts_;
            on_demand = static_cast<bool>(capture_handler_);
            latest_seq = on_demand ? on_demand_latest_seq_ : ring_.latest_seq();
            avg_capture_copy_latency_ms = avg_capture_copy_latency_ms_;
        }
        std::string header = "{\"type\":\"response\",\"method\":\"health\",\"status\":\"OK\"";
        header += ",\"state\":\"" + escape_json(state) + "\"";
        header += ",\"capture_mode\":\"" + std::string(on_demand ? "on_demand" : "buffered") + "\"";
        header += ",\"latest_seq\":" + std::to_string(latest_seq);
        header += ",\"frame_age_ms\":" + std::to_string(frame_age_ms());
        header += ",\"ring_buffer_size\":" + std::to_string(on_demand ? 0 : ring_.capacity());
        header += ",\"ring_buffer_used\":" + std::to_string(on_demand ? 0 : ring_.size());
        header += ",\"consecutive_failures\":" + std::to_string(consecutive_failures);
        header += ",\"last_error\":\"" + escape_json(last_error) + "\"";
        header += ",\"last_recovery_ts\":" + u64_json(last_recovery_ts);
        header += ",\"avg_frame_serve_latency_ms\":" + std::to_string(avg_frame_serve_latency_ms());
        header += ",\"avg_capture_copy_latency_ms\":" + std::to_string(avg_capture_copy_latency_ms);
        header += "}";
        write_uds_message(fd, header, std::vector<uint8_t>());
    } else if (method == "latest_frame") {
        const PeerIdentity peer = peer_identity(fd);
        uint64_t since_seq = json_u64(root, "since_seq");
        uint32_t timeout_ms = json_u32(root, "timeout_ms");
        std::string format = json_string(root, "format");
        int quality = static_cast<int>(json_u32(root, "quality"));
        uint64_t requested_minimal_width = json_u64(root, "minimal_width");
        uint64_t requested_screen_width = json_u64(root, "screen_width");
        uint64_t requested_screen_height = json_u64(root, "screen_height");
        if (quality <= 0) {
            quality = 80;
        }
        if (format.empty()) {
            format = "raw";
        }
        bool crop_black = json_bool(root, "crop_black");

        std::shared_ptr<const FrameBufferFrame> frame;
        bool recovering = false;
        CaptureHandler capture_handler;
        uint64_t request_id = 0;
        std::string request_state;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            capture_handler = capture_handler_;
            request_id = ++capture_request_count_;
            request_state = state_;
        }
        AICO_LOG_INFO("server", "capture_request_received",
                       "request_id=%llu peer_pid=%ld peer_uid=%ld peer_gid=%ld peer_name=%s state=%s capture_mode=%s timeout_ms=%u format=%s",
                       static_cast<unsigned long long>(request_id),
                       peer.pid, peer.uid, peer.gid, peer.process_name.c_str(),
                       request_state.c_str(),
                       capture_handler ? "on_demand" : "buffered",
                       timeout_ms, format.c_str());
        const uint64_t request_started_ns = monotonic_ns();
        FrameServiceStatus status = FrameServiceStatus::NO_NEW_FRAME;
        if (capture_handler) {
            FrameMetadata metadata;
            std::vector<uint8_t> data;
            const uint64_t capture_started_ns = monotonic_ns();
            status = capture_handler(timeout_ms, &metadata, &data);
            if (status == FrameServiceStatus::OK) {
                record_capture_copy_latency(capture_started_ns);
                std::shared_ptr<FrameBufferFrame> captured(new FrameBufferFrame());
                captured->metadata = metadata;
                captured->metadata.bytes = data.size();
                {
                    std::lock_guard<std::mutex> lock(mutex_);
                    captured->metadata.seq = ++on_demand_latest_seq_;
                    on_demand_capture_ts_ns_ = captured->metadata.capture_ts_ns;
                    consecutive_failures_ = 0;
                }
                captured->data.swap(data);
                frame = captured;
            }
        } else {
            recovering = is_recovering();
            status = recovering ? ring_.latest_frame_ref(0, &frame)
                                : ring_.latest_frame_ref(since_seq, &frame);
            if (recovering && status == FrameServiceStatus::NO_NEW_FRAME) {
                status = FrameServiceStatus::SERVICE_RECOVERING;
            }
            if (status == FrameServiceStatus::NO_NEW_FRAME && timeout_ms > 0) {
                std::unique_lock<std::mutex> lock(mutex_);
                frame_cv_.wait_for(lock, std::chrono::milliseconds(timeout_ms), [&]() {
                    return !running_ || state_ == "RECOVERING" || ring_.latest_seq() > since_seq;
                });
                recovering = state_ == "RECOVERING";
                status = recovering ? ring_.latest_frame_ref(0, &frame)
                                    : ring_.latest_frame_ref(since_seq, &frame);
                if (status == FrameServiceStatus::NO_NEW_FRAME) {
                    status = FrameServiceStatus::TIMEOUT;
                }
            }
        }
        const uint64_t request_capture_elapsed_ms =
            (monotonic_ns() - request_started_ns) / 1000000ULL;
        if (status == FrameServiceStatus::OK) {
            AICO_LOG_INFO("server", "capture_request_resolved",
                           "request_id=%llu status=%s capture_elapsed_ms=%llu bytes=%zu",
                           static_cast<unsigned long long>(request_id),
                           frame_service_status_to_string(status),
                           static_cast<unsigned long long>(request_capture_elapsed_ms),
                           frame ? frame->data.size() : 0U);
        } else {
            AICO_LOG_WARN("server", "capture_request_resolved",
                           "request_id=%llu status=%s capture_elapsed_ms=%llu bytes=0",
                           static_cast<unsigned long long>(request_id),
                           frame_service_status_to_string(status),
                           static_cast<unsigned long long>(request_capture_elapsed_ms));
        }
        if (status == FrameServiceStatus::OK) {
            uint64_t started_ns = monotonic_ns();
            FrameMetadata metadata = frame->metadata;
            if (recovering) {
                metadata.stale = true;
            }
            const uint32_t minimal_width = requested_minimal_width > metadata.width
                                               ? metadata.width
                                               : static_cast<uint32_t>(requested_minimal_width);
            const uint32_t screen_width = requested_screen_width > UINT32_MAX
                                              ? 0
                                              : static_cast<uint32_t>(requested_screen_width);
            const uint32_t screen_height = requested_screen_height > UINT32_MAX
                                               ? 0
                                               : static_cast<uint32_t>(requested_screen_height);

            std::vector<uint8_t> transformed_payload;
            const std::vector<uint8_t>* payload = &frame->data;
            FrameMetadata transformed_metadata = metadata;
            std::vector<uint8_t> cropped_raw_payload;
            enum class RequestedCropMode { kAspect, kCentered, kBlackBars };
            const RequestedCropMode crop_mode =
                screen_width > 0 && screen_height > 0
                    ? RequestedCropMode::kAspect
                    : minimal_width > 0 ? RequestedCropMode::kCentered
                                         : RequestedCropMode::kBlackBars;
            const auto resolve_requested_crop = [&]() -> bool {
                if (crop_mode == RequestedCropMode::kAspect) {
                    return resolve_frame_center_aspect_auto_crop(
                        metadata, frame->data, screen_width, screen_height,
                        &transformed_metadata);
                }
                if (crop_mode == RequestedCropMode::kCentered) {
                    return resolve_frame_center_crop(metadata, frame->data, minimal_width,
                                                     metadata.height, &transformed_metadata);
                }
                return resolve_frame_black_bars_crop(metadata, frame->data,
                                                     &transformed_metadata);
            };
            const auto materialize_requested_crop = [&]() -> bool {
                if (crop_mode == RequestedCropMode::kAspect) {
                    return crop_frame_center_aspect_auto(
                        metadata, frame->data, screen_width, screen_height,
                        &transformed_metadata, &cropped_raw_payload);
                }
                if (crop_mode == RequestedCropMode::kCentered) {
                    return crop_frame_horizontal_center(metadata, frame->data, minimal_width,
                                                        &transformed_metadata,
                                                        &cropped_raw_payload);
                }
                return crop_frame_black_bars(metadata, frame->data,
                                             &transformed_metadata,
                                             &cropped_raw_payload);
            };

            if (crop_black && format == "jpeg") {
                if (!resolve_requested_crop()) {
                    crop_black = false;
                    transformed_metadata = metadata;
                }
            } else if (crop_black && format == "raw") {
                if (!materialize_requested_crop()) {
                    crop_black = false;
                    transformed_metadata = metadata;
                } else {
                    payload = &cropped_raw_payload;
                }
            }
            if (format == "jpeg") {
                const uint32_t source_width = metadata.width;
                const uint32_t source_height = metadata.height;
                uint32_t encoded_width = 0, encoded_height = 0;
                uint32_t crop_x = 0, crop_y = 0;
                const uint32_t venc_crop_x = crop_black ? transformed_metadata.crop_x : 0;
                const uint32_t venc_crop_y = crop_black ? transformed_metadata.crop_y : 0;
                const uint32_t venc_crop_width = crop_black
                                                     ? transformed_metadata.crop_width
                                                     : metadata.width;
                const uint32_t venc_crop_height = crop_black
                                                      ? transformed_metadata.crop_height
                                                      : metadata.height;
                const bool venc_encoded =
                    (metadata.pixel_format == "uyvy" || metadata.pixel_format == "yuyv") &&
                    encode_yuv_to_jpeg_hw_with_crop(
                        frame->data, metadata.width, metadata.height,
                        metadata.pixel_format, quality,
                        venc_crop_x, venc_crop_y, venc_crop_width, venc_crop_height,
                        &transformed_payload);
                if (venc_encoded) {
                    encoded_width = venc_crop_width;
                    encoded_height = venc_crop_height;
                    crop_x = venc_crop_x;
                    crop_y = venc_crop_y;
                } else {
                    if (crop_black) {
                        if (!materialize_requested_crop()) {
                            crop_black = false;
                            transformed_metadata = metadata;
                            payload = &frame->data;
                        } else {
                            payload = &cropped_raw_payload;
                        }
                    }
                    uint32_t encoder_crop_x = 0, encoder_crop_y = 0;
                    if (!encode_yuv_to_jpeg_hw(*payload, transformed_metadata, quality,
                                               &transformed_payload,
                                               &encoded_width, &encoded_height,
                                               &encoder_crop_x, &encoder_crop_y,
                                               0, false)) {
                        write_uds_message(fd, status_response("latest_frame", FrameServiceStatus::INTERNAL_ERROR), std::vector<uint8_t>());
                        cJSON_Delete(root);
                        return;
                    }
                    crop_x = encoder_crop_x;
                    crop_y = encoder_crop_y;
                }
                payload = &transformed_payload;
                metadata.source_width = source_width;
                metadata.source_height = source_height;
                metadata.crop_x = crop_x;
                metadata.crop_y = crop_y;
                metadata.crop_width = encoded_width;
                metadata.crop_height = encoded_height;
                metadata.width = encoded_width;
                metadata.height = encoded_height;
                metadata.stride = 0;
                metadata.planes.clear();
                metadata.pixel_format = "jpeg";
                metadata.bytes = payload->size();
            } else if (format == "raw" && crop_black) {
                metadata = transformed_metadata;
            }

            std::string header = "{\"type\":\"response\",\"method\":\"latest_frame\",\"status\":\"OK\",\"frame\":" +
                                 frame_metadata_json(metadata) + "}";
            if (write_payload_message(fd, header, *payload) == FrameServiceStatus::OK) {
                record_serve_latency(started_ns);
            }
        } else {
            write_uds_message(fd, status_response("latest_frame", status), std::vector<uint8_t>());
        }
    } else if (method == "get_frame") {
        uint64_t seq = json_u64(root, "seq");
        std::shared_ptr<const FrameBufferFrame> frame;
        bool on_demand = false;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            on_demand = static_cast<bool>(capture_handler_);
        }
        FrameServiceStatus status = on_demand
            ? FrameServiceStatus::FRAME_NOT_FOUND
            : ring_.get_frame_ref(seq, &frame);
        if (status == FrameServiceStatus::OK) {
            uint64_t started_ns = monotonic_ns();
            std::string header = "{\"type\":\"response\",\"method\":\"get_frame\",\"status\":\"OK\",\"frame\":" +
                                 frame_metadata_json(frame->metadata) + "}";
            if (write_payload_message(fd, header, frame->data) == FrameServiceStatus::OK) {
                record_serve_latency(started_ns);
            }
        } else {
            write_uds_message(fd, status_response("get_frame", status), std::vector<uint8_t>());
        }
    } else if (method == "list_frames") {
        uint32_t count = json_u32(root, "count");
        bool on_demand = false;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            on_demand = static_cast<bool>(capture_handler_);
        }
        std::vector<FrameMetadata> frames = on_demand
            ? std::vector<FrameMetadata>()
            : ring_.list_frames(count);
        std::string header = "{\"type\":\"response\",\"method\":\"list_frames\",\"status\":\"OK\",\"frames\":[";
        for (size_t i = 0; i < frames.size(); ++i) {
            if (i > 0) header += ",";
            header += frame_metadata_json(frames[i]);
        }
        header += "]}";
        write_uds_message(fd, header, std::vector<uint8_t>());
    } else if (method == "restart") {
        const PeerIdentity peer = peer_identity(fd);
        AICO_LOG_WARN("server", "restart_requested",
                       "peer_pid=%ld peer_uid=%ld peer_gid=%ld peer_name=%s",
                       peer.pid, peer.uid, peer.gid, peer.process_name.c_str());
        std::function<void()> handler;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            handler = restart_handler_;
        }
        if (handler) {
            handler();
        }
        write_uds_message(fd, "{\"type\":\"response\",\"method\":\"restart\",\"status\":\"OK\"}", std::vector<uint8_t>());
    } else {
        write_uds_message(fd, status_response(method.empty() ? "unknown" : method.c_str(), FrameServiceStatus::INTERNAL_ERROR), std::vector<uint8_t>());
    }

    cJSON_Delete(root);
}

}  // namespace aico
