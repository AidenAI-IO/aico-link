---
sidebar_position: 4
---

# Paths, Artifacts, and Configuration Quick Reference

## Build Artifacts

| Path | Description |
| --- | --- |
| `build-host/bin/aico_tests` | Host-native C++ test binary |
| `output/debian-apps/apps/bin/` | Cross-compiled application and diagnostic binaries |
| `output/debian-apps/apps/lib/` | Runtime libraries staged into rootfs platform/lib |
| `output/debian-apps/apps-audit/` | ELF, dependency, and allowlist audit results |
| `output/debian-system/rootfs.ext4` | Reproducible Debian armhf rootfs image |
| `output/debian-system/image/` | Audited system partition and factory images |
| `output/debian/image/update.img` | Final directly flashable local firmware |
| `output/debian/image/manifest.json` | Locally signed OTA manifest |

## Important Source Paths

| Path | Description |
| --- | --- |
| `src/aico_sdk.h` | C++ SDK API |
| `src/frame_service_main.cpp` | Frame Service entry point |
| `src/audio_service_main.cpp` | Audio Service entry point |
| `src/agent/internal/configweb/` | Go Config Web implementation (served by `agent config-web`) |
| `src/config_web/web/` | Config Web HTML, CSS, and JavaScript assets |
| `src/system_env_*` | Strict persistent-to-runtime environment generation |
| `src/agent/cmd/daemon` | Go Agent daemon |
| `src/agent/cmd/ota` | OTA CLI |
| `src/agent/cmd/abctl` | A/B metadata diagnostic tool |
| `src/agent/internal/agent` | Agent runtime and tools |
| `src/agent/internal/ota` | OTA download, slot, health, and state machine |
| `overlay-debian/` | Debian rootfs overlay and systemd integration |
| `assets/business/` | Business package models and notification sounds |
| `overlay-debian/usr/lib/aico/platform/lib/` | Platform runtime libraries, including the VQE AEC/beamforming libs registered by `aico-platform-ldconfig` |
| `scripts/debian-apps/` | Application cross-build and audit |
| `scripts/debian-system/` | Rootfs, BSP, image assembly, and audit |
| `tests/` | Host-native C++ tests |

## Device Default Paths

| Path | Description |
| --- | --- |
| `/usr/lib/aico/` | Packaged applications and base-owned service helpers |
| `/usr/lib/aico/platform/lib/` | Vendor and application runtime libraries |
| `/usr/lib/aico/models/` | VAD models and weights |
| `/usr/share/aico/config-web/` | Config Web static assets served by `agent config-web` |
| `/userdata/agent/agent.toml` | Agent configuration |
| `/userdata/agent/python/` | Persistent pip userbase |
| `/userdata/agent/skills/` | Agent skills |
| `/userdata/agent/memory/` | Agent memory |
| `/userdata/agent/log/` | Agent and LLM HTTP logs |
| `/userdata/system/env` | Persistent device environment source |
| `/userdata/system/wifi-proxies.json` | Mode and optional upstream proxy URL selected per saved Wi-Fi SSID |
| `/run/aico/system.env` | Validated runtime environment |
| `/userdata/debian/wifi/wpa_supplicant-wlan0.conf` | Wi-Fi configuration |
| `/userdata/debian/ota/config.json` | Debian OTA repository and factory baseline |
| `/userdata/ota/` | Dedicated OTA state and download partition |
| `/usr/share/keyrings/aico-ota.pem` | OTA manifest Ed25519 public key |
| `/run/frame_service/frame_service.sock` | Frame service socket |
| `/run/audio_service/audio_service.sock` | Audio service socket |
| `/run/ble_service/ble_service.sock` | BLE service socket |
| `/run/mcu_service/mcu_service.sock` | MCU service socket |
| `/run/aico/agent-mcu-wakeup.json` | Agent ASR event cursor, retained across Agent restarts within a boot |
| `/run/aico-mcu-poweroff/intent.json` | Current-boot shutdown intent, retained until final poweroff |
| `/run/aico/storage.state` | StorageManager state shared by Config Web and Agent |
| `/run/agent/storage_level` | Current StorageMonitor level |

Service sockets under `/run/*_service/` are mode `0660 root:aico`. The audio and
frame units declare `Group=aico` and `SupplementaryGroups=audio video`, so the
plain `aico` user can run the diagnostic CLIs against them. A socket that shows
up as `0755 root:root`, or a client that gets `TRANSPORT_ERROR`, means the unit
lost those group settings or the process ran under an unexpected umask.

Login shells for both `root` and `aico` append `/usr/lib/aico` to `PATH` via
`/etc/profile.d/aico-path.sh`. `/etc/sudoers.d/20-aico-path` also includes it in
sudo's `secure_path`, so installed tools can be called by name, including
`sudo ota status`. Reconnect SSH or the Web terminal after installing this
configuration; an existing shell can load it with
`. /etc/profile.d/aico-path.sh`. Child shells inherit the exported path.

## Configuration Sources

| File | Description |
| --- | --- |
| `overlay-debian/etc/aico_boot.conf` | Product feature gates |
| `overlay-debian/etc/aico_frame_service.conf` | Frame launch and capture policy |
| `overlay-debian/etc/aico_audio_service.conf` | Audio socket and volume state |
| `overlay-debian/etc/aico_ble_service.conf` | BLE service parameters |
| `overlay-debian/etc/systemd/system/` | Debian service and mount units |
| `overlay-debian/etc/systemd/network/` | systemd-networkd configuration |
| `overlay-debian/etc/profile.d/aico-python.sh` | Fixed persistent Python userbase |
| `assets/business/models/` | Business VAD models |
| `assets/business/audio/` | Business notification audio |
| `overlay-debian/usr/share/aico/audio/config_aivqe.json` | Platform VQE configuration |
| `overlay-debian/etc/ld.so.conf.d/aico-platform.conf` | Registers platform libraries from the active slot |
| `AGENT_CONFIG_PATH` | External Agent configuration required by image assembly |
| `OTA_PUBLIC_KEY_PATH` | External OTA verification key required by rootfs and image assembly |

## Common Commands

```bash
# Build and test (all compiler/test commands run in Docker)
make check             # Quick local feedback
make check-full        # Complete CI test gate
make check-production  # ARM production smoke
scripts/debian-apps/build-apps.sh all
./debian_build.sh

# Flash on Linux (after entering Loader/Maskrom)
FLASH_TOOL=pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool
IMAGE=output/debian/image/update.img
scripts/flash.sh inspect --tool "${FLASH_TOOL}"
sudo scripts/flash.sh flash --tool "${FLASH_TOOL}" \
  --image "${IMAGE}" --sha256 "$(awk '{print $1}' "${IMAGE}.sha256")" \
  --confirm-erase-all-data

# Flash on macOS
./upgrade_tool/upgrade_tool uf ./output/debian/image/update.img

# Service status
systemctl status aico-frame.service --no-pager
systemctl status aico-audio.service --no-pager
systemctl status aico-wifi-proxy.service --no-pager
systemctl status aico-agent.service --no-pager
systemctl --failed

# Frame debugging
frame_service_cli --socket /run/frame_service/frame_service.sock health
frame_service_cli --socket /run/frame_service/frame_service.sock screenshot --out /tmp/screenshot.bmp

# Audio debugging
audio_service_cli --socket /run/audio_service/audio_service.sock health
audio_service_cli --socket /run/audio_service/audio_service.sock get-volume

# Config Web management API
curl http://<device-ip>/api/storage/status

# Agent runtime API
curl http://<device-ip>:8080/api/tools
curl http://<device-ip>:8080/api/storage/monitor/status
curl -X POST -H 'Content-Type: application/json' -d '{"force":false,"targets":[]}' http://<device-ip>:8080/api/storage/cleanup

# OTA
/usr/lib/aico/ota status
/usr/lib/aico/ota update
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc

# Logs
tail -f /userdata/agent/log/agent.log
journalctl -u aico-frame.service -u aico-agent.service
```

The diagnostic CLIs are apps artifacts and are not generally included in the business package; `frame_service_cli` and
`audio_service_cli` are included for OTA health checks. Copy a required CLI to `/userdata` for a bounded device test.

## Persistent Logs

| Path | Description |
| --- | --- |
| `/userdata/agent/log/agent.log` | Agent supervisor and runtime output; capped by `aico-agent-log-retention.timer` |
| `/userdata/agent/log/llm-http-*.log` | Session-partitioned LLM HTTP JSONL |
| `/var/log/frame_service/frame_service.log` | Frame service output |
| `/var/log/audio_service/audio_service.log` | Audio service output |
| `/var/log/ble_service/ble_service.log` | BLE service output |
| `/var/log/ota/ota.log` | OTA health output |
| `/var/log/adb/adb-startup.log` | adb host startup output |

When StorageMonitor reports `critical` or `emergency`, the Agent runtime
trims managed logs and Python temporary data according to the configured
policy under `storage_settings.storage.degraded_mode`.

## EDID Files

Development EDIDs live in `edid/`. The Debian rootfs installs its production
TC358743 EDID at:

```text
/usr/share/aico/edid/hdmi_1080p30_cta.hex
```
