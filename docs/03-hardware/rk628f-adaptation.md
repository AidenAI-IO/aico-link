# RK628F hardware adaptation

Aico Link SCH v1 uses J3 for both USB2 ECM and Type-C video. The video path is
CSA171 -> CS5366 -> RK628F HDMI RX -> four CSI lanes -> RV1106.
`RK628_Plugin_DET` is routed to the MCU, not to an RV1106 GPIO. The device
tree therefore deliberately has no `plugin-det-gpios` property.

The older RK628D driver cannot initialize this silicon. The board reports
`0x20230321` at register `0x0200`, identifying RK628F/H. The SDK now
backports the coherent RK628 receiver, clock/reset, MIPI PHY and
post-processing drivers from Rockchip commit
`81417fbf13edb25f203ea794a723b8bf1f457d5c`. See
`pico-sdk/sysdrv/source/kernel/drivers/media/i2c/rk628/README.aico` for
the source and local compatibility changes.

The board retains its onboard 24 MHz oscillator, I2C4 M1 at 100 kHz,
GPIO1_A0 power enable, GPIO1_A1 active-low reset, GPIO1_B1 interrupt,
and four continuous-clock CSI lanes. HPD is controlled by the receiver
driver, not a GPIO-high startup workaround. The built-in EDID matches
`edid/hdmi_1080p60_cta.hex` and advertises 1080p60 by default. The frame
service applies the selected profile specifically to RK628; the generic
libaico fallback remains 1080p30 for TC358743 compatibility. A persistent
1080p30 compatibility profile
preserves 1920x1080 capture while limiting the HDMI pixel clock to 74.25 MHz. With the tested Android source, the
onboard Type-C bridge path repeatedly lost all three data-channel locks at
148.5 MHz, including with capture stopped and HPD held steady. Both 720p60
and 1080p30 locked correctly at 74.25 MHz. This is a compatibility limit for
the observed path; it does not establish which physical component causes
the higher-rate instability.

## Persistent input mode

On the board, use:

```bash
sudo aico-hdmi-mode 1080p30   # Apply compatibility mode and save it
sudo aico-hdmi-mode 1080p60   # Return to 60 Hz and save it
aico-hdmi-mode status        # Show the configured selection
```

The command atomically saves `/userdata/system/hdmi-mode` and restarts
`aico-frame.service`. The selection survives reboot and rootfs slot changes;
without a saved selection, the default is 1080p60. Status reports the configured
mode, not confirmation of a locked video input. There is no automatic downgrade.

After discovering an RK628 receiver, the frame service applies the selection
before opening capture, including when the bridge appears after service startup.
It compares the current EDID first and applies a changed profile once; ordinary
capture recovery does not repeat that negotiation. TC358743 retains its existing
policy. An explicitly configured `FRAME_SERVICE_EDID` overrides this mode policy;
keep it set to `auto` when using the mode command.

An EDID change can take the Android source about 30 seconds to finish
renegotiating. A temporary zero pixel clock during that interval does not
establish that the new mode is unsupported. Observe source display state
and receiver lock before evaluating capture.

Userspace EDID updates stop streaming, hold HPD low, and schedule receiver
configuration again. An absent input returns `ENOLCK` rather than a
successful fallback VGA timing. Without a plugin GPIO, failed training
retries autonomously with a ten-second delay.

## Receiver recovery

System resume must restore CPLL to the same 1194 MHz reference used at probe,
before receiver initialization and timing acquisition. Power loss followed by
generic CRU initialization leaves it at 1188 MHz, while HDMI timing calculations
assume 1194 MHz. On the tested board this reported a 1080p60 input near 149.25 MHz
instead of 148.5 MHz and produced persistent diagonal corruption in raw frames.
Clock and all HDMI channels remained locked; restarting capture or rewriting
the same EDID did not repair it. Internal RK628 color bars were intact, and
restoring the calibrated CPLL rate restored the phone image at 1080p60.
PLL lock polling is limited to 100 reads with 1 ms sleeps between attempts.
Timeouts return `ETIMEDOUT`; failed I2C reads retain their original errno.
Probe and resume stop on CPLL failure. Failed resume powers the receiver back
off and leaves IRQs and hotplug work disabled instead of proceeding with an
unlocked reference clock. Audio parent-PLL errors also propagate without
programming a divider from an invalid rate.

Receiver configuration samples character errors once and uses that result
for both readiness and retry decisions. Reading the counters can update
their baseline and the equalizer, so a second read could appear clear and
incorrectly suppress a required retry.
When character-error monitoring is enabled, sampling its counters can retune
the equalizer based on errors accumulated during a long idle and disrupt a
locked input. Stream start therefore leaves these counters alone; equalizer
changes stay in receiver training. The board device tree leaves character-error
monitoring and dynamic EQ disabled because trials with dynamic EQ did not
establish reliable capture. Character-error counters alone do not identify a
cable fault.

RK628F can raise vertical-active-line interrupts while its measurement
settles even though the HDMI clock, all three data channels, and decoded
input mode remain stable. The driver debounces these interrupts for 100 ms
and compares the input with the last configured timings and color metadata.
Channel-lock checks retry over 80 ms so a brief lock-status drop does not
reset an otherwise unchanged input; persistent lock loss still retrains it.
Unchanged input, allowing 0.1% pixel-clock measurement variation, keeps the
current stream running. Link loss or a real mode/color change still stops
the stream and retrains the receiver. Timing queries cannot replace the
configured comparison baseline during the debounce interval.

AVI packets can continue advertising 1080p60 while the receiver loses data-channel
lock and measures fewer than 1080 active lines. The standard-mode lookup alone
therefore cannot establish stable video. Each sample used for timing acquisition
checks the clock and all three data-channel locks before and after reading the
timings. Lock loss breaks the sequence of eight stable samples, returns `ENOLCK`,
and clears the output timings; register-read errors retain their original errno.
This prevents stale AVI metadata from publishing input readiness. It does not
repair an unstable HDMI link or guarantee subsequent frame delivery.

The parent HDMI interrupt stays masked during recovery and leaf-mask
updates. This prevents a pending interrupt from monopolizing I2C while the
recovery worker holds its configuration mutex. CSI interrupt routing is
preserved. A shared mutex serializes the threaded HDMI handler with mask
updates. `VIDIOC_QUERY_DV_TIMINGS` returns `EAGAIN` while configuration is busy
and `ENOLCK` when no signal is available.

Routine timing queries select single or dual MIPI mode on every successful
query. Both 1080p60 (148.5 MHz) and 1080p30 (74.25 MHz) use single MIPI mode. These selection
messages are available at driver debug level 1; their repetition alone does
not indicate receiver recovery or a capture failure.

Stream start/stop and format-readiness checks share the receiver configuration
mutex so recovery cannot reset CSI halfway through a stream transition.
`STREAMON` returns `ENOLCK` when the input is unavailable and `EAGAIN` when
stream setup itself triggers recovery. A failed start disables CSI before
returning the error, allowing the capture service to reopen the source.
CSI PHY lock and lane stop-state polling failures return their original error,
such as `ETIMEDOUT` or an I2C error, through `STREAMON`. Failed setup leaves both
the transmitter and PHY disabled instead of reporting a working stream.

On an affected image, the frame service can remain `RECOVERING` with
`latest_seq=0`, `frame_age_ms=0`, and `last_error=open failed`. Here a zero
frame age means that no frame timestamp exists. Check
`/var/log/frame_service/frame_service.log` and the RK628 kernel log for
repeated receiver resets. Display enumeration alone does not establish that
capture is working.

CIF startup can fail before it enables its frame-completion tasklet, for
example when receiver format readiness returns `EAGAIN` or `ENOLCK`.
Failure cleanup must disable the tasklet only if that startup attempt enabled
it. Otherwise each early failure increases the disable count, and later
successful `STREAMON` calls leave frame completion disabled. A pending tasklet
then repeatedly wakes `ksoftirqd`, capture dequeues time out, and reopening the
camera does not clear the imbalance. The startup path tracks whether it enabled
the tasklet before balancing that operation during cleanup. Deploying the
corrected CIF module requires a reboot to clear an already affected instance.
This addresses a persistent capture stall after transient receiver failures;
receiver lock loss still needs to be evaluated independently.

## Hardware verification

Check all of the following with the connected host awake:

1. Kernel log reports `RK628F/H (20230321)`.
2. The host enumerates `Linux FHD60` at 1920x1080, 60 Hz by default, or
   `Linux FHD30` at 30 Hz after selecting compatibility mode.
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
production recovery functions with simulated registers, exercises CIF startup
after repeated early and late failures, and checks the
board/EDID integration contract. Run the required local `make check` profile
after changes; CI runs `make check-full`.
