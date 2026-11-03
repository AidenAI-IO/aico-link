/*
 * Whisper log-mel features in plain C, matching transformers'
 * WhisperFeatureExtractor(do_normalize=True) for a window that is already
 * exactly n_samples long (front-padded with zeros, as Smart Turn does).
 */
#ifndef WHISPER_MEL_H
#define WHISPER_MEL_H

#define WHISPER_SAMPLE_RATE 16000
#define WHISPER_HOP 160
#define WHISPER_N_FFT 400

/*
 * audio:  n_samples float samples in [-1, 1]; n_samples must be a multiple of
 *         WHISPER_HOP (64000 for 4 s, 128000 for 8 s).
 * out:    80 x (n_samples / WHISPER_HOP) features, row-major (mel, frame),
 *         i.e. the model's (1, 80, frames) input.
 * scratch: at least whisper_mel_scratch_floats(n_samples) floats.
 * Returns 0 on success, -1 on invalid arguments.
 */
int whisper_log_mel(const float *audio, int n_samples, float *out, float *scratch);
int whisper_mel_scratch_floats(int n_samples);

#endif
