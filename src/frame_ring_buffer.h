#pragma once

#include "frame_service_protocol.h"
#include <cstddef>
#include <cstdint>
#include <memory>
#include <mutex>
#include <vector>

namespace aico {

struct FrameBufferFrame {
    FrameMetadata metadata;
    std::vector<uint8_t> data;
};

class FrameRingBuffer {
public:
    explicit FrameRingBuffer(size_t capacity);

    size_t capacity() const;
    size_t size() const;
    uint64_t latest_seq() const;

    FrameServiceStatus append_frame(const FrameMetadata& metadata,
                                    const uint8_t* data,
                                    size_t bytes,
                                    uint64_t* seq_out);
    FrameServiceStatus latest_frame(uint64_t since_seq, FrameBufferFrame* out) const;
    FrameServiceStatus latest_frame_ref(uint64_t since_seq, std::shared_ptr<const FrameBufferFrame>* out) const;
    FrameServiceStatus get_frame(uint64_t seq, FrameBufferFrame* out) const;
    FrameServiceStatus get_frame_ref(uint64_t seq, std::shared_ptr<const FrameBufferFrame>* out) const;
    std::vector<FrameMetadata> list_frames(size_t count) const;

private:
    size_t capacity_;
    uint64_t next_seq_;
    std::vector<std::shared_ptr<const FrameBufferFrame> > frames_;
    mutable std::mutex mutex_;
};

}  // namespace aico
