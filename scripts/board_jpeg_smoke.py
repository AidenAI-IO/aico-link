#!/usr/bin/env python3
"""Exercise frame_service JPEG requests on a board using only Python stdlib.

The summary describes protocol correctness and request latency. Confirm hardware
execution separately using encoder driver counters and the frame_service log.
Saved raw/JPEG samples can be copied to a host for full image decoding and visual
comparison; the board-side JPEG checks validate markers and dimensions only.
"""

import argparse
import concurrent.futures
import datetime
import json
import math
import pathlib
import socket
import statistics
import struct
import sys
import time


SOF_MARKERS = frozenset((0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7,
                         0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF))
VARIANTS = (
    ("full", {}),
    ("center608", {"crop_black": True, "minimal_width": 608}),
    ("center610", {"crop_black": True, "minimal_width": 610}),
    ("portrait", {"crop_black": True, "screen_width": 1080,
                  "screen_height": 1920}),
    ("landscape", {"crop_black": True, "screen_width": 1920,
                   "screen_height": 1080}),
    ("blackbars", {"crop_black": True}),
)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def recv_exact(connection, length):
    data = bytearray()
    while len(data) < length:
        chunk = connection.recv(min(length - len(data), 1024 * 1024))
        if not chunk:
            raise ValueError("truncated UDS response: expected %d bytes, got %d" %
                             (length, len(data)))
        data.extend(chunk)
    return bytes(data)


def request(socket_path, fields, timeout):
    header = json.dumps(dict(type="request", **fields),
                        separators=(",", ":")).encode("utf-8")
    started = time.perf_counter()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(timeout)
        connection.connect(socket_path)
        connection.sendall(struct.pack("<IQ", len(header), 0) + header)
        header_size, payload_size = struct.unpack("<IQ", recv_exact(connection, 12))
        require(0 < header_size <= 1024 * 1024, "invalid response header length")
        require(payload_size <= 128 * 1024 * 1024, "response payload exceeds 128 MiB")
        response = json.loads(recv_exact(connection, header_size).decode("utf-8"))
        payload = recv_exact(connection, payload_size)
    elapsed_ms = (time.perf_counter() - started) * 1000.0
    require(isinstance(response, dict), "response header is not an object")
    require(response.get("type") == "response", "invalid response type")
    require(response.get("method") == fields["method"], "response method mismatch")
    require(response.get("status") == "OK", "service response: %s" % response)
    return response, payload, elapsed_ms


def jpeg_dimensions(data):
    require(data.startswith(b"\xff\xd8"), "JPEG SOI marker missing")
    require(data.endswith(b"\xff\xd9"), "JPEG EOI marker missing")
    position = 2
    dimensions = None
    while position < len(data) - 2:
        require(data[position] == 0xFF, "invalid JPEG marker boundary")
        while position < len(data) and data[position] == 0xFF:
            position += 1
        require(position < len(data), "truncated JPEG marker")
        marker = data[position]
        position += 1
        require(marker not in (0x00, 0xD8, 0xD9), "unexpected JPEG header marker")
        if marker == 0x01 or 0xD0 <= marker <= 0xD7:
            continue
        require(position + 2 <= len(data), "truncated JPEG segment length")
        length = struct.unpack_from(">H", data, position)[0]
        require(length >= 2 and position + length <= len(data) - 2,
                "truncated JPEG segment")
        if marker in SOF_MARKERS:
            require(length >= 8, "short JPEG SOF segment")
            height, width = struct.unpack_from(">HH", data, position + 3)
            components = data[position + 7]
            require(width > 0 and height > 0 and components > 0,
                    "invalid JPEG SOF dimensions/components")
            require(length == 8 + 3 * components, "invalid JPEG SOF segment length")
            require(dimensions is None, "multiple JPEG SOF segments")
            dimensions = (width, height)
        if marker == 0xDA:
            require(dimensions is not None, "JPEG SOS precedes SOF")
            require(position + length < len(data) - 2, "JPEG scan data missing")
            return dimensions
        position += length
    raise ValueError("JPEG SOS/SOF markers missing")


def integer_field(metadata, key, minimum=0):
    value = metadata.get(key)
    require(isinstance(value, (int, str)) and not isinstance(value, bool),
            "missing/invalid frame field: %s" % key)
    parsed = int(value)
    require(parsed >= minimum, "invalid frame field: %s=%s" % (key, value))
    return parsed


def validate_frame(response, payload, requested_format):
    metadata = response.get("frame")
    require(isinstance(metadata, dict), "response frame metadata missing")
    width = integer_field(metadata, "width", 1)
    height = integer_field(metadata, "height", 1)
    integer_field(metadata, "seq", 1)
    integer_field(metadata, "capture_ts_ns", 1)
    require(integer_field(metadata, "bytes", 1) == len(payload),
            "frame byte count differs from payload length")
    require(metadata.get("stale") is False, "frame is stale or stale flag missing")
    if requested_format == "jpeg":
        require(metadata.get("pixel_format") == "jpeg", "expected JPEG pixel format")
        require(jpeg_dimensions(payload) == (width, height),
                "JPEG SOF dimensions differ from metadata")
        require(integer_field(metadata, "stride") == 0, "JPEG stride must be zero")
        source_width = integer_field(metadata, "source_width", 1)
        source_height = integer_field(metadata, "source_height", 1)
        crop_x = integer_field(metadata, "crop_x")
        crop_y = integer_field(metadata, "crop_y")
        require(integer_field(metadata, "crop_width", 1) == width and
                integer_field(metadata, "crop_height", 1) == height,
                "JPEG crop dimensions differ from frame dimensions")
        require(crop_x + width <= source_width and crop_y + height <= source_height,
                "JPEG crop exceeds source dimensions")
    else:
        pixel_format = metadata.get("pixel_format")
        require(pixel_format in ("nv12", "nv16", "uyvy", "yuyv"),
                "unsupported raw pixel format: %s" % pixel_format)
        require(width % 2 == 0, "YUV frame width must be even")
        if pixel_format == "nv12":
            require(height % 2 == 0, "NV12 frame height must be even")
        expected_bytes = width * height * (3 if pixel_format == "nv12" else 4) // 2
        require(len(payload) == expected_bytes, "raw frame is not tightly packed")
        require(integer_field(metadata, "stride", 1) ==
                width * (2 if pixel_format in ("uyvy", "yuyv") else 1),
                "raw frame stride differs from compact layout")
        for plane in metadata.get("planes", []):
            require(integer_field(plane, "offset") + integer_field(plane, "bytes", 1)
                    <= len(payload), "raw plane exceeds payload bounds")
    return metadata


def latency_summary(records):
    values = sorted(record["latency_ms"] for record in records if record.get("ok"))
    if not values:
        return {"count": 0}
    return {"count": len(values), "min_ms": values[0],
            "mean_ms": statistics.mean(values), "median_ms": statistics.median(values),
            "p95_ms": values[max(0, math.ceil(len(values) * 0.95) - 1)],
            "max_ms": values[-1]}


def run(args):
    output = pathlib.Path(args.output_dir)
    output.mkdir(parents=True, exist_ok=True)
    records = []
    summary = {"started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
               "socket": args.socket, "iterations": args.iterations,
               "first_format": args.first_format,
               "hardware_execution_verified": False,
               "validation": "UDS metadata and JPEG markers; full decode required on host",
               "requests": records}
    baseline = None

    def capture(label, variant, quality=80, sample=False, raw=False):
        name, options = variant
        fields = dict(method="latest_frame", since_seq="0", timeout_ms=5000,
                      format="raw" if raw else "jpeg", quality=quality, **options)
        record = {"label": label, "variant": name, "request": fields, "ok": False}
        started = time.perf_counter()
        try:
            response, payload, elapsed_ms = request(args.socket, fields, args.timeout)
            record["latency_ms"] = elapsed_ms
            record["response"] = response
            metadata = validate_frame(response, payload, fields["format"])
            if baseline is not None:
                source = (metadata["width"], metadata["height"])
                if not raw or options.get("crop_black"):
                    source = (integer_field(metadata, "source_width"),
                              integer_field(metadata, "source_height"))
                require(source == baseline,
                        "source resolution changed during JPEG smoke test")
                if name == "full":
                    require((metadata["width"], metadata["height"]) == baseline,
                            "full JPEG frame was unexpectedly cropped")
                if options.get("minimal_width"):
                    require(metadata["width"] == min(options["minimal_width"], baseline[0]) and
                            metadata["height"] == baseline[1],
                            "%s frame dimensions are incorrect" % name)
            if sample:
                sample_path = output / (label + (".raw" if raw else ".jpg"))
                sample_path.write_bytes(payload)
                sample_path.with_suffix(".json").write_text(
                    json.dumps(response, indent=2) + "\n", encoding="utf-8")
                record["sample"] = sample_path.name
            record["ok"] = True
        except (OSError, ValueError, TypeError, struct.error) as error:
            record["latency_ms"] = (time.perf_counter() - started) * 1000.0
            record["error"] = str(error)
        return record

    try:
        health, payload, _ = request(args.socket, {"method": "health"}, args.timeout)
        require(not payload, "health response has an unexpected payload")
        summary["health_before"] = health
        if args.first_format == "jpeg":
            records.append(capture("cold_q80_full", VARIANTS[0], sample=True))
        record = capture("baseline", VARIANTS[0], sample=True, raw=True)
        records.append(record)
        require(record["ok"], "raw baseline failed: %s" % record.get("error"))
        frame = record["response"]["frame"]
        baseline = (frame["width"], frame["height"])
        if args.first_format == "jpeg" and records[0]["ok"]:
            cold_frame = records[0]["response"]["frame"]
            require((cold_frame["width"], cold_frame["height"]) == baseline,
                    "cold JPEG dimensions differ from raw baseline")

        for quality in (30, 80, 95):
            for variant in VARIANTS:
                records.append(capture("q%d_%s" % (quality, variant[0]), variant,
                                       quality=quality, sample=True))
        for iteration in range(args.iterations):
            for variant in VARIANTS:
                records.append(capture("alternate_%03d_%s" % (iteration, variant[0]),
                                       variant))
        with concurrent.futures.ThreadPoolExecutor(max_workers=3) as executor:
            pending = [executor.submit(capture, "concurrent_%02d_%s" % (i, variant[0]),
                                       variant, raw=i % 7 == 0)
                       for i, variant in enumerate(VARIANTS * 3)]
            records.extend(future.result() for future in pending)
        health, payload, _ = request(args.socket, {"method": "health"}, args.timeout)
        require(not payload, "health response has an unexpected payload")
        summary["health_after"] = health
    except (OSError, ValueError, TypeError, struct.error) as error:
        summary["error"] = str(error)
    summary["latency"] = latency_summary(records)
    summary["latency_by_variant"] = {
        name: latency_summary([record for record in records
                               if record["variant"] == name and
                               record["request"]["format"] == "jpeg"])
        for name, _ in VARIANTS}
    summary["failures"] = sum(not record["ok"] for record in records)
    summary["ok"] = not summary.get("error") and summary["failures"] == 0
    summary["finished_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    (output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n",
                                         encoding="utf-8")
    print(json.dumps({key: summary[key] for key in
                      ("ok", "failures", "latency", "latency_by_variant")}))
    print("Saved samples and summary: %s" % output.resolve())
    if summary.get("error"):
        print(summary["error"], file=sys.stderr)
    for record in records:
        if not record["ok"]:
            print("%s: %s" % (record["label"], record.get("error")), file=sys.stderr)
    return 0 if summary["ok"] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", default="/run/frame_service/frame_service.sock")
    parser.add_argument("--output-dir", default="/tmp/aiden-jpeg-smoke")
    parser.add_argument("--first-format", choices=("jpeg", "raw"), default="jpeg",
                        help="capture JPEG before the raw baseline for cold-start testing (default: jpeg)")
    parser.add_argument("--iterations", type=int, default=10,
                        help="number of full/crop alternation cycles (default: 10)")
    parser.add_argument("--timeout", type=float, default=30.0,
                        help="per-request socket timeout in seconds (default: 30)")
    args = parser.parse_args()
    if args.iterations < 1 or args.timeout <= 0 or not math.isfinite(args.timeout):
        parser.error("iterations and timeout must be positive")
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
