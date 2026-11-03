/*
 * RV1106 zero-copy RKNN embedding benchmark. Input: raw little-endian float32
 * NCHW [1,1,H,W]. Output: raw float32 in the model's logical tensor order.
 * Build on the target with the matching RKNN SDK's rknn_api.h and librknnmrt:
 *   cc -O2 rv1106_bench.c -o rv1106_bench -lrknnmrt -lm
 * Run: ./rv1106_bench model.rknn input.f32 output.f32 [iterations]
 *
 * Deliberately restricted to single-channel, static 4D input. Do not silently
 * interpret an unsupported layout as a different mel-spectrogram orientation.
 */
#define _POSIX_C_SOURCE 200809L
#include <errno.h>
#include <inttypes.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#if defined(AICO_RKNN_MICRO_RUNTIME) || defined(AICO_RKNN_FULL_RUNTIME)
#include "rknn_api_minimal.h"
#else
#include "rknn_api.h"
#endif

#define FAIL(...) do { fprintf(stderr, __VA_ARGS__); fputc('\n', stderr); goto cleanup; } while (0)
#define CHECK(call) do { int code = (call); if (code != RKNN_SUCC) FAIL("%s failed: %d", #call, code); } while (0)

static double milliseconds(void)
{
    struct timespec t;
    if (clock_gettime(CLOCK_MONOTONIC, &t) != 0) return -1;
    return (double)t.tv_sec * 1000.0 + (double)t.tv_nsec / 1e6;
}

static void describe(const char *label, const rknn_tensor_attr *a)
{
    printf("%s: name=%s fmt=%s type=%s quant=%s dims=[", label, a->name,
           get_format_string(a->fmt), get_type_string(a->type),
           get_qnt_type_string(a->qnt_type));
    for (uint32_t i = 0; i < a->n_dims && i < RKNN_MAX_DIMS; ++i)
        printf("%s%u", i ? "," : "", a->dims[i]);
    printf("] elems=%u bytes=%u stride_width=%u stride_bytes=%u zp=%d scale=%g\n",
           a->n_elems, a->size, a->w_stride, a->size_with_stride,
           a->zp, (double)a->scale);
}

static int supported_type(const rknn_tensor_attr *a)
{
    if (a->type == RKNN_TENSOR_FLOAT32)
        return a->qnt_type == RKNN_TENSOR_QNT_NONE;
    return ((a->type == RKNN_TENSOR_INT8 && a->zp >= -128 && a->zp <= 127) ||
            (a->type == RKNN_TENSOR_UINT8 && a->zp >= 0 && a->zp <= 255)) &&
           a->qnt_type == RKNN_TENSOR_QNT_AFFINE_ASYMMETRIC &&
           isfinite(a->scale) && a->scale > 0;
}

static size_t element_bytes(const rknn_tensor_attr *a)
{
    return a->type == RKNN_TENSOR_FLOAT32 ? sizeof(float) : 1;
}

static int same_elements(const rknn_tensor_attr *a)
{
    size_t product = 1;
    if (!a->n_dims || a->n_dims > RKNN_MAX_DIMS) return 0;
    for (uint32_t i = 0; i < a->n_dims; ++i) {
        if (!a->dims[i] || product > SIZE_MAX / a->dims[i]) return 0;
        product *= a->dims[i];
    }
    return product == a->n_elems;
}

/* File size must match exactly: a wrong feature shape is not silently truncated. */
static int load_floats(const char *path, float *values, size_t count)
{
    FILE *f = fopen(path, "rb");
    if (!f) { perror(path); return -1; }
    int ok = fread(values, sizeof(float), count, f) == count && fgetc(f) == EOF && !ferror(f);
    if (fclose(f) != 0) ok = 0;
    if (!ok) fprintf(stderr, "%s: expected exactly %zu float32 values\n", path, count);
    return ok ? 0 : -1;
}

static int write_floats(const char *path, const float *values, size_t count)
{
    FILE *f = fopen(path, "wb");
    if (!f) { perror(path); return -1; }
    int ok = fwrite(values, sizeof(float), count, f) == count;
    if (fclose(f) != 0) ok = 0;
    if (!ok) fprintf(stderr, "%s: could not write %zu float32 values\n", path, count);
    return ok ? 0 : -1;
}

static void put_value(void *p, rknn_tensor_type type, int32_t zp, float scale, float value)
{
    if (type == RKNN_TENSOR_FLOAT32) {
        memcpy(p, &value, sizeof(value));
    } else {
        double q = nearbyint((double)value / scale + zp);
        if (type == RKNN_TENSOR_INT8) {
            int8_t v = (int8_t)fmax(-128.0, fmin(127.0, q));
            memcpy(p, &v, sizeof(v));
        } else {
            uint8_t v = (uint8_t)fmax(0.0, fmin(255.0, q));
            memcpy(p, &v, sizeof(v));
        }
    }
}

static float get_value(const void *p, const rknn_tensor_attr *a)
{
    if (a->type == RKNN_TENSOR_FLOAT32) {
        float v;
        memcpy(&v, p, sizeof(v));
        return v;
    }
    int32_t v = a->type == RKNN_TENSOR_INT8 ? *(const int8_t *)p : *(const uint8_t *)p;
    return (float)(((double)v - a->zp) * a->scale);
}

int main(int argc, char **argv)
{
    int status = EXIT_FAILURE;
    rknn_context ctx = 0;
    rknn_tensor_mem *input_mem = NULL, *output_mem = NULL;
    float *input = NULL, *output = NULL;
    rknn_input_output_num io = {0};
    rknn_sdk_version version = {0};
    rknn_tensor_attr in = {0}, native_in = {0}, out = {0}, native_out = {0};
    unsigned long iterations = 10;
    if (argc != 4 && argc != 5) {
        fprintf(stderr, "usage: %s model.rknn input.f32 output.f32 [iterations]\n", argv[0]);
        return status;
    }
    if (argc == 5) {
        char *end;
        errno = 0;
        iterations = strtoul(argv[4], &end, 10);
        if (errno || *end || end == argv[4] || !iterations || iterations > 1000000) {
            fprintf(stderr, "iterations must be an integer from 1 to 1000000\n");
            return status;
        }
    }
    double start = milliseconds();
    if (start < 0) FAIL("clock_gettime failed");
    CHECK(rknn_init(&ctx, argv[1], 0, 0, NULL));
    double init_ms = milliseconds() - start;
    if (init_ms < 0) FAIL("clock_gettime failed");
    CHECK(rknn_query(ctx, RKNN_QUERY_SDK_VERSION, &version, sizeof(version)));
    printf("RKNN API: %s driver: %s\ninit: %.3f ms\n", version.api_version, version.drv_version, init_ms);
    CHECK(rknn_query(ctx, RKNN_QUERY_IN_OUT_NUM, &io, sizeof(io)));
    if (io.n_input != 1 || io.n_output != 1)
        FAIL("expected one input and one output; got %u/%u", io.n_input, io.n_output);
    CHECK(rknn_query(ctx, RKNN_QUERY_INPUT_ATTR, &in, sizeof(in)));
    CHECK(rknn_query(ctx, RKNN_QUERY_NATIVE_INPUT_ATTR, &native_in, sizeof(native_in)));
    CHECK(rknn_query(ctx, RKNN_QUERY_OUTPUT_ATTR, &out, sizeof(out)));
    CHECK(rknn_query(ctx, RKNN_QUERY_NATIVE_NHWC_OUTPUT_ATTR, &native_out, sizeof(native_out)));
    describe("logical input", &in);
    describe("native NHWC input", &native_in);
    describe("logical output", &out);
    describe("native NHWC output", &native_out);

    /* Only [1,1,H,W] logical NCHW or [1,H,W,1] logical NHWC. */
    if (!same_elements(&in) || !same_elements(&native_in) ||
        in.n_dims != 4 || native_in.n_dims != 4 || native_in.fmt != RKNN_TENSOR_NHWC ||
        in.dims[0] != 1 || native_in.dims[0] != 1 ||
        !supported_type(&native_in))
        FAIL("unsupported input shape, format or quantization");
    uint32_t h, w;
    if (in.fmt == RKNN_TENSOR_NCHW && in.dims[1] == 1) {
        h = in.dims[2]; w = in.dims[3];
    } else if (in.fmt == RKNN_TENSOR_NHWC && in.dims[3] == 1) {
        h = in.dims[1]; w = in.dims[2];
    } else FAIL("input must have one channel in NCHW or NHWC order");
    if (native_in.dims[1] != h || native_in.dims[2] != w || native_in.dims[3] != 1 ||
        native_in.n_elems != in.n_elems)
        FAIL("native input shape differs from logical input");

    /* Querying NATIVE_NHWC_OUTPUT_ATTR requests unpacked NHWC rather than
       assuming the NPU's internal NC1HWC2 channel-blocking scheme. */
    if (!same_elements(&out) || !same_elements(&native_out) ||
        !supported_type(&native_out) || out.n_elems != native_out.n_elems)
        FAIL("unsupported output tensor or quantization");
    uint32_t on, oc, oh, ow;
    if (out.n_dims == 2 && out.dims[0] == 1) {
        on = 1; oc = out.dims[1]; oh = ow = 1;
    } else if (out.n_dims == 4 && out.fmt == RKNN_TENSOR_NCHW) {
        on = out.dims[0]; oc = out.dims[1]; oh = out.dims[2]; ow = out.dims[3];
    } else if (out.n_dims == 4 && out.fmt == RKNN_TENSOR_NHWC) {
        on = out.dims[0]; oh = out.dims[1]; ow = out.dims[2]; oc = out.dims[3];
    } else FAIL("unsupported logical output layout");
    /* Some runtimes expose an embedding as [1,C], others as [1,1,1,C]. */
    int flat_output = out.n_dims == 2 && native_out.n_dims == 2 &&
                      native_out.dims[0] == 1 && native_out.dims[1] == oc &&
                      (native_out.fmt == RKNN_TENSOR_NHWC ||
                       native_out.fmt == RKNN_TENSOR_UNDEFINED);
    if (on != 1 && !flat_output) FAIL("only batch-one output is supported");
    if (!flat_output && (native_out.fmt != RKNN_TENSOR_NHWC || native_out.n_dims != 4 ||
        native_out.dims[0] != on || native_out.dims[1] != oh ||
        native_out.dims[2] != ow || native_out.dims[3] != oc))
        FAIL("native NHWC output layout does not match logical output");

    size_t iw_stride = native_in.w_stride ? native_in.w_stride : w;
    size_t ow_stride = flat_output ? 1 : (native_out.w_stride ? native_out.w_stride : ow);
    size_t ib = element_bytes(&native_in), ob = element_bytes(&native_out);
    if (iw_stride < w || (!flat_output && ow_stride < ow) ||
        (uint64_t)h * iw_stride > native_in.size_with_stride / ib ||
        (uint64_t)oh * ow_stride > native_out.size_with_stride / ob / oc ||
        native_in.size_with_stride == 0 || native_out.size_with_stride == 0)
        FAIL("unsupported native buffer stride or size");
    if (flat_output && native_out.w_stride != 0 && native_out.w_stride != oc)
        FAIL("unsupported flat output stride");
    if ((uint64_t)in.n_elems > SIZE_MAX / sizeof(*input) ||
        (uint64_t)out.n_elems > SIZE_MAX / sizeof(*output))
        FAIL("float buffer size overflows address space");
    input = malloc((size_t)in.n_elems * sizeof(*input));
    output = malloc((size_t)out.n_elems * sizeof(*output));
    if (!input || !output) FAIL("could not allocate float buffers");
    if (load_floats(argv[2], input, in.n_elems) != 0) goto cleanup;
    input_mem = rknn_create_mem(ctx, native_in.size_with_stride);
    output_mem = rknn_create_mem(ctx, native_out.size_with_stride);
    if (!input_mem || !output_mem || !input_mem->virt_addr || !output_mem->virt_addr ||
        input_mem->size < native_in.size_with_stride || output_mem->size < native_out.size_with_stride)
        FAIL("could not allocate native tensor memory");
    /* Quantized padding represents zero, not necessarily the integer zero. */
    memset(input_mem->virt_addr, native_in.type == RKNN_TENSOR_FLOAT32 ? 0 :
           (uint8_t)native_in.zp, input_mem->size);
    for (size_t y = 0; y < h; ++y)
        for (size_t x = 0; x < w; ++x) {
            float v = input[y * w + x];
            if (!isfinite(v)) FAIL("input has a nonfinite value at (%zu,%zu)", y, x);
            put_value((uint8_t *)input_mem->virt_addr + (y * iw_stride + x) * ib,
                      native_in.type, native_in.zp, native_in.scale, v);
        }
    /* We quantize the native input ourselves. Mark it as already in the model's
       native representation; otherwise some runtimes apply an extra conversion. */
    native_in.pass_through = 1;
    CHECK(rknn_set_io_mem(ctx, input_mem, &native_in));
    CHECK(rknn_set_io_mem(ctx, output_mem, &native_out));
    start = milliseconds();
    if (start < 0) FAIL("clock_gettime failed");
    for (unsigned long i = 0; i < iterations; ++i)
        CHECK(rknn_run(ctx, NULL));
    double run_ms = milliseconds() - start;
    if (run_ms < 0) FAIL("clock_gettime failed");
    printf("runs: %lu total: %.3f ms average: %.3f ms\n", iterations, run_ms, run_ms / iterations);

    for (size_t y = 0; y < oh; ++y)
        for (size_t x = 0; x < ow; ++x)
            for (size_t c = 0; c < oc; ++c) {
                size_t offset = ((y * ow_stride + x) * oc + c) * ob;
                float v = get_value((const uint8_t *)output_mem->virt_addr + offset, &native_out);
                if (!isfinite(v)) FAIL("nonfinite model output at (%zu,%zu,%zu)", y, x, c);
                size_t dst = out.n_dims == 4 && out.fmt == RKNN_TENSOR_NCHW
                           ? (c * oh + y) * ow + x : (y * ow + x) * oc + c;
                output[dst] = v;
            }
    if (write_floats(argv[3], output, out.n_elems) != 0) goto cleanup;
    printf("saved %u float32 values to %s\n", out.n_elems, argv[3]);
    status = EXIT_SUCCESS;
cleanup:
    free(input);
    free(output);
    if (output_mem && ctx && rknn_destroy_mem(ctx, output_mem) != RKNN_SUCC) {
        fprintf(stderr, "rknn_destroy_mem(output) failed\n"); status = EXIT_FAILURE;
    }
    if (input_mem && ctx && rknn_destroy_mem(ctx, input_mem) != RKNN_SUCC) {
        fprintf(stderr, "rknn_destroy_mem(input) failed\n"); status = EXIT_FAILURE;
    }
    if (ctx && rknn_destroy(ctx) != RKNN_SUCC) {
        fprintf(stderr, "rknn_destroy failed\n"); status = EXIT_FAILURE;
    }
    return status;
}
