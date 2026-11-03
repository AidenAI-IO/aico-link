#include <algorithm>
#include <cctype>
#include <cerrno>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <dirent.h>
#include <fcntl.h>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <limits.h>
#include <map>
#include <sstream>
#include <stdexcept>
#include <string>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>
#include <vector>

#include "hid_server.h"

namespace {

struct Options {
    std::string gadget_root = "/sys/kernel/config/usb_gadget";
    std::string gadget_name = "aico_hid";
    std::string keyboard_dev = "/dev/hidg0";
    std::string touch_dev = "/dev/hidg1";
    std::string state_dir = "/tmp";
    std::string manufacturer = "Aico";
    std::string product_name = "Aico Link HID Gadget";
    std::string serial = "0001";
    std::string udc;
    int width = 32767;
    int height = 32767;
    int duration_ms = 50;
    int vendor_id = 0x1d6b;
    int product_id = 0x0104;
    int device_bcd = 0x0100;
    int usb_bcd = 0x0200;
    bool force = false;
    bool pointer_touchscreen = false;
};

struct ParsedKey {
    ParsedKey() : modifier(0), keycode(0) {}
    ParsedKey(uint8_t modifier_value, uint8_t keycode_value)
        : modifier(modifier_value), keycode(keycode_value) {}

    uint8_t modifier;
    uint8_t keycode;
};

const uint8_t kKeyboardDescriptor[] = {
    0x05, 0x01,
    0x09, 0x06,
    0xa1, 0x01,
    0x05, 0x07,
    0x19, 0xe0,
    0x29, 0xe7,
    0x15, 0x00,
    0x25, 0x01,
    0x75, 0x01,
    0x95, 0x08,
    0x81, 0x02,
    0x95, 0x01,
    0x75, 0x08,
    0x81, 0x01,
    0x95, 0x06,
    0x75, 0x08,
    0x15, 0x00,
    0x25, 0x65,
    0x05, 0x07,
    0x19, 0x00,
    0x29, 0x65,
    0x81, 0x00,
    0xc0,
};

// Absolute Mouse descriptor - Based on PiKVM implementation
// Testing iOS absolute positioning support
// Reference: https://github.com/pikvm/kvmd/blob/master/kvmd/apps/otg/hid/mouse.py
const uint8_t kTouchDescriptor[] = {
    0x05, 0x01,             // Usage Page (Generic Desktop)
    0x09, 0x02,             // Usage (Mouse)
    0xa1, 0x01,             // Collection (Application)
    0x09, 0x01,             //   Usage (Pointer)
    0xa1, 0x00,             //   Collection (Physical) - Required by Apple
    // 8 Buttons
    0x05, 0x09,             //     Usage Page (Button)
    0x19, 0x01,             //     Usage Minimum (Button 1)
    0x29, 0x08,             //     Usage Maximum (Button 8)
    0x15, 0x00,             //     Logical Minimum (0)
    0x25, 0x01,             //     Logical Maximum (1)
    0x95, 0x08,             //     Report Count (8)
    0x75, 0x01,             //     Report Size (1)
    0x81, 0x02,             //     Input (Data, Variable, Absolute)
    // X and Y - Absolute positioning
    0x05, 0x01,             //     Usage Page (Generic Desktop)
    0x09, 0x30,             //     Usage (X)
    0x09, 0x31,             //     Usage (Y)
    0x16, 0x00, 0x00,       //     Logical Minimum (0)
    0x26, 0xff, 0x7f,       //     Logical Maximum (32767)
    0x75, 0x10,             //     Report Size (16)
    0x95, 0x02,             //     Report Count (2)
    0x81, 0x02,             //     Input (Data, Variable, Absolute)
    // Wheel
    0x09, 0x38,             //     Usage (Wheel)
    0x15, 0x81,             //     Logical Minimum (-127)
    0x25, 0x7f,             //     Logical Maximum (127)
    0x75, 0x08,             //     Report Size (8)
    0x95, 0x01,             //     Report Count (1)
    0x81, 0x06,             //     Input (Data, Variable, Relative)
    0xc0,                   //   End Collection (Physical)
    0xc0,                   // End Collection (Application)
};

const uint8_t kTouchscreenDescriptor[] = {
    0x05, 0x0d,             // Usage Page (Digitizers)
    0x09, 0x04,             // Usage (Touch Screen)
    0xa1, 0x01,             // Collection (Application)
    0x09, 0x22,             //   Usage (Finger)
    0xa1, 0x02,             //   Collection (Logical)
    0x05, 0x0d,             //     Usage Page (Digitizers)
    0x09, 0x42,             //     Usage (Tip Switch)
    0x09, 0x32,             //     Usage (In Range)
    0x15, 0x00,             //     Logical Minimum (0)
    0x25, 0x01,             //     Logical Maximum (1)
    0x75, 0x01,             //     Report Size (1)
    0x95, 0x02,             //     Report Count (2)
    0x81, 0x02,             //     Input (Data,Var,Abs)
    0x75, 0x06,             //     Report Size (6)
    0x95, 0x01,             //     Report Count (1)
    0x81, 0x03,             //     Input (Const,Var,Abs)
    0x09, 0x51,             //     Usage (Contact Identifier)
    0x75, 0x08,             //     Report Size (8)
    0x95, 0x01,             //     Report Count (1)
    0x15, 0x00,             //     Logical Minimum (0)
    0x25, 0x0f,             //     Logical Maximum (15)
    0x81, 0x02,             //     Input (Data,Var,Abs)
    0x05, 0x01,             //     Usage Page (Generic Desktop)
    0x09, 0x30,             //     Usage (X)
    0x09, 0x31,             //     Usage (Y)
    0x16, 0x00, 0x00,       //     Logical Minimum (0)
    0x26, 0xff, 0x7f,       //     Logical Maximum (32767)
    0x75, 0x10,             //     Report Size (16)
    0x95, 0x02,             //     Report Count (2)
    0x81, 0x02,             //     Input (Data,Var,Abs)
    0xc0,                   //   End Collection (Logical)
    0xc0,                   // End Collection (Application)
};

const uint8_t kAndroidExtensionDescriptor[] = {
    0x05, 0x0c,             // Usage Page (Consumer)
    0x09, 0x01,             // Usage (Consumer Control)
    0xa1, 0x01,             // Collection (Application)
    0x15, 0x00,             //   Logical Minimum (0)
    0x26, 0xff, 0x03,       //   Logical Maximum (1023)
    0x1a, 0x00, 0x00,       //   Usage Minimum (0)
    0x2a, 0xff, 0x03,       //   Usage Maximum (1023)
    0x75, 0x10,             //   Report Size (16)
    0x95, 0x01,             //   Report Count (1)
    0x81, 0x00,             //   Input (Data, Ary, Abs)
    0xc0,                   // End Collection (Application)
};

const uint8_t kAbsoluteMediaKeyDescriptor[] = {
    0x05, 0x0c,             // Usage Page (Consumer)
    0x09, 0x01,             // Usage (Consumer Control)
    0xa1, 0x01,             // Collection (Application)
    0x15, 0x00,             //   Logical Minimum (0)
    0x25, 0x01,             //   Logical Maximum (1)
    0x09, 0xe2,             //   Usage (Mute)
    0x09, 0xe9,             //   Usage (Volume Increment)
    0x09, 0xea,             //   Usage (Volume Decrement)
    0x09, 0xcd,             //   Usage (Play/Pause)
    0x09, 0xb7,             //   Usage (Stop)
    0x09, 0xb5,             //   Usage (Scan Next Track)
    0x09, 0xb6,             //   Usage (Scan Previous Track)
    0x09, 0xb4,             //   Usage (Rewind)
    0x09, 0xb3,             //   Usage (Fast Forward)
    0x09, 0x65,             //   Usage (Snapshot)
    0x09, 0x6f,             //   Usage (Brightness Increment)
    0x09, 0x70,             //   Usage (Brightness Decrement)
    0x75, 0x01,             //   Report Size (1)
    0x95, 0x0c,             //   Report Count (12)
    0x81, 0x02,             //   Input (Data, Var, Abs)
    0x75, 0x01,             //   Report Size (1)
    0x95, 0x04,             //   Report Count (4)
    0x81, 0x03,             //   Input (Const, Var, Abs)
    0xc0,                   // End Collection (Application)
};

std::string gadget_path(const Options& options) {
    return options.gadget_root + "/" + options.gadget_name;
}

std::string keyboard_state_path(const Options& options) {
    return options.state_dir + "/" + options.gadget_name + "-keyboard.state";
}

std::string touch_state_path(const Options& options) {
    return options.state_dir + "/" + options.gadget_name + "-touch.state";
}

bool path_exists(const std::string& path) {
    struct stat st;
    return lstat(path.c_str(), &st) == 0;
}

void ensure_dir(const std::string& path, mode_t mode = 0755) {
    if (mkdir(path.c_str(), mode) == 0 || errno == EEXIST) {
        return;
    }
    throw std::runtime_error("mkdir failed for " + path + ": " + std::strerror(errno));
}

void remove_if_exists(const std::string& path) {
    if (!path_exists(path)) {
        return;
    }
    if (unlink(path.c_str()) != 0 && errno != EISDIR) {
        throw std::runtime_error("unlink failed for " + path + ": " + std::strerror(errno));
    }
}

void rmdir_if_exists(const std::string& path) {
    if (!path_exists(path)) {
        return;
    }
    if (rmdir(path.c_str()) != 0) {
        throw std::runtime_error("rmdir failed for " + path + ": " + std::strerror(errno));
    }
}

void write_text_file(const std::string& path, const std::string& value) {
    int fd = open(path.c_str(), O_WRONLY | O_TRUNC);
    if (fd < 0) {
        throw std::runtime_error("open failed for " + path + ": " + std::strerror(errno));
    }
    ssize_t written = write(fd, value.data(), value.size());
    int saved_errno = errno;
    close(fd);
    if (written < 0 || static_cast<size_t>(written) != value.size()) {
        throw std::runtime_error("write failed for " + path + ": " + std::strerror(saved_errno));
    }
}

void write_binary_file(const std::string& path, const uint8_t* data, size_t size) {
    int fd = open(path.c_str(), O_WRONLY | O_TRUNC);
    if (fd < 0) {
        throw std::runtime_error("open failed for " + path + ": " + std::strerror(errno));
    }
    ssize_t written = write(fd, data, size);
    int saved_errno = errno;
    close(fd);
    if (written < 0 || static_cast<size_t>(written) != size) {
        throw std::runtime_error("write failed for " + path + ": " + std::strerror(saved_errno));
    }
}

void write_report(const std::string& path, const std::vector<uint8_t>& report) {
    int fd = open(path.c_str(), O_WRONLY);
    if (fd < 0) {
        throw std::runtime_error("open failed for " + path + ": " + std::strerror(errno));
    }
    ssize_t written = write(fd, report.data(), report.size());
    int saved_errno = errno;
    close(fd);
    if (written < 0 || static_cast<size_t>(written) != report.size()) {
        throw std::runtime_error("write failed for " + path + ": " + std::strerror(saved_errno));
    }
}

std::vector<uint8_t> load_state(const std::string& path, size_t size) {
    std::vector<uint8_t> state(size, 0);
    std::ifstream input(path.c_str(), std::ios::binary);
    if (!input.good()) {
        return state;
    }
    input.read(reinterpret_cast<char*>(state.data()), size);
    return state;
}

void save_state(const std::string& path, const std::vector<uint8_t>& state) {
    std::ofstream output(path.c_str(), std::ios::binary | std::ios::trunc);
    if (!output.good()) {
        throw std::runtime_error("open failed for " + path);
    }
    output.write(reinterpret_cast<const char*>(state.data()), state.size());
}

std::string normalize_key_name(const std::string& key) {
    std::string normalized;
    normalized.reserve(key.size());
    for (size_t i = 0; i < key.size(); ++i) {
        char ch = key[i];
        if (ch == '-' || ch == ' ') {
            normalized.push_back('_');
        } else {
            normalized.push_back(static_cast<char>(std::toupper(static_cast<unsigned char>(ch))));
        }
    }
    return normalized;
}

ParsedKey parse_named_key(const std::string& key) {
    static const std::map<std::string, ParsedKey> keys = {
        {"ENTER", {0x00, 0x28}},
        {"RETURN", {0x00, 0x28}},
        {"ESC", {0x00, 0x29}},
        {"ESCAPE", {0x00, 0x29}},
        {"BACKSPACE", {0x00, 0x2a}},
        {"TAB", {0x00, 0x2b}},
        {"SPACE", {0x00, 0x2c}},
        {"MINUS", {0x00, 0x2d}},
        {"EQUAL", {0x00, 0x2e}},
        {"LEFTBRACE", {0x00, 0x2f}},
        {"RIGHTBRACE", {0x00, 0x30}},
        {"BACKSLASH", {0x00, 0x31}},
        {"SEMICOLON", {0x00, 0x33}},
        {"APOSTROPHE", {0x00, 0x34}},
        {"GRAVE", {0x00, 0x35}},
        {"COMMA", {0x00, 0x36}},
        {"DOT", {0x00, 0x37}},
        {"PERIOD", {0x00, 0x37}},
        {"SLASH", {0x00, 0x38}},
        {"CAPSLOCK", {0x00, 0x39}},
        {"F1", {0x00, 0x3a}},
        {"F2", {0x00, 0x3b}},
        {"F3", {0x00, 0x3c}},
        {"F4", {0x00, 0x3d}},
        {"F5", {0x00, 0x3e}},
        {"F6", {0x00, 0x3f}},
        {"F7", {0x00, 0x40}},
        {"F8", {0x00, 0x41}},
        {"F9", {0x00, 0x42}},
        {"F10", {0x00, 0x43}},
        {"F11", {0x00, 0x44}},
        {"F12", {0x00, 0x45}},
        {"INSERT", {0x00, 0x49}},
        {"HOME", {0x00, 0x4a}},
        {"PAGEUP", {0x00, 0x4b}},
        {"DELETE", {0x00, 0x4c}},
        {"END", {0x00, 0x4d}},
        {"PAGEDOWN", {0x00, 0x4e}},
        {"RIGHT", {0x00, 0x4f}},
        {"LEFT", {0x00, 0x50}},
        {"DOWN", {0x00, 0x51}},
        {"UP", {0x00, 0x52}},
        {"LCTRL", {0x01, 0x00}},
        {"LEFTCTRL", {0x01, 0x00}},
        {"CTRL", {0x01, 0x00}},
        {"RCTRL", {0x10, 0x00}},
        {"RIGHTCTRL", {0x10, 0x00}},
        {"LSHIFT", {0x02, 0x00}},
        {"LEFTSHIFT", {0x02, 0x00}},
        {"SHIFT", {0x02, 0x00}},
        {"RSHIFT", {0x20, 0x00}},
        {"RIGHTSHIFT", {0x20, 0x00}},
        {"LALT", {0x04, 0x00}},
        {"LEFTALT", {0x04, 0x00}},
        {"ALT", {0x04, 0x00}},
        {"RALT", {0x40, 0x00}},
        {"RIGHTALT", {0x40, 0x00}},
        {"LMETA", {0x08, 0x00}},
        {"LGUI", {0x08, 0x00}},
        {"META", {0x08, 0x00}},
        {"GUI", {0x08, 0x00}},
        {"SUPER", {0x08, 0x00}},
        {"RMETA", {0x80, 0x00}},
        {"RGUI", {0x80, 0x00}},
    };

    std::string normalized = normalize_key_name(key);
    std::map<std::string, ParsedKey>::const_iterator it = keys.find(normalized);
    if (it != keys.end()) {
        return it->second;
    }

    if (normalized.size() == 1 && normalized[0] >= 'A' && normalized[0] <= 'Z') {
        return ParsedKey{0x00, static_cast<uint8_t>(0x04 + normalized[0] - 'A')};
    }
    if (normalized.size() == 1 && normalized[0] >= '1' && normalized[0] <= '9') {
        return ParsedKey{0x00, static_cast<uint8_t>(0x1e + normalized[0] - '1')};
    }
    if (normalized == "0") {
        return ParsedKey{0x00, 0x27};
    }

    throw std::runtime_error("unsupported key: " + key);
}

ParsedKey key_from_character(char ch) {
    if (ch >= 'a' && ch <= 'z') {
        return ParsedKey{0x00, static_cast<uint8_t>(0x04 + ch - 'a')};
    }
    if (ch >= 'A' && ch <= 'Z') {
        return ParsedKey{0x02, static_cast<uint8_t>(0x04 + ch - 'A')};
    }
    if (ch >= '1' && ch <= '9') {
        return ParsedKey{0x00, static_cast<uint8_t>(0x1e + ch - '1')};
    }

    switch (ch) {
        case '0': return ParsedKey{0x00, 0x27};
        case '\n': return ParsedKey{0x00, 0x28};
        case '\t': return ParsedKey{0x00, 0x2b};
        case ' ': return ParsedKey{0x00, 0x2c};
        case '-': return ParsedKey{0x00, 0x2d};
        case '_': return ParsedKey{0x02, 0x2d};
        case '=': return ParsedKey{0x00, 0x2e};
        case '+': return ParsedKey{0x02, 0x2e};
        case '[': return ParsedKey{0x00, 0x2f};
        case '{': return ParsedKey{0x02, 0x2f};
        case ']': return ParsedKey{0x00, 0x30};
        case '}': return ParsedKey{0x02, 0x30};
        case '\\': return ParsedKey{0x00, 0x31};
        case '|': return ParsedKey{0x02, 0x31};
        case ';': return ParsedKey{0x00, 0x33};
        case ':': return ParsedKey{0x02, 0x33};
        case '\'': return ParsedKey{0x00, 0x34};
        case '"': return ParsedKey{0x02, 0x34};
        case '`': return ParsedKey{0x00, 0x35};
        case '~': return ParsedKey{0x02, 0x35};
        case ',': return ParsedKey{0x00, 0x36};
        case '<': return ParsedKey{0x02, 0x36};
        case '.': return ParsedKey{0x00, 0x37};
        case '>': return ParsedKey{0x02, 0x37};
        case '/': return ParsedKey{0x00, 0x38};
        case '?': return ParsedKey{0x02, 0x38};
        case '!': return ParsedKey{0x02, 0x1e};
        case '@': return ParsedKey{0x02, 0x1f};
        case '#': return ParsedKey{0x02, 0x20};
        case '$': return ParsedKey{0x02, 0x21};
        case '%': return ParsedKey{0x02, 0x22};
        case '^': return ParsedKey{0x02, 0x23};
        case '&': return ParsedKey{0x02, 0x24};
        case '*': return ParsedKey{0x02, 0x25};
        case '(': return ParsedKey{0x02, 0x26};
        case ')': return ParsedKey{0x02, 0x27};
        default: throw std::runtime_error(std::string("unsupported text character: ") + ch);
    }
}

void apply_key_press(std::vector<uint8_t>& report, const ParsedKey& key) {
    report[0] |= key.modifier;
    if (key.keycode == 0) {
        return;
    }
    for (size_t i = 2; i < report.size(); ++i) {
        if (report[i] == key.keycode) {
            return;
        }
    }
    for (size_t i = 2; i < report.size(); ++i) {
        if (report[i] == 0) {
            report[i] = key.keycode;
            return;
        }
    }
    throw std::runtime_error("keyboard rollover exceeded 6 simultaneous non-modifier keys");
}

void apply_key_release(std::vector<uint8_t>& report, const ParsedKey& key) {
    report[0] &= static_cast<uint8_t>(~key.modifier);
    if (key.keycode == 0) {
        return;
    }
    for (size_t i = 2; i < report.size(); ++i) {
        if (report[i] == key.keycode) {
            report[i] = 0;
        }
    }
    std::vector<uint8_t> compacted(report.size(), 0);
    compacted[0] = report[0];
    size_t index = 2;
    for (size_t i = 2; i < report.size(); ++i) {
        if (report[i] != 0) {
            compacted[index++] = report[i];
        }
    }
    report.swap(compacted);
}

void sleep_ms(int duration_ms) {
    if (duration_ms > 0) {
        usleep(static_cast<useconds_t>(duration_ms) * 1000);
    }
}

int parse_int(const std::string& value) {
    char* end = NULL;
    long parsed = std::strtol(value.c_str(), &end, 0);
    if (end == value.c_str() || *end != '\0') {
        throw std::runtime_error("invalid integer: " + value);
    }
    return static_cast<int>(parsed);
}

std::string first_udc_name() {
    DIR* dir = opendir("/sys/class/udc");
    if (dir == NULL) {
        throw std::runtime_error("unable to open /sys/class/udc");
    }
    struct dirent* entry = NULL;
    while ((entry = readdir(dir)) != NULL) {
        if (entry->d_name[0] == '.') {
            continue;
        }
        std::string name(entry->d_name);
        closedir(dir);
        return name;
    }
    closedir(dir);
    throw std::runtime_error("no UDC controller found under /sys/class/udc");
}

void ensure_symlink(const std::string& target, const std::string& path) {
    if (path_exists(path)) {
        return;
    }
    if (symlink(target.c_str(), path.c_str()) != 0) {
        throw std::runtime_error("symlink failed for " + path + ": " + std::strerror(errno));
    }
}

void setup_keyboard_function(const std::string& gadget) {
    std::string function_path = gadget + "/functions/hid.usb0";
    ensure_dir(function_path);
    write_text_file(function_path + "/protocol", "1");
    write_text_file(function_path + "/subclass", "1");
    write_text_file(function_path + "/report_length", "8");
    write_binary_file(function_path + "/report_desc", kKeyboardDescriptor, sizeof(kKeyboardDescriptor));
    ensure_symlink(function_path, gadget + "/configs/c.1/hid.usb0");
}

void setup_touch_function(const std::string& gadget, const Options& options) {
    std::string function_path = gadget + "/functions/hid.usb1";
    ensure_dir(function_path);
    write_text_file(function_path + "/protocol", "0");
    write_text_file(function_path + "/subclass", "0");
    if (options.pointer_touchscreen) {
        write_text_file(function_path + "/report_length", "6");
        write_binary_file(function_path + "/report_desc", kTouchscreenDescriptor, sizeof(kTouchscreenDescriptor));
    } else {
        write_text_file(function_path + "/report_length", "6");
        write_binary_file(function_path + "/report_desc", kTouchDescriptor, sizeof(kTouchDescriptor));
    }
    ensure_symlink(function_path, gadget + "/configs/c.1/hid.usb1");
}

void setup_android_extension_function(const std::string& gadget, bool pointer_touchscreen) {
    std::string function_path = gadget + "/functions/hid.usb2";
    ensure_dir(function_path);
    write_text_file(function_path + "/protocol", "0");
    write_text_file(function_path + "/subclass", "0");
    write_text_file(function_path + "/report_length", "2");
    if (pointer_touchscreen) {
        write_binary_file(function_path + "/report_desc", kAndroidExtensionDescriptor, sizeof(kAndroidExtensionDescriptor));
    } else {
        write_binary_file(function_path + "/report_desc", kAbsoluteMediaKeyDescriptor, sizeof(kAbsoluteMediaKeyDescriptor));
    }
    ensure_symlink(function_path, gadget + "/configs/c.1/hid.usb2");
}

void cleanup_gadget(const Options& options) {
    std::string gadget = gadget_path(options);
    if (!path_exists(gadget)) {
        return;
    }

    std::string udc_file = gadget + "/UDC";
    if (path_exists(udc_file)) {
        write_text_file(udc_file, "");
    }

    remove_if_exists(gadget + "/configs/c.1/hid.usb0");
    remove_if_exists(gadget + "/configs/c.1/hid.usb1");
    remove_if_exists(gadget + "/configs/c.1/hid.usb2");
    rmdir_if_exists(gadget + "/functions/hid.usb0");
    rmdir_if_exists(gadget + "/functions/hid.usb1");
    rmdir_if_exists(gadget + "/functions/hid.usb2");
    rmdir_if_exists(gadget + "/configs/c.1/strings/0x409");
    rmdir_if_exists(gadget + "/configs/c.1");
    rmdir_if_exists(gadget + "/strings/0x409");
    rmdir_if_exists(gadget);
}

void setup_gadget(const Options& options, const std::string& mode) {
    std::string gadget = gadget_path(options);
    if (path_exists(gadget)) {
        if (!options.force) {
            throw std::runtime_error("gadget already exists at " + gadget + "; use cleanup first or pass --force");
        }
        cleanup_gadget(options);
    }

    ensure_dir(gadget);
    write_text_file(gadget + "/idVendor", [&options]() {
        std::ostringstream out;
        out << "0x" << std::hex << std::setfill('0') << std::setw(4) << options.vendor_id;
        return out.str();
    }());
    write_text_file(gadget + "/idProduct", [&options]() {
        std::ostringstream out;
        out << "0x" << std::hex << std::setfill('0') << std::setw(4) << options.product_id;
        return out.str();
    }());
    write_text_file(gadget + "/bcdDevice", [&options]() {
        std::ostringstream out;
        out << "0x" << std::hex << std::setfill('0') << std::setw(4) << options.device_bcd;
        return out.str();
    }());
    write_text_file(gadget + "/bcdUSB", [&options]() {
        std::ostringstream out;
        out << "0x" << std::hex << std::setfill('0') << std::setw(4) << options.usb_bcd;
        return out.str();
    }());

    ensure_dir(gadget + "/strings/0x409");
    write_text_file(gadget + "/strings/0x409/serialnumber", options.serial);
    write_text_file(gadget + "/strings/0x409/manufacturer", options.manufacturer);
    write_text_file(gadget + "/strings/0x409/product", options.product_name);

    ensure_dir(gadget + "/configs");
    ensure_dir(gadget + "/configs/c.1");
    ensure_dir(gadget + "/configs/c.1/strings");
    ensure_dir(gadget + "/configs/c.1/strings/0x409");
    write_text_file(gadget + "/configs/c.1/MaxPower", "250");
    write_text_file(gadget + "/configs/c.1/strings/0x409/configuration", "Aico Link HID");

    if (mode == "keyboard" || mode == "composite") {
        setup_keyboard_function(gadget);
    }
    if (mode == "keyboard" || mode == "composite") {
        setup_android_extension_function(gadget, options.pointer_touchscreen);
    }
    if (mode == "touch" || mode == "composite") {
        setup_touch_function(gadget, options);
    }

    std::string udc = options.udc.empty() ? first_udc_name() : options.udc;
    write_text_file(gadget + "/UDC", udc);
}

void print_usage() {
    std::cout
        << "Usage:\n"
        << "  example_usb_hid [global options] setup <keyboard|touch|composite>\n"
        << "  example_usb_hid [global options] cleanup\n"
        << "  example_usb_hid [global options] keyboard press <KEY> [KEY ...]\n"
        << "  example_usb_hid [global options] keyboard release [KEY ...]\n"
        << "  example_usb_hid [global options] keyboard tap <KEY> [KEY ...]\n"
        << "  example_usb_hid [global options] keyboard text <TEXT>\n"
        << "  example_usb_hid [global options] touch move <X> <Y>\n"
        << "  example_usb_hid [global options] touch click <X> <Y> [left|right|middle]\n"
        << "  example_usb_hid [global options] touch click [left|right|middle]\n"
        << "  example_usb_hid [global options] touch down [left|right|middle]\n"
        << "  example_usb_hid [global options] touch up\n"
        << "  example_usb_hid [global options] touch scroll <amount>\n"
        << "  example_usb_hid [global options] server [PORT]\n"
        << "\n"
        << "Global options:\n"
        << "  --gadget-root <PATH>\n"
        << "  --gadget-name <NAME>\n"
        << "  --keyboard-dev <PATH>\n"
        << "  --touch-dev <PATH>\n"
        << "  --state-dir <PATH>\n"
        << "  --manufacturer <TEXT>\n"
        << "  --product-name <TEXT>\n"
        << "  --serial <TEXT>\n"
        << "  --vendor <INT>\n"
        << "  --product-id <INT>\n"
        << "  --udc <NAME>\n"
        << "  --width <INT>\n"
        << "  --height <INT>\n"
        << "  --duration-ms <INT>\n"
        << "  --pointer-mode <absolute|touchscreen>\n"
        << "  --force\n"
        << "\n"
        << "Examples:\n"
        << "  example_usb_hid setup composite\n"
        << "  example_usb_hid keyboard tap CTRL ALT DELETE\n"
        << "  example_usb_hid keyboard text \"hello from pico\"\n"
        << "  example_usb_hid touch click 12000 8000\n"
        << std::endl;
}

Options parse_global_options(std::vector<std::string>& args) {
    Options options;
    std::vector<std::string> remaining;

    for (size_t i = 0; i < args.size(); ++i) {
        const std::string& arg = args[i];
        if (arg == "--gadget-root") {
            options.gadget_root = args.at(++i);
        } else if (arg == "--gadget-name") {
            options.gadget_name = args.at(++i);
        } else if (arg == "--keyboard-dev") {
            options.keyboard_dev = args.at(++i);
        } else if (arg == "--touch-dev") {
            options.touch_dev = args.at(++i);
        } else if (arg == "--state-dir") {
            options.state_dir = args.at(++i);
        } else if (arg == "--manufacturer") {
            options.manufacturer = args.at(++i);
        } else if (arg == "--product-name") {
            options.product_name = args.at(++i);
        } else if (arg == "--serial") {
            options.serial = args.at(++i);
        } else if (arg == "--vendor") {
            options.vendor_id = parse_int(args.at(++i));
        } else if (arg == "--product-id") {
            options.product_id = parse_int(args.at(++i));
        } else if (arg == "--udc") {
            options.udc = args.at(++i);
        } else if (arg == "--width") {
            options.width = parse_int(args.at(++i));
        } else if (arg == "--height") {
            options.height = parse_int(args.at(++i));
        } else if (arg == "--duration-ms") {
            options.duration_ms = parse_int(args.at(++i));
        } else if (arg == "--pointer-mode") {
            const std::string mode = args.at(++i);
            if (mode == "touchscreen") {
                options.pointer_touchscreen = true;
            } else if (mode == "absolute") {
                options.pointer_touchscreen = false;
            } else {
                throw std::runtime_error("pointer-mode must be absolute or touchscreen");
            }
        } else if (arg == "--force") {
            options.force = true;
        } else if (arg == "--help" || arg == "-h") {
            print_usage();
            std::exit(0);
        } else if (arg.rfind("--", 0) == 0) {
            throw std::runtime_error("unknown option: " + arg);
        } else {
            remaining.push_back(arg);
        }
    }

    const char* env_mode = getenv("AICO_POINTER_MODE");
    if (env_mode != nullptr) {
        if (std::string(env_mode) == "touchscreen") {
            options.pointer_touchscreen = true;
        } else if (std::string(env_mode) == "absolute") {
            options.pointer_touchscreen = false;
        }
    }

    args.swap(remaining);
    return options;
}

std::string current_executable_path(const char* argv0) {
    char path[PATH_MAX];
    ssize_t len = readlink("/proc/self/exe", path, sizeof(path) - 1);
    if (len > 0) {
        path[len] = '\0';
        return path;
    }
    return argv0 ? argv0 : "example_usb_hid";
}

std::vector<std::string> build_server_command_prefix(const Options& options,
                                                     const std::string& executable_path) {
    std::vector<std::string> command;
    command.push_back(executable_path);
    command.push_back("--gadget-root");
    command.push_back(options.gadget_root);
    command.push_back("--gadget-name");
    command.push_back(options.gadget_name);
    command.push_back("--keyboard-dev");
    command.push_back(options.keyboard_dev);
    command.push_back("--touch-dev");
    command.push_back(options.touch_dev);
    command.push_back("--state-dir");
    command.push_back(options.state_dir);
    command.push_back("--manufacturer");
    command.push_back(options.manufacturer);
    command.push_back("--product-name");
    command.push_back(options.product_name);
    command.push_back("--serial");
    command.push_back(options.serial);
    command.push_back("--vendor");
    command.push_back(std::to_string(options.vendor_id));
    command.push_back("--product-id");
    command.push_back(std::to_string(options.product_id));
    if (!options.udc.empty()) {
        command.push_back("--udc");
        command.push_back(options.udc);
    }
    command.push_back("--width");
    command.push_back(std::to_string(options.width));
    command.push_back("--height");
    command.push_back(std::to_string(options.height));
    command.push_back("--duration-ms");
    command.push_back(std::to_string(options.duration_ms));
    if (options.force) {
        command.push_back("--force");
    }
    return command;
}

void handle_keyboard(const Options& options, const std::vector<std::string>& args) {
    if (args.size() < 2) {
        throw std::runtime_error("keyboard command requires an action");
    }

    const std::string& action = args[1];
    std::vector<uint8_t> state = load_state(keyboard_state_path(options), 8);

    if (action == "press") {
        if (args.size() < 3) {
            throw std::runtime_error("keyboard press requires at least one key");
        }
        for (size_t i = 2; i < args.size(); ++i) {
            apply_key_press(state, parse_named_key(args[i]));
        }
        write_report(options.keyboard_dev, state);
        save_state(keyboard_state_path(options), state);
        return;
    }

    if (action == "release") {
        if (args.size() == 2) {
            std::fill(state.begin(), state.end(), 0);
        } else {
            for (size_t i = 2; i < args.size(); ++i) {
                apply_key_release(state, parse_named_key(args[i]));
            }
        }
        write_report(options.keyboard_dev, state);
        save_state(keyboard_state_path(options), state);
        return;
    }

    if (action == "tap") {
        if (args.size() < 3) {
            throw std::runtime_error("keyboard tap requires at least one key");
        }
        std::vector<uint8_t> original = state;
        for (size_t i = 2; i < args.size(); ++i) {
            apply_key_press(state, parse_named_key(args[i]));
        }
        write_report(options.keyboard_dev, state);
        save_state(keyboard_state_path(options), state);
        sleep_ms(options.duration_ms);
        write_report(options.keyboard_dev, original);
        save_state(keyboard_state_path(options), original);
        return;
    }

    if (action == "text") {
        if (args.size() != 3) {
            throw std::runtime_error("keyboard text requires exactly one string argument");
        }
        std::vector<uint8_t> original = state;
        for (size_t i = 0; i < args[2].size(); ++i) {
            std::vector<uint8_t> report = original;
            ParsedKey key = key_from_character(args[2][i]);
            apply_key_press(report, key);
            write_report(options.keyboard_dev, report);
            sleep_ms(options.duration_ms);
            write_report(options.keyboard_dev, original);
            sleep_ms(10);
        }
        save_state(keyboard_state_path(options), original);
        return;
    }

    throw std::runtime_error("unknown keyboard action: " + action);
}

int clamp_coordinate(int value, int maximum) {
    if (value < 0) {
        return 0;
    }
    if (value > maximum) {
        return maximum;
    }
    return value;
}

bool try_parse_int_arg(const std::string& text, int* value) {
    char* end = nullptr;
    errno = 0;
    long parsed = std::strtol(text.c_str(), &end, 0);
    if (errno != 0 || !end || *end != '\0') {
        return false;
    }
    if (value) {
        *value = static_cast<int>(parsed);
    }
    return true;
}

int parse_mouse_button(const std::string& arg) {
    if (arg == "left") return 0x01;
    if (arg == "right") return 0x02;
    if (arg == "middle") return 0x04;
    return parse_int(arg);
}

int clamp_int(int value, int min_value, int max_value) {
    if (value < min_value) {
        return min_value;
    }
    if (value > max_value) {
        return max_value;
    }
    return value;
}

void load_pointer_xy(const Options& options, int* x, int* y) {
    std::vector<uint8_t> state = load_state(touch_state_path(options), 4);
    if (state.size() >= 4) {
        *x = static_cast<int>(state[0]) | (static_cast<int>(state[1]) << 8);
        *y = static_cast<int>(state[2]) | (static_cast<int>(state[3]) << 8);
        return;
    }
    *x = options.width / 2;
    *y = options.height / 2;
}

void save_pointer_xy(const Options& options, int x, int y) {
    std::vector<uint8_t> state(4, 0);
    state[0] = static_cast<uint8_t>(x & 0xff);
    state[1] = static_cast<uint8_t>((x >> 8) & 0xff);
    state[2] = static_cast<uint8_t>(y & 0xff);
    state[3] = static_cast<uint8_t>((y >> 8) & 0xff);
    save_state(touch_state_path(options), state);
}

void send_touchscreen_report(const Options& options, bool touching, int x, int y) {
    x = clamp_coordinate(x, options.width);
    y = clamp_coordinate(y, options.height);
    std::vector<uint8_t> report(6, 0);
    report[0] = touching ? 0x03 : 0x00;
    report[1] = 0x01;
    report[2] = static_cast<uint8_t>(x & 0xff);
    report[3] = static_cast<uint8_t>((x >> 8) & 0xff);
    report[4] = static_cast<uint8_t>(y & 0xff);
    report[5] = static_cast<uint8_t>((y >> 8) & 0xff);
    write_report(options.touch_dev, report);
    save_pointer_xy(options, x, y);
}

// Send absolute mouse report: [buttons][X_lo][X_hi][Y_lo][Y_hi][wheel]
void send_mouse_report(const Options& options, uint8_t buttons, int x, int y, int wheel) {
    if (options.pointer_touchscreen) {
        send_touchscreen_report(options, buttons != 0, x, y);
        return;
    }

    x = clamp_coordinate(x, options.width);
    y = clamp_coordinate(y, options.height);
    std::vector<uint8_t> report(6, 0);
    report[0] = buttons;
    report[1] = static_cast<uint8_t>(x & 0xff);
    report[2] = static_cast<uint8_t>((x >> 8) & 0xff);
    report[3] = static_cast<uint8_t>(y & 0xff);
    report[4] = static_cast<uint8_t>((y >> 8) & 0xff);
    report[5] = static_cast<uint8_t>(static_cast<int8_t>(std::max(-127, std::min(127, wheel))));
    write_report(options.touch_dev, report);
    save_pointer_xy(options, x, y);
}

void send_mouse_report_at_current_position(const Options& options, uint8_t buttons, int wheel) {
    int x = 0;
    int y = 0;
    load_pointer_xy(options, &x, &y);
    send_mouse_report(options, buttons, x, y, wheel);
}

void handle_touch(const Options& options, const std::vector<std::string>& args) {
    if (args.size() < 2) {
        throw std::runtime_error("touch command requires an action");
    }

    const std::string& action = args[1];

    if (action == "move") {
        if (args.size() != 4) {
            throw std::runtime_error("touch move requires X and Y (0-32767)");
        }
        send_mouse_report(options, 0x00, parse_int(args[2]), parse_int(args[3]), 0);
        return;
    }

    if (action == "click") {
        int button = 0x01;
        int x = 0;
        int y = 0;
        bool has_coords = false;

        if (args.size() == 3) {
            button = parse_mouse_button(args[2]);
        } else if (args.size() == 4 || args.size() == 5) {
            if (!try_parse_int_arg(args[2], &x) || !try_parse_int_arg(args[3], &y)) {
                throw std::runtime_error(
                    "touch click requires either [button] or <X> <Y> [left|right|middle]");
            }
            has_coords = true;
            if (args.size() == 5) {
                button = parse_mouse_button(args[4]);
            }
        } else if (args.size() > 5) {
            throw std::runtime_error(
                "touch click requires either [button] or <X> <Y> [left|right|middle]");
        }

        if (has_coords) {
            send_mouse_report(options, 0x00, x, y, 0);
        }

        if (has_coords) {
            send_mouse_report(options, button, x, y, 0);
        } else {
            send_mouse_report_at_current_position(options, button, 0);
        }
        sleep_ms(options.duration_ms);
        if (has_coords) {
            send_mouse_report(options, 0x00, x, y, 0);
        } else {
            send_mouse_report_at_current_position(options, 0x00, 0);
        }
        return;
    }

    if (action == "down") {
        int button = 0x01;
        if (args.size() >= 3) {
            button = parse_mouse_button(args[2]);
        }
        send_mouse_report_at_current_position(options, button, 0);
        return;
    }

    if (action == "up") {
        send_mouse_report_at_current_position(options, 0x00, 0);
        return;
    }

    if (action == "scroll") {
        if (args.size() != 3) {
            throw std::runtime_error("touch scroll requires amount (-127 to 127)");
        }
        send_mouse_report_at_current_position(options, 0x00, parse_int(args[2]));
        return;
    }

    throw std::runtime_error("unknown touch action: " + action);
}

}  // namespace

int main(int argc, char** argv) {
    try {
        std::vector<std::string> args;
        for (int i = 1; i < argc; ++i) {
            args.push_back(argv[i]);
        }

        Options options = parse_global_options(args);
        if (args.empty()) {
            print_usage();
            return 1;
        }

        const std::string& command = args[0];
        if (command == "setup") {
            if (args.size() != 2) {
                throw std::runtime_error("setup requires one of: keyboard, touch, composite");
            }
            if (args[1] != "keyboard" && args[1] != "touch" && args[1] != "composite") {
                throw std::runtime_error("unsupported setup mode: " + args[1]);
            }
            setup_gadget(options, args[1]);
            std::cout << "Configured gadget '" << options.gadget_name << "'" << std::endl;
            return 0;
        }

        if (command == "cleanup") {
            cleanup_gadget(options);
            std::cout << "Removed gadget '" << options.gadget_name << "'" << std::endl;
            return 0;
        }

        if (command == "keyboard") {
            handle_keyboard(options, args);
            return 0;
        }

        if (command == "touch") {
            handle_touch(options, args);
            return 0;
        }

        if (command == "server") {
            int port = 8000;
            if (args.size() >= 2) {
                port = parse_int(args[1]);
            }
            const std::string self = current_executable_path(argv[0]);
            run_hid_server(port, build_server_command_prefix(options, self));
            return 0;
        }

        throw std::runtime_error("unknown command: " + command);
    } catch (const std::exception& error) {
        std::cerr << "error: " << error.what() << std::endl;
        return 1;
    }
}
