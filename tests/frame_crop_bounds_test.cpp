#include "doctest.h"
#include "frame_crop_bounds.h"

TEST_CASE("minimal width includes the centered screen interval after asymmetric detection") {
    int left = 756;
    int right = 1263;

    aico::include_centered_minimal_width(1920, 608, &left, &right);

    CHECK(left == 656);
    CHECK(right == 1263);
}

TEST_CASE("minimal width preserves detected content outside the centered interval") {
    int left = 500;
    int right = 1400;

    aico::include_centered_minimal_width(1920, 608, &left, &right);

    CHECK(left == 500);
    CHECK(right == 1400);
}

TEST_CASE("minimal width is clamped to the source width") {
    int left = 700;
    int right = 1200;

    aico::include_centered_minimal_width(1920, 3000, &left, &right);

    CHECK(left == 0);
    CHECK(right == 1919);
}
