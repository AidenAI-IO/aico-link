#!/usr/bin/env python3
"""Board-side helpers for the Aico hardware tests (Python standard library only).

Runs on the board under the board_hw_*.sh suites. Subcommands:

  audio-gen       write a test signal (tone, tone plan, silence) as s16le PCM
  audio-stats     level, DC offset, clipping and noise floor of an s16le capture
  audio-loopback  play a tone plan while capturing; frequency response, SNR,
                  THD and round-trip latency of the speaker-to-microphone path
  frame-stats     content statistics of a BMP screenshot, or the change
                  between two screenshots
  mcu             request to the MCU service socket, printed as JSON
  mcu-events      wait for MCU hardware events (wake sources, HDMI plug)
  ble-status      ble_service status, printed as JSON
  evidence-finalize write the run evidence: run.json, kernel.log, SHA256SUMS, tarball
  self-test       exercise the signal analysis and socket framing locally

Every check prints "[PASS] ...", "[WARN] ...", "[FAIL] ..." or "[SKIP] ..."
lines, as the shell suites do, and the analysis subcommands exit 1 when any
check failed. Thresholds are options; the defaults are starting points to
calibrate against a known-good board, not product limits.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time

FULL_SCALE = 32768.0
DEFAULT_RATE = 16000
MCU_SOCKET = "/run/mcu_service/mcu_service.sock"
BLE_SOCKET = "/run/ble_service/ble_service.sock"
WAKE_SOURCES = ("external_power", "power_switch", "touch_key", "rtc_timer", "other", "asr")


class Report:
    """Collects check results with the same line format as the shell suites."""

    def __init__(self) -> None:
        self.fails = 0
        self.warns = 0

    def pass_(self, message: str) -> None:
        print(f"[PASS] {message}", flush=True)

    def warn(self, message: str) -> None:
        self.warns += 1
        print(f"[WARN] {message}", flush=True)

    def fail(self, message: str) -> None:
        self.fails += 1
        print(f"[FAIL] {message}", flush=True)

    def skip(self, message: str) -> None:
        print(f"[SKIP] {message}", flush=True)

    def info(self, message: str) -> None:
        print(f"    {message}", flush=True)

    def check(self, ok: bool, message: str, warn_only: bool = False) -> bool:
        if ok:
            self.pass_(message)
        elif warn_only:
            self.warn(message)
        else:
            self.fail(message)
        return ok

    def exit_code(self) -> int:
        return 1 if self.fails else 0


# ---------------------------------------------------------------- signals


def db(value: float) -> float:
    return 20.0 * math.log10(value) if value > 0 else -200.0


def tone(freq: float, ms: int, rate: int, amplitude: float) -> list[int]:
    count = int(rate * ms / 1000)
    fade = min(count // 2, int(rate * 0.005))  # 5 ms ramps avoid clicks
    out = []
    for i in range(count):
        gain = 1.0
        if fade and i < fade:
            gain = i / fade
        elif fade and i >= count - fade:
            gain = (count - 1 - i) / fade
        out.append(int(amplitude * gain * FULL_SCALE * math.sin(2 * math.pi * freq * i / rate)))
    return out


def silence(ms: int, rate: int) -> list[int]:
    return [0] * int(rate * ms / 1000)


def tone_plan(freqs: list[float], rate: int, amplitude: float, lead_ms: int, marker_ms: int,
              settle_ms: int, segment_ms: int, gap_ms: int) -> tuple[list[int], list[tuple[float, int, int]]]:
    """Silence, a 1 kHz marker burst, then one segment per frequency.

    Returns the samples and (freq, start, end) sample offsets of each segment,
    measured from the marker onset so a capture can be aligned on the marker.
    """
    samples = silence(lead_ms, rate)
    marker_start = len(samples)
    samples += tone(1000.0, marker_ms, rate, amplitude)
    samples += silence(settle_ms, rate)
    segments = []
    for freq in freqs:
        start = len(samples) - marker_start
        samples += tone(freq, segment_ms, rate, amplitude)
        segments.append((freq, start, start + int(rate * segment_ms / 1000)))
        samples += silence(gap_ms, rate)
    samples += silence(lead_ms, rate)
    return samples, segments


def pack_s16le(samples: list[int], channels: int = 1) -> bytes:
    clipped = [max(-32768, min(32767, s)) for s in samples]
    if channels == 1:
        return struct.pack(f"<{len(clipped)}h", *clipped)
    interleaved = [s for s in clipped for _ in range(channels)]
    return struct.pack(f"<{len(interleaved)}h", *interleaved)


def unpack_s16le(data: bytes, channels: int) -> list[list[int]]:
    usable = len(data) - len(data) % (2 * channels)
    values = struct.unpack(f"<{usable // 2}h", data[:usable])
    return [list(values[c::channels]) for c in range(channels)]


def rms(samples: list[int]) -> float:
    if not samples:
        return 0.0
    mean = sum(samples) / len(samples)
    return math.sqrt(sum((s - mean) ** 2 for s in samples) / len(samples))


_HANN: dict[int, list[float]] = {}


def goertzel_amplitude(samples: list[int], freq: float, rate: int) -> float:
    """Amplitude (in sample units) of a sinusoid at freq, with a Hann window.

    The window mean is removed first: these microphones can sit at a large DC
    offset, which would otherwise leak into low-frequency bins.
    """
    n = len(samples)
    if n < 8:
        return 0.0
    window = _HANN.get(n)
    if window is None:
        window = _HANN[n] = [0.5 - 0.5 * math.cos(2.0 * math.pi * i / (n - 1)) for i in range(n)]
    mean = sum(samples) / n
    coeff = 2.0 * math.cos(2.0 * math.pi * freq / rate)
    s1 = s2 = 0.0
    for x, w in zip(samples, window):
        s0 = (x - mean) * w + coeff * s1 - s2
        s2, s1 = s1, s0
    power = s1 * s1 + s2 * s2 - coeff * s1 * s2
    return 2.0 * math.sqrt(max(power, 0.0)) / (n * 0.5)  # Hann coherent gain 0.5


def window_rms_db(samples: list[int], rate: int, window_ms: int = 100) -> list[float]:
    size = max(1, int(rate * window_ms / 1000))
    return [db(rms(samples[i:i + size]) / FULL_SCALE) for i in range(0, len(samples) - size + 1, size)]


def channel_stats(samples: list[int], rate: int) -> dict:
    if not samples:
        return {"samples": 0}
    peak = max(abs(s) for s in samples)
    clipped = sum(1 for s in samples if s >= 32760 or s <= -32760)
    windows = window_rms_db(samples, rate)
    return {
        "samples": len(samples),
        "seconds": round(len(samples) / rate, 3),
        "rms_dbfs": round(db(rms(samples) / FULL_SCALE), 1),
        "peak_dbfs": round(db(peak / FULL_SCALE), 1),
        "dc_offset_pct": round(100.0 * (sum(samples) / len(samples)) / FULL_SCALE, 3),
        "clip_pct": round(100.0 * clipped / len(samples), 4),
        "noise_floor_dbfs": round(min(windows), 1) if windows else None,
        "loudest_window_dbfs": round(max(windows), 1) if windows else None,
        "all_zero": peak == 0,
        "constant": min(samples) == max(samples),
    }


def align_tone_plan(samples: list[int], rate: int, parts: list[tuple[float, int, int]],
                    min_rise_db: float = 10.0) -> int | None:
    """Sample offset at which the tone plan best matches the capture, or None.

    parts are (freq, start, end) offsets relative to the plan start. Each
    frequency gets a level track in 20 ms windows, expressed as dB above its
    own median (the noise in that bin); the offset maximizing the mean rise
    inside the expected windows wins. Unlike a broadband level onset this is
    not fooled by DC offset, start-up clicks or one weak or missing tone.
    """
    hop = max(8, rate // 50)
    hops = len(samples) // hop
    span = max(end for _, _, end in parts)
    if hops < 4 or len(samples) < span:
        return None
    rises: dict[float, list[float]] = {}
    for freq in sorted({f for f, _, _ in parts}):
        track = [goertzel_amplitude(samples[i * hop:(i + 1) * hop], freq, rate) for i in range(hops)]
        floor = max(sorted(track)[len(track) // 2], 1.0)
        prefix = [0.0]
        for level in track:
            prefix.append(prefix[-1] + (min(40.0, db(level / floor)) if level > floor else 0.0))
        rises[freq] = prefix
    best, best_offset, best_parts = -1.0, None, []
    for offset in range(0, hops - (span + hop - 1) // hop + 1):
        scores = []
        for freq, start, end in parts:
            a, b = offset + start // hop, offset + max(start // hop + 1, end // hop)
            prefix = rises[freq]
            scores.append((prefix[b] - prefix[a]) / (b - a))
        if sum(scores) > best:
            best, best_offset, best_parts = sum(scores), offset, scores
    if best_offset is None or max(best_parts) < min_rise_db:
        return None
    return best_offset * hop


def analyze_tone_plan(samples: list[int], rate: int, segments: list[tuple[float, int, int]],
                      onset: int, noise: list[int], trim_ms: int = 80) -> list[dict]:
    trim = int(rate * trim_ms / 1000)
    results = []
    for freq, start, end in segments:
        window = samples[onset + start + trim: onset + end - trim]
        if len(window) < rate // 20:
            results.append({"freq": freq, "missing": True})
            continue
        level = goertzel_amplitude(window, freq, rate)
        floor = goertzel_amplitude(noise[: len(window)], freq, rate) if noise else 0.0
        entry = {"freq": freq, "level_dbfs": round(db(level / FULL_SCALE), 1),
                 "snr_db": round(db(level) - db(max(floor, 1e-3)), 1)}
        harmonics = [goertzel_amplitude(window, freq * k, rate) for k in range(2, 6) if freq * k < rate / 2 - 100]
        if harmonics and level > 0:
            entry["thd_pct"] = round(100.0 * math.sqrt(sum(h * h for h in harmonics)) / level, 2)
        results.append(entry)
    return results


# ---------------------------------------------------------------- audio commands


def cmd_audio_gen(args: argparse.Namespace) -> int:
    amp = 10 ** (args.level_dbfs / 20.0)
    if args.kind == "tone":
        samples = tone(args.freq, args.ms, args.rate, amp)
    elif args.kind == "silence":
        samples = silence(args.ms, args.rate)
    else:
        samples, _ = tone_plan(parse_freqs(args.freqs), args.rate, amp, 500, 40, 400, args.segment_ms, 150)
    data = pack_s16le(samples, args.channels)
    if args.out == "-":
        sys.stdout.buffer.write(data)
    else:
        with open(args.out, "wb") as handle:
            handle.write(data)
    return 0


def parse_freqs(text: str) -> list[float]:
    return [float(f) for f in text.split(",") if f.strip()]


def cmd_audio_stats(args: argparse.Namespace) -> int:
    report = Report()
    with open(args.input, "rb") as handle:
        data = handle.read()
    channels = unpack_s16le(data, args.channels)
    label = args.label or "capture"
    if not data:
        report.fail(f"{label}: capture is empty")
        return report.exit_code()
    stats = [channel_stats(ch, args.rate) for ch in channels]
    for index, entry in enumerate(stats):
        name = f"{label} ch{index}" if args.channels > 1 else label
        report.info(f"{name}: " + json.dumps(entry, sort_keys=True))
        if entry["all_zero"]:
            report.fail(f"{name}: every sample is zero (dead input or muted path)")
            continue
        if entry["constant"]:
            if index > 0:
                # audio_service uses the left channel only.
                report.info(f"{name}: constant {channels[index][0]} (no microphone on this input); not analyzed")
            else:
                report.fail(f"{name}: stuck at {channels[index][0]} for the whole capture (dead input)")
            continue
        report.check(entry["clip_pct"] <= args.max_clip_pct,
                     f"{name}: clipping {entry['clip_pct']}% <= {args.max_clip_pct}%")
        report.check(abs(entry["dc_offset_pct"]) <= args.max_dc_pct,
                     f"{name}: DC offset {entry['dc_offset_pct']}% within +/-{args.max_dc_pct}%", warn_only=True)
        if args.expect == "quiet":
            floor = entry["noise_floor_dbfs"]
            report.check(floor <= args.max_noise_dbfs,
                         f"{name}: noise floor {floor} dBFS <= {args.max_noise_dbfs} dBFS")
            report.check(floor >= args.min_noise_dbfs,
                         f"{name}: noise floor {floor} dBFS >= {args.min_noise_dbfs} dBFS "
                         "(lower means a dead or digitally muted microphone)", warn_only=True)
        elif args.expect == "voice":
            rise = entry["loudest_window_dbfs"] - entry["noise_floor_dbfs"]
            report.check(rise >= args.min_voice_rise_db,
                         f"{name}: speech rises {round(rise, 1)} dB above the quietest window "
                         f"(>= {args.min_voice_rise_db} dB)")
    live = [s for s in stats if s.get("samples") and not s["constant"]]
    if args.channels > 1 and len(live) >= 2:
        spread = max(s["rms_dbfs"] for s in live) - min(s["rms_dbfs"] for s in live)
        report.check(spread <= args.max_channel_spread_db,
                     f"{label}: channel level spread {round(spread, 1)} dB <= {args.max_channel_spread_db} dB",
                     warn_only=True)
    return report.exit_code()


def wait_and_read(proc: subprocess.Popen, timeout: float) -> tuple[int, str]:
    try:
        _, err = proc.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        proc.kill()
        _, err = proc.communicate()
        return -1, "timed out"
    return proc.returncode, (err or b"").decode("utf-8", "replace").strip()


def cmd_audio_loopback(args: argparse.Namespace) -> int:
    report = Report()
    freqs = parse_freqs(args.freqs)
    amp = 10 ** (args.level_dbfs / 20.0)
    lead_ms, marker_ms, settle_ms, gap_ms = 600, 40, 400, 150
    plan, segments = tone_plan(freqs, args.rate, amp, lead_ms, marker_ms, settle_ms, args.segment_ms, gap_ms)
    play_seconds = len(plan) / args.rate
    capture_seconds = int(math.ceil(play_seconds + args.capture_lead_s + 1.5))
    workdir = tempfile.mkdtemp(prefix="aico_loopback_")
    play_path = os.path.join(workdir, "plan.pcm")
    capture_path = args.save or os.path.join(workdir, "capture.pcm")
    with open(play_path, "wb") as handle:
        handle.write(pack_s16le(plan, 1))

    if args.player == "service":
        cli = args.audio_cli
        if not cli or not os.access(cli, os.X_OK):
            report.skip(f"audio_service_cli not found ({cli}); cannot play through audio_service")
            return report.exit_code()
        play_cmd = [cli, "--socket", args.audio_socket, "play-stream", "--rate", str(args.rate), "--ch", "1", "--bits", "16"]
    else:
        if not shutil.which("aplay"):
            report.skip("aplay not installed; cannot play straight to ALSA")
            return report.exit_code()
        play_cmd = ["aplay", "-q", "-D", args.playback_device, "-t", "raw", "-f", "S16_LE", "-r", str(args.rate), "-c", "1", play_path]
    if not shutil.which("arecord"):
        report.skip("arecord not installed; cannot capture the speaker through the microphone")
        return report.exit_code()
    rec_cmd = ["arecord", "-q", "-D", args.capture_device, "-t", "raw", "-f", "S16_LE", "-r", str(args.rate),
               "-c", str(args.channels), "-d", str(capture_seconds), capture_path]

    rec = subprocess.Popen(rec_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    time.sleep(args.capture_lead_s)
    if rec.poll() is not None:
        _, err = wait_and_read(rec, 2)
        report.fail(f"arecord failed to start on {args.capture_device} (busy or wrong device?): {err}")
        return report.exit_code()
    play_started = time.monotonic()
    if args.player == "service":
        with open(play_path, "rb") as source:
            player = subprocess.Popen(play_cmd, stdin=source, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            play_rc, play_err = wait_and_read(player, play_seconds + 20)
    else:
        player = subprocess.Popen(play_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        play_rc, play_err = wait_and_read(player, play_seconds + 20)
    play_wall = time.monotonic() - play_started
    rec_rc, rec_err = wait_and_read(rec, capture_seconds + 10)

    report.check(play_rc == 0, f"playback via {args.player} finished (rc={play_rc}, {round(play_wall, 1)} s)"
                 + (f": {play_err}" if play_rc else ""))
    if rec_rc not in (0,):
        report.warn(f"arecord rc={rec_rc}: {rec_err}")
    try:
        with open(capture_path, "rb") as handle:
            data = handle.read()
    except OSError as exc:
        report.fail(f"capture missing: {exc}")
        return report.exit_code()
    if not data:
        report.fail("capture is empty")
        return report.exit_code()
    report.info(f"capture: {capture_path} ({len(data)} bytes)")

    channels = unpack_s16le(data, args.channels)
    summary = {"player": args.player, "channels": []}
    per_channel_ok = []
    parts = [(1000.0, 0, int(args.rate * marker_ms / 1000))] + segments
    for index, samples in enumerate(channels):
        name = f"mic ch{index}" if args.channels > 1 else "mic"
        if index > 0 and min(samples) == max(samples):
            # audio_service uses the left channel only; a constant right
            # channel means no microphone on that ADC input.
            report.info(f"{name}: constant {samples[0]} for the whole capture (no microphone on this input); not analyzed")
            continue
        onset = align_tone_plan(samples, args.rate, parts, min_rise_db=args.min_snr_db / 2)
        if onset is None:
            report.fail(f"{name}: no speaker signal detected (speaker, amplifier or microphone path silent)")
            per_channel_ok.append(False)
            continue
        # arecord starts some time after it is spawned, so this is approximate.
        latency_ms = (onset / args.rate - args.capture_lead_s - lead_ms / 1000.0) * 1000.0
        before = samples[int(args.rate * 0.2):max(0, onset - int(args.rate * 0.05))]
        after = samples[onset + segments[-1][2] + int(args.rate * 0.15):]
        noise = max(before, after, key=len)
        results = analyze_tone_plan(samples, args.rate, segments, onset, noise)
        ref = next((r for r in results if r.get("freq") == args.reference_freq and not r.get("missing")), None)
        if ref and ref["snr_db"] < args.min_snr_db:
            ref = None  # too weak to use as the reference for THD and response
        channel = {"index": index, "latency_ms": round(latency_ms, 1), "tones": results}
        summary["channels"].append(channel)
        report.info(f"{name}: tone plan found at {round(onset / args.rate, 3)} s, round trip ~{round(latency_ms)} ms "
                    "(approximate: arecord start time)")
        for r in results:
            if r.get("missing"):
                report.fail(f"{name}: {r['freq']:.0f} Hz segment missing from the capture")
                continue
            rel = f", {round(r['level_dbfs'] - ref['level_dbfs'], 1):+} dB vs {args.reference_freq:.0f} Hz" if ref else ""
            thd = f", THD {r['thd_pct']}%" if "thd_pct" in r and r["snr_db"] >= args.min_snr_db else ""
            line = f"{name}: {r['freq']:.0f} Hz level {r['level_dbfs']} dBFS, SNR {r['snr_db']} dB{rel}{thd}"
            in_band = args.band_low <= r["freq"] <= args.band_high
            if in_band:
                report.check(r["snr_db"] >= args.min_snr_db, line + f" (SNR >= {args.min_snr_db} dB)")
            else:
                report.info(line + " (outside the checked band)")
        if ref:
            report.check(ref.get("thd_pct", 0.0) <= args.max_thd_pct,
                         f"{name}: THD at {args.reference_freq:.0f} Hz {ref.get('thd_pct')}% <= {args.max_thd_pct}%",
                         warn_only=True)
            band = [r for r in results if not r.get("missing") and args.band_low <= r["freq"] <= args.band_high
                    and r["snr_db"] >= args.min_snr_db]
            if band:
                spread = max(r["level_dbfs"] for r in band) - min(r["level_dbfs"] for r in band)
                report.check(spread <= args.max_band_spread_db,
                             f"{name}: response spread {round(spread, 1)} dB across {args.band_low:.0f}-{args.band_high:.0f} Hz "
                             f"<= {args.max_band_spread_db} dB", warn_only=True)
        report.check(latency_ms <= args.max_latency_ms,
                     f"{name}: round-trip latency ~{round(latency_ms)} ms <= {args.max_latency_ms} ms", warn_only=True)
        per_channel_ok.append(True)
    if args.channels > 1 and len(summary["channels"]) >= 2:
        levels = []
        for channel in summary["channels"]:
            ref = next((r for r in channel["tones"] if r.get("freq") == args.reference_freq and not r.get("missing")), None)
            if ref:
                levels.append(ref["level_dbfs"])
        if len(levels) >= 2:
            spread = max(levels) - min(levels)
            report.check(spread <= args.max_channel_spread_db,
                         f"microphone channel balance at {args.reference_freq:.0f} Hz: {round(spread, 1)} dB "
                         f"<= {args.max_channel_spread_db} dB", warn_only=True)
    if args.json:
        with open(args.json, "w") as handle:
            json.dump(summary, handle, indent=2)
    if not args.save:
        shutil.rmtree(workdir, ignore_errors=True)
    return report.exit_code()


# ---------------------------------------------------------------- frames


def read_bmp(path: str) -> tuple[int, int, list[tuple[int, int, int]]]:
    """Return width, height and a sample grid (about 64x64) of RGB pixels."""
    with open(path, "rb") as handle:
        data = handle.read()
    if data[:2] != b"BM" or len(data) < 54:
        raise ValueError(f"{path} is not a BMP file")
    offset = struct.unpack_from("<I", data, 10)[0]
    width, height = struct.unpack_from("<ii", data, 18)
    bpp = struct.unpack_from("<H", data, 28)[0]
    if bpp not in (24, 32):
        raise ValueError(f"unsupported BMP depth {bpp}")
    top_down = height < 0
    height = abs(height)
    stride = ((width * bpp // 8) + 3) & ~3
    step_x = max(1, width // 64)
    step_y = max(1, height // 64)
    pixels = []
    for y in range(0, height, step_y):
        row = y if top_down else height - 1 - y
        base = offset + row * stride
        for x in range(0, width, step_x):
            i = base + x * (bpp // 8)
            if i + 2 < len(data):
                b, g, r = data[i], data[i + 1], data[i + 2]
                pixels.append((r, g, b))
    return width, height, pixels


def luma(pixel: tuple[int, int, int]) -> float:
    r, g, b = pixel
    return 0.299 * r + 0.587 * g + 0.114 * b


def cmd_frame_stats(args: argparse.Namespace) -> int:
    report = Report()
    try:
        width, height, pixels = read_bmp(args.image)
    except (OSError, ValueError) as exc:
        report.fail(f"cannot read screenshot: {exc}")
        return report.exit_code()
    values = [luma(p) for p in pixels]
    mean = sum(values) / len(values)
    std = math.sqrt(sum((v - mean) ** 2 for v in values) / len(values))
    black = sum(1 for v in values if v < 16) / len(values)
    colors = len({(r >> 4, g >> 4, b >> 4) for r, g, b in pixels})
    report.info(f"screenshot {width}x{height}: luma mean {round(mean, 1)}, std {round(std, 1)}, "
                f"black {round(100 * black, 1)}%, colors(4-bit) {colors}")
    if args.expect_size:
        report.check(f"{width}x{height}" == args.expect_size, f"screenshot size {width}x{height} == {args.expect_size}")
    report.check(black < args.max_black, f"screenshot is not black ({round(100 * black, 1)}% black pixels < {round(100 * args.max_black)}%)")
    report.check(std >= args.min_std and colors >= args.min_colors,
                 f"screenshot has image content (luma std {round(std, 1)} >= {args.min_std}, colors {colors} >= {args.min_colors})",
                 warn_only=True)
    if args.compare:
        try:
            w2, h2, other = read_bmp(args.compare)
        except (OSError, ValueError) as exc:
            report.fail(f"cannot read second screenshot: {exc}")
            return report.exit_code()
        if (w2, h2) != (width, height):
            report.info(f"second screenshot size differs: {w2}x{h2}")
            changed = 1.0
        else:
            changed = sum(1 for a, b in zip(pixels, other) if abs(luma(a) - luma(b)) > 12) / max(1, len(pixels))
        report.info(f"changed between screenshots: {round(100 * changed, 1)}% of sampled pixels")
        if args.expect_change:
            report.check(changed >= args.min_change, f"screenshots differ after the source image changed ({round(100 * changed, 1)}%)")
    return report.exit_code()


# ---------------------------------------------------------------- sockets


def uds_request(path: str, header: dict, timeout: float = 30.0, payload_length: int = 0) -> dict:
    """MCU/BLE service framing: <u32 header length><u64 payload length><JSON header>."""
    body = json.dumps(header).encode("utf-8")
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as peer:
        peer.settimeout(timeout)
        peer.connect(path)
        peer.sendall(struct.pack("<IQ", len(body), payload_length) + body)

        def receive(count: int) -> bytes:
            data = b""
            while len(data) < count:
                block = peer.recv(count - len(data))
                if not block:
                    raise ConnectionError("service closed the connection")
                data += block
            return data

        size, extra = struct.unpack("<IQ", receive(12))
        response = json.loads(receive(size))
        if extra:
            receive(extra)
        return response


MCU_MUTATING_OPS = {"set_wakeup_timer", "cancel_wakeup_timer", "set_led"}


def cmd_mcu(args: argparse.Namespace) -> int:
    request = {"op": args.op}
    for key in ("led", "mode", "since", "generation"):
        value = getattr(args, key)
        if value:
            request[key] = value
    for key in ("on_ms", "off_ms", "lease_s", "delay_s", "limit", "wait_ms"):
        value = getattr(args, key)
        if value:
            request[key] = value
    if args.op in MCU_MUTATING_OPS:
        # The service rejects writes without a request id or with a stale
        # service generation (src/agent/internal/mcu/service.go).
        request["request_id"] = f"aico-hwtest-{os.getpid()}-{time.time_ns()}"
        if not args.generation:
            try:
                state = uds_request(args.socket, {"op": "get_state"}, timeout=10.0)
            except OSError as exc:
                print(json.dumps({"status": "CLIENT_ERROR", "error": str(exc)}))
                return 1
            generation = (state.get("state") or {}).get("generation")
            if generation:
                request["generation"] = generation
    try:
        response = uds_request(args.socket, request, timeout=max(10.0, args.wait_ms / 1000.0 + 10))
    except OSError as exc:
        print(json.dumps({"status": "CLIENT_ERROR", "error": str(exc)}))
        return 1
    print(json.dumps(response, sort_keys=True))
    return 0 if response.get("status") == "OK" else 1


def cmd_mcu_events(args: argparse.Namespace) -> int:
    """Wait for MCU events; pass when every expected type/source was seen."""
    report = Report()
    expected = [e for e in args.expect.split(",") if e]
    seen: dict[str, int] = {}
    try:
        first = uds_request(args.socket, {"op": "events_since", "since": "0", "limit": 1})
    except OSError as exc:
        report.fail(f"MCU service socket {args.socket}: {exc}")
        return report.exit_code()
    if first.get("status") != "OK":
        report.fail(f"MCU events_since failed: {first}")
        return report.exit_code()
    cursor, generation = first.get("last_id", "0"), first.get("generation", "")
    if args.prompt:
        print(f">>> {args.prompt}", flush=True)
    deadline = time.monotonic() + args.seconds
    while time.monotonic() < deadline and not all(seen.get(e) for e in expected):
        wait_ms = int(min(20000, max(100, (deadline - time.monotonic()) * 1000)))
        try:
            page = uds_request(args.socket, {"op": "events_since", "since": cursor, "generation": generation,
                                             "limit": 50, "wait_ms": wait_ms}, timeout=wait_ms / 1000 + 10)
        except OSError as exc:
            report.fail(f"MCU events_since: {exc}")
            break
        if page.get("reset_required"):
            generation = page.get("generation", "")
            cursor = page.get("last_id", "0")
            report.warn("MCU event stream reset (service restarted?)")
            continue
        for event in page.get("events") or []:
            key = event.get("source") or event.get("type")
            if event.get("type") == "rk628_plugin_det_changed":
                key = "hdmi_inserted" if event.get("inserted") else "hdmi_removed"
            seen[key] = seen.get(key, 0) + 1
            report.info(f"event {event.get('id')}: {json.dumps(event, sort_keys=True)}")
        cursor = page.get("next_id", cursor)
    report.info(f"events seen: {json.dumps(seen, sort_keys=True) if seen else 'none'}")
    for name in expected:
        report.check(bool(seen.get(name)), f"MCU reported {name} within {args.seconds} s")
    return report.exit_code()


def cmd_ble_status(args: argparse.Namespace) -> int:
    try:
        response = uds_request(args.socket, {"op": args.op})
    except OSError as exc:
        print(json.dumps({"status": "CLIENT_ERROR", "error": str(exc)}))
        return 1
    print(json.dumps(response, sort_keys=True))
    return 0 if response.get("status") == "OK" else 1


# ---------------------------------------------------------------- evidence


def sha256_file(path: str) -> str:
    import hashlib
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def read_text(path: str) -> str:
    try:
        with open(path, "r", errors="replace") as handle:
            return handle.read().strip()
    except OSError:
        return ""


def run_text(cmd: list[str], timeout: float = 20) -> str:
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        return ""


def board_identity() -> dict:
    import hashlib
    serial = read_text("/proc/device-tree/serial-number").strip("\x00")
    if not serial:
        for line in read_text("/proc/cpuinfo").splitlines():
            if line.lower().startswith("serial"):
                serial = line.split(":", 1)[-1].strip()
    machine_id = read_text("/etc/machine-id")
    macs = {}
    for name in ("wlan0", "usb0", "eth0"):
        mac = read_text(f"/sys/class/net/{name}/address")
        if mac:
            macs[name] = mac
    return {
        "hostname": socket.gethostname(),
        "boot_id": read_text("/proc/sys/kernel/random/boot_id"),
        "serial": serial,
        "machine_id_sha256": hashlib.sha256(machine_id.encode()).hexdigest()[:16] if machine_id else "",
        "mac_addresses": macs,
        "model": read_text("/proc/device-tree/model").strip("\x00"),
        "kernel": " ".join(os.uname()),
        "aico_release": read_text("/etc/aico-release"),
        "packages": run_text(["dpkg-query", "-W", "-f=${Package} ${Version}\\n", "aico*"]).splitlines(),
        "uptime_s": read_text("/proc/uptime").split(" ")[0] if read_text("/proc/uptime") else None,
        "operator": os.environ.get("SUDO_USER") or os.environ.get("USER") or "",
    }


def cmd_evidence_finalize(args: argparse.Namespace) -> int:
    """Write run.json, kernel.log, SHA256SUMS and a tarball for a run directory."""
    import tarfile
    run_dir = os.path.abspath(args.run_dir)
    start_epoch = read_text(os.path.join(run_dir, ".start_epoch"))
    finished = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    events = []
    try:
        with open(os.path.join(run_dir, "events.jsonl")) as handle:
            for line in handle:
                try:
                    events.append(json.loads(line))
                except ValueError:
                    pass
    except OSError:
        pass
    suites: dict[str, dict] = {}
    for event in events:
        suite = suites.setdefault(event.get("suite") or "?", {"PASS": 0, "WARN": 0, "FAIL": 0, "SKIP": 0,
                                                                "first_event": event.get("time"), "last_event": None})
        if event.get("result") in ("PASS", "WARN", "FAIL", "SKIP"):
            suite[event["result"]] += 1
        suite["last_event"] = event.get("time")
        if event.get("result") == "END":
            suite["exit"] = event.get("message", "")
    for name, suite in suites.items():
        failed_exit = suite.get("exit", "exit=0") != "exit=0"
        suite["result"] = "FAIL" if suite["FAIL"] or failed_exit else ("WARN" if suite["WARN"] else "PASS")

    if start_epoch:
        kernel = run_text(["journalctl", "-k", "--no-pager", "-o", "short-iso", "--since", f"@{start_epoch}"], timeout=30)
        if not kernel:
            kernel = run_text(["dmesg", "-T"], timeout=30)
        with open(os.path.join(run_dir, "kernel.log"), "w") as handle:
            handle.write(kernel + "\n")

    scripts = {}
    for name in sorted(os.listdir(args.script_dir)):
        path = os.path.join(args.script_dir, name)
        if os.path.isfile(path) and (name.endswith(".sh") or name.endswith(".py")):
            scripts[name] = sha256_file(path)

    artifacts = {}
    for root, _dirs, files in os.walk(run_dir):
        for name in sorted(files):
            if name in ("run.json", "SHA256SUMS") or name.startswith("."):
                continue
            path = os.path.join(root, name)
            artifacts[os.path.relpath(path, run_dir)] = {"bytes": os.path.getsize(path), "sha256": sha256_file(path)}

    manifest = {
        "run_id": args.run_id,
        "entry": args.entry,
        "command": args.command,
        "started_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(int(start_epoch))) if start_epoch else None,
        "finished_utc": finished,
        "exit_code": args.exit_code,
        "board": board_identity(),
        "suites": suites,
        "event_count": len(events),
        "scripts_sha256": scripts,
        "artifacts": artifacts,
    }
    with open(os.path.join(run_dir, "run.json"), "w") as handle:
        json.dump(manifest, handle, indent=2, sort_keys=True)

    lines = []
    for root, _dirs, files in os.walk(run_dir):
        for name in sorted(files):
            if name == "SHA256SUMS" or name.startswith("."):
                continue
            path = os.path.join(root, name)
            lines.append(f"{sha256_file(path)}  {os.path.relpath(path, run_dir)}")
    with open(os.path.join(run_dir, "SHA256SUMS"), "w") as handle:
        handle.write("\n".join(lines) + "\n")

    tar_path = run_dir.rstrip("/") + ".tar.gz"
    with tarfile.open(tar_path, "w:gz") as archive:
        for root, _dirs, files in os.walk(run_dir):
            for name in sorted(files):
                if name.startswith("."):
                    continue
                path = os.path.join(root, name)
                archive.add(path, arcname=os.path.join(os.path.basename(run_dir), os.path.relpath(path, run_dir)))

    print("\n================ evidence ================")
    board = manifest["board"]
    print(f"run id      {args.run_id}  ({manifest['started_utc']} -> {finished}, exit {args.exit_code})")
    print(f"board       serial={board['serial'] or '?'} boot_id={board['boot_id'] or '?'} host={board['hostname']}")
    for name, suite in sorted(suites.items()):
        print(f"suite       {name:<12} {suite['result']:<5} pass={suite['PASS']} warn={suite['WARN']} "
              f"fail={suite['FAIL']} skip={suite['SKIP']}  {suite['first_event']} .. {suite['last_event']}")
    print(f"events      {len(events)} timestamped entries in events.jsonl")
    print(f"directory   {run_dir}  ({len(artifacts)} files, SHA256SUMS)")
    print(f"bundle      {tar_path}")
    print(f"bundle sha256 {sha256_file(tar_path)}")
    return 0


# ---------------------------------------------------------------- self-test


def _fake_service(path: str, responses: list[dict], seen: list[dict]) -> threading.Thread:
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(path)
    server.listen(len(responses))

    def run() -> None:
        for response in responses:
            conn, _ = server.accept()
            with conn:
                size, extra = struct.unpack("<IQ", conn.recv(12))
                body = b""
                while len(body) < size:
                    body += conn.recv(size - len(body))
                seen.append(json.loads(body))
                data = json.dumps(response).encode()
                conn.sendall(struct.pack("<IQ", len(data), 0) + data)
        server.close()

    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    return thread


def cmd_self_test(_args: argparse.Namespace) -> int:
    failures = []

    def expect(condition: bool, message: str) -> None:
        if not condition:
            failures.append(message)

    rate = DEFAULT_RATE
    sig = tone(1000.0, 500, rate, 0.5)
    expect(abs(db(goertzel_amplitude(sig, 1000.0, rate) / FULL_SCALE) - db(0.5)) < 1.0, "goertzel level of a -6 dBFS tone")
    expect(goertzel_amplitude(sig, 3000.0, rate) < goertzel_amplitude(sig, 1000.0, rate) / 100, "goertzel rejects other bins")

    distorted = [int(0.5 * FULL_SCALE * (math.sin(2 * math.pi * 1000 * i / rate) + 0.1 * math.sin(2 * math.pi * 2000 * i / rate)))
                 for i in range(rate // 2)]
    thd = analyze_tone_plan(distorted, rate, [(1000.0, 0, len(distorted))], 0, [0] * 2000)[0]["thd_pct"]
    expect(8.0 < thd < 12.0, f"THD of a 10% second harmonic, got {thd}")

    amp = 10 ** (-12 / 20.0)
    plan, segments = tone_plan([500.0, 1000.0, 2000.0], rate, amp, 600, 40, 400, 400, 150)
    delay = int(rate * 0.120)
    noise_level = 30
    captured = [s // 4 + ((i * 7919) % (2 * noise_level + 1) - noise_level) for i, s in enumerate([0] * delay + plan)]
    # A DC offset and a start-up click must not break alignment.
    captured = [v + 6000 for v in captured]
    captured[:40] = [-32768] * 40
    parts = [(1000.0, 0, int(rate * 0.04))] + segments
    onset = align_tone_plan(captured, rate, parts)
    expect(onset is not None and abs(onset - (delay + int(rate * 0.6))) <= rate // 50, f"tone plan aligned, got {onset}")
    expect(align_tone_plan([3] * len(captured), rate, parts) is None, "silent capture has no tone plan")
    results = analyze_tone_plan(captured, rate, segments, onset or 0, captured[:2000])
    expect(all(r["snr_db"] > 30 for r in results), f"tone plan SNR, got {[r.get('snr_db') for r in results]}")

    stats = channel_stats([0] * rate, rate)
    expect(stats["all_zero"], "all-zero capture detected")
    stats = channel_stats([32767 if i % 2 else -32768 for i in range(rate)], rate)
    expect(stats["clip_pct"] > 90, "clipping detected")

    two = pack_s16le([1, 2, 3], 2)
    expect(unpack_s16le(two, 2) == [[1, 2, 3], [1, 2, 3]], "stereo pack/unpack round trip")

    with tempfile.TemporaryDirectory() as directory:
        bmp = os.path.join(directory, "a.bmp")
        width, height = 32, 16
        stride = (width * 3 + 3) & ~3
        rows = b"".join(bytes(sum(([(x * 8) % 256, (y * 16) % 256, 128] for x in range(width)), [])) + b"\0" * (stride - width * 3)
                        for y in range(height))
        header = b"BM" + struct.pack("<IHHI", 54 + len(rows), 0, 0, 54) + struct.pack("<IiiHHIIiiII", 40, width, height, 1, 24, 0, len(rows), 0, 0, 0, 0)
        with open(bmp, "wb") as handle:
            handle.write(header + rows)
        w, h, pixels = read_bmp(bmp)
        expect((w, h) == (32, 16) and len(pixels) == 32 * 16, f"BMP sampling, got {w}x{h} {len(pixels)}")

        sock = os.path.join(directory, "mcu.sock")
        seen: list[dict] = []
        thread = _fake_service(sock, [{"status": "OK", "battery": {"percent": 80}}], seen)
        reply = uds_request(sock, {"op": "get_battery"}, timeout=5)
        thread.join(5)
        expect(reply.get("battery", {}).get("percent") == 80 and seen == [{"op": "get_battery"}], "UDS framing round trip")

    if failures:
        for message in failures:
            print(f"[FAIL] self-test: {message}")
        return 1
    print("[PASS] board_hw_tools self-test")
    return 0


# ---------------------------------------------------------------- CLI


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("audio-gen", help="write a test signal as s16le PCM")
    p.add_argument("--kind", choices=("tone", "plan", "silence"), default="tone")
    p.add_argument("--freq", type=float, default=1000.0)
    p.add_argument("--freqs", default="500,1000,2000,4000")
    p.add_argument("--ms", type=int, default=2000)
    p.add_argument("--segment-ms", type=int, default=500)
    p.add_argument("--level-dbfs", type=float, default=-12.0)
    p.add_argument("--rate", type=int, default=DEFAULT_RATE)
    p.add_argument("--channels", type=int, default=1)
    p.add_argument("--out", default="-")
    p.set_defaults(func=cmd_audio_gen)

    p = sub.add_parser("audio-stats", help="analyze an s16le capture")
    p.add_argument("input")
    p.add_argument("--rate", type=int, default=DEFAULT_RATE)
    p.add_argument("--channels", type=int, default=1)
    p.add_argument("--label", default="")
    p.add_argument("--expect", choices=("quiet", "voice", "any"), default="any")
    p.add_argument("--max-noise-dbfs", type=float, default=-45.0)
    p.add_argument("--min-noise-dbfs", type=float, default=-95.0)
    p.add_argument("--min-voice-rise-db", type=float, default=15.0)
    p.add_argument("--max-clip-pct", type=float, default=0.1)
    p.add_argument("--max-dc-pct", type=float, default=2.0)
    p.add_argument("--max-channel-spread-db", type=float, default=6.0)
    p.set_defaults(func=cmd_audio_stats)

    p = sub.add_parser("audio-loopback", help="speaker-to-microphone measurement")
    p.add_argument("--player", choices=("alsa", "service"), default="alsa")
    p.add_argument("--playback-device", default="default")
    p.add_argument("--capture-device", default="default")
    p.add_argument("--audio-cli", default="/usr/lib/aico/audio_service_cli")
    p.add_argument("--audio-socket", default="/run/audio_service/audio_service.sock")
    p.add_argument("--rate", type=int, default=DEFAULT_RATE)
    p.add_argument("--channels", type=int, default=1)
    p.add_argument("--freqs", default="300,500,1000,2000,3000,4000,6000")
    p.add_argument("--reference-freq", type=float, default=1000.0)
    p.add_argument("--segment-ms", type=int, default=500)
    p.add_argument("--level-dbfs", type=float, default=-12.0)
    p.add_argument("--capture-lead-s", type=float, default=0.8)
    p.add_argument("--band-low", type=float, default=500.0)
    p.add_argument("--band-high", type=float, default=4000.0)
    p.add_argument("--min-snr-db", type=float, default=15.0)
    p.add_argument("--max-thd-pct", type=float, default=10.0)
    p.add_argument("--max-band-spread-db", type=float, default=20.0)
    p.add_argument("--max-latency-ms", type=float, default=600.0)
    p.add_argument("--max-channel-spread-db", type=float, default=6.0)
    p.add_argument("--save", default="", help="keep the capture at this path")
    p.add_argument("--json", default="", help="write the measurements as JSON")
    p.set_defaults(func=cmd_audio_loopback)

    p = sub.add_parser("frame-stats", help="screenshot content statistics")
    p.add_argument("image")
    p.add_argument("--compare", default="")
    p.add_argument("--expect-change", action="store_true")
    p.add_argument("--min-change", type=float, default=0.02)
    p.add_argument("--expect-size", default="")
    p.add_argument("--max-black", type=float, default=0.98)
    p.add_argument("--min-std", type=float, default=4.0)
    p.add_argument("--min-colors", type=int, default=8)
    p.set_defaults(func=cmd_frame_stats)

    p = sub.add_parser("mcu", help="one MCU service request")
    p.add_argument("op")
    p.add_argument("--socket", default=MCU_SOCKET)
    p.add_argument("--led", default="")
    p.add_argument("--mode", default="")
    p.add_argument("--on-ms", type=int, default=0)
    p.add_argument("--off-ms", type=int, default=0)
    p.add_argument("--lease-s", type=int, default=0)
    p.add_argument("--delay-s", type=int, default=0)
    p.add_argument("--since", default="")
    p.add_argument("--generation", default="")
    p.add_argument("--limit", type=int, default=0)
    p.add_argument("--wait-ms", type=int, default=0)
    p.set_defaults(func=cmd_mcu)

    p = sub.add_parser("mcu-events", help="wait for MCU hardware events")
    p.add_argument("--socket", default=MCU_SOCKET)
    p.add_argument("--expect", default="", help="comma list: " + ",".join(WAKE_SOURCES) + ",hdmi_inserted,hdmi_removed")
    p.add_argument("--seconds", type=int, default=30)
    p.add_argument("--prompt", default="")
    p.set_defaults(func=cmd_mcu_events)

    p = sub.add_parser("ble-status", help="ble_service status (or another request with --op)")
    p.add_argument("--socket", default=BLE_SOCKET)
    p.add_argument("--op", choices=("status", "pairing_start"), default="status")
    p.set_defaults(func=cmd_ble_status)

    p = sub.add_parser("evidence-finalize", help="write run.json, kernel.log, SHA256SUMS and a tarball")
    p.add_argument("--run-dir", required=True)
    p.add_argument("--run-id", required=True)
    p.add_argument("--entry", default="")
    p.add_argument("--exit-code", type=int, default=0)
    p.add_argument("--script-dir", default=os.path.dirname(os.path.abspath(__file__)))
    p.add_argument("command", nargs=argparse.REMAINDER)
    p.set_defaults(func=cmd_evidence_finalize)

    p = sub.add_parser("self-test", help="local checks of the analysis code")
    p.set_defaults(func=cmd_self_test)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
