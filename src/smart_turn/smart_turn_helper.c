/*
 * Smart Turn helper process, in the style of the firmware's rknn_vad helper.
 *
 *   smart_turn_helper --model MODEL.rknn [--self-test]
 *
 * Protocol (stdin binary, stdout text lines):
 *   startup             -> "READY <window_samples>"
 *   'T' u32 n int16[n]  -> "P <probability> <features_ms> <run_ms>"
 *                          n = 16 kHz mono samples of the current user turn;
 *                          only the last window_samples are used
 *   'Q'                 -> exit
 *   on errors           -> "ERR <message>"
 */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "smart_turn.h"

#if defined(AIDEN_RKNN_GLIBC_COMPAT)
extern int aiden_rknn_glibc_compat_init(void);
#endif

#define MAX_SAMPLES (16000 * 120) /* refuse turns longer than two minutes */

static int read_exact(void *dst, size_t n) { return fread(dst, 1, n, stdin) == n; }

int main(int argc, char **argv) {
    const char *model = NULL;
    int self_test = 0;
    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--model") && i + 1 < argc) model = argv[++i];
        else if (!strcmp(argv[i], "--self-test")) self_test = 1;
    }
    if (!model) {
        printf("ERR missing --model path\n");
        return 2;
    }

#if defined(AIDEN_RKNN_GLIBC_COMPAT)
    if (aiden_rknn_glibc_compat_init() != 0) {
        printf("ERR RKNN glibc compatibility initialization failed\n");
        return 1;
    }
#endif

    char err[256];
    smart_turn *st = smart_turn_open(model, err, sizeof err);
    if (!st) {
        printf("ERR %s\n", err);
        return 1;
    }
    const int window = smart_turn_window_samples(st);
    int16_t *pcm = calloc(window, sizeof(int16_t));
    float prob;
    smart_turn_timing t;

    if (self_test) {
        int ret = smart_turn_predict(st, pcm, window, &prob, &t);
        if (ret) printf("ERR inference failed: %d\n", ret);
        else printf("P %.4f %.2f %.2f\n", prob, t.features_ms, t.run_ms);
        smart_turn_close(st);
        return ret != 0;
    }

    size_t capacity = window;
    printf("READY %d\n", window);
    fflush(stdout);
    for (;;) {
        char command;
        if (!read_exact(&command, 1) || command == 'Q') break;
        if (command != 'T') {
            printf("ERR unknown command\n");
            fflush(stdout);
            continue;
        }
        uint32_t n;
        if (!read_exact(&n, sizeof n)) break;
        if (n > MAX_SAMPLES) {
            printf("ERR turn too long: %u samples\n", n);
            fflush(stdout);
            break; /* the stream is out of sync; let the caller restart us */
        }
        if (n > capacity) {
            int16_t *grown = realloc(pcm, n * sizeof(int16_t));
            if (!grown) break;
            pcm = grown;
            capacity = n;
        }
        if (!read_exact(pcm, n * sizeof(int16_t))) break;
        int ret = smart_turn_predict(st, pcm, n, &prob, &t);
        if (ret) printf("ERR inference failed: %d\n", ret);
        else printf("P %.4f %.2f %.2f\n", prob, t.features_ms, t.run_ms);
        fflush(stdout);
    }
    free(pcm);
    smart_turn_close(st);
    return 0;
}
