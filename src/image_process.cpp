#include "image_process.h"

#include <opencv2/imgproc.hpp>

#include <algorithm>
#include <cmath>
#include <cstring>
#include <sstream>
#include <string>

#define STB_IMAGE_WRITE_IMPLEMENTATION
#include "stb_image_write.h"

namespace aico_image {
namespace {

static void append_stbi(void* context, void* data, int size) {
    auto* out = static_cast<std::vector<unsigned char>*>(context);
    const auto* p = static_cast<const unsigned char*>(data);
    out->insert(out->end(), p, p + static_cast<std::size_t>(size));
}

bool is_valid_rgb8(const cv::Mat& image) {
    return !image.empty() && image.type() == CV_8UC3 && image.cols > 0 && image.rows > 0;
}

}  // namespace

int encode(const cv::Mat& image, std::vector<unsigned char>& buf, int format, int quality) {
    buf.clear();
    if (!is_valid_rgb8(image)) {
        return kErrInvalidArg;
    }
    cv::Mat contiguous = image.isContinuous() ? image : image.clone();

    if (format == kFormatPpm) {
        std::ostringstream header;
        header << "P6\n" << contiguous.cols << ' ' << contiguous.rows << "\n255\n";
        const std::string h = header.str();
        buf.assign(h.begin(), h.end());
        const std::size_t pixels =
            static_cast<std::size_t>(contiguous.rows) * static_cast<std::size_t>(contiguous.cols) * 3U;
        buf.resize(buf.size() + pixels);
        std::memcpy(buf.data() + buf.size() - pixels, contiguous.ptr<unsigned char>(0), pixels);
        return kSuccess;
    }

    if (format == kFormatJpeg) {
        int q = quality;
        if (q < 1) {
            q = 1;
        }
        if (q > 100) {
            q = 100;
        }
        if (!stbi_write_jpg_to_func(append_stbi,
                                    &buf,
                                    contiguous.cols,
                                    contiguous.rows,
                                    3,
                                    contiguous.ptr<void>(0),
                                    q)) {
            return kErrEncode;
        }
        return kSuccess;
    }

    return kErrInvalidArg;
}

int crop(const cv::Mat& image, cv::Mat& dst, int t, int r, int b, int l) {
    if (!is_valid_rgb8(image) || t < 0 || r < 0 || b < 0 || l < 0) {
        return kErrInvalidArg;
    }
    const int x1 = l;
    const int y1 = t;
    const int x2 = image.cols - r;
    const int y2 = image.rows - b;
    if (x2 <= x1 || y2 <= y1) {
        return kErrInvalidArg;
    }
    cv::Rect roi(x1, y1, x2 - x1, y2 - y1);
    image(roi).copyTo(dst);
    return kSuccess;
}

int crop_black_bars(const cv::Mat& image, cv::Mat& dst, unsigned char black_threshold) {
    if (!is_valid_rgb8(image)) {
        return kErrInvalidArg;
    }
    cv::Mat gray;
    cv::cvtColor(image, gray, cv::COLOR_RGB2GRAY);

    const double stddev_limit = 6.0;

    auto is_border_row = [&](int row) -> bool {
        cv::Scalar m, s;
        cv::meanStdDev(gray.row(row), m, s);
        return m[0] <= static_cast<double>(black_threshold) && s[0] <= stddev_limit;
    };

    auto is_border_col = [&](int col) -> bool {
        cv::Scalar m, s;
        cv::meanStdDev(gray.col(col), m, s);
        return m[0] <= static_cast<double>(black_threshold) && s[0] <= stddev_limit;
    };

    int top = 0;
    for (; top < gray.rows; ++top) {
        if (!is_border_row(top)) break;
    }

    int bottom = gray.rows - 1;
    for (; bottom >= top; --bottom) {
        if (!is_border_row(bottom)) break;
    }

    int left = 0;
    for (; left < gray.cols; ++left) {
        if (!is_border_col(left)) break;
    }

    int right = gray.cols - 1;
    for (; right >= left; --right) {
        if (!is_border_col(right)) break;
    }

    if (right < left || bottom < top) {
        image.copyTo(dst);
        return kSuccess;
    }

    cv::Rect roi(left, top, right - left + 1, bottom - top + 1);
    image(roi).copyTo(dst);
    return kSuccess;
}

int scale(const cv::Mat& image, cv::Mat& dst, float s) {
    if (!is_valid_rgb8(image) || !(s > 0.0f)) {
        return kErrInvalidArg;
    }
    int new_w = static_cast<int>(std::lround(static_cast<float>(image.cols) * s));
    int new_h = static_cast<int>(std::lround(static_cast<float>(image.rows) * s));
    if (new_w < 1) {
        new_w = 1;
    }
    if (new_h < 1) {
        new_h = 1;
    }
    cv::resize(image, dst, cv::Size(new_w, new_h), 0.0, 0.0, cv::INTER_AREA);
    return kSuccess;
}

int rotate(const cv::Mat& image, cv::Mat& dst, float angle_deg) {
    if (!is_valid_rgb8(image)) {
        return kErrInvalidArg;
    }
    const cv::Point2f center(static_cast<float>(image.cols) * 0.5f,
                             static_cast<float>(image.rows) * 0.5f);
    cv::Mat rot_mat = cv::getRotationMatrix2D(center, static_cast<double>(angle_deg), 1.0);
    const cv::Rect bbox =
        cv::RotatedRect(center, image.size(), static_cast<float>(angle_deg)).boundingRect();

    rot_mat.at<double>(0, 2) += static_cast<double>(bbox.width) * 0.5 - static_cast<double>(center.x);
    rot_mat.at<double>(1, 2) += static_cast<double>(bbox.height) * 0.5 - static_cast<double>(center.y);

    cv::warpAffine(image,
                   dst,
                   rot_mat,
                   bbox.size(),
                   cv::INTER_LINEAR,
                   cv::BORDER_CONSTANT,
                   cv::Scalar(0, 0, 0));
    return kSuccess;
}

int encode_to_jpg(const cv::Mat& image_rgb, std::vector<unsigned char>& buf, int quality) {
    return encode(image_rgb, buf, kFormatJpeg, quality);
}

int encode_to_jpg_fit(const cv::Mat& image_rgb,
                      std::vector<unsigned char>& buf,
                      std::size_t max_bytes,
                      int quality_start,
                      int quality_min) {
    buf.clear();
    if (!is_valid_rgb8(image_rgb)) {
        return kErrInvalidArg;
    }
    if (quality_start < quality_min) {
        std::swap(quality_start, quality_min);
    }
    for (int q = quality_start; q >= quality_min; --q) {
        buf.clear();
        const int rc = encode_to_jpg(image_rgb, buf, q);
        if (rc != kSuccess) {
            return rc;
        }
        if (buf.size() <= max_bytes) {
            return kSuccess;
        }
    }
    return kSuccess;
}

}  // namespace aico_image
