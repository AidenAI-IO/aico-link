/*
 * Smart Turn end-of-turn detection on Rockchip RV1103/RV1106 (RKNN).
 *
 *   smart_turn *st = smart_turn_open("smart-turn-....rknn", err, sizeof err);
 *   float prob;
 *   smart_turn_predict(st, pcm, n_samples, &prob, NULL);   // 16 kHz mono int16
 *   ...
 *   smart_turn_close(st);
 *
 * Pass the audio of the user's current turn (it may be longer or shorter than
 * the model's window; the last window_samples are used, front-padded with
 * silence). prob is the probability that the turn is complete; > 0.5 means
 * complete. The window (4 s or 8 s) is read from the model's input shape.
 *
 * A handle is not thread-safe; use one per thread.
 */
#ifndef SMART_TURN_H
#define SMART_TURN_H

#include <stddef.h>
#include <stdint.h>

#if defined(AIDEN_RKNN_MICRO_RUNTIME) || defined(AIDEN_RKNN_FULL_RUNTIME)
#include "rknn_api_minimal.h"
#else
#include "rknn_api.h"
#endif

typedef struct smart_turn smart_turn;

typedef struct {
    double features_ms; /* log-mel on the CPU */
    double quantize_ms; /* float -> int8 input */
    double run_ms;      /* rknn_run (NPU, plus the runtime's CPU ops) */
} smart_turn_timing;

smart_turn *smart_turn_open(const char *model_path, char *err, size_t err_len);
void smart_turn_close(smart_turn *st);

/* Samples per model window (64000 for 4 s) and features per input (80 x frames). */
int smart_turn_window_samples(const smart_turn *st);
int smart_turn_feature_count(const smart_turn *st);

/* Audio -> probability. timing may be NULL. Returns 0 on success. */
int smart_turn_predict(smart_turn *st, const int16_t *pcm, size_t n_samples, float *prob,
                       smart_turn_timing *timing);

/* Lower-level steps, for benchmarking and checking against a reference. */
int smart_turn_compute_features(smart_turn *st, const int16_t *pcm, size_t n_samples, float *features);
int smart_turn_predict_features(smart_turn *st, const float *features, float *prob, smart_turn_timing *timing);

/* The underlying RKNN context and tensor attributes, e.g. for rknn_query. */
rknn_context smart_turn_rknn(const smart_turn *st);
const rknn_tensor_attr *smart_turn_input_attr(const smart_turn *st);
const rknn_tensor_attr *smart_turn_output_attr(const smart_turn *st);

#endif
