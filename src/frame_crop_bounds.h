#pragma once

#include <algorithm>
#include <cstdint>

namespace aico {

inline void include_centered_minimal_width(int source_width,
                                           uint32_t minimal_width,
                                           int* left,
                                           int* right) {
    if (source_width <= 0 || !left || !right || minimal_width == 0) {
        return;
    }

    const int required_width = static_cast<int>(
        std::min<uint32_t>(minimal_width, static_cast<uint32_t>(source_width)));
    const int centered_left = (source_width - required_width) / 2;
    const int centered_right = centered_left + required_width - 1;
    *left = std::min(*left, centered_left);
    *right = std::max(*right, centered_right);
}

}  // namespace aico
