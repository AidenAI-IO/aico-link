---
sidebar_position: 5
---

# Build and Development Environment

## Clone the Project

```bash
git clone --recursive git@github.com:AidenAI-IO/aiden-firmware.git
cd aiden-firmware
```

The project includes Git submodules such as `pico-sdk` and MobileGym. It's recommended to use `--recursive` on the first clone. For an existing checkout, synchronize submodule URLs before initializing or updating them:

```bash
git submodule sync --recursive
git submodule update --init --recursive
```

## Docker Test Build

The host is only a Docker client. Build and test toolchains run inside the
pinned `docker/test/Dockerfile` image:

```bash
make check        # short local smoke/profile, Docker-only
make check-full   # all required suites; same gate as CI
```

`make check` is for short development iterations; it does not replace the full
CI check. The suite selection and commands live in
[`tests/test-manifest.yaml`](../../tests/test-manifest.yaml).
Full production ARM smoke validation is available after the SDK submodule is
initialized; the Docker smoke image prepares the pinned OpenCV-Mobile input when
needed:

```bash
make check-production
```

The quick and full profiles compile and audit the TF card device tree from
`pico-sdk`, so initialize that submodule before running either profile:

```bash
git submodule update --init --depth=1 pico-sdk
```

Full hardware targets depend on Rockchip/Luckfox armhf libraries. Use the Debian
cross-compilation workflow for device-runnable artifacts.

The Debian BSP stage also rebuilds the vendor media modules against the
configured kernel. Its compatibility helpers handle the `struct page` layout
used by `CONFIG_MEMCG`, which is required for hardware JPEG encoding. The
assembly and helpers are staged under `output/debian-system/media-compat/`;
the SDK sources remain unchanged. Deploy the rebuilt `mpp_vcodec.ko` and
`rockit.ko` with the matching kernel. Updating only the application bundle on
an older platform keeps JPEG on software until these modules are updated.

## Debian ARM Cross-Compilation

Build and audit the Debian armhf application bundle with:

```bash
scripts/debian-apps/build-apps.sh all
```

Git worktrees are supported. Build containers mount the checkout at both `/work`
and its host path so Git can resolve the SDK submodule's `core.worktree` path
when recording source provenance.

This workflow will:

1. Build the pinned Debian armhf toolchain container and opencv-mobile;
2. Compile C/C++ programs using `cmake/toolchains/armhf-debian.cmake`;
3. Install/use Go 1.26.0;
4. Cross-compile the Go Agent and BLE daemon for `linux/arm GOARM=7`;
5. Audit the application and shared-library bundle under `output/debian-apps/`.

Use `./debian_build.sh` for the complete signed local firmware image set. It
builds applications, the RV1106 BSP, the Debian rootfs with its business package, A/B images, and
the local OTA manifest in one workflow.

```bash
./debian_build.sh
```

Rootfs packaging preserves executable permissions from `overlay-debian/`.
Keep helpers invoked by systemd executable in Git (`100755`). The required
`debian-systemd-overlay` test suite checks these permissions, and the image
audit checks the installed Wi-Fi log retention helper because both Wi-Fi
driver startup and `wpa_supplicant` run it through `ExecStartPre`.

To build and stage only the business `.deb` (default `0.0.1-2`), run
`scripts/debian-package/release.sh build` on Linux amd64. See
[Debian business package](../08-ota/debian-package.md) for independent GitHub
Release publication and installation.

### Debian archive mirror

Both builder images and the device rootfs fetch Debian packages from
`http://mirrors.ustc.edu.cn/debian` and `/debian-security`, declared once in
`scripts/debian-apps/debian.sources` and `scripts/debian-system/debian.sources`
(the latter is also installed into the device rootfs, so on-device `apt` uses
the same mirror; `container-build-rootfs.sh` reads the debootstrap URI from it).
Plain http is deliberate: apt verifies every InRelease signature against the
Debian archive keyring, so https adds nothing to package integrity, and it
keeps the fetches cacheable by an http proxy. Measured from Shanghai,
`mirrors.aliyun.com` throttles HTTP/1.1 clients such as apt and debootstrap to
about 300 kB/s (only its HTTP/2 path is fast, which curl-based speed tests
show and apt never gets), while ustc serves apt at 5-10 MB/s.

This is a live mirror, not a snapshot.debian.org timestamp: `trixie-security`
moves daily, so two builds of the same commit can differ in package versions.
Each build records what it consumed in
`output/debian-system/build-metadata.json` (`debian_mirror`,
`debian_release_version`, `debian_release_date`, `debian_updates_date`,
`debian_security_date`) and in `packages.txt`.

### Optional CI APT Cache

The reusable GitHub Actions workflow accepts an `apt_cache_proxy` input, or
uses the repository variable `DEBIAN_SYSTEM_APT_CACHE_PROXY` when the input is
empty. Set it to an HTTP cache proxy URL reachable from the rootfs container,
such as `http://172.17.0.1:3128` on a runner with that Docker bridge gateway.
No cache is selected by default, and the workflow does not install a proxy.

The rootfs builder downloads the mirror's `trixie` `InRelease` through the
candidate proxy, verifies its Debian archive signature and checks its codename
before selecting it. The probe has a 20-second limit. An unavailable or invalid
cache leaves the original download settings in place. Existing `http_proxy`,
`HTTP_PROXY`, `all_proxy`, or `ALL_PROXY` settings take precedence; HTTPS proxy
settings remain unchanged. This validation happens before rootfs creation and
does not provide automatic fallback if an enabled cache fails later.

## macOS Apple Silicon + Colima

On Apple Silicon, it's recommended to use a native `aarch64` Colima VM and run the Luckfox image via Docker with `--platform linux/amd64`:

```bash
brew install docker docker-buildx colima
colima start --vm-type vz --vz-rosetta

docker buildx version
docker buildx ls

./debian_build.sh
```

Do not start a `--arch x86_64` Colima VM for this workflow; keep the native VM and let the container run as `linux/amd64`.

## Makefile Shortcuts

```bash
make build          # Dockerized ARM production smoke build
make clean          # Remove generated build/
make check          # Quick Dockerized local feedback
make check-fast     # Alias for make check
make check-full     # All required suites, also run by CI
make test           # Alias for make check-full
make check-docker   # Docker-backed package contract tests
make check-production  # Dockerized ARM production smoke test
make test-clean     # Remove generated build-host/
```

`make check` and `make check-full` both build `docker/test/Dockerfile` and
execute `tests/test-manifest.yaml`; only their suite profiles differ. No host
compiler or host language runtime is required. CI always uses the full profile.

## Main Build Targets

| Target | Description |
| --- | --- |
| `libaiden.a` | C++ hardware SDK and service common library |
| `libaiden_image.a` | Image processing library |
| `frame_service` / `frame_service_cli` | HDMI frame capture service and CLI |
| `audio_service` / `audio_service_cli` | Audio recording/playback service and CLI |
| `image_process` | Image processing CLI |
| `example_*` | Wake word, audio, camera, USB HID examples |
| `agent` | Go Agent daemon, including the runtime `config-web` subcommand; additionally built by the application cross-build task |
| `ble_service` | Go BlueZ GATT/ANCS daemon, additionally built by the application cross-build task |
