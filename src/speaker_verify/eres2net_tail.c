/*
 * ERes2Net-base512 CPU tail for RV1106 (little-endian IEEE float32 files).
 * frames.f32: [1,512,10,25], channel-major, 128000 floats.
 * Pool over the last 25 values only: mean, sqrt(unbiased variance + 1e-8).
 * Concatenate flattened means [5120] then standard deviations [5120].
 * tail.f32: nn.Linear weight [512,10240] row-major, then bias [512].
 * embedding.f32: 512 floats, not unit normalized.
 *
 * Usage: eres2net_tail tail.f32 frames.f32 embedding.f32
 * Only one 40 KiB weight row is resident, never the entire 20 MiB matrix.
 */
#define _POSIX_C_SOURCE 200809L
#include <errno.h>
#include <fcntl.h>
#include <float.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

#define TIME_STEPS 25
#define CELLS (512 * 10)
#define FEATURES (2 * CELLS)
#define EMBEDDING 512
#define FRAME_COUNT ((size_t)CELLS * TIME_STEPS)
#define TAIL_COUNT ((size_t)EMBEDDING * FEATURES + EMBEDDING)
#define FAIL(...) do { fprintf(stderr, __VA_ARGS__); fputc('\n', stderr); goto cleanup; } while (0)

static FILE *open_input(const char *path, size_t count, struct stat *info)
{
    FILE *file = fopen(path, "rb");
    if (!file) { perror(path); return NULL; }
    if (fstat(fileno(file), info) != 0) {
        perror(path);
        fclose(file);
        return NULL;
    }
    if (!S_ISREG(info->st_mode) || info->st_size != (off_t)(count * sizeof(float))) {
        fprintf(stderr, "%s: expected exactly %zu float32 values in a regular file\n", path, count);
        fclose(file);
        return NULL;
    }
    return file;
}

static int read_finite(FILE *file, float *data, size_t count)
{
    if (fread(data, sizeof(*data), count, file) != count) {
        fprintf(stderr, "input read failed or input length changed\n");
        return -1;
    }
    for (size_t i = 0; i < count; ++i) {
        if (!isfinite(data[i])) {
            fprintf(stderr, "input contains nonfinite values\n");
            return -1;
        }
    }
    return 0;
}

static int at_end(FILE *file)
{
    if (fgetc(file) != EOF || ferror(file)) {
        fprintf(stderr, "input read failed or input length changed\n");
        return 0;
    }
    return 1;
}

/* Open without truncating, then compare actual descriptors before truncation.
 * This also catches symlinks and hardlinks to either input. */
static int write_output(const char *path, const float *data,
                        const struct stat *tail, const struct stat *frames)
{
    int fd = open(path, O_WRONLY | O_CREAT, 0666);
    if (fd < 0) { perror(path); return -1; }
    struct stat output;
    if (fstat(fd, &output) != 0) {
        perror(path);
        close(fd);
        return -1;
    }
    if ((output.st_dev == tail->st_dev && output.st_ino == tail->st_ino) ||
        (output.st_dev == frames->st_dev && output.st_ino == frames->st_ino)) {
        fprintf(stderr, "output must not alias input\n");
        close(fd);
        return -1;
    }
    if (!S_ISREG(output.st_mode) || ftruncate(fd, 0) != 0) {
        fprintf(stderr, "%s: cannot truncate regular output file\n", path);
        close(fd);
        return -1;
    }
    FILE *file = fdopen(fd, "wb");
    if (!file) { perror(path); close(fd); return -1; }
    int ok = fwrite(data, sizeof(*data), EMBEDDING, file) == EMBEDDING;
    if (fclose(file) != 0) ok = 0;
    if (!ok) fprintf(stderr, "%s: failed to write embedding\n", path);
    return ok ? 0 : -1;
}

int main(int argc, char **argv)
{
    if (argc != 4) {
        fprintf(stderr, "usage: %s tail.f32 frames.f32 embedding.f32\n", argv[0]);
        return EXIT_FAILURE;
    }
    const uint32_t endian = 1;
    if (*(const unsigned char *)&endian != 1 || sizeof(float) != 4 ||
        FLT_RADIX != 2 || FLT_MANT_DIG != 24 || FLT_MAX_EXP != 128) {
        fprintf(stderr, "requires a little-endian IEEE float32 host\n");
        return EXIT_FAILURE;
    }
    int status = EXIT_FAILURE;
    FILE *tail = NULL, *input = NULL;
    struct stat tail_info, frame_info;
    float *frames = malloc(FRAME_COUNT * sizeof(*frames));
    float *row = malloc(FEATURES * sizeof(*row));
    float pooled[FEATURES], bias[EMBEDDING], embedding[EMBEDDING];
    double projection[EMBEDDING];
    if (!frames || !row) FAIL("could not allocate frame or weight-row buffer");
    tail = open_input(argv[1], TAIL_COUNT, &tail_info);
    input = open_input(argv[2], FRAME_COUNT, &frame_info);
    if (!tail || !input) goto cleanup;
    if (read_finite(input, frames, FRAME_COUNT) != 0 || !at_end(input)) goto cleanup;

    /* Process CPU time includes streaming/validation work, not I/O wait. */
    clock_t start = clock();
    if (start == (clock_t)-1) FAIL("CPU clock unavailable");
    for (size_t c = 0; c < CELLS; ++c) {
        double sum = 0;
        for (size_t t = 0; t < TIME_STEPS; ++t)
            sum += frames[c * TIME_STEPS + t];
        double mean = sum / TIME_STEPS, squared = 0;
        for (size_t t = 0; t < TIME_STEPS; ++t) {
            double delta = (double)frames[c * TIME_STEPS + t] - mean;
            squared += delta * delta;
        }
        double std = sqrt(squared / (TIME_STEPS - 1) + 1e-8);
        if (!isfinite(mean) || fabs(mean) > FLT_MAX || !isfinite(std) || std > FLT_MAX)
            FAIL("statistics pooling produced invalid float32 features");
        pooled[c] = (float)mean;
        pooled[CELLS + c] = (float)std;
    }
    for (size_t j = 0; j < EMBEDDING; ++j) {
        if (read_finite(tail, row, FEATURES) != 0) goto cleanup;
        double sum = 0;
        for (size_t k = 0; k < FEATURES; ++k)
            sum += (double)row[k] * pooled[k];
        projection[j] = sum;
    }
    if (read_finite(tail, bias, EMBEDDING) != 0 || !at_end(tail)) goto cleanup;
    int nonzero = 0;
    for (size_t j = 0; j < EMBEDDING; ++j) {
        double value = projection[j] + bias[j];
        if (!isfinite(value) || fabs(value) > FLT_MAX)
            FAIL("projection produced invalid float32 embedding at %zu", j);
        embedding[j] = (float)value;
        nonzero |= embedding[j] != 0;
    }
    if (!nonzero) FAIL("tail produced a zero embedding");
    clock_t end = clock();
    if (end == (clock_t)-1 || end < start) FAIL("CPU clock unavailable or wrapped");
    if (write_output(argv[3], embedding, &tail_info, &frame_info) != 0) goto cleanup;
    printf("CPU tail (stats pooling + streamed projection): %.3f ms\n",
           1000.0 * (double)(end - start) / CLOCKS_PER_SEC);
    status = EXIT_SUCCESS;
cleanup:
    if (tail && fclose(tail) != 0) status = EXIT_FAILURE;
    if (input && fclose(input) != 0) status = EXIT_FAILURE;
    free(row);
    free(frames);
    return status;
}
