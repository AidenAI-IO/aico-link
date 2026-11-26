# Aico Link SCH v1 启动修复

这些修改基于 aico_sch_v1_finalnew1.pdf（2026-09-07）的第 1、4、6–8、13 和 15 页，
并对照了 Rockchip RV1106 Datasheet V1.3 表 2-1 以及 SDK 的 pinctrl 定义进行核对。

## 接线修正

| 功能 | RV1106 引脚 / 复用 | 此前的冲突 |
| --- | --- | --- |
| WL_EN | 引脚 3，GPIO3_C5，无外部上拉 | Linux 不得驱动由 MCU 控制的电源轨 |
| BT 唤醒 | 引脚 4，GPIO3_C6 | 活动指示 LED 也驱动同一电气网络（net） |
| CPU AVS | 引脚 74，GPIO1_A2，PWM0 M0 | 固定稳压器和 BT 主机 IRQ |
| RK628 I2C | 引脚 93/92，GPIO1_C2/C3，I2C4 M1 | I2C3 M1 连到了 MCU UART |
| RK628 3.3V 使能 | 引脚 72，GPIO1_A0 | 被误分配为 TF 供电；旧的唤醒 GPIO32 |
| RK628 复位 | 引脚 73，GPIO1_A1 | 错误的 GPIO1_B0；旧的唤醒 GPIO33 |
| RK628 中断 | 引脚 78，GPIO1_B1 | 保留 |
| TF 供电使能 | 引脚 59，GPIO0_A1 | 错误的 GPIO1_A0 |
| 扬声器使能 | 引脚 22，GPIO4_C1，codec pa-ctl | GPIO3_C0 是 CSI 时钟；simple-card 未使用 spk-con |
| RV_HOLD | 引脚 87，GPIO1_D1 | 错误的 GPIO0_A3；与 UART3 M1 冲突 |
| MCU UART | 引脚 86/85，UART5 M1 | 错误的 UART3 M1 |
| ASRPro UART | 引脚 91/90，UART4 M1 | UART2 M1 属于调试控制台 |

充电器和 BQ27220 位于配套 MCU 的 I2C 总线上。移除虚构的 RV1106 I2C1 电量计节点；
I2C1 M1 会占用蓝牙 UART0 的 RX/TX。BT_WAKE_MCU 连到 MCU，不是 Linux 主机 IRQ。
必须由 MCU 拉高 WIFI_VCC_PWREN（GD32 PA4）；Linux 无法直接切换该电源。
Linux 可以通过 WL_EN_1V8 复位模块：RV1106 GPIO3_C5（GPIO 117）-> R169（1 kohm）-> 模块引脚 12。
第 15 页把 R168（10K_NC）放在 WF_WAKE_MCU_1V8 上，而不是 WL_EN 上；
旧 DTS 注释中声称 WL_EN 有外部上拉的说法是错误的。

镜像默认启用驱动（ENABLE_WIFIDRV=1）。在 Aico Link SCH v1 上，加载任何 AIC 模块之前，
aico-wifi-driver 会调用 aico-wifi-prepare。它只解绑 Wi-Fi 主机 ff9a0000.mmc，
将 WL_EN 拉低 1 秒，再拉高，等待 2 秒后重新绑定主机，并最多等待 5 秒让两个 AIC8800D80 SDIO
功能出现。由 MCU 控制的电源必须已经打开。枚举失败会中止驱动启动。GPIO 117 保持导出状态并为高电平。
如果服务重启时 wlan0 已存在，会跳过复位；如果接口不存在但已有 AIC 模块加载，
该辅助脚本会拒绝在运行中的驱动底下执行复位。

2026-10-08，两次软件重启复现了 SDIO 设备缺失的问题。仅重新探测主机，或不经低电平脉冲直接拉高 WL_EN，
都无法恢复。先给 WL_EN 一个从低到高的脉冲，再重新探测主机，可以恢复两个 SDIO 功能和 wlan0，
在 2.4/5 GHz 上分别扫描到 32 和 30 个 BSS 条目。不可移除的主机只扫描一次，
因此单独的 GPIO 脉冲不会触发枚举。这次恢复并不能证明 MCU 电源缺失时可以通过 WL_EN 恢复。
WLAN guard 功能开关仍保持禁用。

蓝牙 HCI 和 BLE 服务开关已启用（ENABLE_BLUETOOTH_HCI=1、ENABLE_BLE_SERVICE=1）。
2026-10-12，在一块运行中的 SCH v1 开发板上启用这两个开关后，AIC8800D80 控制器以 1.5 Mbaud
挂载在 /dev/ttyS0 上，出现了 hci0，ble_service 在 /run/ble_service/ble_service.sock 上启动，
并注册了一个广播实例；同时 wlan0 仍保持关联，失败的 systemd 单元数为零。
这次检查是在运行中的 rootfs 上修改开关并手动启动服务；重启、物理冷启动、手机配对和 ANCS 均未测试。

安装启动辅助脚本后，又进行了两次软件重启，Wi-Fi 均自动恢复，在两个频段上分别扫描到 29 和 33 个
BSS 条目，失败的 systemd 单元数为零。在 wlan0 存在时重启服务，日志记录了跳过复位。
这些检查是把 shell 辅助脚本直接部署到运行中的 rootfs 上完成的；本次修改没有烧录新的出厂镜像。
关联/DHCP 以及之后的物理冷启动均未测试。

## TF 卡适配

J4 使用 SDMMC0（`ffaa0000.mmc`），别名为 `mmc1`。DAT0/1/2/3 分别是 GPIO3_A3/A2/A7/A6，
CLK 是经过 R184（22 ohms）的 GPIO3_A4，CMD 是 GPIO3_A5，卡检测是经过 R126（100 ohms）的
GPIO3_A1，低电平有效。GPIO0_A1 使能 U20（WS4622C），将 VCC_3V3 切换到 VCC3V3_SD。
VCCIO4 通过 R185 固定为 3.3 V。

开发板 DTS 使用 `bus-width = <4>` 和 `max-frequency = <50000000>`。
这在内核首次探测时即生效，不需要启动服务或运行时 overlay。控制器使用 DAT0-3，
并配合下文描述的 pinctrl 修复。独立的 eMMC 和 Wi-Fi 总线仍分别为 8 位和 4 位。
仅枚举 SD 并使用固定的 3.3 V I/O 电源，可以避免探测其他卡协议或协商不受支持的信号电压。

厂商的 DW-MMC 驱动需要同时具备 `normal` 和 `idle` 两种 pinctrl 状态，才能保留其 pinctrl 句柄。
normal 选择 CMD/DAT/CLK，而 idle 在 GPIO3_A2-A7 上选择 GPIO 下拉，用于电源循环。
卡检测在两种状态下都保持 GPIO 模式并带上拉。开关稳压器声明了其 GPIO 和上游电源、
10 ms 的启动延迟以及 200 ms 的关/开延迟。主机使用 20 ms 的上电延迟和 200 ms 的断电延迟。
这些保守设置保留了经过测试的上电时序。

当 DAT2/DAT3（GPIO3_A7/A6）选择 SDMMC 功能 1 时，RV1106 pinctrl 驱动还会清除
`GPIO3_IOC_FORCE_JTAG_SDMMC.JTAG_SDMMC_AUTO_SWITCH_EN`。即使 IOMUX 设置正确，
自动 JTAG 覆盖也可能抢占这些线路，导致 4 位读取失败。pinctrl 路由会向 IOC 偏移 `0x202f4`
（物理地址 `0xff5582f4`）写入 `0x00010000`，利用高半字掩码只清除 bit 0。
常规引脚选择会在探测时以及 idle/电源循环切换之后应用此修复；regmap 写入失败会通过现有的路由路径向上传递。

存储发现现在通过 `/sys/block/mmcblk*/device/type` 选择 SD 卡，而不再假定为 `mmcblk2`；
这块开发板暴露的是 `/dev/mmcblk1p1`。挂载检查包括一次同步的临时写入和回读，
并拒绝只读挂载或 I/O 错误。读取失败不会触发空白卡格式化。Debian 包含 FAT32、ext4 和 exFAT
的检查/格式化工具。完整的可用性检查、显式设备覆盖以及默认 64 MiB 最小剩余空间，
参见 [存储管理器](04-agent/storage-manager.md)。

### 验证

`scripts/test_tf_dtb.sh` 会编译源 DTB，并检查 TF 模式、电源、卡检测和 pinctrl 状态，
包括针对错误总线宽度和时钟上限的回归检查。BSP 和出厂镜像审计会对两个打包的启动槽位应用相同的约定。
SDK 中针对 TF 开发板的专项测试也是本地快速检查的一部分。`tests/test_tf_pinctrl.py` 会编译生产环境的
RV1106 路由表和查找逻辑，以验证 DAT2/DAT3 两种选择、带掩码的寄存器更新，以及对无关引脚/功能的排除。
它在 quick 和 full 两种配置中都运行于同一个 `tf-card-dtb` 套件。不包含临时的控制器跟踪。

2026-10-15，烧录后的固件报告为 1 位、实际 24.75 MHz、3.3 V。FAT32 文件测试使用 32 MiB 的
direct-I/O 写入（包括 `fdatasync`），随后进行两次经 SHA-256 校验的直接读取：

| 卡 | 顺序读取 | 顺序写入 |
| --- | ---: | ---: |
| 4 GB NCard | 2.65 MB/s | 1.84 MB/s |
| 16 GB SL16G | 2.73 MB/s | 2.62 MB/s |

两项测试都通过了数据校验，且没有新的 TF I/O 错误；临时文件已删除，卡仍以读写方式挂载。
一张 128 MB 的卡也通过了直接读取，但其剩余空间低于应用的 64 MiB 阈值。
这些历史检查只覆盖 1 位 / 25 MHz；下文后续的 JTAG 调查覆盖了更宽模式下的抽样读取。

2026-10-22，在 Codex 会话 `01a15ddf-f96f-7eb3-a1c3-181507b1c5c2` 中进行的独立调试固件调查
定位到了 JTAG 覆盖问题：启用 bit 0 会复现 4 位读取失败，清除它则恢复读取。
在清除该位的情况下，一张 123 MiB 的卡在两种总线宽度下、请求频率为 100 kHz、400 kHz、1 MHz、
25 MHz 和 50 MHz 时均通过了抽样读取。本项目把该会话中的永久 pinctrl 修复直接移植到了内置的 SDK 中。
这些调试结果并未验证写入，也未验证本项目重新构建的固件在冷启动后能否自动应用修复。
生产默认值现为 4 位 / 50 MHz 并带 JTAG 修复；重新构建镜像的冷启动和写入验证仍待完成。

检查已烧录的开发板：

```bash
ssh aico@192.168.42.1 'sudo -n cat /sys/kernel/debug/mmc1/ios'
ssh aico@192.168.42.1 'curl -fsS http://127.0.0.1/api/storage/status'
ssh aico@192.168.42.1 'sudo -n journalctl -k -b --no-pager'
```

TF 设置位于 SDK 开发板 DTS 中，JTAG 修复位于内核 pinctrl 驱动中。两者都需要重新构建 BSP 启动镜像；
仅更新 Agent 不会改变内核的总线模式，也不会应用 pinctrl 修复。

## 媒体初始化

不要写入 CIF/ISP clr_unready_dev。它会在 RK628 端点缺失时强制完成，而厂商的 CIF 格式检查会解引用空的输入格式。
应保留异步图以进行正常绑定，并允许 RGA、MPP、NPU、codec 和 Rockit 在没有桥接芯片时加载。
同时在内核中拒绝空的 CIF 输入格式。缺失桥接芯片的加载器测试覆盖了这种失败模式以及幂等的重复初始化。

在型号为 Aico Link SCH v1 的设备上，Agent 会禁用 Pico Zero 的 GPIO32/33 唤醒监视器。
这些引脚现在专用于 RK628 的电源/复位。这并没有实现 MCU 按键/语音唤醒协议支持。

## CPU 稳压器

SY8892 的反馈电压为 0.6V。R82=56k、R84=100k、R85=24k、R86=150k、R87=10k，可得：

- Vx = (Vpwm/24 + 0.6/150) / (1/24 + 1/150 + 1/10)
- Varm = 0.6 + 56 * (0.6/100 + (0.6 - Vx)/150)
- PWM 为低时 Varm 标称值为 1.149933V，PWM 为高时为 0.803865V。

使用反相的 200kHz PWM、标称 0.804–1.150V 的限值以及 250us 的稳定时间。
这些是根据原理图推导出的值，而非开发板实测值。烧录后请在多个 OPP 下用万用表/示波器验证 VDD_ARM。
继承的 OPP 请求（0.85–1.0V）现在落在稳压器范围之内。

## 烧录后验证

重启进入匹配的 boot/OEM/rootfs 构建：之前的 Oops 会让运行中的内核处于 tainted 状态，
因此重启其服务并不是一次干净的内核测试。

- 检查 systemctl --failed、本次启动的内核日志、/sys/kernel/debug/gpio 和 /sys/kernel/debug/pwm。
- 确认 SDIO 功能枚举、wlan0、AIC 固件加载以及 hci0。如果 SDIO 仍未枚举，
  测量由 MCU 控制的 WiFi 3.3V 和 1.8V 电源。
- 确认 4-0050 绑定到 rk628-csi-v4l2，然后在连接信号源的情况下验证 HDMI。
  也要测试不连接 HDMI 信号源的启动；不能接受任何 CIF Oops。
- 检查音频播放/录音、NPU/RGA/MPP 设备节点，以及 CPU 频率切换过程中没有电压设置错误。
- 将 WiFi 连接到提供 DHCP 路由/DNS 的网络，然后验证 ip route、timedatectl timesync-status
  和 timedatectl show -p NTPSynchronized。仅 USB 的 192.168.42.0/24 没有互联网路由，
  因此 NTP 无法同步。不要把开发主机硬编码为默认网关，也不要伪造 NTP 同步。
