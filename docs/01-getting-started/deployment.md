---
sidebar_position: 6
---

# Deployment to Device

## Complete Debian Firmware

The production deployment path is a complete Debian image. Build the audited
application bundle, Debian rootfs, BSP, A/B images, and signed local OTA
metadata with:

```bash
./debian_build.sh
```

The directly flashable image is:

```text
output/debian/image/update.img
```

On Linux, flash it with the guarded helper below. It checks the image digest,
requires Loader/Maskrom mode, and makes the destructive userdata overwrite
explicit:

```bash
FLASH_TOOL=pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool
IMAGE=output/debian/image/update.img
SHA256=$(awk '{print $1}' "${IMAGE}.sha256")
scripts/flash.sh inspect --tool "${FLASH_TOOL}"
sudo scripts/flash.sh flash \
  --tool "${FLASH_TOOL}" \
  --image "${IMAGE}" \
  --sha256 "${SHA256}" \
  --confirm-erase-all-data
```

The repository-root `upgrade_tool/upgrade_tool` is a macOS Mach-O binary; use
the Linux tool from the repository `pico-sdk` submodule. On macOS, flash a
locally built image with:

```bash
./upgrade_tool/upgrade_tool uf ./output/debian/image/update.img
```

The image installs these main runtime trees:
```text
/usr/lib/aiden/                                  # Audited production applications
/usr/lib/aiden/platform/lib/                                  # Vendor and application libraries
/usr/lib/aiden/models/                                # VAD model and weights
/usr/share/aiden/                          # Config Web, skills, audio, and EDID assets
/usr/lib/aiden/                                # Debian rootfs service helpers
/etc/systemd/system/aiden-*.service            # Product systemd units
/userdata/agent/agent.toml                     # External build-time Agent configuration
/userdata/debian/wifi/wpa_supplicant-wlan0.conf
/userdata/debian/ota/config.json               # Debian OTA factory configuration
```

The Pico Zero Debian device tree does not enable the vendor
`restart-poweroff` node. A kernel poweroff therefore follows the board's
native power-management path instead of deliberately rebooting through U-Boot.
This applies after rebuilding and flashing the BSP boot images; an existing
board keeps the device tree from its currently installed image.

`overlay-debian/` owns the Debian platform additions. The rootfs stage installs
`aiden-business`, BSP libraries and modules, and the OTA trust key. Business models
and notification sounds come from `assets/business/`. Both rootfs slots start with
the same image; no OEM filesystem is produced.

## Development Binary Update

Build and audit applications without rebuilding the firmware:

```bash
scripts/debian-apps/build-apps.sh all
```

Production binaries are under `output/debian-apps/apps/bin/`. For a temporary
device-side test, stop the owning systemd unit before replacing its binary:

```bash
ssh root@<device-ip> 'systemctl stop aiden-frame.service'
scp output/debian-apps/apps/bin/frame_service root@<device-ip>:/usr/lib/aiden/frame_service
ssh root@<device-ip> 'chmod 0755 /usr/lib/aiden/frame_service && systemctl start aiden-frame.service'
```

Use the same pattern for `audio_service`, `ble_service`, or `agent`. The Agent
binary also provides the `config-web` subcommand. Copying a binary directly mutates only the active rootfs slot and can
invalidate the factory hash expected by OTA diagnostics, so use it for short
development cycles only. Rebuild and flash a complete image for a reproducible
deployment.

`frame_service_cli` and `audio_service_cli` are included in the business package
for OTA self-checks. Other diagnostics such as `example_*` remain apps build
outputs. Copy only the tool needed for a bounded test to a directory under
`/userdata`, then remove it when the test is complete.

Do not deploy service definitions by copying individual files into `/etc`.
Rootfs helpers and systemd units are versioned with the Debian rootfs image and
must move together.

## Service Relationships

`aiden.target` groups the product services. Important ordering is expressed by
systemd dependencies rather than filename order:

1. Slot resolution and rootfs growth expose the active rootfs, userdata, and OTA partitions.
2. Userdata migration, machine identity, platform library registration, and strict environment generation complete.
3. Media, Wi-Fi, Bluetooth, USB gadget, DHCP, and time services prepare hardware and networking.
4. Frame, audio, BLE, Agent, Config Web, adb host, and WLAN recovery services start independently.
   The MCU event service owns `/dev/ttyS5` and writes received V1 EVENT frames to the journal.
5. The OTA health aggregator checks required local services before committing a pending slot.

Inspect the dependency graph with:

```bash
systemctl list-dependencies aiden.target
systemctl --failed
```

## Common Service Commands

```bash
systemctl status aiden-frame.service --no-pager
systemctl restart aiden-frame.service
systemctl status aiden-audio.service --no-pager
systemctl restart aiden-audio.service
systemctl status aiden-ble.service --no-pager
systemctl status aiden-agent.service --no-pager
systemctl restart aiden-agent.service
systemctl restart aiden-config-web.service
systemctl status aiden-usb-gadget.service --no-pager
systemctl status aiden-mcu-event.service --no-pager
sudo journalctl -u aiden-mcu-event.service -f
```

## SSH Sessions and Shutdown Notifications

The minimal rootfs explicitly installs `libpam-systemd`. With `UsePAM yes`,
new SSH connections belong to `session-*.scope` units under `user-1000.slice`.
These scopes are stopped during shutdown. Debian's `ssh.service` retains
`KillMode=process`, so restarting the SSH listener preserves active connections.
Installing the package on an existing board only affects new logins; reconnect
before testing. No SSH service restart is needed.

Session registration alone does not restore shutdown broadcasts on this board.
Debian armhf systemd 257 is built with `-UTMP`, and OpenSSH opens its PAM session
before allocating a PTY. The resulting logind session initially has no `TTY`.
`/etc/ssh/sshrc` runs `aiden-ssh-session-tty` after allocation to register that
terminal through logind's `SetTTY` API. It runs as the login user, briefly takes
session control without forcing out another controller, then exits. No daemon
or additional Python package is needed. Connections without a PTY are skipped;
registration failures do not prevent login. The hook preserves X11 cookie setup.
Users with a custom `~/.ssh/rc` must invoke `/usr/lib/aiden/aiden-ssh-session-tty`
there, because OpenSSH uses the user hook instead of the system hook.

In a fresh interactive SSH login, verify:

```bash
cat /proc/$$/cgroup
loginctl show-session "$XDG_SESSION_ID" -p Scope -p TTY
```

Expect a `session-*.scope` cgroup and `TTY=pts/...`. Open a second SSH terminal
before testing. Run `sudo shutdown -k +1` in the second terminal and observe the
broadcast in the first, then cancel with `sudo shutdown -c` in the second.
logind excludes the terminal that requested shutdown from the broadcast.
Keep both connections open: scheduling within five minutes temporarily blocks
new logins. On systemd 257, `shutdown -k now` does not exercise the same scheduled
warning path. Both normal `shutdown` and `systemctl poweroff` support wall
broadcasts unless `--no-wall` is used.

For a real shutdown test, use `sudo shutdown now` in the second terminal and
observe the notification and SSH close in the first. Full poweroff timing still requires
serial-console observation after SSH exits; session cleanup does not prove that
USB teardown, swapoff or filesystem unmounts finish promptly. `user@1000.service`
is left enabled (about 3 MB on the tested board).

## Key Configuration Files

| File | Description |
| --- | --- |
| `/etc/aiden_boot.conf` | Product feature gates loaded by systemd units |
| `/etc/aiden_frame_service.conf` | Frame service launch and capture settings |
| `/etc/aiden_audio_service.conf` | Audio service socket and volume-state settings |
| `/etc/aiden_ble_service.conf` | BLE socket, name, event capacity, and pairing window |
| `/userdata/agent/agent.toml` | Agent runtime configuration |
| `/userdata/system/env` | Persistent device environment source |
| `/run/aiden/system.env` | Validated, generated environment consumed by services |
| `/userdata/debian/wifi/wpa_supplicant-wlan0.conf` | Wi-Fi configuration managed by Config Web |
| `/userdata/system/wifi-proxies.json` | Per-SSID proxy mode and optional upstream URL |
| `/userdata/debian/ota/config.json` | Debian OTA repository and factory baseline |

## Log Locations

| Service | Log |
| --- | --- |
| Frame Service | `/var/log/frame_service/frame_service.log` |
| Audio Service | `/var/log/audio_service/audio_service.log` |
| BLE Service | `/var/log/ble_service/ble_service.log` |
| adb host startup | `/var/log/adb/adb-startup.log` |
| OTA health | `/var/log/ota/ota.log` |
| OTA recovery | `/var/log/ota/ota-recovery.log` |
| Config Web OTA update | `/userdata/ota/config_web_ota_update.log` |
| Agent | `/userdata/agent/log/agent.log` |
| TTYD terminal | `/var/log/ttyd/ttyd.log` |
| Bluetooth attach | `/var/log/aiden-hciattach.log` |
| bluetoothd | `/var/log/bluetoothd/bluetoothd.log` |
| Wi-Fi Proxy | `/var/log/wifi_proxy/wifi_proxy.log` |
| Wi-Fi driver | `/var/log/wifi_driver/wifi_driver.log` |
| wpa_supplicant (wlan0) | `/var/log/wpa_supplicant/wlan0.log` |
| WLAN guard | `/var/log/wlan_guard/wlan_guard.log` |
| Boot timeline | `/var/log/aiden_boot_timeline.log` |

The Config Web **Export logs** action (`GET /api/logs/support`) packages the
latest diagnostic data into `aiden-logs.tar.gz`. In addition to the Agent,
Langfuse episode, and latest LLM HTTP logs, it includes the service logs above,
the Bluetooth helper logs (`/var/log/aiden-hciattach.log` and
`/var/log/bluetoothd/bluetoothd.log`), OTA recovery/TTYD/boot-timeline logs,
and the current kernel ring buffer as `dmesg.log`. Service logs and dmesg are
limited to their most recent 1 MiB (the existing LLM HTTP export retains up to
4 MiB); unavailable files are included as text placeholders so one missing or
permission-restricted service log does not abort the export.

Use `journalctl -u <unit>` for systemd lifecycle and helper failures. Service
stdout/stderr that is intentionally persisted remains in the files above.

The `aiden-mcu-event` service reads V1 frames from `/dev/ttyS5` at 115200 8N1.
Every valid MCU EVENT is logged with its command, sequence, and raw payload;
`WAKEUP` (`0x12`) and `RK628_PLUGIN_DET_CHANGED` (`0x11`) have names, and
unknown event commands retain their command and payload in the journal. It
announces `RV_READY` and answers MCU `GET_STATE` probes so queued events can be
delivered. It does not query wake reasons or perform any event action. A
`SHUTDOWN_REQUEST` is logged and answered `DENIED`; this diagnostic service
does not perform Linux shutdown or send `SHUTDOWN_READY`, so it is not a
complete power-management peer. Stop it before using another owner of
`/dev/ttyS5`. This board configures journald with volatile storage, so export
the event journal before reboot when the record needs to be retained.

For a Wi-Fi startup failure, collect the following without rebooting if
possible:

```bash
systemctl status aiden-wifi-driver.service wpa_supplicant@wlan0.service \
    systemd-networkd.service aiden-wlan-guard.service --no-pager
cat /var/log/wifi_driver/wifi_driver.log
cat /var/log/wpa_supplicant/wlan0.log
cat /var/log/wlan_guard/wlan_guard.log
wpa_cli -i wlan0 status
networkctl status wlan0 --no-pager
```

The Wi-Fi logs intentionally record configuration metadata and state changes,
not the contents of `wpa_supplicant-wlan0.conf`, so the PSK is not copied into
the diagnostic files.

`frame_service` exclusively owns `/dev/video0`; stop `aiden-frame.service`
before a direct camera diagnostic. The Agent screenshot tool depends on the
frame service, while voice and volume tools depend on the audio service.
