#!/usr/bin/env python3
"""Validate a flashed MCU through mcu_service; never flash or send raw power commands."""

import argparse
import hashlib
import json
from pathlib import Path
import socket
import struct
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


class ServiceError(RuntimeError):
    def __init__(self, response):
        self.status = response.get("status")
        super().__init__(json.dumps(response))


def read_exact(connection, size):
    result = bytearray()
    while len(result) < size:
        chunk = connection.recv(size - len(result))
        if not chunk:
            raise RuntimeError("MCU service disconnected mid-response")
        result.extend(chunk)
    return bytes(result)


class Client:
    def __init__(self, socket_path, url=None):
        self.socket_path, self.url = socket_path, url.rstrip("/") if url else None

    def request(self, operation, **fields):
        request = {"op": operation, **fields}
        http_status = None
        timeout = fields.get("wait_ms", 0) / 1000 + 5
        if self.url:
            routes = {
                "get_state": ("GET", "/state"), "get_battery": ("GET", "/battery"),
                "events_since": ("GET", "/events"), "get_wakeup_timer": ("GET", "/wakeup-timer"),
                "set_wakeup_timer": ("PUT", "/wakeup-timer"), "cancel_wakeup_timer": ("DELETE", "/wakeup-timer"),
                "get_led": ("GET", "/leds/" + fields.get("led", "")),
                "set_led": ("PUT", "/leds/" + fields.get("led", "")),
            }
            method, path = routes[operation]
            if operation == "events_since":
                path += "?" + urllib.parse.urlencode(fields)
            body = json.dumps(fields).encode() if method in ("PUT", "DELETE") else None
            req = urllib.request.Request(self.url + "/api/mcu" + path, data=body, method=method,
                                         headers={"Content-Type": "application/json"})
            try:
                response = urllib.request.urlopen(req, timeout=timeout)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                http_status = response.status
                raw = response.read(256 * 1024 + 1)
        else:
            data = json.dumps(request).encode()
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
                connection.settimeout(timeout)
                connection.connect(self.socket_path)
                connection.sendall(struct.pack("<IQ", len(data), 0) + data)
                header_len, payload_len = struct.unpack("<IQ", read_exact(connection, 12))
                if not 0 < header_len <= 256 * 1024 or payload_len:
                    raise RuntimeError("invalid MCU UDS envelope")
                raw = read_exact(connection, header_len)
        if len(raw) > 256 * 1024:
            raise RuntimeError("oversized MCU response")
        try:
            response = json.loads(raw)
        except ValueError as error:
            if http_status == 503:
                raise ServiceError({"status": "SERVICE_UNAVAILABLE", "error": "HTTP 503 during Agent shutdown"}) from error
            raise RuntimeError("non-JSON MCU response") from error
        if not isinstance(response, dict):
            raise RuntimeError("MCU response is not a JSON object")
        if response.get("status") != "OK":
            raise ServiceError(response)
        return response


def mutate(client, generation, operation, **fields):
    # Never retry a timed-out mutation with a fresh ID. Reconcile explicitly.
    return client.request(operation, generation=generation, request_id=str(uuid.uuid4()), **fields)


def wait_for_source(client, cursor, generation, source, timeout, allow_disconnect=False):
    deadline = time.monotonic() + timeout
    disconnected = False
    initial_generation = generation
    events = []
    while time.monotonic() < deadline:
        try:
            page = client.request("events_since", since=cursor, generation=generation,
                                  limit=100, wait_ms=min(1000, max(0, int((deadline-time.monotonic())*1000))))
        except (OSError, ServiceError, urllib.error.URLError) as error:
            if not allow_disconnect or (isinstance(error, ServiceError) and error.status not in ("SERVICE_UNAVAILABLE", "NOT_READY")):
                raise
            disconnected = True
            time.sleep(min(1, max(0, deadline-time.monotonic())))
            continue
        if page["reset_required"]:
            cursor, generation = "0", ""
            continue
        if page["truncated"]:
            raise RuntimeError("event history was truncated; test result is inconclusive")
        generation, cursor = page["generation"], page["next_id"]
        events.extend(page["events"])
        for event in page["events"]:
            if event.get("type") == "wakeup" and event.get("source") == source:
                if allow_disconnect and (not disconnected or generation == initial_generation):
                    raise RuntimeError("timer fired without disconnect and a new service generation; power-cycle test did not occur")
                return {"event": event, "generation": generation, "disconnect_observed": disconnected,
                        "events_observed": events}
    raise RuntimeError("timed out waiting for WAKEUP source " + source)


def event_cursor(client):
    page = client.request("events_since", since="0", limit=100, wait_ms=0)
    # A test starts after all existing history, deliberately skipping old actions.
    return page["last_id"], page["generation"]


def validate(client, args, report=None):
    state = client.request("get_state")["state"]
    if state["rv_state"] != "ready" or not state["uart_connected"]:
        raise RuntimeError("MCU service is not ready: " + json.dumps(state))
    generation = state["generation"]
    if report is None:
        report = {}
    report.update(state=state, checks=[])
    report["battery"] = client.request("get_battery")["battery"]
    report["timer"] = client.request("get_wakeup_timer")["timer"]
    for led in ("red", "white"):
        try:
            report[led + "_led"] = client.request("get_led", led=led)["led"]
        except ServiceError as error:
            if error.status != "UNSUPPORTED" or args.require_led or args.exercise_led:
                raise
            report[led + "_led"] = {"supported": False}
    report["checks"].append("state, battery, timer and LED capability queries")
    if args.exercise_led:
        if report["white_led"]["requested_mode"] != "auto":
            raise RuntimeError("white LED already has an application override; refusing to replace it")
        try:
            led = mutate(client, generation, "set_led", led="white", mode="blink",
                         on_ms=200, off_ms=800, lease_s=2)["led"]
            if led["requested_mode"] != "blink":
                raise RuntimeError("MCU did not accept LED blink")
            time.sleep(2.2)
            if client.request("get_led", led="white")["led"]["requested_mode"] != "auto":
                raise RuntimeError("LED lease did not expire")
            report["checks"].append("white LED blink configuration and automatic lease expiry")
        finally:
            prior_error = sys.exc_info()[0] is not None
            try:
                mutate(client, generation, "set_led", led="white", mode="auto")
            except (OSError, RuntimeError) as error:
                report.setdefault("cleanup_errors", []).append(str(error))
                if not prior_error:
                    raise
    if args.exercise_timer or args.power_cycle:
        if report["timer"]["active"]:
            raise RuntimeError("wake timer already active; refusing to overwrite an application timer")
        cursor, event_generation = event_cursor(client)
        delay = args.wake_delay if args.power_cycle else 2
        try:
            timer = mutate(client, generation, "set_wakeup_timer", delay_s=delay)["timer"]
            if not timer["active"] or not 0 < timer["remaining_s"] <= delay:
                raise RuntimeError("timer was not armed")
            if args.power_cycle:
                print("Timer armed. Keep the maintained switch ON and run sudo /usr/lib/aiden/mcu_service poweroff on the board. "
                      "Keep this host running; switch OFF cancels the timer and inhibits every wake source.", flush=True)
            report["timer_wake"] = wait_for_source(client, cursor, event_generation, "rtc_timer",
                                                   delay + 90 if args.power_cycle else 10, args.power_cycle)
            timer = client.request("get_wakeup_timer")["timer"]
            if timer["active"] or timer["remaining_s"] != 0:
                raise RuntimeError("expired one-shot timer remains active")
            report["checks"].append("timer expiry and source-bearing WAKEUP" +
                                    (" across observed disconnect/reconnect" if args.power_cycle else " while RV running"))
        finally:
            prior_error = sys.exc_info()[0] is not None
            try:
                # A reboot changes the daemon generation. Read it before cleanup.
                current = client.request("get_state")["state"]["generation"]
                mutate(client, current, "cancel_wakeup_timer")
            except (OSError, RuntimeError) as error:
                report.setdefault("cleanup_errors", []).append(str(error))
                if not prior_error:
                    raise
    if args.expect_source:
        cursor, event_generation = event_cursor(client)
        print("Waiting for a physical " + args.expect_source + " wake trigger.", flush=True)
        report["physical_wake"] = wait_for_source(client, cursor, event_generation,
                                                 args.expect_source, args.timeout)
    return report


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", default="/run/mcu_service/mcu_service.sock")
    parser.add_argument("--url", help="Agent URL on USB ECM or loopback, e.g. http://192.168.42.1:8080")
    parser.add_argument("--report", type=Path, required=True, help="JSON report path; use /tmp on the board")
    parser.add_argument("--image", type=Path, help="Expected MCU .bin; records its SHA256, not proof of flashing")
    parser.add_argument("--require-led", action="store_true")
    parser.add_argument("--exercise-led", action="store_true", help="Temporarily override white LED and verify lease expiry")
    parser.add_argument("--exercise-timer", action="store_true", help="Arm an unused two-second timer and observe WAKEUP")
    parser.add_argument("--expect-source", choices=("touch_key", "asr", "rtc_timer", "external_power", "power_switch", "other"))
    parser.add_argument("--timeout", type=int, default=60)
    parser.add_argument("--power-cycle", action="store_true", help="Host-side coordinated software-shutdown/timer-wake scenario; keep the switch ON")
    parser.add_argument("--allow-power-cycle", action="store_true", help="Acknowledge the power-removal scenario")
    parser.add_argument("--wake-delay", type=int, default=120)
    args = parser.parse_args(argv)
    if args.power_cycle and (not args.allow_power_cycle or not args.url):
        parser.error("--power-cycle requires --allow-power-cycle and --url; run it from the external host")
    if not 60 <= args.wake_delay <= 604800 or not 1 <= args.timeout <= 3600:
        parser.error("wake-delay must be 60..604800 and timeout 1..3600")
    return args


def main(argv=None):
    args = arguments(argv)
    report = {"started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
              "hardware_timing_measured": False}
    try:
        if args.image:
            report["expected_mcu_image_sha256"] = hashlib.sha256(args.image.read_bytes()).hexdigest()
        validate(Client(args.socket, args.url), args, report)
        report["result"] = "passed"
    except (OSError, ValueError, RuntimeError, KeyError, urllib.error.URLError) as error:
        report.update(result="failed", error=str(error))
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    return 0 if report["result"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
