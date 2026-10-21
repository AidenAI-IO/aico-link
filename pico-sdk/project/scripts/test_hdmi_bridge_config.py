#!/usr/bin/env python3
import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
DTS = ROOT / "sysdrv/source/kernel/arch/arm/boot/dts/rv1106-luckfox-pico-zero-ipc.dtsi"
KERNEL_FRAGMENT = ROOT / "sysdrv/source/kernel/arch/arm/configs/aiden-rk628.config"
TC358743_DRIVER = ROOT / "sysdrv/source/kernel/drivers/media/i2c/tc358743.c"


class HdmiBridgeConfigTest(unittest.TestCase):
    def test_rk628_driver_is_enabled_and_tc358743_is_disabled(self):
        text = KERNEL_FRAGMENT.read_text()
        self.assertIn("CONFIG_VIDEO_RK628_CSI=y", text)
        self.assertIn("# CONFIG_VIDEO_TC358743 is not set", text)
        self.assertIn("# CONFIG_VIDEO_TC358743_CEC is not set", text)

    def test_both_i2c_bridges_are_declared(self):
        text = DTS.read_text()
        self.assertIn("rk628-csi@50", text)
        self.assertIn('compatible = "rockchip,rk628-csi-v4l2";', text)
        self.assertIn("tc358743@f", text)
        self.assertIn('compatible = "toshiba,tc358743";', text)

    def test_bridges_have_separate_dphy_endpoints(self):
        text = DTS.read_text()
        self.assertIn("rk628_csi_in: endpoint@0", text)
        self.assertIn("tc358743_csi_in: endpoint@1", text)
        self.assertIn("remote-endpoint = <&rk628_csi_out>;", text)
        self.assertIn("remote-endpoint = <&tc358743_csi_out>;", text)

    def test_bridge_specific_lane_and_clock_modes_are_preserved(self):
        text = DTS.read_text()
        rk_start = text.index("rk628_csi: rk628-csi@50")
        tc_start = text.index("tc358743_csi: tc358743@f")
        rk_text = text[rk_start:tc_start]
        tc_text = text[tc_start:text.index("&mipi0_csi2")]

        self.assertIn("continues-clk;", rk_text)
        self.assertIn("data-lanes = <1 2 3 4>;", rk_text)
        self.assertIn("clock-noncontinuous;", tc_text)
        self.assertIn("data-lanes = <1 2>;", tc_text)
        self.assertIn("link-frequencies = /bits/ 64 <297000000>;", tc_text)

    def test_tc358743_probe_rejects_missing_i2c_device(self):
        text = TC358743_DRIVER.read_text()
        self.assertIn("static int i2c_rd16_checked", text)
        self.assertIn("err = i2c_rd16_checked(sd, CHIPID, &chip_id);", text)
        self.assertIn("no TC358743 response", text)


if __name__ == "__main__":
    unittest.main()
