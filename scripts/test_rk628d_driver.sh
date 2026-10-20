#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SDK_DIR="${PICO_SDK_DIR:-$ROOT_DIR/pico-sdk}"
KERNEL_DIR="$SDK_DIR/sysdrv/source/kernel"
DRIVER="$KERNEL_DIR/drivers/media/i2c/rk628/rk628_csi_v4l2.c"
BT1120="$KERNEL_DIR/drivers/media/i2c/rk628/rk628_bt1120_v4l2.c"
HDMIRX="$KERNEL_DIR/drivers/media/i2c/rk628/rk628_hdmirx.c"
HDMIRX_HEADER="$KERNEL_DIR/drivers/media/i2c/rk628/rk628_hdmirx.h"
KERNEL_FRAGMENT="$KERNEL_DIR/arch/arm/configs/aiden-rk628.config"
DTS="$KERNEL_DIR/arch/arm/boot/dts/rv1106g-aiden-custom.dts"
BOARD_CONFIG="$ROOT_DIR/scripts/debian-system/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk"

require_pattern() {
    local pattern="$1"
    local file="$2"
    local message="$3"

    if ! grep -Eq "$pattern" "$file"; then
        echo "FAIL: $message" >&2
        exit 1
    fi
}

reject_pattern() {
    local pattern="$1"
    local file="$2"
    local message="$3"

    if grep -Eq "$pattern" "$file"; then
        echo "FAIL: $message" >&2
        exit 1
    fi
}

require_pattern '^CONFIG_MEDIA_CONTROLLER=y$' "$KERNEL_FRAGMENT" \
    "RK628 CSI requires the media controller API"
require_pattern '^CONFIG_VIDEO_V4L2_SUBDEV_API=y$' "$KERNEL_FRAGMENT" \
    "RK628 CSI requires the V4L2 subdevice API"
require_pattern '^CONFIG_VIDEO_RK628_CSI=y$' "$KERNEL_FRAGMENT" \
    "the kernel fragment must enable the RK628 CSI driver"
require_pattern '^# CONFIG_VIDEO_TC358743 is not set$' "$KERNEL_FRAGMENT" \
    "the RK628F production image must disable the unused TC358743 driver"
require_pattern '^# CONFIG_VIDEO_TC358743_CEC is not set$' "$KERNEL_FRAGMENT" \
    "the unused TC358743 CEC path must stay disabled"
require_pattern 'RK_KERNEL_DEFCONFIG_FRAGMENT=.*aiden-rk628\.config' "$BOARD_CONFIG" \
    "the board build must apply the HDMI bridge kernel fragment"

require_pattern 'compatible = "rockchip,rk628-csi-v4l2";' "$DTS" \
    "Aiden DTS must bind the RK628 CSI driver"
require_pattern 'rk628: rk628@50' "$DTS" \
    "Aiden DTS must use the strapped RK628 address 0x50"
reject_pattern 'tc358743' "$DTS" \
    "Aiden RK628F hardware must not retain the legacy TC358743 node"
require_pattern 'clock-frequency = <100000>;' "$DTS" \
    "RK628F I2C4 must run at the validated 100 kHz rate"
require_pattern 'pinctrl-0 = <&i2c4m1_xfer>;' "$DTS" \
    "RK628F must use the Aiden I2C4 M1 pin group"
require_pattern 'reset-gpios = <&gpio1 RK_PA1 GPIO_ACTIVE_LOW>;' "$DTS" \
    "RK628F reset must use Aiden GPIO1_A1"
require_pattern 'interrupts = <RK_PB1 IRQ_TYPE_LEVEL_HIGH>;' "$DTS" \
    "RK628F interrupt must use Aiden GPIO1_B1"
require_pattern 'rk628_reset_pin: rk628-reset-pin' "$DTS" \
    "RK628 reset must have a dedicated pinctrl group"
require_pattern '<1 RK_PA1 RK_FUNC_GPIO &pcfg_pull_none>' "$DTS" \
    "RK628 reset must be push-pull without an internal pull-up"
require_pattern '<1 RK_PB1 RK_FUNC_GPIO &pcfg_pull_up>' "$DTS" \
    "RK628 interrupt must retain its Aiden GPIO pull-up"

rk628_node="$(sed -n '/rk628: rk628@50 {/,/^[[:space:]]*};/p' "$DTS")"
rk628_input="$(sed -n '/rk628_csi_in: endpoint@0 {/,/^[[:space:]]*};/p' "$DTS")"

if ! grep -q '^[[:space:]]*reg = <0x50>;$' <<< "$rk628_node"; then
    echo "FAIL: RK628 node must use I2C register address 0x50" >&2
    exit 1
fi
if ! grep -q 'continues-clk;' <<< "$rk628_node" || \
        ! grep -q 'data-lanes = <1 2 3 4>;' <<< "$rk628_node" || \
        ! grep -q 'data-lanes = <1 2 3 4>;' <<< "$rk628_input"; then
    echo "FAIL: RK628 must retain its validated four-lane continuous-clock CSI contract" >&2
    exit 1
fi
if grep -qE 'clocks = <&cru MCLK_REF_MIPI0>|clock-names = "soc_24M"|GPIO_OPEN_DRAIN' <<< "$rk628_node"; then
    echo "FAIL: Aiden RK628F must use its onboard clock and push-pull reset" >&2
    exit 1
fi

require_pattern 'remote-endpoint = <&rk628_csi_out>;' "$DTS" \
    "CSI D-PHY must expose the RK628 endpoint"
require_pattern 'remote-endpoint = <&rk628_csi_in>;' "$DTS" \
    "RK628 output must link back to the CSI D-PHY"

require_pattern 'case RKMODULE_GET_HDMI_MODE:' "$DRIVER" \
    "RK628 driver must identify itself as an HDMI input"
require_pattern 'RKMODULE_HDMIIN_MODE' "$DRIVER" \
    "RK628 driver must report Rockchip HDMI input mode"
require_pattern '\.query_dv_timings[[:space:]]*=' "$DRIVER" \
    "RK628 driver must support HDMI timing discovery"
require_pattern '\.set_edid[[:space:]]*=' "$DRIVER" \
    "RK628 driver must accept the existing EDID setup path"
require_pattern 'def_edid\.blocks = ARRAY_SIZE\(edid_init_data\) / EDID_BLOCK_SIZE;' "$DRIVER" \
    "RK628 default EDID block count must be derived from its data"
require_pattern 'msleep\(200\);' "$DRIVER" \
    "RK628 EDID updates must hold HPD low long enough for HDMI sources"
require_pattern '\.get_mbus_config[[:space:]]*=' "$DRIVER" \
    "RK628 driver must report CSI lane and clock configuration"
require_pattern 'of_property_read_bool\(dev->of_node,' "$DRIVER" \
    "RK628 clock mode must remain device-tree controlled"
require_pattern '"continues-clk"' "$DRIVER" \
    "RK628 driver must consume the continuous-clock property"
require_pattern 'V4L2_MBUS_CSI2_CONTINUOUS_CLOCK' "$DRIVER" \
    "RK628 mbus configuration must report continuous clock when selected"
require_pattern 'V4L2_MBUS_CSI2_NONCONTINUOUS_CLOCK' "$DRIVER" \
    "RK628 mbus configuration must retain non-continuous-clock support"
# Hardware-version detection must select the F-specific receiver and CSI paths.
CORE="$KERNEL_DIR/drivers/media/i2c/rk628/rk628.c"
HEADER="$KERNEL_DIR/drivers/media/i2c/rk628/rk628.h"
require_pattern '#define GRF_SOC_VERSION[[:space:]]+0x0200' "$HEADER" \
    "RK628F chip identity must be read at the actual silicon version register"
require_pattern 'version == 0x20230321' "$CORE" \
    "the RK628F silicon revision must be recognized"
require_pattern 'rk628_hdmirx_verisyno_phy_power_on' "$DRIVER" \
    "RK628F must use its Synopsys receiver PHY instead of the RK628D combo PHY"
require_pattern 'SW_OUTPUT_COMBTX_MODE_MASK' "$DRIVER" \
    "RK628F must configure its own CSI output mux"
require_pattern 'rk628_mipi_dphy.o rk628_post_process.o' \
    "$KERNEL_DIR/drivers/media/i2c/rk628/Makefile" \
    "RK628F PHY and post-processing dependencies must be linked"
require_pattern 'link_freq->flags \|= V4L2_CTRL_FLAG_READ_ONLY' "$DRIVER" \
    "RK628 link frequency must be derived read-only state"
require_pattern 'v4l2_ctrl_s_ctrl\(csi->link_freq, index\);' "$DRIVER" \
    "link frequency updates must take the V4L2 handler lock"
require_pattern '\.set_edid = rk628_csi_set_edid,' "$DRIVER" \
    "userspace EDID updates must restart RK628F receiver configuration"
require_pattern '(return|ret =) -ENOLCK;' "$DRIVER" \
    "no signal must not be reported as a locked fallback VGA source"
require_pattern 'rk628_is_avi_ready\(csi->rk628, &csi->avi_rcv_rdy\)' "$DRIVER" \
    "RK628 CSI setup must observe live AVI readiness changes"
require_pattern 'rk628_is_avi_ready\(bt1120->rk628, &bt1120->avi_rcv_rdy\)' "$BT1120" \
    "the shared AVI API change must cover the BT1120 caller"
require_pattern 'READ_ONCE\(\*avi_rcv_rdy\)' "$HDMIRX" \
    "AVI polling must reload state updated by the interrupt path"
require_pattern 'i2c_set_clientdata\(client, sd\);' "$DRIVER" \
    "RK628 remove and audio callbacks must retain the V4L2 subdevice"

remove_body="$(sed -n '/^static int rk628_csi_remove(/,/^}/p' "$DRIVER")"
for cleanup in \
        'v4l2_async_unregister_subdev(sd);' \
        'cancel_work_sync(&csi->work_i2c_poll);' \
        'rk628_hdmirx_audio_destroy(csi->audio_info);' \
        'media_entity_cleanup(&sd->entity);' \
        'v4l2_ctrl_handler_free(&csi->hdl);'; do
    if ! grep -Fq "$cleanup" <<< "$remove_body"; then
        echo "FAIL: RK628 remove is missing cleanup: $cleanup" >&2
        exit 1
    fi
done

python3 - "$DRIVER" "$ROOT_DIR" <<'PYTEST'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[2])
driver = pathlib.Path(sys.argv[1]).read_text()
sdk = (root / "src/aiden_sdk.cpp").read_text()
fixtures = {}
for mode, rate, vic, clock in (("1080p60", 60, 16, 148500000), ("1080p30", 30, 34, 74250000)):
    fixture = bytes.fromhex((root / f"edid/hdmi_{mode}_cta.hex").read_text())
    fixtures[mode] = fixture
    assert len(fixture) == 256, f"{mode}: expected two EDID blocks"
    assert all(sum(fixture[i:i+128]) % 256 == 0 for i in (0, 128)), f"{mode}: bad checksum"
    assert fixture[20] & 0x80 and fixture[126] == 1 and fixture[128] == 2
    packaged = bytes.fromhex((root / f"overlay-debian/usr/share/aiden/hdmi/hdmi_{mode}_cta.hex").read_text())
    assert packaged == fixture, f"{mode}: installed EDID differs from fixture"
    dtd = fixture[54:72]
    pixel_clock = int.from_bytes(dtd[:2], "little") * 10000
    width = dtd[2] | ((dtd[4] & 0xf0) << 4)
    height = dtd[5] | ((dtd[7] & 0xf0) << 4)
    hblank = dtd[3] | ((dtd[4] & 0x0f) << 8)
    vblank = dtd[6] | ((dtd[7] & 0x0f) << 8)
    assert (width, height, pixel_clock) == (1920, 1080, clock), mode
    assert pixel_clock == rate * (width + hblank) * (height + vblank), mode
    cta = fixture[128:]
    video_vics = []
    has_hdmi_vsdb = False
    offset = 4
    while offset < cta[2]:
        header = cta[offset]
        length = header & 0x1f
        assert offset + 1 + length <= cta[2], f"{mode}: truncated CTA block"
        payload = cta[offset+1:offset+1+length]
        if header >> 5 == 2:
            video_vics.extend(value & 0x7f for value in payload)
        elif header >> 5 == 3 and payload[:3] == bytes((3, 12, 0)):
            has_hdmi_vsdb = True
        offset += 1 + length
    assert video_vics == [vic] and has_hdmi_vsdb, f"{mode}: unexpected CTA modes"


def extract_edid(source, declaration):
    match = re.search(declaration + r"\s*=\s*\{(?P<body>.*?)\};", source, re.DOTALL)
    assert match, f"Missing {declaration}"
    return bytes(int(value, 16) for value in re.findall(r"0x([0-9A-Fa-f]{2})", match.group("body")))

assert extract_edid(driver, r"static u8 edid_init_data\[\]") == fixtures["1080p60"]
assert extract_edid(sdk, r"static const uint8_t kDefaultHdmiEdid1080p30\[\]") == fixtures["1080p30"]
PYTEST

echo "PASS: RK628F driver integration contract"
