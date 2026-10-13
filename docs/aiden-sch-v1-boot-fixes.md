# Aiden SCH v1 boot repair

Changes are based on aiden_sch_v1_finalnew1.pdf (2026-09-07), sheets 1,
4, 6–8, 13 and 15, checked against Rockchip RV1106 Datasheet V1.3
table 2-1 and the SDK pinctrl definitions.

## Wiring corrections

| Function | RV1106 pin / mux | Previous conflict |
| --- | --- | --- |
| WL_EN | pin 3, GPIO3_C5, external pull-up to V_WIFI_1V8 | Linux must not drive the MCU-owned rail |
| BT wake | pin 4, GPIO3_C6 | Activity LED drove the same net |
| CPU AVS | pin 74, GPIO1_A2, PWM0 M0 | Fixed regulator and BT host IRQ |
| RK628 I2C | pins 93/92, GPIO1_C2/C3, I2C4 M1 | I2C3 M1 goes to MCU UART |
| RK628 3.3V enable | pin 72, GPIO1_A0 | Misassigned TF supply; legacy wakeup GPIO32 |
| RK628 reset | pin 73, GPIO1_A1 | Incorrect GPIO1_B0; legacy wakeup GPIO33 |
| RK628 interrupt | pin 78, GPIO1_B1 | Retained |
| TF supply enable | pin 59, GPIO0_A1 | Incorrect GPIO1_A0 |
| Speaker enable | pin 22, GPIO4_C1, codec pa-ctl | GPIO3_C0 is CSI clock; simple-card did not consume spk-con |
| RV_HOLD | pin 87, GPIO1_D1 | Incorrect GPIO0_A3; UART3 M1 conflict |
| MCU UART | pins 86/85, UART5 M1 | Incorrect UART3 M1 |
| ASRPro UART | pins 91/90, UART4 M1 | UART2 M1 belongs to debug console |

The charger and BQ27220 are on the companion MCU's I2C bus. Remove
the fictitious RV1106 I2C1 fuel-gauge node; I2C1 M1 steals Bluetooth UART0
RX/TX. BT_WAKE_MCU is routed to the MCU and is not a Linux host IRQ.
The MCU must assert WIFI_VCC_PWREN (GD32 PA4); Linux cannot switch that
supply directly. Linux can reset the module through WL_EN_1V8:
RV1106 GPIO3_C5 (GPIO 117) -> R169 (1 kohm) -> module pin 12. Sheet 15
places R168 (10K_NC) on WF_WAKE_MCU_1V8, not WL_EN; the legacy DTS comment
claiming an external WL_EN pull-up is incorrect.

The image enables the driver by default (ENABLE_WIFIDRV=1). Before loading
any AIC module, aiden-wifi-driver calls aiden-wifi-prepare on Aiden SCH v1.
It detaches only the Wi-Fi host ff9a0000.mmc, holds WL_EN low for 1 second,
drives it high, waits 2 seconds, rebinds the host and waits up to 5 seconds
for both AIC8800D80 SDIO functions. The MCU-controlled supply must already
be on. Enumeration failures stop driver startup. GPIO 117 stays exported
and high. A service restart with wlan0 present skips the reset; if the
interface is absent but an AIC module is already loaded, the helper refuses
to reset underneath a live driver.

On 2026-10-08, two software reboots reproduced missing SDIO devices. Host
reprobing alone and driving WL_EN high without a low pulse both failed.
A low-to-high WL_EN pulse followed by host reprobing restored both SDIO
functions and wlan0, with 32 and 30 scanned BSS entries across 2.4/5 GHz.
The non-removable host only scans once, so a GPIO pulse alone did not
trigger enumeration. This recovery does not prove that an absent MCU
supply can be restored by WL_EN. The WLAN guard feature gate remains
disabled.

The Bluetooth HCI and BLE service gates are enabled (ENABLE_BLUETOOTH_HCI=1,
ENABLE_BLE_SERVICE=1). On 2026-10-12, enabling both gates on a running
SCH v1 board attached the AIC8800D80 controller on /dev/ttyS0 at 1.5 Mbaud,
exposed hci0, started ble_service on /run/ble_service/ble_service.sock and
registered one advertising instance, with wlan0 still associated and zero
failed systemd units. This check edited the gates on the running rootfs
and started the services by hand; a reboot, a physical cold boot, phone
pairing and ANCS were not tested.

After installing the boot helper, two further software reboots recovered
Wi-Fi automatically, with 29 and 33 scanned BSS entries across both bands
and zero failed systemd units. Restarting the service with wlan0 present
logged a reset skip. These checks deployed the shell helpers directly to
the running rootfs; no new factory image was flashed for this change.
Association/DHCP and a subsequent physical cold boot were not tested.

## Media initialization

Do not write CIF/ISP clr_unready_dev. It forces completion while the RK628
endpoint is missing, and the vendor CIF format check dereferences a null input
format. Preserve the async graph for normal binding and allow RGA, MPP, NPU,
codec and Rockit to load without a bridge. Also reject null CIF input formats
in the kernel. The missing-bridge loader test exercises this failure mode and
idempotent repeated initialization.

The agent disables Pico Zero GPIO32/33 wakeup watchers on model
Aiden SCH v1. Those pins now belong exclusively to RK628 power/reset.
This does not implement MCU button/voice wakeup protocol support.

## CPU regulator

SY8892 feedback is 0.6V. R82=56k, R84=100k, R85=24k, R86=150k,
R87=10k give:

- Vx = (Vpwm/24 + 0.6/150) / (1/24 + 1/150 + 1/10)
- Varm = 0.6 + 56 * (0.6/100 + (0.6 - Vx)/150)
- Nominal Varm is 1.149933V at PWM low and 0.803865V at PWM high.

Use an inverted 200kHz PWM, nominal 0.804–1.150V limits and 250us
settling time. These are schematic-derived values, not board measurements.
Validate VDD_ARM with a meter/scope at several OPPs after flashing.
The inherited OPP requests (0.85–1.0V) now fall within the regulator range.

## Validation after flashing

Reboot into the matched boot/OEM/rootfs build: the previous Oops leaves the
running kernel tainted, so restarting its service is not a clean kernel test.

- Check systemctl --failed, the current boot kernel journal, /sys/kernel/debug/gpio
  and /sys/kernel/debug/pwm.
- Confirm SDIO function enumeration, wlan0, AIC firmware load and hci0.
  If SDIO still does not enumerate, measure MCU-controlled WiFi 3.3V and 1.8V.
- Confirm 4-0050 binds rk628-csi-v4l2, then verify HDMI with a source connected.
  Test a boot without an HDMI source as well; no CIF Oops is acceptable.
- Check audio playback/recording, NPU/RGA/MPP device nodes and CPU frequency
  transitions with no voltage-setting errors.
- Connect WiFi to a network that supplies DHCP routing/DNS, then verify
  ip route, timedatectl timesync-status and timedatectl show -p NTPSynchronized.
  USB-only 192.168.42.0/24 has no Internet route, so NTP cannot synchronize.
  Do not hardcode the development host as the default gateway or fake NTP sync.
