#!/bin/bash
# TC358743 I2C 通信诊断脚本

echo "=== TC358743 I2C 诊断 ==="
echo

echo "1. 检查 HDMI 桥接芯片检测情况："
for f in /sys/class/video4linux/v4l-subdev*/name; do
    if [ -r "$f" ]; then
        echo "  $f: $(cat "$f")"
    fi
done
echo

echo "2. 检查 I2C 总线状态："
if command -v i2cdetect >/dev/null 2>&1; then
    echo "  扫描 I2C bus 4 (TC358743 应该在 0x0f):"
    i2cdetect -y 4 2>/dev/null || echo "  无法访问 I2C bus 4"
else
    echo "  i2c-tools 未安装，跳过 I2C 扫描"
fi
echo

echo "3. 检查内核消息（最近 50 行 TC358743 相关）："
dmesg | grep -i tc358743 | tail -50
echo

echo "4. 检查视频设备状态："
ls -l /dev/video* 2>/dev/null || echo "  未找到 video 设备"
echo

echo "5. 检查 v4l-subdev 设备："
ls -l /dev/v4l-subdev* 2>/dev/null || echo "  未找到 v4l-subdev 设备"
echo

echo "6. 检查 frame_service 状态："
if command -v systemctl >/dev/null 2>&1; then
    systemctl status aico-frame.service --no-pager 2>/dev/null || echo "  aico-frame.service 未运行"
else
    echo "  未找到 systemctl"
fi
echo

echo "7. 检查配置文件中的桥接芯片设置："
if [ -f /etc/aico_frame_service.conf ]; then
    echo "  /etc/aico_frame_service.conf:"
    grep -E "SUBDEV|EDID|TRIGGER" /etc/aico_frame_service.conf | sed 's/^/    /'
fi
echo

echo "8. 检查加载的驱动模块："
lsmod | grep -E "tc358743|rk628|video" | sed 's/^/  /'
echo

echo "=== 诊断建议 ==="
echo "如果 I2C 扫描显示 0x0f 地址无响应 (显示 '--')："
echo "  → 硬件问题：检查供电、FPC 连接、I2C 线路"
echo
echo "如果检测到的是 rk628-csi 而不是 tc358743："
echo "  → 驱动加载错误：你的硬件是 RK628D 但内核加载了 TC358743 驱动"
echo
echo "如果完全没有 subdev 设备："
echo "  → CSI 连接问题或内核驱动未加载"
