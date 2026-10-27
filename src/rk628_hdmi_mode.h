#pragma once

#include <string>

namespace aico {

// Apply the persistent RK628 profile once before opening capture. Reading the
// current EDID avoids an HPD pulse when a service restart keeps the same mode.
bool prepare_rk628_hdmi_mode(
    int fd, const std::string& mode_file, const std::string& edid_directory,
    std::string* selected_edid,
    int (*ioctl_fn)(int, unsigned long, void*) = nullptr);

}  // namespace aico
