---
sidebar_position: 3
---

# Hardware, Wiring & Self-Assembly

## Target Hardware

This page describes the current Aico Link development-board reference setup. It is intended for prototyping and development. Complete all FPC and Dupont-wire connections with the power disconnected, then recheck power and signal wiring before first power-on.

Board markings, header orientation, and connector placement can vary by module revision, so always follow the markings on the physical module.

| Hardware | Purpose | Quantity |
| --- | --- | --- |
| [Luckfox Pico Zero](https://wiki.luckfox.com/Luckfox-Pico-Zero) (RV1106) | Main board. Runs Aico Link firmware and provides CSI, USB HID, and networking. | 1 |
| [Firefly HDMI TO MIPI CSI](https://wiki.t-firefly.com/HDMI-TO-MIPI-CSI-RK628D/rk628d.html) (RK628D) | Four-lane HDMI-to-CSI video capture board, powered from 5 V. | 1 |
| HDMI ESD protector | Protects the HDMI video-input path. | 1 |
| USB-to-TTL module | Serial-console debugging when the Pico Zero USB port is in use. | 1 (optional) |
| ASRPro 2.0 development board | Voice recognition module. | 1 |
| Type-C hub (with HDMI and USB) | Breaks out the target device's Type-C connection into HDMI video and USB data/power. | 1 |
| USB-C-to-USB-A data cable | Connects the hub's USB-A port to the Pico Zero. | 1 |
| HDMI cable | Connects the hub, ESD protector, and RK628D board. | 1 |
| Custom 30P-to-22P FPC adapter | Connects the Firefly RK628D CSI output and control signals to the Pico Zero CSI input. | 1 |
| 1 W / 8 Ω speaker | Voice playback through the Pico Zero speaker output. | 1 |
| Dupont wires | Connect the ASRPro, Pico Zero, and USB-to-TTL module. Prepare male-to-male, male-to-female, and female-to-female wires. | As needed |

## Core Connection Topology

The target device (iPhone / PC) connects to Aico Link through a USB-C hub. The hub splits the connection into HDMI output (for screen capture) and USB-C output (for data/power to the Pico Zero):

```text
Target Device (iPhone / PC)
          │ USB-C
          ▼
      USB-C Hub
          ├─ HDMI out ──→ Firefly RK628D (HDMI to CSI bridge)
          │                      │ four-lane CSI + I2C + reset
          │                      ▼
          └─ USB-A out ──→ Luckfox Pico Zero (CSI input + USB)
                                   │
                                   ├─ Video capture: /dev/video0 (from RK628D CSI)
                                   ├─ HID keyboard/pointer/control: /dev/hidg0, /dev/hidg1, /dev/hidg2
                                   ├─ Audio codec / ALSA
                                   └─ Network: Wi-Fi or USB gadget network
```

Physical wiring summary:

1. Target device USB-C → USB-C hub input
2. Hub HDMI output → Firefly RK628D HDMI input
3. Firefly RK628D 30-pin output → custom adapter → Pico Zero 22-pin CSI input
4. Hub USB-A output → Pico Zero USB-C port (data + power)

## RK628D 30-pin to Pico Zero 22-pin Adapter

Number pins from the connector markings on each board, not from the visible
copper direction of a flipped FPC cable.

| Firefly 30-pin | Signal | Pico Zero 22-pin | Signal |
| ---: | --- | ---: | --- |
| 1 | SDA, 1.8 V | 21 | SDA |
| 2 | SCL, 1.8 V | 20 | SCL |
| 4 | HDMIIN_RESET | 17 | RST |
| 11 | D0P | 3 | D0P |
| 12 | D0N | 2 | D0N |
| 14 | D1P | 6 | D1P |
| 15 | D1N | 5 | D1N |
| 17 | CLK_P | 9 | CLK_P |
| 18 | CLK_N | 8 | CLK_N |
| 20 | D2P | 12 | D2P |
| 21 | D2N | 11 | D2N |
| 23 | D3P | 15 | D3P |
| 24 | D3N | 14 | D3N |
| 30 | 5 V input | 5 V | 5 V |
| GND pins | GND | GND pins | GND |

Keep every available ground return between the differential pairs; do not
reduce the adapter to one shared ground wire. The Firefly board specification
lists both pins 29 and 30 as the same 5 V input rail; the current adapter feeds
pin 30. Pico 22-pin MCLK (pin 18) and 3.3 V (pin 22) remain unconnected.

Firefly pins 7 (`HDMIIN_INT`) and 8 (`HDMIRX_DET`) are also unconnected in the
current adapter. The kernel therefore uses the RK628 driver's I2C polling path.
The board has its own oscillator, so no Pico MCLK is required.

The Pico drives `HDMIIN_RESET` from GPIO3_C5 as an active-low push-pull output
with no internal pull-up or pull-down. I2C4 runs at 100 kHz for reliable RK628D
32-bit register transfers; the same bus also retains support for TC358743.

## Assembly Steps

1. Connect the target phone or computer to the upstream Type-C port of the Type-C hub.
2. Connect the hub HDMI output to the HDMI ESD protector, HDMI cable, and Firefly RK628D HDMI input in sequence.
3. Use the custom 30P-to-22P adapter to connect the RK628D output to the Pico Zero CSI connector. Ensure both connector latches are closed and verify pin numbering before applying 5 V.
4. Use the USB-C-to-USB-A data cable to connect the hub USB-A port to the Pico Zero USB-C port. This connection provides the USB data path and power between the target device and Pico Zero.
5. Connect ASRPro power, ground, and control signals to the Pico Zero. Connect the 1 W / 8 Ω speaker directly to the Pico Zero speaker output, not to the ASRPro. Verify each connection against the board markings.
6. If serial debugging is needed, connect the Pico Zero debug header to the USB-to-TTL module. Do not connect the TTL module's power wire to the Pico Zero unless the power design has been explicitly verified.

## Pre-Power Checklist

- HDMI, FPC, and USB cables are fully seated; the FPC cable is neither reversed nor damaged.
- ASRPro and Pico Zero power and ground wires have been verified against the board markings; no Dupont wires short adjacent pins.
- The speaker is connected directly to the Pico Zero speaker output, not to ASRPro.
- The HDMI ESD protector is installed between the hub and RK628D board.
- The RK628D adapter supplies 5 V only; Pico MCLK and 3.3 V are not connected.
- All four MIPI lanes, the clock pair, I2C, reset, and the available ground returns are continuous end to end.
- The USB-to-TTL module uses only signal and ground wires, with `TX` and `RX` crossed.
- During first-time testing, confirm that the system detects the video capture device before testing USB HID and voice features.

## iOS HID Prerequisites

To control an iOS device via USB HID, enable on the target iOS device:

```text
Settings > Accessibility > Touch > AssistiveTouch
```

It is also recommended to enable **Show Onscreen Keyboard** on the AssistiveTouch page for a more stable keyboard input experience.

## Default Device Nodes

| Capability | Default node / path | Description |
| --- | --- | --- |
| Video capture | `/dev/video0` | RK628D or TC358743 bridge output to the V4L2 capture device |
| HDMI subdev | Auto-detected `rk628-csi` or `tc358743` node | EDID / DV timings / HDMI sync; the exact `/dev/v4l-subdevX` index can vary |
| Keyboard HID | `/dev/hidg0` | Default keyboard device for Go Agent and example tools |
| Mouse/touch HID | `/dev/hidg1` | Go Agent uses the same HID device for mouse/touch input |
| Auxiliary control HID | `/dev/hidg2` | Android extension keys in touchscreen mode; Consumer Control media, volume, brightness, and screenshot keys in absolute mode |
| Frame socket | `/run/frame_service/frame_service.sock` | Default path for system service deployment |
| Audio socket | `/run/audio_service/audio_service.sock` | Default path for system service deployment |

When running the binary directly during development, the default frame socket is `/tmp/frame_service.sock`; after deployment with the firmware startup script, it defaults to `/run/frame_service/frame_service.sock`.
