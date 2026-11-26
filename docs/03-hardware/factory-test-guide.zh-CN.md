# 产线测试指南

[English](factory-test-guide.md)

本指南面向贴片（SMT/PCBA）产线及配合产线的硬件工程师，覆盖一块
Aico Link SCH v1 板子从空板刷机到留存测试记录的全过程：

1. [准备测试工位](#1-准备测试工位)（每个工位做一次）
2. [刷固件](#2-刷固件)
3. [登录板子](#3-登录板子)
4. [运行硬件测试](#4-运行硬件测试)
5. [留存测试记录](#5-留存测试记录)
6. [查看失败原因](#6-查看失败原因)
7. [出货前处理](#7-出货前处理)

每项检查测什么、合格标准是多少，见
[Board Hardware Tests](board-hardware-tests.md)。本指南只覆盖 RV1106
这一侧，MCU 固件另行烧录。

> 刷机会清空板子上的所有数据，包括 `/userdata`。客户或工程师正在使用的板子，
> 未经对方同意不要刷机。

## 1. 准备测试工位

### 设备

| 设备 | 用途 |
| --- | --- |
| 测试电脑，Linux（Ubuntu 22.04 及以上）或 macOS | 刷机、登录、运行测试。Windows 可以刷机（见下文），但没有板子 USB 网卡（CDC ECM）的自带驱动，登录和测试请用 Linux 或 macOS |
| USB-C 数据线 | 连接电脑和板子的 USB-C 口 J3。只能充电的线不行 |
| 带 USB-C 视频输出（DisplayPort Alt Mode）的视频源 | HDMI 采集测试需要。USB-C 口能输出视频的笔记本，可以同时当测试电脑和视频源 |
| USB 转 TTL 串口模块（可选） | USB 网络起不来时用作调试串口 |
| 产线 Wi-Fi 热点（可选） | Wi-Fi 检查需要，请使用 2.4 GHz 网络 |
| 安静环境、5 V 充电器、TF 卡、带蓝牙的手机（可选） | 交互检查用：喇叭/麦克风、充电检测、TF 卡、BLE 连接 |

### 发布包

每个固件版本由工程侧提供一套发布包，不同版本的文件不要混用。

| 文件 | 用途 |
| --- | --- |
| `update.img` | 完整固件镜像 |
| `update.img.sha256` | 镜像的 SHA-256，用来确认文件完整 |
| `board/` | 板端测试脚本（同一版本的 `scripts/board/`） |
| `board_hw.conf`（可选） | 产线测试配置，例如期望的视频分辨率或测速服务器。由 `board/board_hw.conf.example` 修改而来 |
| 工位测试密码 | 产线给每块板子设置的测试用密码（见第 3 步） |

### 刷机工具

- **Linux**：`pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool`；如果发布包里带了代码仓库，
  还可以用带保护的辅助脚本 `scripts/flash.sh`。工具在工位上装一次，用
  `sudo ./upgrade_tool ld`（列出已连接的瑞芯微设备）确认可用。
- **macOS**：仓库根目录下的 `upgrade_tool/upgrade_tool`。
- **Windows**：在 `pico-sdk/tools/windows/` 中先安装一次 `DriverAssitant`，
  单板刷机用 `SocToolKit`，产线多板并行刷机用 `FactoryTool`。

## 2. 刷固件

### 2.1 校验镜像

每套发布包校验一次即可，不需要每块板都做：

```bash
sha256sum -c update.img.sha256      # macOS：shasum -a 256 -c update.img.sha256
```

结果必须是 `update.img: OK`，否则重新拷贝发布包。

### 2.2 让板子进入刷机模式（Maskrom/Loader）

- **新板，或无法启动的板子**：按住 BOOT（Maskrom）键，同时把 USB-C 线插到 J3，
  约两秒后松开。空板会自动进入 Maskrom 模式。
- **已经在运行固件的板子**：登录板子（第 3 步）后执行
  `sudo systemctl reboot --reboot-argument=loader`。

确认电脑只识别到一块板子：

```bash
sudo ./upgrade_tool ld
```

必须列出一台处于 `Maskrom` 或 `Loader` 模式的设备。如果什么都没列出，
换一根线或换一个 USB 口，再按一遍按键流程。

### 2.3 写入镜像

在 Linux 上用仓库里的辅助脚本（会校验哈希，并确认只连了一块板）：

```bash
sudo scripts/flash.sh flash \
  --tool pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool \
  --image update.img \
  --sha256 "$(awk '{print $1}' update.img.sha256)" \
  --confirm-erase-all-data
```

只用刷机工具（Linux 或 macOS）：

```bash
sudo ./upgrade_tool uf update.img
```

Windows 上在 SocToolKit 或 FactoryTool 里加载 `update.img` 并开始升级。
所有工具都必须以成功提示结束（`upgrade_tool` 显示 `Upgrade firmware ok.`），
之后板子会自动重启。

### 2.4 首次启动

首次启动比之后慢（几分钟），系统要扩展文件系统并生成设备身份。
电脑上出现一个新的网卡、地址在 `192.168.42.0/24` 网段，并且
`ping 192.168.42.1` 能通，说明板子已就绪。

## 3. 登录板子

刚刷完的板子**没有登录密码**：在设置设备密码之前，`aico` 账号是锁定的。
请先设置工位测试密码。

### 3.1 设置测试密码

1. 在测试电脑的浏览器里打开 `http://192.168.42.1`。通过 USB 访问时不需要登录。
2. 进入 **高级设置 → 设备密码**，设置工位测试密码。

### 3.2 通过 USB 用 SSH 登录（常规方式）

```bash
ssh aico@192.168.42.1
```

输入工位测试密码。重新刷过机的板子会生成新的 SSH 主机密钥；如果 SSH 提示
"REMOTE HOST IDENTIFICATION HAS CHANGED" 并拒绝连接，删除旧密钥后再连：

```bash
ssh-keygen -R 192.168.42.1
```

管理员命令用 `sudo`，密码相同。

### 3.3 串口登录（备用方式）

USB 网络没有出现时使用。

- 把 USB 转 TTL 模块接到板子的调试串口（RV1106 UART2 M1，焊盘位置见板子丝印或原理图）：
  模块 TX 接板子 RX，模块 RX 接板子 TX，GND 接 GND。模块的电源脚不要接。
- 串口参数：**115200 波特率，8N1，无流控**。例如 Linux 上用
  `screen /dev/ttyUSB0 115200`，Windows 上用 PuTTY。
- 按回车，用 `aico` 和设备密码登录。如果还没设置设备密码，串口会显示启动信息，
  但不接受登录。

### 3.4 连接产线 Wi-Fi（做 Wi-Fi 检查时）

在 `http://192.168.42.1` 打开 **Wi-Fi**，选择产线热点并输入密码。
如果测试计划不包含 Wi-Fi 检查，可以跳过这一步，Wi-Fi 检查会报告板子未连接网络。

## 4. 运行硬件测试

### 4.1 拷贝测试脚本

在发布包 `board/` 文件夹所在的目录下执行：

```bash
scp -r board aico@192.168.42.1:/tmp/
```

`/tmp` 里的副本在板子重启后会消失，每次重启后都要重新拷贝。

### 4.2 无人值守测试（每块板都做）

把视频源接到 J3，并让它显示任意画面，然后执行：

```bash
ssh -t aico@192.168.42.1 \
  'sudo sh /tmp/board/board_hw_test.sh --artifact-dir /userdata/aico-hwtest'
```

如果有产线配置文件，把它和脚本一起拷过去，并加上
`--config /tmp/board/board_hw.conf`。

整个测试约三分钟，不需要人在板子旁边。会检查系统服务、HID 和 USB、
喇叭和麦克风（会播放测试音）、HDMI 采集、MCU（电池、定时器、唤醒源）、
Wi-Fi、蓝牙、存储、NPU 和 RTC。

### 4.3 交互测试（抽检或维修时）

加上 `--interactive` 会包含需要人操作的步骤。每一步之前脚本会给出提示，
等待回车或 `y`/`n` 回答：

```bash
ssh -t aico@192.168.42.1 \
  'sudo sh /tmp/board/board_hw_test.sh --artifact-dir /userdata/aico-hwtest \
     --interactive --hotplug --charger --ble-connect'
```

| 选项 | 操作员需要做什么 |
| --- | --- |
| `--interactive` | 对着麦克风说话、确认 LED、触摸触摸键、说唤醒词、插入 TF 卡 |
| `--hotplug` | 按提示拔下并重新插上视频线 |
| `--sleep-test` | 按提示关闭再打开视频源的屏幕 |
| `--charger` | 按提示拔下并重新插上充电器 |
| `--ble-connect`（新手机再加 `--ble-pair`） | 用手机通过蓝牙连接板子 |
| `--modes all` | 不需要操作，脚本会切换 HDMI 输入模式并恢复原设置 |

用 `--only` 只运行某个测试套件，例如 `--only audio` 或 `--only mcu,hdmi`。
`--help` 会列出所有选项。

### 4.4 判读结果

输出的末尾类似这样：

```text
================ summary ================
lowlevel     PASS  pass=..  warn=0   fail=0   skip=..
audio        PASS  ...
hdmi         PASS  ...
mcu          PASS  ...
peripherals  PASS  ...
RESULT: no failures
...
================ evidence ================
run id      20261125T071150Z  (2026-11-25T07:11:50Z -> 2026-11-25T07:14:43Z, exit 0)
board       serial=168028e8bac29c2e boot_id=... host=aico
clock       2026-11-25T07:14:44Z ntp_synchronized=True rtc_minus_system=0s
failures    0 FAIL, 0 WARN; details with context in failures.txt
bundle      /userdata/aico-hwtest/aico_hw_20261125T071150Z.tar.gz
bundle sha256 <64 位十六进制>
```

| 结果 | 含义 | 处理 |
| --- | --- | --- |
| `RESULT: no failures` 且没有 `WARN` | 合格 | 留存记录（第 5 步） |
| `RESULT: no failures`，但有 `WARN` 行 | 合格但有备注：数值接近限值，或某项没能完整检查 | 留存记录，并在报告中列出警告项交给工程侧 |
| `RESULT: FAILED` | 至少有一项检查失败 | 留存记录，然后看第 6 步 |
| `[SKIP]` 行 | 该检查需要额外选项、配件或人工操作（`--interactive`） | 不算失败 |

每条 `[FAIL]` 和 `[WARN]` 也会带着时间和所属段落，列在 evidence 区块的 `failures` 下面。

## 5. 留存测试记录

每次运行都会在板子上生成一个证据包，用来证明这块板子在什么时间做过这些测试。
证据包包含：板子序列号、boot ID、MAC 地址、固件版本、每项检查的时间、
时钟状态、测试期间的内核日志、所有测量数据，以及每个文件的 SHA-256。

把证据包拷到测试电脑，按 evidence 区块里打印的板子序列号归档：

```bash
mkdir -p records
scp 'aico@192.168.42.1:/userdata/aico-hwtest/aico_hw_*.tar.gz' records/
```

在随工单或产线报告上记下序列号、run id、测试结果和 `bundle sha256` 的值。
之后要核对记录是否被改动：

```bash
sha256sum records/aico_hw_<run-id>.tar.gz          # 必须和记下的值一致
tar -xzf records/aico_hw_<run-id>.tar.gz -C /tmp
cd /tmp/aico_hw_<run-id> && sha256sum -c SHA256SUMS
```

只有时钟校准过，时间戳才可信。板子连上时间服务器后（需要能上网的 Wi-Fi），
`clock` 行会显示 `ntp_synchronized=True`。没有校准时，时间来自板子的 RTC，
但 `uptime` 数值仍然能反映各项检查的先后顺序和耗时。

## 6. 查看失败原因

先打开解压后证据包里的 `failures.txt`。它先列出所有 FAIL，再列出所有 WARN，
每条包括时间、测试套件和段落、失败信息，以及检查当时打印的详细信息：

```text
[FAIL] 2026-11-25T07:13:15Z (uptime 4070.03 s) hdmi / capture devices
  no locked DV timings on /dev/v4l-subdev2 (source asleep, cable or receiver lock problem)
    | status: plugin
    | Clk-Ch:Unlock	Ch0:Unlock	Ch1:Unlock	Ch2:Unlock
```

每条失败前后的完整输出在 `<套件名>.log` 里（按失败信息搜索），
同一时间段的内核日志在 `kernel.log` 里。

常见失败的首要排查点：

| 失败 | 先检查 |
| --- | --- |
| `hdmi`：no locked DV timings、frame_service `RECOVERING`、截图失败 | 视频源接在 J3 上、处于唤醒状态，并通过 USB-C 输出视频；用 `--only hdmi` 重测 |
| `audio`：no speaker signal detected | 喇叭插座和走线；板子没有静音、喇叭没有被遮挡 |
| `audio`：底噪过高、削波 | 换到更安静的地方测试；仍不行再查麦克风装配 |
| `mcu`：UART 链路未连接、MCU 服务缺失 | MCU 固件已烧录并在运行；MCU 串口走线 |
| `peripherals` / wifi：找不到网卡 | Wi-Fi 模块焊接和供电；MCU 固件（模块由 MCU 供电） |
| `peripherals` / wifi：未连接网络 | 板子已连上产线热点（第 3.4 步） |
| `peripherals` / storage：检测不到 TF 卡 | 卡已插好，并且在电脑上能读；卡槽 |
| `lowlevel`：`example_usb_hid not found` | 该固件缺少这个开发辅助程序；上报工程侧，但不要因此判板子不良 |

修好连接后，只重测失败的套件（`--only <套件名>`）。每次运行都会生成独立的证据包，
板子上和 `records/` 里的都要保留。原因不明时，把证据包随板子一起交给工程侧。

## 7. 出货前处理

板子测试合格并且记录已拷出后，在 `http://192.168.42.1` 打开
**高级设置 → 设备密码**，清除测试密码。这样 `aico` 账号会恢复到刚刷完机时的锁定状态。
