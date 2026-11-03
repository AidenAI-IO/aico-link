#include "image_process.h"

#include <opencv2/core.hpp>

#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#define STB_IMAGE_IMPLEMENTATION
#include "stb_image.h"

namespace {

void print_usage() {
    std::fprintf(stderr,
                 "Usage:\n"
                 "  image_process crop -b T,R,B,L -i INPUT -o OUTPUT [-q 1-100 for JPEG]\n"
                 "  image_process crop_black_bars -i INPUT -o OUTPUT [-q JPEG quality]\n"
                 "  image_process scale -s FACTOR -i INPUT -o OUTPUT [-q JPEG quality]\n"
                 "  image_process rotate -d DEG -i INPUT -o OUTPUT [-q JPEG quality]\n"
                 "Input: PPM (P6) or formats supported by stb_image (JPEG, PNG, ...).\n"
                 "Output format from extension: .jpg .jpeg .ppm\n");
}

bool ends_with(const std::string& s, const char* suf) {
    const std::size_t n = std::strlen(suf);
    return s.size() >= n && s.compare(s.size() - n, n, suf) == 0;
}

bool read_next_int(std::FILE* fp, int* value) {
    if (!fp || !value) {
        return false;
    }

    int ch = 0;
    for (;;) {
        ch = std::fgetc(fp);
        if (ch == EOF) {
            return false;
        }
        if (ch == '#') {
            while ((ch = std::fgetc(fp)) != '\n' && ch != EOF) {}
            continue;
        }
        if (std::isspace(static_cast<unsigned char>(ch))) {
            continue;
        }
        std::ungetc(ch, fp);
        break;
    }

    return std::fscanf(fp, "%d", value) == 1;
}

int output_format_from_path(const std::string& path, int* format) {
    if (ends_with(path, ".ppm") || ends_with(path, ".PPM")) {
        *format = aico_image::kFormatPpm;
        return 0;
    }
    if (ends_with(path, ".jpg") || ends_with(path, ".JPG") || ends_with(path, ".jpeg") ||
        ends_with(path, ".JPEG")) {
        *format = aico_image::kFormatJpeg;
        return 0;
    }
    return -1;
}

bool read_ppm_p6(const std::string& path, cv::Mat* out) {
    std::FILE* fp = std::fopen(path.c_str(), "rb");
    if (!fp) {
        return false;
    }
    char sig[3] = {};
    if (std::fread(sig, 1, 2, fp) != 2 || sig[0] != 'P' || sig[1] != '6') {
        std::fclose(fp);
        return false;
    }

    int width = 0;
    int height = 0;
    int maxval = 0;
    if (!read_next_int(fp, &width) || !read_next_int(fp, &height) ||
        !read_next_int(fp, &maxval) || width <= 0 || height <= 0 || maxval != 255) {
        std::fclose(fp);
        return false;
    }
    int ch = std::fgetc(fp);
    if (ch == EOF) {
        std::fclose(fp);
        return false;
    }
    if (!std::isspace(static_cast<unsigned char>(ch))) {
        std::ungetc(ch, fp);
    }

    std::vector<unsigned char> data(static_cast<std::size_t>(width) * static_cast<std::size_t>(height) * 3U);
    const std::size_t nread = std::fread(data.data(), 1, data.size(), fp);
    std::fclose(fp);
    if (nread != data.size()) {
        return false;
    }
    *out = cv::Mat(height, width, CV_8UC3, data.data()).clone();
    return true;
}

bool load_image(const std::string& path, cv::Mat* out) {
    if (ends_with(path, ".ppm") || ends_with(path, ".PPM")) {
        return read_ppm_p6(path, out);
    }
    int w = 0;
    int h = 0;
    int comp = 0;
    unsigned char* data = stbi_load(path.c_str(), &w, &h, &comp, 3);
    if (!data || w <= 0 || h <= 0) {
        if (data) {
            stbi_image_free(data);
        }
        return false;
    }
    cv::Mat rgb(h, w, CV_8UC3, data);
    rgb.copyTo(*out);
    stbi_image_free(data);
    return true;
}

bool write_output(const std::string& path, const cv::Mat& rgb, int jpeg_quality) {
    int fmt = 0;
    if (output_format_from_path(path, &fmt) != 0) {
        std::fprintf(stderr, "Unsupported output extension (use .ppm or .jpg)\n");
        return false;
    }
    std::vector<unsigned char> buf;
    const int rc = aico_image::encode(rgb, buf, fmt, jpeg_quality);
    if (rc != aico_image::kSuccess) {
        std::fprintf(stderr, "encode failed (%d)\n", rc);
        return false;
    }
    std::FILE* fp = std::fopen(path.c_str(), "wb");
    if (!fp) {
        std::perror(path.c_str());
        return false;
    }
    const std::size_t n = std::fwrite(buf.data(), 1, buf.size(), fp);
    std::fclose(fp);
    return n == buf.size();
}

struct ParsedArgs {
    std::string cmd;
    std::string input;
    std::string output;
    int crop_t = 0;
    int crop_r = 0;
    int crop_b = 0;
    int crop_l = 0;
    float scale = 1.0f;
    float rotate_deg = 0.0f;
    int quality = 85;
};

bool parse_int_csv(const char* s, int* t, int* r, int* b, int* l) {
    return std::sscanf(s, "%d,%d,%d,%d", t, r, b, l) == 4;
}

bool parse_args(int argc, char** argv, ParsedArgs* a) {
    if (argc < 2) {
        return false;
    }
    a->cmd = argv[1];
    for (int i = 2; i < argc; ++i) {
        if (std::strcmp(argv[i], "-i") == 0 && i + 1 < argc) {
            a->input = argv[++i];
        } else if (std::strcmp(argv[i], "-o") == 0 && i + 1 < argc) {
            a->output = argv[++i];
        } else if (std::strcmp(argv[i], "-q") == 0 && i + 1 < argc) {
            a->quality = std::atoi(argv[++i]);
        } else if (std::strcmp(argv[i], "-b") == 0 && i + 1 < argc) {
            if (!parse_int_csv(argv[++i], &a->crop_t, &a->crop_r, &a->crop_b, &a->crop_l)) {
                return false;
            }
        } else if (std::strcmp(argv[i], "-s") == 0 && i + 1 < argc) {
            a->scale = static_cast<float>(std::atof(argv[++i]));
        } else if (std::strcmp(argv[i], "-d") == 0 && i + 1 < argc) {
            a->rotate_deg = static_cast<float>(std::atof(argv[++i]));
        } else {
            return false;
        }
    }
    return !a->input.empty() && !a->output.empty();
}

}  // namespace

int main(int argc, char** argv) {
    ParsedArgs args;
    if (!parse_args(argc, argv, &args)) {
        print_usage();
        return 1;
    }

    cv::Mat img;
    if (!load_image(args.input, &img)) {
        std::fprintf(stderr, "Failed to load: %s\n", args.input.c_str());
        return 1;
    }

    cv::Mat work;
    int rc = aico_image::kSuccess;

    if (args.cmd == "crop") {
        rc = aico_image::crop(img, work, args.crop_t, args.crop_r, args.crop_b, args.crop_l);
    } else if (args.cmd == "crop_black_bars") {
        rc = aico_image::crop_black_bars(img, work);
    } else if (args.cmd == "scale") {
        rc = aico_image::scale(img, work, args.scale);
    } else if (args.cmd == "rotate") {
        rc = aico_image::rotate(img, work, args.rotate_deg);
    } else {
        print_usage();
        return 1;
    }

    if (rc != aico_image::kSuccess) {
        std::fprintf(stderr, "Operation failed (%d)\n", rc);
        return 1;
    }

    if (!write_output(args.output, work, args.quality)) {
        return 1;
    }
    return 0;
}
