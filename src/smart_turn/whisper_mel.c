/*
 * Whisper log-mel features, equivalent to transformers' WhisperFeatureExtractor:
 *
 *   1. zero-mean / unit-variance normalisation over the whole window
 *   2. STFT: reflect padding of n_fft/2, periodic Hann window, n_fft=400,
 *      hop=160, power spectrum; the last frame is dropped
 *   3. 80 Slaney mel filters, log10(max(mel, 1e-10))
 *   4. clamp to (global max - 8), then (x + 4) / 4
 *
 * The 400-point real FFT is computed as a 200-point complex FFT (mixed radix
 * 2 and 5) plus the usual real-input split step.
 */
#include "whisper_mel.h"

#include <math.h>
#include <string.h>

#include "whisper_mel_filters.h"

#define HALF (WHISPER_N_FFT / 2) /* 200-point complex FFT */
#define PAD (WHISPER_N_FFT / 2)  /* reflect padding on each side */

typedef struct {
    float re, im;
} cpx;

static cpx g_twiddle[HALF];           /* exp(-2*pi*i*k/HALF) */
static cpx g_split[HALF + 1];         /* exp(-2*pi*i*k/N_FFT) */
static float g_window[WHISPER_N_FFT]; /* periodic Hann */
static int g_ready;

static void init_tables(void) {
    if (g_ready) return;
    for (int k = 0; k < HALF; k++) {
        double a = -2.0 * M_PI * k / HALF;
        g_twiddle[k].re = (float)cos(a);
        g_twiddle[k].im = (float)sin(a);
    }
    for (int k = 0; k <= HALF; k++) {
        double a = -2.0 * M_PI * k / WHISPER_N_FFT;
        g_split[k].re = (float)cos(a);
        g_split[k].im = (float)sin(a);
    }
    for (int i = 0; i < WHISPER_N_FFT; i++) g_window[i] = (float)(0.5 - 0.5 * cos(2.0 * M_PI * i / WHISPER_N_FFT));
    g_ready = 1;
}

static inline cpx cmul(cpx a, cpx b) {
    cpx r = {a.re * b.re - a.im * b.im, a.re * b.im + a.im * b.re};
    return r;
}

/* Recursive decimation-in-time FFT of n points of `in` (taken every `stride`
 * elements) into out[0..n). Twiddles are exp(-2*pi*i*e/HALF) with e scaled
 * by tw_step = HALF / n, so every index below stays under HALF. Radices 2
 * and 5 cover HALF = 200 = 2^3 * 5^2. */
static void fft_rec(cpx *out, const cpx *in, int n, int stride, int tw_step) {
    if (n == 1) {
        out[0] = in[0];
        return;
    }
    if (n % 2 == 0) {
        const int m = n / 2;
        fft_rec(out, in, m, stride * 2, tw_step * 2);
        fft_rec(out + m, in + stride, m, stride * 2, tw_step * 2);
        for (int k = 0; k < m; k++) {
            cpx t = cmul(out[m + k], g_twiddle[k * tw_step]);
            cpx a = out[k];
            out[k].re = a.re + t.re;
            out[k].im = a.im + t.im;
            out[m + k].re = a.re - t.re;
            out[m + k].im = a.im - t.im;
        }
        return;
    }
    /* Radix 5 (KissFFT's butterfly). */
    const int m = n / 5;
    for (int q = 0; q < 5; q++) fft_rec(out + q * m, in + q * stride, m, stride * 5, tw_step * 5);
    const cpx ya = g_twiddle[HALF / 5], yb = g_twiddle[2 * HALF / 5];
    for (int k = 0; k < m; k++) {
        cpx *f0 = out + k, *f1 = f0 + m, *f2 = f1 + m, *f3 = f2 + m, *f4 = f3 + m;
        cpx s0 = *f0;
        cpx s1 = cmul(*f1, g_twiddle[k * tw_step]), s2 = cmul(*f2, g_twiddle[2 * k * tw_step]);
        cpx s3 = cmul(*f3, g_twiddle[3 * k * tw_step]), s4 = cmul(*f4, g_twiddle[4 * k * tw_step]);
        cpx s7 = {s1.re + s4.re, s1.im + s4.im}, s10 = {s1.re - s4.re, s1.im - s4.im};
        cpx s8 = {s2.re + s3.re, s2.im + s3.im}, s9 = {s2.re - s3.re, s2.im - s3.im};
        f0->re = s0.re + s7.re + s8.re;
        f0->im = s0.im + s7.im + s8.im;
        cpx s5 = {s0.re + s7.re * ya.re + s8.re * yb.re, s0.im + s7.im * ya.re + s8.im * yb.re};
        cpx s6 = {s10.im * ya.im + s9.im * yb.im, -s10.re * ya.im - s9.re * yb.im};
        f1->re = s5.re - s6.re;
        f1->im = s5.im - s6.im;
        f4->re = s5.re + s6.re;
        f4->im = s5.im + s6.im;
        cpx s11 = {s0.re + s7.re * yb.re + s8.re * ya.re, s0.im + s7.im * yb.re + s8.im * ya.re};
        cpx s12 = {-s10.im * yb.im + s9.im * ya.im, s10.re * yb.im - s9.re * ya.im};
        f2->re = s11.re + s12.re;
        f2->im = s11.im + s12.im;
        f3->re = s11.re - s12.re;
        f3->im = s11.im - s12.im;
    }
}

/* Power spectrum |X[k]|^2, k = 0..HALF, of a real 400-sample frame. */
static void power_spectrum(const float *frame, float *power) {
    cpx z[HALF], Z[HALF];
    for (int i = 0; i < HALF; i++) {
        z[i].re = frame[2 * i];
        z[i].im = frame[2 * i + 1];
    }
    fft_rec(Z, z, HALF, 1, 1);
    for (int k = 0; k <= HALF; k++) {
        cpx a = Z[k % HALF], b = Z[(HALF - k) % HALF];
        /* even = (a + conj b) / 2, odd = (a - conj b) / (2i) */
        cpx even = {0.5f * (a.re + b.re), 0.5f * (a.im - b.im)};
        cpx odd = {0.5f * (a.im + b.im), -0.5f * (a.re - b.re)};
        cpx x = cmul(odd, g_split[k]);
        x.re += even.re;
        x.im += even.im;
        power[k] = x.re * x.re + x.im * x.im;
    }
}

int whisper_mel_scratch_floats(int n_samples) { return n_samples + 2 * PAD; }

int whisper_log_mel(const float *audio, int n_samples, float *out, float *scratch) {
    if (n_samples <= 0 || n_samples % WHISPER_HOP != 0) return -1;
    init_tables();
    const int n_frames = n_samples / WHISPER_HOP;

    /* 1. Normalise (double accumulators: 64000+ samples). */
    double sum = 0, sum_sq = 0;
    for (int i = 0; i < n_samples; i++) sum += audio[i];
    const double mean = sum / n_samples;
    for (int i = 0; i < n_samples; i++) {
        double d = audio[i] - mean;
        sum_sq += d * d;
    }
    const float inv_std = (float)(1.0 / sqrt(sum_sq / n_samples + 1e-7));
    float *x = scratch + PAD;
    for (int i = 0; i < n_samples; i++) x[i] = (float)(audio[i] - mean) * inv_std;

    /* 2. Reflect padding (numpy/torch "reflect": the edge sample is not repeated). */
    for (int i = 1; i <= PAD; i++) {
        x[-i] = x[i];
        x[n_samples - 1 + i] = x[n_samples - 1 - i];
    }

    /* 3. STFT -> mel -> log10, tracking the global maximum. */
    float frame[WHISPER_N_FFT], power[HALF + 1];
    float max_val = -INFINITY;
    for (int t = 0; t < n_frames; t++) {
        const float *src = scratch + t * WHISPER_HOP;
        for (int i = 0; i < WHISPER_N_FFT; i++) frame[i] = src[i] * g_window[i];
        power_spectrum(frame, power);
        for (int m = 0; m < WHISPER_N_MELS; m++) {
            const float *w = kMelWeights + kMelOffset[m];
            const float *p = power + kMelStart[m];
            float acc = 0;
            for (int j = 0; j < kMelCount[m]; j++) acc += w[j] * p[j];
            float v = log10f(acc > 1e-10f ? acc : 1e-10f);
            out[m * n_frames + t] = v;
            if (v > max_val) max_val = v;
        }
    }

    /* 4. Dynamic-range clamp and scaling. */
    const float floor_val = max_val - 8.0f;
    for (int i = 0; i < WHISPER_N_MELS * n_frames; i++) {
        float v = out[i] > floor_val ? out[i] : floor_val;
        out[i] = (v + 4.0f) * 0.25f;
    }
    return 0;
}
