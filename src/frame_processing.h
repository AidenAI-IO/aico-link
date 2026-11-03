#pragma once

#include "frame_service_protocol.h"
#include <cstdint>
#include <vector>

namespace aico {

bool convert_frame_to_rgb(const FrameMetadata& metadata,
                          const std::vector<uint8_t>& frame,
                          std::vector<uint8_t>* rgb);

// Crop a supported raw frame to a centered rectangle derived from known screen
// geometry. The crop is aligned to complete chroma samples.
bool crop_frame_center(const FrameMetadata& metadata,
                       const std::vector<uint8_t>& frame,
                       uint32_t target_width,
                       uint32_t target_height,
                       FrameMetadata* cropped_metadata,
                       std::vector<uint8_t>* cropped_frame);

// Resolve the centered crop metadata without copying frame pixels.
bool resolve_frame_center_crop(const FrameMetadata& metadata,
                               const std::vector<uint8_t>& frame,
                               uint32_t target_width,
                               uint32_t target_height,
                               FrameMetadata* cropped_metadata);

// Crop to a known screen aspect ratio while deriving the current orientation
// from black bars in this frame. screen_width/screen_height provide only the
// aspect ratio; their ordering is not treated as a cached orientation.
bool crop_frame_center_aspect_auto(const FrameMetadata& metadata,
                                   const std::vector<uint8_t>& frame,
                                   uint32_t screen_width,
                                   uint32_t screen_height,
                                   FrameMetadata* cropped_metadata,
                                   std::vector<uint8_t>* cropped_frame);

// Resolve the current-orientation aspect crop without copying frame pixels.
bool resolve_frame_center_aspect_auto_crop(const FrameMetadata& metadata,
                                           const std::vector<uint8_t>& frame,
                                           uint32_t screen_width,
                                           uint32_t screen_height,
                                           FrameMetadata* cropped_metadata);

// Backward-compatible horizontal-only wrapper.
bool crop_frame_horizontal_center(const FrameMetadata& metadata,
                                   const std::vector<uint8_t>& frame,
                                   uint32_t target_width,
                                   FrameMetadata* cropped_metadata,
                                   std::vector<uint8_t>* cropped_frame);

// Detect centered black bars on either the horizontal or vertical axis and
// crop them while keeping the other source dimension intact.
bool crop_frame_black_bars(const FrameMetadata& metadata,
                           const std::vector<uint8_t>& frame,
                           FrameMetadata* cropped_metadata,
                           std::vector<uint8_t>* cropped_frame);

// Resolve detected horizontal or vertical black bars without copying pixels.
bool resolve_frame_black_bars_crop(const FrameMetadata& metadata,
                                   const std::vector<uint8_t>& frame,
                                   FrameMetadata* cropped_metadata);

// Backward-compatible wrapper retaining the legacy minimal-width behavior.
bool crop_frame_horizontal_black_bars(const FrameMetadata& metadata,
                                      const std::vector<uint8_t>& frame,
                                      uint32_t minimal_width,
                                      FrameMetadata* cropped_metadata,
                                      std::vector<uint8_t>* cropped_frame,
                                      bool crop_by_aspect = false);

bool encode_rgb_to_bmp(const std::vector<uint8_t>& rgb,
                       uint32_t width,
                       uint32_t height,
                       std::vector<uint8_t>* bmp);

bool encode_frame_to_bmp(const FrameMetadata& metadata,
                         const std::vector<uint8_t>& frame,
                         std::vector<uint8_t>* bmp);

bool encode_rgb_to_png(const std::vector<uint8_t>& rgb,
                       uint32_t width,
                       uint32_t height,
                       std::vector<uint8_t>* png);

bool encode_frame_to_png(const FrameMetadata& metadata,
                         const std::vector<uint8_t>& frame,
                         std::vector<uint8_t>* png);

bool scale_rgb_nearest(const std::vector<uint8_t>& rgb,
                       uint32_t src_width,
                       uint32_t src_height,
                       uint32_t dst_width,
                       uint32_t dst_height,
                       std::vector<uint8_t>* scaled);

}  // namespace aico
