#include "doctest.h"
#include "image_process.h"

#include <opencv2/core.hpp>

TEST_CASE("crop removes margins") {
    cv::Mat img(100, 100, CV_8UC3, cv::Scalar(200, 100, 50));
    cv::Mat out;
    REQUIRE(aico_image::crop(img, out, 10, 10, 10, 10) == aico_image::kSuccess);
    CHECK(out.cols == 80);
    CHECK(out.rows == 80);
}

TEST_CASE("crop_black_bars removes uniform border") {
    cv::Mat img = cv::Mat::zeros(80, 80, CV_8UC3);
    const cv::Rect inner(20, 20, 40, 40);
    img(inner) = cv::Scalar(200, 200, 200);
    cv::Mat out;
    REQUIRE(aico_image::crop_black_bars(img, out) == aico_image::kSuccess);
    CHECK(out.rows == 40);
    CHECK(out.cols == 40);
}

TEST_CASE("scale halves dimensions") {
    cv::Mat img(100, 100, CV_8UC3, cv::Scalar(1, 2, 3));
    cv::Mat out;
    REQUIRE(aico_image::scale(img, out, 0.5f) == aico_image::kSuccess);
    CHECK(out.cols == 50);
    CHECK(out.rows == 50);
}

TEST_CASE("rotate expands canvas") {
    cv::Mat img(20, 10, CV_8UC3, cv::Scalar(255, 0, 0));
    cv::Mat out;
    REQUIRE(aico_image::rotate(img, out, 45.f) == aico_image::kSuccess);
    CHECK(out.rows > 20);
    CHECK(out.cols > 10);
}

TEST_CASE("encode PPM and JPEG") {
    cv::Mat img(4, 4, CV_8UC3, cv::Scalar(10, 50, 100));
    std::vector<unsigned char> ppm;
    REQUIRE(aico_image::encode(img, ppm, aico_image::kFormatPpm, 0) == aico_image::kSuccess);
    REQUIRE(ppm.size() >= 3);
    CHECK(ppm[0] == 'P');
    CHECK(ppm[1] == '6');

    std::vector<unsigned char> jpg;
    REQUIRE(aico_image::encode(img, jpg, aico_image::kFormatJpeg, 90) == aico_image::kSuccess);
    REQUIRE(jpg.size() >= 2);
    CHECK(jpg[0] == 0xff);
    CHECK(jpg[1] == 0xd8);
}

TEST_CASE("encode_to_jpg_fit respects size budget") {
    cv::Mat img(320, 240, CV_8UC3, cv::Scalar(30, 120, 200));
    std::vector<unsigned char> jpg;
    REQUIRE(aico_image::encode_to_jpg_fit(img, jpg, 64U * 1024U) == aico_image::kSuccess);
    CHECK_FALSE(jpg.empty());
    CHECK(jpg.size() <= 64U * 1024U);
}
