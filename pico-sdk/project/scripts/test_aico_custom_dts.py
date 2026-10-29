#!/usr/bin/env python3
"""Checks for the Aico Link SCH v1 (RV1106) board adaptation.

The custom board was migrated from the Luckfox Pico Zero reference design;
this validates the selected device tree and the AIC8800D80 SDIO/BT integration
that boots alongside it.
"""
import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
DTS = ROOT / "sysdrv/source/kernel/arch/arm/boot/dts/rv1106g-aico-custom.dts"
BOARD_CONFIG = (
    ROOT
    / "project/cfg/BoardConfig_IPC"
    / "BoardConfig-EMMC-Buildroot-RV1106_Luckfox_Pico_Zero-IPC.mk"
)
WIFI_MAKEFILE = ROOT / "sysdrv/drv_ko/wifi/Makefile"
WIFI_LOADER = ROOT / "sysdrv/drv_ko/wifi/insmod_wifi.sh"
WIFI_START = ROOT / "project/app/wifi_app/bin/wifi_start.sh"
BT_INIT = (
    ROOT
    / "project/cfg/BoardConfig_IPC/overlay/overlay-luckfox-buildroot-init"
    / "etc/init.d/S99hciinit"
)
AICO_BT_CONFIG = (
    ROOT
    / "project/cfg/BoardConfig_IPC/overlay/overlay-luckfox-buildroot-aico"
    / "etc/default/aic8800-bt"
)
AICO_WIFI_INIT = (
    ROOT
    / "project/cfg/BoardConfig_IPC/overlay/overlay-luckfox-buildroot-aico"
    / "etc/init.d/S26wifi"
)
AIC_FW_DIR = ROOT / "sysdrv/drv_ko/wifi/aic8800dc/aic8800dc_fw"


class AicoCustomBoardTest(unittest.TestCase):
    def setUp(self):
        self.dts = DTS.read_text()

    def node(self, name):
        """Return a DTS node, including its children, for scoped assertions."""
        start = self.dts.index(name + " {")
        end = self.dts.index("{", start) + 1
        depth = 1
        while depth:
            if self.dts[end] == "{":
                depth += 1
            elif self.dts[end] == "}":
                depth -= 1
            end += 1
        return self.dts[start:end]

    def test_board_config_selects_the_aico_dts(self):
        text = BOARD_CONFIG.read_text()
        self.assertIn("export RK_KERNEL_DTS=rv1106g-aico-custom.dts", text)
        self.assertIn("export RK_ENABLE_WIFI_CHIP=AIC8800D80", text)
        self.assertIn("rv1106-sdiowifi.config", text)

    # Poweroff is checked on the compiled tree by scripts/test_poweroff_dtb.sh
    # in the firmware repository. Text checks miss inherited nodes and reject
    # the /delete-node/ directive needed to remove the EVB fallback.

    def test_suspend_uses_the_board_power_and_pin_configuration(self):
        # The RV1106 machine compatible activates the platform suspend path;
        # s2idle remains available through the common CONFIG_SUSPEND code.
        self.assertRegex(
            self.dts,
            r'compatible\s*=\s*"rockchip,rv1103g-38x38-ipc-v10",\s*'
            r'"rockchip,rv1106g3",\s*"rockchip,rv1106";',
        )
        suspend = self.node("&rockchip_suspend")
        # SCH v1 has no external 32 kHz source. The reference EVB sleep IO
        # table would also override the board's GPIO0 pinmux and pulls.
        self.assertIn("rockchip,sleep-mode-config = <0>;", suspend)
        self.assertIn("/delete-property/ rockchip,sleep-io-config;", suspend)
        self.assertIn("retain-state-suspended;", self.node("rv_hold"))

    def test_power_management_i2c_nodes(self):
        # Charger and fuel gauge are both on the MCU's bus. The old RV1106
        # I2C1 M1 binding also steals Bluetooth UART0 RX/TX.
        for absent in ("charger@4b", "mps,mp2720", "fuel-gauge@55", "ti,bq27220"):
            self.assertNotIn(absent, self.dts)
        self.assertIn('status = "disabled";', self.node("&i2c1"))
        self.assertIn("vcc3v3_sd: vcc3v3-sd-regulator", self.dts)
        self.assertIn("v_wifi_vcc: v-wifi-vcc", self.dts)

    def test_cpu_voltage_uses_schematic_pwm_feedback(self):
        # SY8892's inverting feedback maps inverted 200 kHz PWM duty to
        # 0.804--1.150 V. Expanding fixed-regulator limits cannot drive AVS.
        regulator = self.node("vdd_arm: vdd-arm")
        for setting in (
            'compatible = "pwm-regulator";',
            "pwms = <&pwm0 0 5000 1>;",
            "regulator-min-microvolt = <804000>;",
            "regulator-max-microvolt = <1150000>;",
            "regulator-init-microvolt = <950000>;",
            "regulator-settling-time-up-us = <250>;",
            "pwm-dutycycle-unit = <1000>;",
            "pwm-dutycycle-range = <0 1000>;",
            "regulator-always-on;",
            "regulator-boot-on;",
        ):
            with self.subTest(setting=setting):
                self.assertIn(setting, regulator)
        self.assertNotIn('compatible = "regulator-fixed";', regulator)
        self.assertIn("cpu-supply = <&vdd_arm>;", self.node("&cpu0"))
        pwm = self.node("&pwm0")
        self.assertIn('status = "okay";', pwm)
        self.assertIn("pinctrl-0 = <&pwm0m0_pins>;", pwm)

    def test_rk628_on_i2c4_with_power_and_reset(self):
        i2c4 = self.node("&i2c4")
        self.assertIn('status = "okay";', i2c4)
        self.assertIn("clock-frequency = <100000>;", i2c4)
        self.assertIn("pinctrl-0 = <&i2c4m1_xfer>;", i2c4)
        self.assertIn("rk628: rk628@50", i2c4)
        bridge = self.node("rk628: rk628@50")
        for setting in (
            'compatible = "rockchip,rk628-csi-v4l2";',
            "reg = <0x50>;",
            "enable-gpios = <&gpio1 RK_PA0 GPIO_ACTIVE_HIGH>;",
            "reset-gpios = <&gpio1 RK_PA1 GPIO_ACTIVE_LOW>;",
            "interrupts = <RK_PB1 IRQ_TYPE_LEVEL_HIGH>;",
            "pinctrl-0 = <&rk628_power_pin &rk628_reset_pin &rk628_int_pin>;",
        ):
            with self.subTest(setting=setting):
                self.assertIn(setting, bridge)
        for label, pin, pull in (
            ("power", "RK_PA0", "pcfg_pull_none"),
            ("reset", "RK_PA1", "pcfg_pull_none"),
            ("int", "RK_PB1", "pcfg_pull_up"),
        ):
            self.assertIn(
                f"<1 {pin} RK_FUNC_GPIO &{pull}>",
                self.node(f"rk628_{label}_pin: rk628-{label}-pin"),
            )
        self.assertIn('status = "disabled";', self.node("&i2c3"))
        self.assertNotIn("tc358743", self.dts)
        self.assertNotIn("out_osc_tc358743", self.dts)

    def test_tf_card_on_sdmmc0(self):
        self.assertIn(
            "gpio = <&gpio0 RK_PA1 GPIO_ACTIVE_HIGH>;",
            self.node("vcc3v3_sd: vcc3v3-sd-regulator"),
        )
        sdmmc = self.node("&sdmmc")
        self.assertIn("vmmc-supply = <&vcc3v3_sd>;", sdmmc)
        self.assertIn("cd-gpios = <&gpio3 RK_PA1 GPIO_ACTIVE_LOW>;", sdmmc)
        self.assertIn(
            "pinctrl-0 = <&sdmmc0_clk &sdmmc0_cmd &sdmmc0_bus4 &sdmmc0_card_detect>;", sdmmc
        )
        for setting in (
            'pinctrl-names = "default", "normal", "idle";',
            "pinctrl-1 = <&sdmmc0_clk &sdmmc0_cmd &sdmmc0_bus4 &sdmmc0_card_detect>;",
            "pinctrl-2 = <&sdmmc0_idle_pins &sdmmc0_card_detect>;",
            "max-frequency = <50000000>;",
            "bus-width = <4>;",
            "vqmmc-supply = <&vcc_3v3>;",
            "no-sdio;", "no-mmc;",
        ):
            with self.subTest(setting=setting):
                self.assertIn(setting, sdmmc)
        self.assertIn(
            "<3 RK_PA1 RK_FUNC_GPIO &pcfg_pull_up>",
            self.node("sdmmc0_card_detect: sdmmc0-card-detect"),
        )
        supply = self.node("vcc3v3_sd: vcc3v3-sd-regulator")
        self.assertIn("startup-delay-us = <10000>;", supply)
        self.assertIn("off-on-delay-us = <200000>;", supply)
        self.assertNotIn("regulator-always-on;", supply)
        self.assertNotIn("rockchip,tf-diagnostics", sdmmc)
        # Card detect stays in GPIO mode, not SDMMC hardware-detect mode.
        self.assertNotIn("sdmmc0_det", self.dts)

    def test_bluetooth_on_uart0_with_hardware_flow_control(self):
        uart = self.node("&uart0")
        self.assertIn('status = "okay";', uart)
        self.assertIn(
            "pinctrl-0 = <&uart0m1_xfer &uart0m1_ctsn &uart0m1_rtsn>;", uart
        )
        bluetooth = self.node("wireless_bluetooth: wireless-bluetooth")
        self.assertIn("BT,wake_gpio = <&gpio3 RK_PC6 GPIO_ACTIVE_HIGH>;", bluetooth)
        # BT_WAKE_MCU ends at the MCU. GPIO1_A2 belongs to CPU PWM feedback.
        self.assertNotIn("BT,wake_host_irq", bluetooth)
        self.assertNotIn("uart_rts_gpios", bluetooth)
        self.assertNotIn("work_led:", self.dts)
        self.assertNotIn("&uart1 {", self.dts)
        self.assertNotIn("uart1_gpios", self.dts)

    def test_wifi_on_sdio_controller_uses_external_wl_en_pullup(self):
        sdio = self.node("&sdio")
        for setting in (
            "supports-sdio;", "non-removable;", "cap-sdio-irq;",
            "keep-power-in-suspend;", "rockchip,default-sample-phase = <90>;",
            "vmmc-supply = <&v_wifi_vcc>;", "vqmmc-supply = <&vcc_1v8>;",
            "pinctrl-0 = <&sdmmc1m0_cmd &sdmmc1m0_clk &sdmmc1m0_bus4>;",
        ):
            with self.subTest(setting=setting):
                self.assertIn(setting, sdio)
        # WIFI_VCC_PWREN is MCU controlled; Linux only switches WL_EN.
        supply = self.node("v_wifi_vcc: v-wifi-vcc")
        self.assertIn("regulator-always-on;", supply)
        self.assertNotIn("gpio =", supply)

    def test_aic8800_submakes_inherit_rockchip_platform(self):
        makefile = (ROOT / "sysdrv/drv_ko/wifi/aic8800dc/Makefile").read_text()
        # The child BSP/FDRV Makefiles default to Ubuntu when these variables
        # are not exported by the parent recursive build.
        self.assertIn("export CONFIG_PLATFORM_ROCKCHIP CONFIG_PLATFORM_ROCKCHIP2", makefile)
        self.assertIn("CONFIG_PLATFORM_UBUNTU", makefile)
        self.assertIn("CONFIG_PLATFORM_ROCKCHIP = y", makefile)
        self.assertIn("CONFIG_PLATFORM_UBUNTU = n", makefile)

    def test_aic8800d80_sdio_driver_path(self):
        makefile = WIFI_MAKEFILE.read_text()
        loader = WIFI_LOADER.read_text()
        wifi_start = WIFI_START.read_text()
        self.assertIn("AIC8800DC AIC8800D80", makefile)
        for sdio_id in ("C08D", "0082"):
            self.assertIn(sdio_id, loader)
            self.assertIn(sdio_id, wifi_start)
        self.assertNotIn("C18D", loader)
        self.assertNotIn("C18D", wifi_start)

    def test_aic8800d80_firmware_is_packaged(self):
        required = {
            "aic_userconfig_8800d80.txt",
            "fmacfw_8800d80_u02.bin",
            "fmacfw_8800d80_h_u02.bin",
            "fw_adid_8800d80_u02.bin",
            "fw_patch_8800d80_u02.bin",
            "fw_patch_8800d80_u02_ext0.bin",
            "fw_patch_table_8800d80_u02.bin",
            "lmacfw_rf_8800d80_u02.bin",
        }
        self.assertTrue(required.issubset({p.name for p in AIC_FW_DIR.iterdir()}))

    def test_aico_wifi_and_bt_init_order(self):
        wifi_init = AICO_WIFI_INIT.read_text()
        bt_init = BT_INIT.read_text()
        bt_config = AICO_BT_CONFIG.read_text()
        self.assertIn("/oem/usr/ko/insmod_wifi.sh", wifi_init)
        self.assertIn("/etc/default/aic8800-bt", bt_init)
        self.assertIn("AIC8800_BT_TTY", bt_init)
        self.assertIn("AIC8800_BT_TTY=/dev/ttyS0", bt_config)

    def test_audio_and_voice_module(self):
        self.assertIn(
            "pa-ctl-gpios = <&gpio4 RK_PC1 GPIO_ACTIVE_HIGH>;",
            self.node("&acodec"),
        )
        self.assertNotIn("spk-con-gpios", self.dts)
        voice = self.node("&uart4")
        self.assertIn('status = "okay";', voice)
        self.assertIn("pinctrl-0 = <&uart4m1_xfer>;", voice)
        # UART2 M1 is owned by the FIQ recovery console.
        self.assertIn('status = "disabled";', self.node("&uart2"))

    def test_gd32_mcu_interface_nodes(self):
        # RV_HOLD lets the SoC take over VCC5V0_SYS from the companion MCU.
        hold = self.node("rv_hold")
        self.assertIn('label = "rv-hold";', hold)
        self.assertIn("gpios = <&gpio1 RK_PD1 GPIO_ACTIVE_HIGH>;", hold)
        self.assertIn('default-state = "on";', hold)
        mcu_uart = self.node("&uart5")
        self.assertIn('status = "okay";', mcu_uart)
        self.assertIn("pinctrl-0 = <&uart5m1_xfer>;", mcu_uart)
        self.assertIn("wakeup-source;", mcu_uart)
        # UART3 M1 RX shares GPIO1_D1 with RV_HOLD.
        self.assertIn('status = "disabled";', self.node("&uart3"))
        self.assertNotIn("uart3m1_xfer", self.dts)

    def test_recovery_key_keeps_its_gpio(self):
        recovery = self.node("recovery")
        self.assertIn('label = "recovery";', recovery)
        self.assertIn("linux,code = <KEY_VENDOR>;", recovery)
        self.assertIn("gpios = <&gpio4 RK_PC0 GPIO_ACTIVE_LOW>;", recovery)
        self.assertIn("debounce-interval = <100>;", recovery)
        self.assertIn('status = "disabled";', self.node("adc-keys"))


if __name__ == "__main__":
    unittest.main()
