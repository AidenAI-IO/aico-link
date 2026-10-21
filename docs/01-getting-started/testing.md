---
sidebar_position: 7
---

# Testing and Validation

## Docker Test Entry Point

The host is only used as a Docker client. The compiler, test runners, Python
packages, Node.js, Go toolchain, and ARM cross compiler are supplied by the
pinned `docker/test/Dockerfile` image.

For local iterations, run the **quick feedback** profile:

```bash
make check
```

It runs shared contracts, C++ host tests (the `aiden_tests` target), Go format/vet
and focused contract tests, Web tests, and a small benchmark sample plus SkillOpt.
It intentionally does **not** run every Go test, all 879 benchmark cases, the
long-running shell/release tests, or the 30-second CTest watchdog check.
It is not a substitute for the full test gate. Before merging, run:

```bash
make check-full
```

CI runs `make check-full` on every PR, including the suites omitted by the quick
profile. `make test` is also an alias for the full check. To run one suite while
iterating, use `bash scripts/run_tests_in_docker.sh --suite go-unit` (or another
name from `bash scripts/run_tests_in_docker.sh --list`). All commands still run
inside the same Docker test image.

Docker-backed package tests that need the host Docker socket are explicit and
separate from the normal manifest because the socket grants access to the
Docker daemon:

```bash
make check-docker
```

The Linux CI runner additionally uses `--docker-socket --host-network` to run
`docker-sandbox-smoke` inside the test image. These options are explicit and
not part of local `make check` or `make check-full`.

The suite list and quick commands live in [`tests/test-manifest.yaml`](../../tests/test-manifest.yaml).
A selected required suite with a missing dependency or an unexpected skip fails
the runner. The JSON result at `output/test-summary.json` records the profile
and the suites actually executed.

The production cross-build smoke test uses the vendored `pico-sdk/` sources
and requires the pinned RKNN archive. The Docker smoke runner downloads and
checksums the pinned OpenCV-Mobile source when it is not already cached:

```bash
make check-production
```

Tests cover UDS messaging, Frame Service protocol, Audio Service protocol, Ring Buffer, Wi-Fi / Agent TOML configuration parsing, image processing, and other modules.

## Frame Service Validation

After the service is running:

```bash
frame_service_cli --socket /run/frame_service/frame_service.sock health
frame_service_cli --socket /run/frame_service/frame_service.sock screenshot --out /tmp/screenshot.bmp
frame_service_cli --socket /run/frame_service/frame_service.sock latest-frame --out /tmp/frame.raw
frame_service_cli --socket /run/frame_service/frame_service.sock list-frames
```

For temporary service execution on development machines, the default socket is `/tmp/frame_service.sock`:

```bash
/usr/lib/aiden/frame_service --socket /tmp/frame_service.sock
frame_service_cli --socket /tmp/frame_service.sock health
```

## Audio Service Validation

```bash
audio_service_cli --socket /run/audio_service/audio_service.sock health
audio_service_cli --socket /run/audio_service/audio_service.sock get-volume
audio_service_cli --socket /run/audio_service/audio_service.sock set-volume --volume 80
```

Record to PCM:

```bash
audio_service_cli --socket /run/audio_service/audio_service.sock record-stream --seconds 3 > /tmp/record.pcm
```

Play PCM:

```bash
cat /tmp/record.pcm | audio_service_cli --socket /run/audio_service/audio_service.sock play-stream --rate 16000 --ch 1 --bits 16
```

## USB HID Validation

```bash
sudo example_usb_hid setup composite
sudo example_usb_hid keyboard tap ENTER
sudo example_usb_hid keyboard text "hello from pico"
sudo example_usb_hid touch click 16000 16000
```

## Agent Validation

Check the service on the device:

```bash
systemctl status aiden-agent.service --no-pager
```

The Agent Web UI is available in every input mode. Open:

```text
http://<device-ip>:8080
```

Tool API:

```bash
curl http://<device-ip>:8080/api/tools
curl -X POST http://<device-ip>:8080/api/tools/shell \
  -H 'Content-Type: application/json' \
  -d '{"input":{"command":"pwd"}}'
```

## Audio Model Roundtrip Script

`scripts/test_audio_roundtrip.sh` is used to generate audio via OpenRouter audio model, then send the audio back as input for recognition:

```bash
export OPENROUTER_KEY=sk-or-...
./scripts/test_audio_roundtrip.sh
```

Optionally set proxy:

```bash
export HTTP_PROXY=http://127.0.0.1:7890
export HTTPS_PROXY=http://127.0.0.1:7890
```
