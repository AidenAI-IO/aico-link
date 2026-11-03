#pragma once

#include <opencv2/core.hpp>
#include <cstddef>
#include <vector>

namespace aico_image {

/** JPEG (memory, via stb). */
constexpr int kFormatJpeg = 0;
/** Binary P6 PPM (RGB). */
constexpr int kFormatPpm = 1;

constexpr int kSuccess = 0;
constexpr int kErrInvalidArg = -1;
constexpr int kErrEncode = -2;

/**
 * Encode image to memory. Mat must be CV_8UC3, RGB byte order (same as stb JPEG).
 * @param format kFormatJpeg or kFormatPpm
 * @param quality JPEG 1–100 (ignored for PPM)
 */
int encode(const cv::Mat& image, std::vector<unsigned char>& buf, int format, int quality);

/** Crop @p t,@p r,@p b,@p l pixels from top, right, bottom, left. RGB CV_8UC3. */
int crop(const cv::Mat& image, cv::Mat& dst, int t, int r, int b, int l);

/**
 * Trim uniform near-black borders (RGB, CV_8UC3).
 * Rows/columns whose mean grayscale is <= @p black_threshold are treated as border.
 */
int crop_black_bars(const cv::Mat& image, cv::Mat& dst, unsigned char black_threshold = 15);

/** Uniform scale by factor @p s (both dimensions, s > 0). */
int scale(const cv::Mat& image, cv::Mat& dst, float s);

/** Rotate by @p angle_deg (degrees, positive = counter-clockwise). Expands canvas. */
int rotate(const cv::Mat& image, cv::Mat& dst, float angle_deg);

/** Single JPEG encode at fixed quality (1–100). */
int encode_to_jpg(const cv::Mat& image_rgb, std::vector<unsigned char>& buf, int quality);

/**
 * JPEG encode while trying to keep output under @p max_bytes by lowering quality.
 * Starts at @p quality_start and steps down to @p quality_min.
 */
int encode_to_jpg_fit(const cv::Mat& image_rgb,
                      std::vector<unsigned char>& buf,
                      std::size_t max_bytes = 512U * 1024U,
                      int quality_start = 88,
                      int quality_min = 18);

}  // namespace aico_image
