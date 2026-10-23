#include "aico_sdk.h"
#include <stdio.h>
#include <signal.h>
#include <stdint.h>
#include <unistd.h>

static bool quit = false;

void signal_handler(int sig) { quit = true; }

void on_audio_frame(const aico::AudioFrame& frame) {
    int16_t* samples = (int16_t*)frame.data;
    uint32_t sample_count = frame.length / (2 * sizeof(int16_t));
    for (uint32_t i = 0; i < sample_count; i++) {
        fwrite(&samples[i * 2], sizeof(int16_t), 1, stdout);
    }
    fflush(stdout);
}

int main(int argc, char* argv[]) {
    signal(SIGINT, signal_handler);
    signal(SIGPIPE, signal_handler);

    aico::AudioConfig config;
    config.device_name = (argc > 1) ? argv[1] : config.device_name;
    config.sample_rate = 16000;
    config.channels = 1;
    config.bit_width = 16;

    aico::AudioCapture capture;
    if (!capture.init(config) || !capture.start(on_audio_frame))
        return 1;

    while (!quit) sleep(1);

    capture.stop();
    return 0;
}
