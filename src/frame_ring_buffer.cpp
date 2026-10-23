#include "frame_ring_buffer.h"

namespace aico {

FrameRingBuffer::FrameRingBuffer(size_t capacity)
    : capacity_(capacity), next_seq_(1) {}

size_t FrameRingBuffer::capacity() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return capacity_;
}

size_t FrameRingBuffer::size() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return frames_.size();
}

uint64_t FrameRingBuffer::latest_seq() const {
    std::lock_guard<std::mutex> lock(mutex_);
    if (frames_.empty()) {
        return 0;
    }
    return frames_.back()->metadata.seq;
}

FrameServiceStatus FrameRingBuffer::append_frame(const FrameMetadata& metadata,
                                                 const uint8_t* data,
                                                 size_t bytes,
                                                 uint64_t* seq_out) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (capacity_ == 0 || (!data && bytes > 0)) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }

    std::shared_ptr<FrameBufferFrame> frame(new FrameBufferFrame());
    frame->metadata = metadata;
    frame->metadata.seq = next_seq_++;
    frame->metadata.bytes = bytes;
    if (bytes > 0) {
        frame->data.assign(data, data + bytes);
    }

    if (frames_.size() == capacity_) {
        frames_.erase(frames_.begin());
    }
    frames_.push_back(frame);

    if (seq_out) {
        *seq_out = frame->metadata.seq;
    }
    return FrameServiceStatus::OK;
}

FrameServiceStatus FrameRingBuffer::latest_frame(uint64_t since_seq, FrameBufferFrame* out) const {
    std::lock_guard<std::mutex> lock(mutex_);
    if (frames_.empty()) {
        return FrameServiceStatus::NO_NEW_FRAME;
    }
    if (frames_.back()->metadata.seq <= since_seq) {
        return FrameServiceStatus::NO_NEW_FRAME;
    }
    if (!out) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }
    *out = *frames_.back();
    return FrameServiceStatus::OK;
}

FrameServiceStatus FrameRingBuffer::latest_frame_ref(uint64_t since_seq,
                                                     std::shared_ptr<const FrameBufferFrame>* out) const {
    std::lock_guard<std::mutex> lock(mutex_);
    if (frames_.empty()) {
        return FrameServiceStatus::NO_NEW_FRAME;
    }
    if (frames_.back()->metadata.seq <= since_seq) {
        return FrameServiceStatus::NO_NEW_FRAME;
    }
    if (!out) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }
    *out = frames_.back();
    return FrameServiceStatus::OK;
}

FrameServiceStatus FrameRingBuffer::get_frame(uint64_t seq, FrameBufferFrame* out) const {
    std::lock_guard<std::mutex> lock(mutex_);
    if (!out) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }
    for (size_t i = 0; i < frames_.size(); ++i) {
        if (frames_[i]->metadata.seq == seq) {
            *out = *frames_[i];
            return FrameServiceStatus::OK;
        }
    }
    return FrameServiceStatus::FRAME_NOT_FOUND;
}

FrameServiceStatus FrameRingBuffer::get_frame_ref(uint64_t seq,
                                                  std::shared_ptr<const FrameBufferFrame>* out) const {
    std::lock_guard<std::mutex> lock(mutex_);
    if (!out) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }
    for (size_t i = 0; i < frames_.size(); ++i) {
        if (frames_[i]->metadata.seq == seq) {
            *out = frames_[i];
            return FrameServiceStatus::OK;
        }
    }
    return FrameServiceStatus::FRAME_NOT_FOUND;
}

std::vector<FrameMetadata> FrameRingBuffer::list_frames(size_t count) const {
    std::lock_guard<std::mutex> lock(mutex_);
    std::vector<FrameMetadata> out;
    if (frames_.empty()) {
        return out;
    }

    size_t n = count == 0 || count > frames_.size() ? frames_.size() : count;
    out.reserve(n);
    size_t start = frames_.size() - n;
    for (size_t i = start; i < frames_.size(); ++i) {
        out.push_back(frames_[i]->metadata);
    }
    return out;
}

}  // namespace aico
