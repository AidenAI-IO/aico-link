# AIC8800 SDIO firmware

This directory contains the firmware copied into `/oem/usr/ko/aic8800dc_fw`
by the SDIO driver build.  The `8800d80` files are the AIC8800D80 U02 SDIO
set from the public AIC driver repository:

- source: https://github.com/gtxaspec/aic8800-wifi
- source path: `SDIO/driver_fw/fw/aic8800D80`
- source revision: `a870f7f15170a5899bf0b98769896fd1a7d4bf1e`

The Aiden SCH v1 board uses `CONFIG_SDIO_BT=n`, so the normal-mode
`fmacfw_8800d80*_u02.bin` files are selected while Bluetooth is attached over
UART.  The `fmacfwbt_8800d80*_u02.bin` files are retained for the driver's
SDIO-Bluetooth build variant.  Do not substitute the AIC8800DC firmware: the
D80 chip has a different firmware/RF-calibration set.
