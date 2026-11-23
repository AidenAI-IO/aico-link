# Ekho build inputs

The application builder compiles Ekho 9.0 from the commit, URL, and SHA-256 in
[`source.env`](source.env). The pinned upstream archive contains both the code
and original Yali Mandarin voice data. A corrupt cached archive is discarded
and the build fails before extraction; rerun to download a fresh copy.

[`flite-slt.patch`](flite-slt.patch) replaces the Linux English backend with
[Flite](https://github.com/festvox/flite)'s American English female `slt` voice.
The builder enables `--enable-flite` and links Debian `libflite1`; eSpeak NG and
its voice data are no longer required by this build. This applies to English
and English phrases embedded in Mandarin input. Single letters in Mandarin-mode
chunks still use Ekho's bundled alphabet recordings.

The patch selects `register_cmu_us_slt()`, implements `--english-speed` through
Flite's `duration_stretch`, resamples English PCM to the current Ekho source
rate, applies a calibrated 2x amplitude gain with PCM16 saturation, and copies
samples into caller-owned buffers before releasing Flite waves. The gain
affects only Flite English audio; the global `-a 0` setting stays unchanged.
Voice initialization and cleanup are paired. The Agent calls Ekho directly
with `-s 20 --english-speed -10 -a 0`, using `-v English` for Latin-only chunks
so numbers reach Flite's English normalization, and `-v Mandarin` for Chinese
and mixed chunks. A synthesis worker prepares the next sentence during current
playback, with at most two WAVs and one queued text sentence. See
[Agent audio features](../../../docs/04-agent/audio-features.md#local-ekho-default)
for language selection, numeric continuations, buffering and cancellation.
English speed percentages are relative to SLT's natural rate and
Ekho's common `-s` processing also applies; matching the old eSpeak percentage
does not guarantee identical spoken duration.

From the repository root on a Linux amd64 Docker build host:

```bash
scripts/debian-apps/build-apps.sh builder
scripts/debian-apps/build-apps.sh ekho
```

The standalone `ekho` action requires neither Go nor OpenCV and leaves other
application outputs intact. Normal `all` and `apps` builds include it
automatically. `DEBIAN_APPS_EKHO_CACHE` overrides `.cache/debian-apps/ekho`.
The helper cross-compiles for Debian armhf, strips the executable, and checks
its ARM hard-float ABI and `$ORIGIN/../lib` RUNPATH.

Staged files under `output/debian-apps/apps/`:

| Build output | Installed by `aico-business` |
| --- | --- |
| `bin/ekho` | `/usr/lib/aico/ekho` |
| `share/ekho-data/` | `/usr/share/aico/ekho-data/` |
| `share/doc/ekho/` | `/usr/share/doc/aico-business/ekho/` |
| `metadata/ekho-files.sha256` | Build-time payload verification |

Only Mandarin and alphabet assets are staged. Prebuilt `zh.dict` and
`pinyin.index` avoid generating dictionary files in the read-only system data
directory. The compiled default data path is `/usr/share/aico/ekho-data`;
`EKHO_DATA_PATH` can point an isolated smoke test at staged data.

The documentation directory contains the original `COPYING` and
`COPYING-mandarin-yali` licenses, build provenance, the full upstream source
archive, the Flite SLT patch, and the build helper/container inputs. Source
and voice archives are downloaded during the build, not committed to this
repository. See [Debian package installation](../../../docs/08-ota/debian-package.md)
for the supported installation flow and a synthesis check.

## Device validation

Validated on 2026-11-20 with Debian 13 armhf, a single-core Cortex-A7, and
145 MiB RAM. Both builds used `-v Mandarin -s 20 --english-speed -10 -a 0
--samplerate 22050`. Times exclude playback but include process startup; each
short/paragraph case ran three times. Wall and CPU columns are medians; RSS
is the largest observed peak. The device's other services remained active.

| Input/backend | Wall seconds | CPU seconds | Peak RSS MiB |
| --- | ---: | ---: | ---: |
| Short English / eSpeak female | 0.495 | 0.339 | 12.65 |
| Short English / Flite SLT | 0.726 | 0.559 | 14.87 |
| Mixed paragraph / eSpeak female | 0.949 | 0.725 | 13.16 |
| Mixed paragraph / Flite SLT | 2.515 | 1.903 | 22.30 |

Short English input: `Hello, welcome to Aico.` The mixed paragraph contained
Chinese around three English sentences (about 12 seconds of SLT audio).
These are practical latency measurements; voice duration differs between
backends. A 24-repetition mixed-language run generated 138.85 seconds of audio
in 25.55 seconds, using 17.95 CPU seconds and 18.02 MiB peak RSS. The Mandarin
only PCM matched the prior build byte for byte. English-only input, stdin,
16 kHz and 22.05 kHz output, and English speed values -50, -10, 50, and 150
were also exercised.

A text-chat Agent test launched two sentence synthesis processes. Their maps
contained `libflite_cmu_us_slt.so.1` and no eSpeak library; sampled peak RSS was
13.32 and 14.82 MiB. The CLI output remained mono PCM16 at 22.05 kHz. Four
libraries needed by the isolated SLT test occupied approximately 3.48 MiB;
Debian's complete `libflite1` package also includes other voices.

The Flite English output gain is 2x (+6.02 dB), calibrated against the
bundled Mandarin voice using five corresponding sentence/paragraph pairs at
the Mandarin-mode bilingual preset above. The earlier 1.5x setting still left English 1.5–2.9 LU
quieter. Saturation at the PCM16 limits prevents integer overflow. This gain
affects only Flite English; the system playback volume and Mandarin
waveforms are unchanged.

After the Agent's 22.05-to-16 kHz linear resampling, measured EBU R128
integrated loudness was:

| Corresponding input pair | Mandarin LUFS | Flite SLT LUFS | English minus Mandarin LU |
| --- | ---: | ---: | ---: |
| Welcome | -12.61 | -12.96 | -0.35 |
| Web chat test | -13.11 | -12.70 | +0.41 |
| Settings instructions | -13.39 | -12.33 | +1.06 |
| Task status | -13.63 | -14.08 | -0.45 |
| Longer explanation | -12.75 | -12.82 | -0.07 |

The Mandarin PCM matched the prior version byte for byte for all five inputs.
The fixed gain preserves sentence dynamics; different text and pronunciation
can still produce different loudness. PCM16 saturation affected at most 0.18%
of output samples in this calibration corpus, so arbitrary inputs are not
guaranteed to remain free of peak clipping.
