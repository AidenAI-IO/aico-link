#include "aico_sdk.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int main(int argc, char* argv[]) {
    const char* input_file = (argc > 1) ? argv[1] : "audio_capture_debug.pcm";

    FILE* fp = fopen(input_file, "rb");
    if (!fp) {
        fprintf(stderr, "Failed to open %s\n", input_file);
        return 1;
    }

    aico::AudioConfig config;
    config.sample_rate = 16000;
    config.channels = 1;
    config.bit_width = 16;

    aico::AudioPlayer player;

    printf("Initializing audio player...\n");
    if (!player.init(config)) {
        fprintf(stderr, "Failed to initialize audio player\n");
        fclose(fp);
        return 1;
    }

    printf("Playing %s (16kHz/16bit/mono)...\n", input_file);

    uint8_t buffer[1024];
    int chunks = 0;
    size_t bytes_read;
    while ((bytes_read = fread(buffer, 1, sizeof(buffer), fp)) > 0) {
        while (!player.play(buffer, bytes_read)) {
            // SendFrame uses 100ms timeout; retry when AO buffer is temporarily full
        }
        chunks++;
    }

    fclose(fp);
    printf("Played %d chunks. Done.\n", chunks);
    player.stop();

    return 0;
}
