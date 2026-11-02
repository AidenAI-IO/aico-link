/* Fixed-window Kaldi-compatible Fbank for ERes2Net (no external dependencies).
 * PCM16 mono 16 kHz WAV -> float32 [1,1,80,200], little endian.
 * 25 ms Povey window, 10 ms hop, 512-point FFT, 80 mel bins, mean subtraction.
 * Only the first 32240 samples (2.015 seconds) are used. This is NOT a VAD.
 * Build: cc -O2 -std=c11 -Wall -Wextra -Werror fbank.c -lm -o fbank
 */
#define _POSIX_C_SOURCE 200809L
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <math.h>
#include <float.h>
#include <time.h>

#define NFFT 512
#define WINDOW 400
#define HOP 160
#define FRAMES 200
#define MELS 80
#define SAMPLES ((FRAMES - 1) * HOP + WINDOW)
#define PI 3.14159265358979323846

static uint32_t u32(const unsigned char *p) {
    return (uint32_t)p[0] | (uint32_t)p[1]<<8 | (uint32_t)p[2]<<16 | (uint32_t)p[3]<<24;
}
static unsigned u16(const unsigned char *p) { return p[0] | (unsigned)p[1]<<8; }

static int read_wav(const char *path, double *audio) {
    FILE *f = fopen(path, "rb");
    if (!f) { perror(path); return -1; }
    unsigned char h[12];
    int valid_fmt = 0, result = -1;
    if (fread(h, 1, 12, f) != 12 || memcmp(h, "RIFF", 4) || memcmp(h+8, "WAVE", 4)) goto done;
    while (fread(h, 1, 8, f) == 8) {
        uint32_t size = u32(h+4);
        if (size > INT32_MAX) goto done;
        if (!memcmp(h, "fmt ", 4)) {
            unsigned char fmt[16];
            if (size < 16 || fread(fmt, 1, 16, f) != 16) goto done;
            valid_fmt = u16(fmt)==1 && u16(fmt+2)==1 && u32(fmt+4)==16000 &&
                        u32(fmt+8)==32000 && u16(fmt+12)==2 && u16(fmt+14)==16;
            if (fseek(f, (long)(size-16) + (size&1), SEEK_CUR)) goto done;
        } else if (!memcmp(h, "data", 4)) {
            if (!valid_fmt || size < 2*SAMPLES || (size&1)) goto done;
            for (int i=0; i<SAMPLES; ++i) {
                unsigned char p[2];
                if (fread(p, 1, 2, f) != 2) goto done;
                unsigned bits = u16(p);
                int sample = bits >= 32768 ? (int)bits-65536 : (int)bits;
                audio[i] = sample / 32768.0;
            }
            result=0;
            break;
        } else if (fseek(f, (long)size+(size&1), SEEK_CUR)) goto done;
    }
done:
    fclose(f);
    if (result) fprintf(stderr, "%s: requires PCM16 mono 16 kHz WAV with at least %d samples\n", path, SAMPLES);
    return result;
}

static void fft(double *re, double *im) {
    for (unsigned i=1,j=0; i<NFFT; ++i) {
        unsigned bit=NFFT>>1;
        for (; j&bit; bit>>=1) j^=bit;
        j^=bit;
        if (i<j) { double v=re[i]; re[i]=re[j]; re[j]=v; }
    }
    for (unsigned len=2; len<=NFFT; len<<=1) {
        double angle=-2*PI/len, wr0=cos(angle), wi0=sin(angle);
        for (unsigned i=0; i<NFFT; i+=len) {
            double wr=1, wi=0;
            for (unsigned j=0; j<len/2; ++j) {
                unsigned a=i+j,b=a+len/2;
                double tr=wr*re[b]-wi*im[b], ti=wr*im[b]+wi*re[b];
                re[b]=re[a]-tr; im[b]=im[a]-ti; re[a]+=tr; im[a]+=ti;
                double next=wr*wr0-wi*wi0; wi=wr*wi0+wi*wr0; wr=next;
            }
        }
    }
}

static void fbank(const double *audio, float *out) {
    double weights[MELS][NFFT/2], window[WINDOW];
    double low=1127*log1p(20.0/700), high=1127*log1p(8000.0/700);
    double step=(high-low)/(MELS+1);
    for (int m=0; m<MELS; ++m) {
        double left=low+m*step, center=left+step, right=center+step;
        for (int k=0; k<NFFT/2; ++k) {
            double mel=1127*log1p((16000.0*k/NFFT)/700);
            weights[m][k]=fmax(0, fmin((mel-left)/(center-left), (right-mel)/(right-center)));
        }
    }
    for (int i=0; i<WINDOW; ++i) window[i]=pow(0.5-0.5*cos(2*PI*i/(WINDOW-1)), 0.85);
    for (int t=0; t<FRAMES; ++t) {
        double re[NFFT]={0}, im[NFFT]={0}, mean=0;
        const double *samples=audio+t*HOP;
        for (int i=0; i<WINDOW; ++i) mean+=samples[i];
        mean/=WINDOW;
        for (int i=0; i<WINDOW; ++i) {
            double previous=samples[i ? i-1 : 0]-mean;
            re[i]=(samples[i]-mean-0.97*previous)*window[i];
        }
        fft(re,im);
        for (int k=0; k<NFFT/2; ++k) re[k]=re[k]*re[k]+im[k]*im[k];
        for (int m=0; m<MELS; ++m) {
            double sum=0;
            for (int k=0; k<NFFT/2; ++k) sum+=weights[m][k]*re[k];
            out[m*FRAMES+t]=(float)log(fmax(sum, FLT_EPSILON));
        }
    }
    for (int m=0; m<MELS; ++m) {
        double mean=0;
        for (int t=0; t<FRAMES; ++t) mean+=out[m*FRAMES+t];
        mean/=FRAMES;
        for (int t=0; t<FRAMES; ++t) out[m*FRAMES+t]-=(float)mean;
    }
}

int main(int argc, char **argv) {
    if (argc!=3) { fprintf(stderr,"usage: %s audio.wav fbank.f32\n",argv[0]); return 1; }
    double *audio=malloc(SAMPLES*sizeof(*audio));
    float *out=malloc(MELS*FRAMES*sizeof(*out));
    int result=1;
    if (!audio || !out || read_wav(argv[1],audio)) goto done;
    struct timespec a,b;
    if (clock_gettime(CLOCK_MONOTONIC,&a)) goto done;
    fbank(audio,out);
    if (clock_gettime(CLOCK_MONOTONIC,&b)) goto done;
    printf("CPU Fbank: %.3f ms\n",(b.tv_sec-a.tv_sec)*1000.0+(b.tv_nsec-a.tv_nsec)/1e6);
    FILE *f=fopen(argv[2],"wb");
    if (!f) { perror(argv[2]); goto done; }
    int ok=fwrite(out,sizeof(*out),MELS*FRAMES,f)==MELS*FRAMES;
    if (fclose(f)) ok=0;
    if (!ok) { fprintf(stderr,"Failed to write features\n"); goto done; }
    result=0;
done:
    free(audio); free(out); return result;
}
