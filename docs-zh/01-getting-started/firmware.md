---
sidebar_position: 4
---

# 固件构建与烧录

固件使用 boot/rootfs A/B 槽位，每个 rootfs 槽位 1792 MiB，OTA 清单 schema 版本为 2。
从旧的 OEM 分区布局迁移需要完整烧录镜像；无法通过 OTA 从旧布局迁移。
当前固件没有 OEM 分区，也没有 `/oem` 目录。

对于已经使用这种 A/B 布局的开发板，过渡到 Aico 技术名称需要一次完整且相互匹配的 boot 和 rootfs OTA，
这会保留 `/userdata`。仅靠业务 `.deb` 无法完成这一过渡。参见
[产品名称与技术标识符](../02-architecture/naming.md)。

## 获取固件

**Aico Link Channel Release** 工作流会将改动与所选通道的上一个发布版本进行比较。系统改动会生成已签名的固件；
仅业务改动会生成 Debian 软件包。主工作流每小时在 UTC `:17` 检查一次 `main`，并将有变化的内容发布到
`dev`；内容未变化时跳过。三个通道（`dev`、`staging`、`prod`）也都支持手动发布；
参见 [通道发布](../08-ota/channel-release.md)。主入口和备用入口默认进行经过验证的发布，
固件构建分别使用 `aico-hosted-01` 和 `aico-hosted-02`。回退构建和独立的软件包构建只上传产物。
你也可以使用 `./debian_build.sh` 在本地构建镜像，或通过项目的手动分发流程获取经过审核的 `update.img`。

烧录完整固件时，通常使用 `update.img`。

## 固件特性

本项目的固件基于 `pico-sdk` 构建，包含以下定制：

- AIC8800D80 Wi-Fi 驱动默认启用（`ENABLE_WIFIDRV=1`）。GD32 MCU 控制 `WIFI_VCC_PWREN`，
  并在 RV1106 重启期间保持模块供电，因此模块可能仍在运行上一次启动时的固件。
  所以 SCH v1 每次启动都会复位模块：`aico-wifi-prepare` 会解绑 Wi-Fi SDIO 主机，
  将 WL_EN 先拉低再拉高，重新绑定主机，并在加载任何 AIC 模块之前等待两个 SDIO 功能出现。
  随后 BSP 从 ROM 将共享的 Wi-Fi/蓝牙固件下载到模块中；绝不会复用之前启动遗留下来仍在运行的固件。
  蓝牙准备依赖已加载的 Wi-Fi 固件驱动。蓝牙 HCI 和 BLE 服务默认启用（`ENABLE_BLUETOOTH_HCI=1`、
  `ENABLE_BLE_SERVICE=1`），并在 `/dev/ttyS0` 上挂载控制器；
- 内核构建 Rockchip RK628 HDMI 转 CSI 的 V4L2 驱动，并禁用未贴装的 Toshiba TC358743 路径；
- Aico Link SCH v1 的 DTS 将 RK628F 声明在 100 kHz 的 I2C4 上、地址为 `0x50`，
  使用 GPIO1_B0 作为复位、GPIO1_B1 作为中断，并配置四条连续时钟的 CSI 通道；
- RK628F 保留驱动提供的 1080p60 EDID；
- USB-C 端口在启动时配置为复合 gadget：键盘 HID、指针/触控 HID，以及 CDC ECM 网络
  （`usb0`，默认 `192.168.42.1`）；
- 从 `overlay-debian/`、BSP 模块和库以及已安装的 `aico-business` 软件包构建 Debian 13 rootfs。
  不会创建 OEM 分区或 `/oem` 目录。

相关的底层改动直接在内置的 `pico-sdk/` 目录中跟踪。

## 在本地构建完整固件

需要 x86_64 Linux + Docker 环境，或能够运行 amd64 容器的兼容环境：

```bash
./debian_build.sh
```

该命令需要外部的 Agent 配置以及匹配的 Ed25519 OTA 密钥对（参见 `./debian_build.sh --help`）。流程概览：

1. 构建并审计 Debian armhf 的 C/C++ 和 Go 应用程序包；
2. 从 `pico-sdk` 构建 RV1106 BSP、bootloader、内核模块和 A/B 启动镜像；
3. 构建固定版本的 Debian 13 rootfs，安装 `aico-business`，并应用 `overlay-debian/`；
4. 在归档之前，将 BSP 库/模块和 OTA 公钥安装到 rootfs 中；
5. 创建 rootfs、userdata 和 OTA 镜像并验证其内容；
6. 生成已签名的本地 OTA 清单和完整的 USB 首次烧录包。

构建完成后，镜像位于：

```text
output/debian/image/
```

## 固件中的 pip

Debian 生产软件包集包含 `python3` 和 `python3-pip`。构建镜像后，用以下命令验证运行时：

```bash
/usr/bin/python3 -m pip --version
```

运行时安装的软件包保存在 `/userdata` 下；参见 [持久化 Python 软件包](../04-agent/python-packages.md)。

## 系统挂起

Debian 开发板配置包含 `rv1106-pm.config`，以及 Aico Link 开发板 DTS 中针对 suspend-to-idle（`s2idle`）
和 suspend-to-RAM（`deep`）的设置。镜像会安装 `/usr/bin/aico-power`，可在常规 shell 的 PATH 中使用。
以 root 身份运行任一命令：

```bash
aico-power sleep       # Coordinated s2idle suspend
aico-power deep-sleep  # Reports unsupported on this board
```

该命令会启动 `aico-suspend.service` 并等待其结果。工作进程独立于发起调用的 SSH 会话运行。
它会验证挂起模式以及已启用的 `rv1106_idle` 计时回调，同步文件系统，并选择 `/sys/power/mem_sleep`。
它会通过 wpa_supplicant 暂时断开处于已连接或正在连接状态的 `wlan0`，然后通过本地服务获取 MCU 的挂起许可，
接着读取 `/sys/power/wakeup_count`，并在把该计数和 `mem` 写入 `/sys/power/state` 之前确认许可仍然有效。
每项控制都使用一次无缓冲写入：挂起失败后，不会在关闭文件时带着已过期的 wakeup-count 检查重试。
唤醒或失败后，它会发送 SUSPEND_RESUME 并请求 Wi-Fi 重新连接。原本处于断开、非活动或禁用状态的接口保持离线；
不会更改任何网络凭据、接口电源策略或已保存的网络配置。supplicant 命令失败会阻止挂起。
发出重连请求后，关联和地址获取是异步进行的。

每次执行外部操作之前，恢复意图都会保存到 root 所有的 `/run/aico-suspend/state.json` 中。
如果工作进程退出或被终止，`ExecStopPost` 会重试清理。每次调用只做一次有时限的尝试。
由 `aico.target` 启动的 `aico-suspend-recover.timer` 也会在启动后 15 秒以及每次恢复激活结束后 10 秒运行恢复。
清理失败时，待处理的操作会保留到下一次激活；清理成功后不会再发送 UART 请求。
在 MCU/UART 仍然供电的情况下，许可可以在 RV 重启后继续存在，而 `/run` 不能。
因此恢复流程会在每次 RV 启动时确认一次 SUSPEND_RESUME，并记录 `/run/aico-suspend/boot-recovered`；
睡眠请求必须在 PREPARE 之前完成同样的恢复。重复的 RV_READY 并不足够。
所有恢复入口都使用工作进程的锁，因此不会撤销仍然有效的挂起许可。
MCU 持续失败会阻止新的睡眠，并让恢复保持待处理状态。`sudo aico-power --recover` 会重试这些操作，
而不会请求新的睡眠。查看 `journalctl -u aico-suspend.service -u aico-suspend-recover.service`
和 `systemctl list-timers aico-suspend-recover.timer` 了解结果。不要从 SSH 会话直接调用内部的
`--execute` 工作进程。如果内核唤醒源持续活动五秒，工作进程会取消许可、恢复 Wi-Fi 并报告阻塞因素。
它会等待挂起请求返回，并以非零退出状态报告内核错误。使用以下命令查看可用状态：

```bash
cat /sys/power/state
cat /sys/power/mem_sleep
```

`mem_sleep` 中可能同时出现 `s2idle` 和 `deep`；方括号表示当前选中的模式。开发板命令只启用 `s2idle`。
深度睡眠会保留 RAM 和 RV 电源保持信号；它不会让 RV1106 断电。RK628 驱动会在系统挂起期间关闭其使能 GPIO，
并在恢复时重新打开。此命令不会更改 ASRPRO 和 Wi-Fi/蓝牙的电源控制。如果某个外设驱动拒绝挂起，
请求就会失败；该命令不会通过解绑驱动来绕过这个错误。

UART5 仍是 MCU 链路的唤醒源，因此周期性的 MCU 通信可能会立即唤醒 `s2idle`。MCU 在批准挂起请求后会暂停周期性探测。
深度睡眠恢复需要合适的 PMU bank GPIO 唤醒源及其内核配置；UART5 不是深度睡眠的唤醒路径。
当前开发板原理图没有把 MCU 输出连接到 PMU bank 上的 RV 唤醒引脚，因此 `deep-sleep` 会返回错误，
而不改变电源状态。未来的开发板版本需要经过验证的 MCU 唤醒输出和内核唤醒 IRQ，才能启用 deep。
硬件测试中使用的临时
J2-13/GPIO0_A6 唤醒按键不会由生产 DTS 安装。挂起期间 shell 和网络访问会暂停。

进行硬件验证时，比较命令执行前后的 `/sys/kernel/debug/suspend_stats`，并在内核日志中检查恢复时的 IRQ。
在这块开发板上，触摸唤醒应该报告 `ttyS5` 以及对应的 MCU `touch_key` 事件。
仅凭 SSH 无响应并不能证明挂起成功。
SCH v1 通过 R204/R203 把 USB_VBUSDET 连接到 VSYS_LOAD。OTG PHY 使用 `rockchip,vbus-always-on`
描述这种接线，并跳过外部 VBUS 充电器检测，否则其 SDP/CDP 路径会无限期占住 `rockchip_otg`。
控制器保持在外设模式。系统挂起会断开 USB gadget；恢复时会重新枚举，因此睡眠期间手机连接会中断。
这并不是 USB 会话保持或 USB 远程唤醒支持。

RK628 的恢复回调会在掉电后恢复其 1194 MHz 的 CPLL 参考时钟；关于未校准的恢复时钟导致的原始帧损坏，
参见 [RK628F 硬件适配](../03-hardware/rk628f-adaptation.md)。除了 HDMI 锁定和服务健康之外，
还要在唤醒后验证一张真实的新截图。

RV1106 CPUidle 驱动在非零的状态索引上提供了一个 WFI `enter_s2idle` 回调，这是 5.10 CPUidle 核心所要求的。
这使核心能够在用户空间被冻结时冻结 tick 和计时。`CLOCK_MONOTONIC` 不计入这段时间；
`CLOCK_BOOTTIME` 和挂钟时间会计入。systemd 服务看门狗保持启用。旧内核没有注册 idle 驱动，
因此其单调时钟在睡眠期间继续前进，服务可能在恢复时立即超时。挂起辅助脚本会拒绝这种旧配置。
该回调只执行 WFI：它不会启用 PMU 深度睡眠或 DDR 自刷新。

如需进行长时间检查，先设置一个 MCU 唤醒定时器，记录两个时钟以及服务的 PID/NRestarts，
然后睡眠超过服务看门狗周期的时间。唤醒后，确认睡眠时间差有所增长、看门狗保持启用、服务没有重启、
Wi-Fi 已恢复，并且一张新截图完好。使用公开命令进行这项检查，以便同时覆盖 systemd 工作进程和 Wi-Fi 自动恢复。
在调用之前设置好 MCU 定时器；定时器包含文件系统同步和挂起准备时间，因此测得的冻结时长会短于定时器请求的延迟。

将冷启动模块和热重启的结果与挂起验收分开记录。在 SCH v1 上，保持型 OFF 开关会让 Wi-Fi 和 ASR 继续供电；
拨动它并不能建立冷启动的 Wi-Fi 基线。比较这些路径时，请记录内核版本、启动 ID 和挂起计数器。
仅能访问串口 shell 或 SDIO 枚举成功，并不能证明 Wi-Fi 固件可以响应命令。

## 烧录固件

> 你需要将 Luckfox Pico Zero 板载的 USB-C 端口连接到电脑。烧录方法有多种；完整说明请参阅 [Luckfox Pico Zero 官方烧录指南](https://wiki.luckfox.com/zh/Luckfox-Pico-Zero/Flash-image/)。

### 1. 进入 Maskrom / Loader 模式

可用方法：

- 按住开发板的 BOOT 按键，同时插入 USB-C；
- 如果用 BOOT 按键触发烧录模式效果不好，可以先通过 USB 网络上的 SSH 或 TTL 串口登录开发板，然后让 systemd 在重启时传递 loader 参数：

```bash
systemctl reboot --reboot-argument=loader
```

固件镜像在开发板上包含 `adb` 客户端，使其可以作为外部 Android 设备的 ADB host。
它不会暴露 `adbd`，因此主机端无法用 `adb shell` 登录开发板本身。该客户端基于 nmeum/android-tools
30.0.5p1 版本（AOSP platform-tools-30）构建，报告的版本为 1.0.41，因此支持当前的 adb 认证和配对协议。

### 2. 使用 upgrade_tool 烧录

在 Linux 上，使用内置 `pico-sdk/` 目录中的 x86_64 工具。仓库根目录下的 `upgrade_tool/upgrade_tool`
是 macOS Mach-O 二进制文件，无法在 Linux 上运行。带保护机制的烧录辅助脚本会校验镜像摘要，
并要求显式确认，因为完整的出厂烧录会覆盖 userdata：

```bash
FLASH_TOOL=pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool
IMAGE=output/debian/image/update.img
SHA256=$(awk '{print $1}' "${IMAGE}.sha256")
scripts/flash.sh inspect --tool "${FLASH_TOOL}"
sudo scripts/flash.sh flash \
  --tool "${FLASH_TOOL}" \
  --image "${IMAGE}" \
  --sha256 "${SHA256}" \
  --confirm-erase-all-data
```

对于预构建的镜像，替换 `IMAGE` 并提供经过独立验证的 SHA-256。在 macOS 上，
可以使用仓库根目录下的 `upgrade_tool/upgrade_tool` 命令烧录本地构建的镜像：

```bash
./upgrade_tool/upgrade_tool uf ./output/debian/image/update.img
```

不要在 Linux shell 中使用那个 Mach-O 二进制文件。

## 分区参考

生产镜像使用 A/B 分区布局：

| 分区 | 大小 | 用途 |
| --- | ---: | --- |
| `env` | 32 KB | bootloader 环境，仅用于出厂/USB 恢复 |
| `idblock` | 512 KB @ 32 KB | Rockchip idblock，仅用于出厂/USB 恢复 |
| `uboot` | 256 KB | bootloader，仅用于出厂/USB 恢复 |
| `misc` | 4 MB | SPL A/B 元数据，AVB A/B 记录位于字节偏移 `2048` |
| `boot_a` | 32 MB | 槽位 A 的 FIT 启动镜像，指向 `rootfs_a` |
| `boot_b` | 32 MB | 槽位 B 的 FIT 启动镜像，指向 `rootfs_b` |
| `rootfs_a` | 1792 MiB | 槽位 A 的根文件系统 |
| `rootfs_b` | 1792 MiB | 槽位 B 的根文件系统 |
| `userdata` | 3 GB | 共享的非 OTA 持久化数据 |
| `ota` | 300 MiB | 专用的 OTA 状态、健康标记和下载缓存 |

`upgrade_tool` 支持更新单个分区；完整升级通常使用 `uf update.img`。

生产镜像使用 A/B 分区布局。在线 OTA 只写入非活动槽位的 `boot_*` 和 `rootfs_*` 分区；`env`、`idblock` 和 `uboot` 只用于出厂或 USB 恢复烧录，不会通过 OTA 更新。`misc` 分区保存 Rockchip SPL A/B 元数据，位于字节偏移 `2048`。

Debian `update.img` 包含一个初始为空的 `ota.img`。生成的出厂配置存储在 `userdata.img` 的 `/debian/ota/config.json` 中，启动后出现在 `/userdata/debian/ota/config.json`。它包含 `repo`、`channel`、`factory_version`、`factory_build_time` 以及按槽位区分的 `factory_partition_hashes`；`/userdata/ota` 仍是用于状态和下载的专用工作区。

更多 OTA 细节参见 [OTA 概览](../08-ota/README.md)。
