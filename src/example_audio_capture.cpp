#include "aico_sdk.h"
#include <stdio.h>
#include <signal.h>
#include <stdint.h>
#include <unistd.h>

static volatile sig_atomic_t quit = 0;
static int frame_count = 0;
static FILE* audio_fp = nullptr;

void signal_handler(int) {
    quit = 1;
}

void on_audio_frame(const aico::AudioFrame& frame) {
    frame_count++;
    if (audio_fp) {
        // Data is interleaved stereo (L R L R...), extract left channel only
        int16_t* samples = (int16_t*)frame.data;
        uint32_t sample_count = frame.length / (2 * sizeof(int16_t));  // 2ch, 16bit
        for (uint32_t i = 0; i < sample_count; i++) {
            fwrite(&samples[i * 2], sizeof(int16_t), 1, audio_fp);
        }
        fflush(audio_fp);
    }
    printf("Received audio frame #%d: %u bytes\n", frame_count, frame.length);
}

int main(int argc, char* argv[]) {
    signal(SIGINT, signal_handler);
    signal(SIGTERM, signal_handler);

    aico::AudioConfig config;
    config.device_name = (argc > 1) ? argv[1] : config.device_name;
    config.sample_rate = 16000;
    config.channels = 1;
    config.bit_width = 16;

    audio_fp = fopen("audio_capture_debug.pcm", "wb");
    if (!audio_fp) {
        fprintf(stderr, "Failed to open audio_capture_debug.pcm\n");
        return 1;
    }

    aico::AudioCapture capture;

    printf("Initializing audio capture...\n");
    if (!capture.init(config)) {
        fprintf(stderr, "Failed to initialize audio capture\n");
        fclose(audio_fp);
        return 1;
    }

    printf("Starting audio capture...\n");
    if (!capture.start(on_audio_frame)) {
        fprintf(stderr, "Failed to start audio capture\n");
        fclose(audio_fp);
        return 1;
    }

    printf("Capturing audio (16kHz/16bit/mono). Press Ctrl+C to exit.\n");
    printf("Saving raw PCM to audio_capture_debug.pcm\n");

    while (!quit) {
        sleep(1);
    }

    capture.stop();
    fclose(audio_fp);
    printf("Captured %d frames. Saved to audio_capture_debug.pcm. Stopped.\n", frame_count);

    return 0;
}
