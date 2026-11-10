/*
 * UI grounding helper: detects interface components on a screenshot with the
 * UI-DETR V6 single-class detector (YOLOv5 student, INT8 RKNN) on the RV1106
 * NPU. Pre- and post-processing follow the validated board runner
 * (UI-DETR-1 tools/benchmark_rv1106_grounding.c, nms50 mode): bilinear
 * letterbox with 114 padding, sigmoid heads, score = objectness x class,
 * NMS on normalized original coordinates, at most 50 boxes. The runner's
 * confidence 0 and NMS IoU 0.8 are the defaults; callers that show the boxes
 * pass a stricter --confidence and --nms-iou to drop duplicates.
 *
 *   ui_grounding_helper --runtime librknnmrt.so --model model.rknn
 *                       --anchors RK_anchors.txt [--confidence C]
 *                       [--nms-iou T] [--self-test]
 *
 * The model was compiled with RKNN Toolkit2 2.5.0, so the matching private
 * Mini Runtime is loaded by path; the static 2.3.2 runtime used by the audio
 * helpers is not involved.
 *
 * Protocol (stdin binary, little-endian; stdout text lines):
 *   startup                          -> "READY <model_width> <model_height>"
 *   'D' u32 w u32 h u8[w*h*3] (RGB)  -> "OK <n> <infer_ms>", then n lines
 *                                       "<x1> <y1> <x2> <y2> <score>" in
 *                                       source pixels, highest score first
 *   'Q'                              -> exit
 *   on errors                        -> "ERR <message>"
 */
#define _DEFAULT_SOURCE /* sync() */
#define _POSIX_C_SOURCE 200809L
#include <dlfcn.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include "rknn_api.h"

#define MAX_KEPT 50
#define MAX_SIDE 8192

typedef struct {
    float x1, y1, x2, y2, score;
    int raw_index;
} Detection;

typedef struct {
    int w, h;
    double scale;
} Letterbox;

static struct {
    int (*init)(rknn_context *, void *, uint32_t, uint32_t, rknn_init_extend *);
    int (*destroy)(rknn_context);
    int (*query)(rknn_context, rknn_query_cmd, void *, uint32_t);
    int (*run)(rknn_context, rknn_run_extend *);
    rknn_tensor_mem *(*create_mem)(rknn_context, uint32_t);
    int (*destroy_mem)(rknn_context, rknn_tensor_mem *);
    int (*set_io_mem)(rknn_context, rknn_tensor_mem *, rknn_tensor_attr *);
} api;

static rknn_context ctx;
static rknn_tensor_attr in_attr, out_attr[3];
static rknn_tensor_mem *in_mem, *out_mem[3];
static int model_w, model_h, in_row_pixels;
static size_t in_size;
static float anchors[3][6];
static Detection *candidates;
static int candidate_count;
static Detection kept[MAX_KEPT];
static int kept_count;
static float min_confidence = 0.0f;
static float nms_iou = 0.8f;

static double now_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec * 1000.0 + ts.tv_nsec / 1e6;
}

static int load_runtime(const char *path, char *err, size_t err_len) {
    void *lib = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    if (!lib) {
        snprintf(err, err_len, "load RKNN runtime %s: %s", path, dlerror());
        return -1;
    }
#define LOAD(field, symbol)                                                   \
    do {                                                                      \
        *(void **)(&api.field) = dlsym(lib, symbol);                          \
        if (!api.field) {                                                     \
            snprintf(err, err_len, "RKNN runtime lacks %s", symbol);          \
            return -1;                                                        \
        }                                                                     \
    } while (0)
    LOAD(init, "rknn_init");
    LOAD(destroy, "rknn_destroy");
    LOAD(query, "rknn_query");
    LOAD(run, "rknn_run");
    LOAD(create_mem, "rknn_create_mem");
    LOAD(destroy_mem, "rknn_destroy_mem");
    LOAD(set_io_mem, "rknn_set_io_mem");
#undef LOAD
    return 0;
}

static int load_anchors(const char *path, char *err, size_t err_len) {
    FILE *f = fopen(path, "r");
    if (!f) {
        snprintf(err, err_len, "cannot read anchors %s", path);
        return -1;
    }
    for (int i = 0; i < 3; i++)
        for (int j = 0; j < 6; j++)
            if (fscanf(f, "%f", &anchors[i][j]) != 1 || !isfinite(anchors[i][j]) || anchors[i][j] <= 0) {
                fclose(f);
                snprintf(err, err_len, "anchors must hold 18 positive numbers");
                return -1;
            }
    fclose(f);
    return 0;
}

static int head_shape(const rknn_tensor_attr *a, int *h, int *w, int *ch) {
    if (a->n_dims != 4 || a->dims[0] != 1) return -1;
    if (a->fmt == RKNN_TENSOR_NCHW) {
        *ch = a->dims[1];
        *h = a->dims[2];
        *w = a->dims[3];
    } else if (a->fmt == RKNN_TENSOR_NHWC) {
        *h = a->dims[1];
        *w = a->dims[2];
        *ch = a->dims[3];
    } else {
        return -1;
    }
    return 0;
}

static void *read_file(const char *path, size_t *size) {
    FILE *f = fopen(path, "rb");
    if (!f) return NULL;
    void *buf = NULL;
    long n = -1;
    if (fseek(f, 0, SEEK_END) == 0) n = ftell(f);
    if (n > 0 && fseek(f, 0, SEEK_SET) == 0 && (buf = malloc((size_t)n)) && fread(buf, 1, (size_t)n, f) != (size_t)n) {
        free(buf);
        buf = NULL;
    }
    fclose(f);
    *size = n > 0 ? (size_t)n : 0;
    return buf;
}

static int open_model(const char *model_path, char *err, size_t err_len) {
    size_t model_size = 0;
    void *model = read_file(model_path, &model_size);
    if (!model) {
        snprintf(err, err_len, "cannot read model %s", model_path);
        return -1;
    }
    int ret = api.init(&ctx, model, (uint32_t)model_size, 0, NULL);
    free(model);
    if (ret != RKNN_SUCC) {
        snprintf(err, err_len, "rknn_init failed: %d", ret);
        return -1;
    }
    rknn_input_output_num io = {0};
    if (api.query(ctx, RKNN_QUERY_IN_OUT_NUM, &io, sizeof io) || io.n_input != 1 || io.n_output != 3) {
        snprintf(err, err_len, "model must have one input and three heads");
        return -1;
    }
    if (api.query(ctx, RKNN_QUERY_NATIVE_INPUT_ATTR, &in_attr, sizeof in_attr) || in_attr.n_dims != 4 ||
        in_attr.dims[0] != 1 || in_attr.fmt != RKNN_TENSOR_NHWC || in_attr.dims[3] != 3 ||
        in_attr.dims[1] < 32 || in_attr.dims[2] < 32 || in_attr.dims[1] > 1280 || in_attr.dims[2] > 1280 ||
        in_attr.dims[1] % 32 || in_attr.dims[2] % 32) {
        snprintf(err, err_len, "model input must be NHWC RGB with sides a multiple of 32");
        return -1;
    }
    in_attr.type = RKNN_TENSOR_UINT8;
    in_attr.fmt = RKNN_TENSOR_NHWC;
    in_attr.pass_through = 0;
    model_h = (int)in_attr.dims[1];
    model_w = (int)in_attr.dims[2];
    in_row_pixels = in_attr.w_stride ? (int)in_attr.w_stride : model_w;
    in_size = in_attr.size_with_stride ? in_attr.size_with_stride : in_attr.size;
    if (in_row_pixels < model_w || in_size < (size_t)in_row_pixels * model_h * 3) {
        snprintf(err, err_len, "unexpected input stride");
        return -1;
    }
    in_mem = api.create_mem(ctx, (uint32_t)in_size);
    if (!in_mem || api.set_io_mem(ctx, in_mem, &in_attr)) {
        snprintf(err, err_len, "cannot bind input memory");
        return -1;
    }
    unsigned strides_seen = 0;
    size_t capacity = 0;
    for (int i = 0; i < 3; i++) {
        out_attr[i].index = i;
        int h = 0, w = 0, ch = 0;
        if (api.query(ctx, RKNN_QUERY_OUTPUT_ATTR, &out_attr[i], sizeof out_attr[i]) ||
            head_shape(&out_attr[i], &h, &w, &ch)) {
            snprintf(err, err_len, "cannot query head %d", i);
            return -1;
        }
        int stride = h ? model_h / h : 0;
        unsigned bit = stride == 8 ? 1 : stride == 16 ? 2 : stride == 32 ? 4 : 0;
        /* One class: three anchors x (4 box + objectness + class). */
        if (ch != 18 || w <= 0 || model_h % h || model_w % w || model_w / w != stride || !bit ||
            (strides_seen & bit)) {
            snprintf(err, err_len, "unexpected head %d shape", i);
            return -1;
        }
        strides_seen |= bit;
        const rknn_tensor_attr *a = &out_attr[i];
        if (a->type != RKNN_TENSOR_INT8 && a->type != RKNN_TENSOR_UINT8 && a->type != RKNN_TENSOR_FLOAT32) {
            snprintf(err, err_len, "unsupported head %d type", i);
            return -1;
        }
        if (a->type != RKNN_TENSOR_FLOAT32 && (a->qnt_type != RKNN_TENSOR_QNT_AFFINE_ASYMMETRIC || !(a->scale > 0))) {
            snprintf(err, err_len, "unsupported head %d quantization", i);
            return -1;
        }
        size_t size = a->size_with_stride ? a->size_with_stride : a->size;
        int row = a->w_stride ? (int)a->w_stride : w;
        if (row < w || size < (size_t)h * row * ch * (a->type == RKNN_TENSOR_FLOAT32 ? 4 : 1)) {
            snprintf(err, err_len, "unexpected head %d stride", i);
            return -1;
        }
        out_mem[i] = api.create_mem(ctx, (uint32_t)size);
        if (!out_mem[i] || api.set_io_mem(ctx, out_mem[i], &out_attr[i])) {
            snprintf(err, err_len, "cannot bind head %d memory", i);
            return -1;
        }
        capacity += (size_t)h * w * 3;
    }
    candidates = calloc(capacity, sizeof *candidates);
    if (!candidates) {
        snprintf(err, err_len, "out of memory");
        return -1;
    }
    return 0;
}

static void close_model(void) {
    for (int i = 0; i < 3; i++) {
        if (out_mem[i]) api.destroy_mem(ctx, out_mem[i]);
        out_mem[i] = NULL;
    }
    if (in_mem) api.destroy_mem(ctx, in_mem);
    in_mem = NULL;
    if (ctx) api.destroy(ctx);
    ctx = 0;
    free(candidates);
    candidates = NULL;
}

/* The NPU buffers come from CMA, which has room, but the kernel also needs a
 * physically contiguous order-4 block of ordinary memory to track them. On a
 * board with a few MB free that block is often missing, and rknn_init fails
 * with ENOMEM until the kernel compacts memory. Compacting on demand (root
 * only) and retrying makes the load reliable without a fixed delay. */
#define OPEN_ATTEMPTS 3

static void compact_memory(void) {
    sync();
    FILE *f = fopen("/proc/sys/vm/compact_memory", "w");
    if (!f) return;
    fputs("1\n", f);
    fclose(f);
}

static int open_model_with_retry(const char *model_path, char *err, size_t err_len) {
    for (int attempt = 1;; attempt++) {
        if (!open_model(model_path, err, err_len)) return 0;
        close_model();
        if (attempt == OPEN_ATTEMPTS) return -1;
        fprintf(stderr, "model load attempt %d failed (%s); compacting memory and retrying\n", attempt, err);
        compact_memory();
    }
}

/* Half-pixel bilinear letterbox, RGB padding 114, centered. */
static Letterbox preprocess(const uint8_t *src, int sw, int sh, uint8_t *dst) {
    Letterbox b;
    b.scale = fmin((double)model_w / sw, (double)model_h / sh);
    b.w = (int)nearbyint(sw * b.scale);
    b.h = (int)nearbyint(sh * b.scale);
    int padx = (model_w - b.w) / 2, pady = (model_h - b.h) / 2;
    memset(dst, 114, in_size);
    for (int y = 0; y < b.h; y++) {
        double sy = (y + 0.5) * (double)sh / b.h - 0.5;
        int y0 = (int)floor(sy);
        float fy = (float)(sy - y0);
        int y1 = y0 + 1;
        if (y0 < 0) y0 = 0;
        if (y1 >= sh) y1 = sh - 1;
        for (int x = 0; x < b.w; x++) {
            double sx = (x + 0.5) * (double)sw / b.w - 0.5;
            int x0 = (int)floor(sx);
            float fx = (float)(sx - x0);
            int x1 = x0 + 1;
            if (x0 < 0) x0 = 0;
            if (x1 >= sw) x1 = sw - 1;
            for (int c = 0; c < 3; c++) {
                float top = src[((size_t)y0 * sw + x0) * 3 + c] * (1 - fx) + src[((size_t)y0 * sw + x1) * 3 + c] * fx;
                float bot = src[((size_t)y1 * sw + x0) * 3 + c] * (1 - fx) + src[((size_t)y1 * sw + x1) * 3 + c] * fx;
                dst[((size_t)(y + pady) * in_row_pixels + x + padx) * 3 + c] =
                    (uint8_t)floorf(top * (1 - fy) + bot * fy + 0.5f);
            }
        }
    }
    return b;
}

/* Heads already hold sigmoid probabilities; dequantize and clamp to [0,1]. */
static float head_value(const rknn_tensor_attr *a, const void *buf, int c, int y, int x, int h, int w, int ch) {
    int row = a->w_stride ? (int)a->w_stride : w;
    size_t k = a->fmt == RKNN_TENSOR_NCHW ? ((size_t)c * h + y) * row + x : ((size_t)y * row + x) * ch + c;
    float f;
    if (a->type == RKNN_TENSOR_INT8) f = (((const int8_t *)buf)[k] - a->zp) * a->scale;
    else if (a->type == RKNN_TENSOR_UINT8) f = (((const uint8_t *)buf)[k] - a->zp) * a->scale;
    else f = ((const float *)buf)[k];
    return fmaxf(0.0f, fminf(1.0f, f));
}

static int by_score(const void *pa, const void *pb) {
    const Detection *a = pa, *b = pb;
    int order = (a->score < b->score) - (a->score > b->score);
    return order ? order : (a->raw_index > b->raw_index) - (a->raw_index < b->raw_index);
}

static float iou(const Detection *a, const Detection *b) {
    float iw = fmaxf(0, fminf(a->x2, b->x2) - fmaxf(a->x1, b->x1));
    float ih = fmaxf(0, fminf(a->y2, b->y2) - fmaxf(a->y1, b->y1));
    float inter = iw * ih;
    float uni = fmaxf(0, a->x2 - a->x1) * fmaxf(0, a->y2 - a->y1) + fmaxf(0, b->x2 - b->x1) * fmaxf(0, b->y2 - b->y1) - inter;
    return uni > 0 ? inter / uni : 0;
}

/* Keep the validated float32 order: remove half padding, divide by the gain,
 * divide by the source size, then clip. */
static void normalize(Detection *d, Letterbox b, int sw, int sh) {
    float half_x = (model_w - b.w) / 2.0f, half_y = (model_h - b.h) / 2.0f, scale = (float)b.scale;
    d->x1 = fmaxf(0, fminf(1, ((d->x1 - half_x) / scale) / (float)sw));
    d->y1 = fmaxf(0, fminf(1, ((d->y1 - half_y) / scale) / (float)sh));
    d->x2 = fmaxf(0, fminf(1, ((d->x2 - half_x) / scale) / (float)sw));
    d->y2 = fmaxf(0, fminf(1, ((d->y2 - half_y) / scale) / (float)sh));
}

static void postprocess(Letterbox b, int sw, int sh) {
    candidate_count = 0;
    kept_count = 0;
    for (int j = 0; j < 3; j++) {
        const rknn_tensor_attr *a = &out_attr[j];
        const void *buf = out_mem[j]->virt_addr;
        int h = 0, w = 0, ch = 0;
        head_shape(a, &h, &w, &ch);
        int stride = model_h / h, branch = stride == 8 ? 0 : stride == 16 ? 1 : 2;
        int head_offset = 0;
        for (int prior = 0; prior < branch; prior++)
            head_offset += 3 * (model_h / (8 << prior)) * (model_w / (8 << prior));
        for (int y = 0; y < h; y++)
            for (int x = 0; x < w; x++)
                for (int k = 0; k < 3; k++) {
                    int off = k * 6;
                    float score = head_value(a, buf, off + 4, y, x, h, w, ch) * head_value(a, buf, off + 5, y, x, h, w, ch);
                    if (score < min_confidence) continue;
                    float cx = (head_value(a, buf, off, y, x, h, w, ch) * 2 - 0.5f + x) * stride;
                    float cy = (head_value(a, buf, off + 1, y, x, h, w, ch) * 2 - 0.5f + y) * stride;
                    float bw = head_value(a, buf, off + 2, y, x, h, w, ch) * 2;
                    float bh = head_value(a, buf, off + 3, y, x, h, w, ch) * 2;
                    bw = bw * bw * anchors[branch][k * 2];
                    bh = bh * bh * anchors[branch][k * 2 + 1];
                    Detection d = {cx - bw / 2, cy - bh / 2, cx + bw / 2, cy + bh / 2, score,
                                   head_offset + k * h * w + y * w + x};
                    normalize(&d, b, sw, sh);
                    candidates[candidate_count++] = d;
                }
    }
    qsort(candidates, candidate_count, sizeof *candidates, by_score);
    for (int i = 0; i < candidate_count && kept_count < MAX_KEPT; i++) {
        int suppressed = 0;
        for (int j = 0; j < kept_count && !suppressed; j++) suppressed = iou(&kept[j], &candidates[i]) > nms_iou;
        if (!suppressed) kept[kept_count++] = candidates[i];
    }
    for (int i = 0; i < kept_count; i++) {
        kept[i].x1 *= sw;
        kept[i].y1 *= sh;
        kept[i].x2 *= sw;
        kept[i].y2 *= sh;
    }
}

static int detect(const uint8_t *rgb, int sw, int sh, double *infer_ms) {
    Letterbox b = preprocess(rgb, sw, sh, in_mem->virt_addr);
    double start = now_ms();
    int ret = api.run(ctx, NULL);
    *infer_ms = now_ms() - start;
    if (ret != RKNN_SUCC) return ret;
    postprocess(b, sw, sh);
    return 0;
}

static void print_detections(double infer_ms) {
    printf("OK %d %.2f\n", kept_count, infer_ms);
    for (int i = 0; i < kept_count; i++)
        printf("%.2f %.2f %.2f %.2f %.6f\n", kept[i].x1, kept[i].y1, kept[i].x2, kept[i].y2, kept[i].score);
    fflush(stdout);
}

static int read_exact(void *dst, size_t n) { return fread(dst, 1, n, stdin) == n; }

/* Parses a decimal in [0,1]; returns -1 on anything else. */
static int unit_interval(const char *text, float *out) {
    char *end = NULL;
    float value = strtof(text, &end);
    if (end == text || *end || !isfinite(value) || value < 0 || value > 1) return -1;
    *out = value;
    return 0;
}

int main(int argc, char **argv) {
    const char *runtime = NULL, *model = NULL, *anchor_path = NULL;
    int self_test = 0, bad_args = 0;
    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--runtime") && i + 1 < argc) runtime = argv[++i];
        else if (!strcmp(argv[i], "--model") && i + 1 < argc) model = argv[++i];
        else if (!strcmp(argv[i], "--anchors") && i + 1 < argc) anchor_path = argv[++i];
        else if (!strcmp(argv[i], "--self-test")) self_test = 1;
        else if (!strcmp(argv[i], "--confidence") && i + 1 < argc) bad_args |= unit_interval(argv[++i], &min_confidence);
        else if (!strcmp(argv[i], "--nms-iou") && i + 1 < argc) bad_args |= unit_interval(argv[++i], &nms_iou);
        else bad_args = 1;
    }
    if (bad_args || !runtime || !model || !anchor_path) {
        printf("ERR usage: ui_grounding_helper --runtime LIB --model MODEL --anchors FILE "
               "[--confidence 0..1] [--nms-iou 0..1] [--self-test]\n");
        return 2;
    }
    char err[256];
    if (load_runtime(runtime, err, sizeof err) || load_anchors(anchor_path, err, sizeof err) ||
        open_model_with_retry(model, err, sizeof err)) {
        printf("ERR %s\n", err);
        fflush(stdout);
        close_model();
        return 1;
    }

    double infer_ms;
    if (self_test) {
        /* A blank 540x1170 page: checks that the NPU path runs end to end. */
        const int w = 540, h = 1170;
        uint8_t *blank = malloc((size_t)w * h * 3);
        int ret = blank ? (memset(blank, 240, (size_t)w * h * 3), detect(blank, w, h, &infer_ms)) : -1;
        if (ret) printf("ERR inference failed: %d\n", ret);
        else print_detections(infer_ms);
        free(blank);
        close_model();
        return ret != 0;
    }

    uint8_t *rgb = NULL;
    size_t capacity = 0;
    printf("READY %d %d\n", model_w, model_h);
    fflush(stdout);
    for (;;) {
        char command;
        if (!read_exact(&command, 1) || command == 'Q') break;
        if (command != 'D') {
            printf("ERR unknown command\n");
            fflush(stdout);
            break; /* the stream is out of sync; let the caller restart us */
        }
        uint32_t w, h;
        if (!read_exact(&w, sizeof w) || !read_exact(&h, sizeof h)) break;
        if (w < 1 || h < 1 || w > MAX_SIDE || h > MAX_SIDE) {
            printf("ERR image size %ux%u out of range\n", w, h);
            fflush(stdout);
            break;
        }
        size_t n = (size_t)w * h * 3;
        if (n > capacity) {
            uint8_t *grown = realloc(rgb, n);
            if (!grown) {
                printf("ERR out of memory\n");
                fflush(stdout);
                break;
            }
            rgb = grown;
            capacity = n;
        }
        if (!read_exact(rgb, n)) break;
        int ret = detect(rgb, (int)w, (int)h, &infer_ms);
        if (ret) {
            printf("ERR inference failed: %d\n", ret);
            fflush(stdout);
            continue;
        }
        print_detections(infer_ms);
    }
    free(rgb);
    close_model();
    return 0;
}
