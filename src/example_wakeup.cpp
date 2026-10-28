#include "aico_sdk.h"
#include <stdio.h>
#include <signal.h>
#include <unistd.h>

static bool quit = false;

void signal_handler(int sig) {
    printf("Received signal %d, exiting...\n", sig);
    quit = true;
}

void on_wakeup() {
    printf("Wakeup event detected!\n");
}

int main() {
    signal(SIGINT, signal_handler);

    aico::WakeupListener listener33;
    aico::WakeupListener listener32;

    printf("Starting wakeup listeners on GPIO 33 and GPIO 32...\n");
    if (!listener33.start(33, on_wakeup) || !listener32.start(32, on_wakeup)) {
        fprintf(stderr, "Failed to start wakeup listeners\n");
        listener33.stop();
        listener32.stop();
        return 1;
    }

    printf("Listening for wakeup events. Press Ctrl+C to exit.\n");

    while (!quit) {
        sleep(1);
    }

    listener32.stop();
    listener33.stop();
    printf("Stopped.\n");

    return 0;
}
