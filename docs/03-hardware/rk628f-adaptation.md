# RK628F hardware adaptation

Aiden SCH v1 uses J3 for both USB2 ECM and Type-C video. The video path is
CSA171 -> CS5366 -> RK628F HDMI RX -> four CSI lanes -> RV1106.
`RK628_Plugin_DET` is routed to the MCU, not to an RV1106 GPIO. The device
tree therefore deliberately has no `plugin-det-gpios` property.

The older RK628D driver cannot initialize this silicon. The board reports
`0x20230321` at register `0x0200`, identifying RK628F/H. The SDK now
backports the coherent RK628 receiver, clock/reset, MIPI PHY and
post-processing drivers from Rockchip commit
`81417fbf13edb25f203ea794a723b8bf1f457d5c`. See
`pico-sdk/sysdrv/source/kernel/drivers/media/i2c/rk628/README.aiden` for
the source and local compatibility changes.

The board retains its onboard 24 MHz oscillator, I2C4 M1 at 100 kHz,
GPIO1_A0 power enable, GPIO1_A1 active-low reset, GPIO1_B1 interrupt,
and four continuous-clock CSI lanes. HPD is controlled by the receiver
driver, not a GPIO-high startup workaround. The built-in EDID matches
`edid/hdmi_1080p60_cta.hex` and advertises 1080p60 only.

Userspace EDID updates stop streaming, hold HPD low, and schedule receiver
configuration again. An absent input returns `ENOLCK` rather than a
successful fallback VGA timing. Without a plugin GPIO, failed training
retries autonomously with a ten-second delay.

## Receiver recovery

Configuration uses one character-error observation for its readiness and
retry decisions, preserving the retry when that observation updates the
counter baseline and equalizer.
When character-error monitoring is enabled, sampling its counters can retune
the equalizer based on errors accumulated during a long idle and disrupt a
locked input. Stream start therefore leaves these counters alone; equalizer
changes stay in receiver training. The board device tree leaves character-error
monitoring and dynamic EQ disabled because trials with dynamic EQ did not
establish reliable capture. Character-error counters alone do not identify a
cable fault.

RK628F can raise vertical-active-line interrupts while its measurement
settles even though the HDMI clock, all three data channels, and decoded
1080p60 mode remain stable. The driver debounces these interrupts for 100 ms
and compares the input with the last configured timings and color metadata.
Channel-lock checks retry over 80 ms so a brief lock-status drop does not
reset an otherwise unchanged input; persistent lock loss still retrains it.
Unchanged input, allowing 0.1% pixel-clock measurement variation, keeps the
current stream running. Link loss or a real mode/color change still stops
the stream and retrains the receiver. Timing queries cannot replace the
configured comparison baseline during the debounce interval.

The parent HDMI interrupt stays masked during recovery and leaf-mask
updates. This prevents a pending interrupt from monopolizing I2C while the
recovery worker holds its configuration mutex. CSI interrupt routing is
preserved. A shared mutex serializes the threaded HDMI handler with mask
updates. `VIDIOC_QUERY_DV_TIMINGS` returns `EAGAIN` while configuration is busy
and `ENOLCK` when no signal is available.

Stream start/stop and format-readiness checks share the receiver configuration
mutex so recovery cannot reset CSI halfway through a stream transition.
`STREAMON` returns `ENOLCK` when the input is unavailable and `EAGAIN` when
stream setup itself triggers recovery. A failed start disables CSI before
returning the error, allowing the capture service to reopen the source.

On an affected image, the frame service can remain `RECOVERING` with
`latest_seq=0`, `frame_age_ms=0`, and `last_error=open failed`. Here a zero
frame age means that no frame timestamp exists. Check
`/var/log/frame_service/frame_service.log` and the RK628 kernel log for
repeated receiver resets. Display enumeration alone does not establish that
capture is working.

## Hardware verification

Check all of the following with the connected host awake:

1. Kernel log reports `RK628F/H (20230321)`.
2. The host enumerates `Linux FHD60` at 1920x1080, 60 Hz without a manual
   HPD override.
3. `v4l2-ctl -d /dev/v4l-subdev2 --query-dv-timings` returns locked,
   nonzero input timings. Discover the actual subdevice using
   `media-ctl -p` if numbering changes.
4. `/sys/kernel/debug/rk628/4-0050/hdmirx/status` shows clock and all three
   HDMI data channels locked.
5. `frame_service_cli --socket /run/frame_service/frame_service.sock
   screenshot --out /tmp/capture.jpg` returns an actual image, and
   successive captures change when the host image changes.
6. Repeat after software reboot and verify Wi-Fi recovery and
   `systemctl --failed`.

Host display enumeration alone verifies HPD/DDC, not pixel transport.
A sleeping host can keep its display enumerated while transmitting no
video clock. The driver must report no signal in that condition.

The SDK BSP build and audit compile the hardware driver. The Docker suite
`bash scripts/run_tests_in_docker.sh --suite rk628-recovery` exercises the
production recovery functions with simulated registers and checks the
board/EDID integration contract. Run the required local `make check` profile
after changes; CI runs `make check-full`.
