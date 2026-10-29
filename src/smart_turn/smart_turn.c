#include "smart_turn.h"

#include <math.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "whisper_mel.h"

struct smart_turn {
    rknn_context ctx;
    rknn_tensor_attr in, out;
    rknn_tensor_mem *in_mem, *out_mem;
    int window_samples, n_frames;
    float *audio, *features, *scratch;
};

static double now_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec * 1e3 + ts.tv_nsec / 1e6;
}

static void set_err(char *err, size_t len, const char *fmt, ...) {
    if (!err || !len) return;
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(err, len, fmt, ap);
    va_end(ap);
}

static void *read_file(const char *path, size_t *size) {
    FILE *f = fopen(path, "rb");
    void *buf = NULL;
    if (!f) return NULL;
    if (fseek(f, 0, SEEK_END) == 0) {
        long n = ftell(f);
        if (n > 0 && fseek(f, 0, SEEK_SET) == 0 && (buf = malloc(n)) && fread(buf, 1, n, f) != (size_t)n) {
            free(buf);
            buf = NULL;
        }
        *size = n > 0 ? (size_t)n : 0;
    }
    fclose(f);
    return buf;
}

smart_turn *smart_turn_open(const char *model_path, char *err, size_t err_len) {
    size_t model_size = 0;
    void *model = read_file(model_path, &model_size);
    if (!model) {
        set_err(err, err_len, "cannot read model %s", model_path);
        return NULL;
    }
    smart_turn *st = calloc(1, sizeof *st);
    int ret = rknn_init(&st->ctx, model, model_size, 0, NULL);
    free(model);
    if (ret != RKNN_SUCC) {
        set_err(err, err_len, "rknn_init failed: %d", ret);
        free(st);
        return NULL;
    }

    /* RV1106 models take int8 input and produce int8 output, via zero-copy
     * buffers in the NPU's native layout. */
    st->in.index = 0;
    st->out.index = 0;
    if (rknn_query(st->ctx, RKNN_QUERY_NATIVE_INPUT_ATTR, &st->in, sizeof st->in) != RKNN_SUCC ||
        rknn_query(st->ctx, RKNN_QUERY_NATIVE_OUTPUT_ATTR, &st->out, sizeof st->out) != RKNN_SUCC) {
        set_err(err, err_len, "rknn_query for tensor attributes failed");
        goto fail;
    }
    /* Expected input: (1, 80, frames), contiguous int8. */
    if (st->in.type != RKNN_TENSOR_INT8 || st->in.n_dims != 3 || st->in.dims[1] != 80 ||
        st->in.size_with_stride != st->in.n_elems || st->out.type != RKNN_TENSOR_INT8) {
        set_err(err, err_len, "unexpected model I/O (want int8 input (1, 80, frames) and int8 output)");
        goto fail;
    }
    st->n_frames = st->in.dims[2];
    st->window_samples = st->n_frames * WHISPER_HOP;

    st->in_mem = rknn_create_mem(st->ctx, st->in.size_with_stride);
    st->out_mem = rknn_create_mem(st->ctx, st->out.size_with_stride);
    if (!st->in_mem || !st->out_mem || rknn_set_io_mem(st->ctx, st->in_mem, &st->in) != RKNN_SUCC ||
        rknn_set_io_mem(st->ctx, st->out_mem, &st->out) != RKNN_SUCC) {
        set_err(err, err_len, "cannot set up RKNN input/output memory");
        goto fail;
    }

    st->audio = malloc(st->window_samples * sizeof(float));
    st->features = malloc(smart_turn_feature_count(st) * sizeof(float));
    st->scratch = malloc(whisper_mel_scratch_floats(st->window_samples) * sizeof(float));
    if (!st->audio || !st->features || !st->scratch) {
        set_err(err, err_len, "out of memory");
        goto fail;
    }
    return st;

fail:
    smart_turn_close(st);
    return NULL;
}

void smart_turn_close(smart_turn *st) {
    if (!st) return;
    if (st->in_mem) rknn_destroy_mem(st->ctx, st->in_mem);
    if (st->out_mem) rknn_destroy_mem(st->ctx, st->out_mem);
    rknn_destroy(st->ctx);
    free(st->audio);
    free(st->features);
    free(st->scratch);
    free(st);
}

int smart_turn_window_samples(const smart_turn *st) { return st->window_samples; }
int smart_turn_feature_count(const smart_turn *st) { return 80 * st->n_frames; }
rknn_context smart_turn_rknn(const smart_turn *st) { return st->ctx; }
const rknn_tensor_attr *smart_turn_input_attr(const smart_turn *st) { return &st->in; }
const rknn_tensor_attr *smart_turn_output_attr(const smart_turn *st) { return &st->out; }

int smart_turn_compute_features(smart_turn *st, const int16_t *pcm, size_t n_samples, float *features) {
    /* Keep the last window of audio; pad the front with silence. */
    const size_t w = st->window_samples;
    const size_t keep = n_samples < w ? n_samples : w;
    const size_t pad = w - keep;
    const int16_t *src = pcm + (n_samples - keep);
    memset(st->audio, 0, pad * sizeof(float));
    for (size_t i = 0; i < keep; i++) st->audio[pad + i] = src[i] * (1.0f / 32768.0f);
    return whisper_log_mel(st->audio, (int)w, features, st->scratch);
}

int smart_turn_predict_features(smart_turn *st, const float *features, float *prob, smart_turn_timing *timing) {
    const double t0 = now_ms();
    const float inv_scale = 1.0f / st->in.scale;
    int8_t *q = st->in_mem->virt_addr;
    for (uint32_t i = 0; i < st->in.n_elems; i++) {
        long v = lrintf(features[i] * inv_scale) + st->in.zp;
        q[i] = (int8_t)(v < -128 ? -128 : v > 127 ? 127 : v);
    }
    const double t1 = now_ms();
    int ret = rknn_run(st->ctx, NULL);
    const double t2 = now_ms();
    if (ret != RKNN_SUCC) return ret;
    *prob = (*(int8_t *)st->out_mem->virt_addr - st->out.zp) * st->out.scale;
    if (timing) {
        timing->quantize_ms = t1 - t0;
        timing->run_ms = t2 - t1;
    }
    return 0;
}

int smart_turn_predict(smart_turn *st, const int16_t *pcm, size_t n_samples, float *prob,
                       smart_turn_timing *timing) {
    const double t0 = now_ms();
    if (smart_turn_compute_features(st, pcm, n_samples, st->features) != 0) return -1;
    const double t1 = now_ms();
    int ret = smart_turn_predict_features(st, st->features, prob, timing);
    if (timing) timing->features_ms = t1 - t0;
    return ret;
}
