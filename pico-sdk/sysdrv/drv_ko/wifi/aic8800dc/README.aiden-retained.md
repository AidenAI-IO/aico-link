# Aiden: AIC8800D80 across RV1106 restarts

The MCU supplies the Wi-Fi/Bluetooth module independently of the RV1106. An
RV1106 reboot therefore creates a new SDIO host while the module may still be
running its shared firmware. Startup must distinguish ROM from running firmware
before downloading anything. WL_EN is not part of this startup path.

This adaptation requires the matching MMC core changes in `core.c`, `sdio.c`,
`sdio_cis.c` and `sdio_ops.h`, plus the board DTS. Opt in on the SDIO host with:

```dts
aiden,aic8800d80-retained-sdio;
```

The property is specific to the RV1106 controller and this two-function D80. It
requires a non-removable, 4-bit high-speed SDIO host with native IRQ support and
fixed, always-on 3.3 V/1.8 V supplies without enable GPIOs. Preserve `no-sd`,
`no-mmc` and `keep-power-in-suspend`; do not add a power sequence, card power-off,
full-power-cycle or UHS capability. Both BSP and FDRV must preserve the board's
`MMC_CAP_NONREMOVABLE` when detaching.

The core suppresses initial module resets. A CMD5 timeout can enter the bounded
retained attachment path, which obtains an RCA and validates live common/F1/F2
CIS before publishing a card. Expected identities are `c8a1:0082` for common/F1
and `c8a1:0182` for F2. It rearms the SDIO interrupt master before drivers attach,
without changing the other IENx bits. The single CMD7 retry applies only after
successful transport whose only R1 error is `OUT_OF_RANGE`; its second
response and subsequent CIS validation must succeed. Card reset and destructive PM
paths are refused on this host. System sleep still requires driver-requested
`MMC_PM_KEEP_POWER`; the DT property alone does not establish sleep support.

`aic8800_bsp/aiden_fw_state.c` implements the firmware decision for normal work
mode on silicon ID `0xfb078820`, CPUID `0x410fc241`:

- **ROM:** VTOR is `0x001c2000` and all eight recorded ROM vector words match.
  Provisioning is refused if the Wi-Fi image start contains `0x001a0000`, or if
  either BT `PT_INF` pointer is present: `[0x001e6cd8] = 0x00201940` or
  `[0x001e6cdc] = 0x001e0000`. These pointers are written before BT `POWER_ON`
  and remain present after initialization. Their presence identifies a known
  partial initialization boundary; their absence does not prove untouched SRAM.
- **Running:** VTOR is `0x00182000`; the eight vector words there and at image
  base `0x00120000` match the supported image. A live `MM_VERSION_REQ` (ID 4)
  must return the exact 28-byte confirmation (ID 5), version `0x06090101` and
  all expected hardware, feature and capacity fields. ROM is never probed with
  MM_VERSION. A valid running result skips shared firmware download.

After initial BT/Wi-Fi provisioning, the same running-state proof is mandatory.
Read failures, mismatched identities, partial initialization markers, unknown
VTOR/vector contents or an unsupported ABI fail initialization. They never fall
back to downloading firmware or pulsing WL_EN. FDRV still performs its normal
`MM_RESET_REQ` to initialize Wi-Fi MAC state; that command is distinct from a
module power/reset cycle and from downloading the shared Wi-Fi/BT firmware.

The supported shipped Wi-Fi image is `aic8800dc_fw/fmacfw_8800d80_u02.bin`
(340236 bytes), SHA256:

```text
17a9fb36a9d6535e12325f05bc95e1f7e87dd4477813c5cab549cb3ee37ced96
```

This hash identifies the validated file; startup checks live signatures and ABI,
not a hash of all device RAM. Firmware upgrades, including BT patch/table changes,
must review the detector constants and `PT_INF` ordering, then repeat cold
provisioning, retained reboot/reload, Wi-Fi and Bluetooth validation. Do not
relax a mismatch into an automatic re-download fallback.

After synchronous BSP insertion, boot services read:

```text
/sys/devices/platform/aic-bsp/aicbsp_info/fw_state
```

Only `provisioned` (new download verified) and `reused` (existing runtime verified)
allow startup to continue. `probing` and `rom` are intermediate states; `failed`,
`legacy` or a missing attribute are not ready on this board. This attribute
records the initialization result, not continuous firmware or radio health. Keep
the SDIO controller awake through BSP/FDRV probing and restore its prior runtime
PM policy afterward, as the Aiden boot service does.

Board validation covers cold provisioning, retained startup, native Wi-Fi scans,
and Bluetooth UART HCI/LE discovery, including coexistence. STA data traffic,
Bluetooth pairing and system suspend/resume have not been validated. A ready
firmware result alone does not establish those capabilities or BT health.
