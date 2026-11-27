---
sidebar_position: 3
---

# vphone-cli 安装与使用指南

项目地址：https://github.com/Lakr233/vphone-cli

## 一、安装前检查

### 1. 硬件要求

请使用以下环境：

- 一台搭载 Apple 芯片的物理 Mac。
- macOS 15 或更高版本。
- 完整版 Xcode。
- 稳定的网络连接。
- 至少 100 GB 可用磁盘空间。
- 一个管理员账户及其密码。

不要在 VMware、Parallels、UTM 或 Apple Virtual Machine 中运行本项目。本项目无法在嵌套虚拟机内部再启动另一个 Virtualization.framework 虚拟机。

### 2. 检查当前环境

打开“终端”，逐条运行以下命令：

```bash
uname -m
sw_vers -productVersion
system_profiler SPHardwareDataType | grep "Model Name"
sysctl -n kern.hv_vmm_present
df -h /
```

预期结果：

- `uname -m` 应显示 `arm64`。
- macOS 版本必须为 15 或更高。
- 型号不能是 `Apple Virtual Machine 1`。
- 如果 `kern.hv_vmm_present` 显示 `1`，说明当前 macOS 系统本身可能运行在虚拟机中，此时不应继续。
- 应有足够的可用磁盘空间。

### 3. 安装完整版 Xcode

从 App Store 安装 Xcode。安装完成后，先启动一次 Xcode 并接受许可协议，然后在“终端”中运行以下命令：

```bash
sudo xcode-select -s /Applications/Xcode.app/Contents/Developer
sudo xcodebuild -license accept
xcrun -sdk iphoneos --show-sdk-path
```

最后一条命令应输出一个指向 `iPhoneOS.sdk` 的路径。

### 4. 安装 Homebrew

```bash
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
```

验证安装：

```bash
brew --version
```

## 二、修改 Mac 安全设置：完全关闭 SIP 并配置 AMFI 启动参数

### 1. 记录现有的启动参数

在正常的 macOS“终端”中运行：

```bash
nvram -p | grep '^boot-args'
```

如果该命令有输出，请拍照或复制保存。之后设置新的 `boot-args` 会覆盖现有值。

然后关闭 Mac。

### 2. 进入 macOS 恢复模式

1. Mac 完全关机后，按住电源键，不要立即松开。
2. 屏幕亮起后继续按住电源键。
3. 看到“正在载入启动选项”或齿轮形状的“选项”图标时，松开电源键。
4. 选择 **选项**。
5. 点击 **继续**。
6. 如果提示选择系统卷宗，选择 **Macintosh HD**。
7. 如果提示选择管理员用户，选择当前管理员账户并输入其登录密码。

### 3. 在恢复模式中打开终端

进入恢复实用工具界面后，从屏幕顶部菜单栏选择 **实用工具 → 终端**。

### 4. 在恢复模式终端中运行命令

确认当前“终端”窗口运行在恢复模式中，然后逐条执行以下命令：

```bash
csrutil disable
csrutil allow-research-guests enable
```

选择日常使用的系统卷宗，通常是 **Macintosh HD**。

命令完成后，可以验证设置：

```bash
csrutil status
csrutil allow-research-guests status
```

然后点击左上角的 **苹果菜单 → 重新启动**。

### 5. 配置 AMFI 启动参数

重启进入正常 macOS 后，打开“终端”并运行：

```bash
sudo nvram boot-args="amfi_get_out_of_my_way=1 -v"
```

该命令会覆盖现有的 `boot-args`，因此必须事先保存原始值。

然后重启 Mac。

### 6. 重启后验证配置

再次进入 macOS 后，运行：

```bash
csrutil status
csrutil allow-research-guests status
sysctl -n kern.bootargs
```

预期结果：

- SIP 显示 `disabled`。
- Research Guests 显示 `enabled`。
- `kern.bootargs` 包含：

```text
amfi_get_out_of_my_way=1 -v
```

## 三、构建与验证

### 1. 构建程序

克隆项目并初始化其子模块。

在项目根目录运行以下命令：

```bash
make build
```

构建成功时应显示类似如下的输出：

```text
=== Building vphone-cli ===
=== Signing with entitlements ===
signed OK
```

### 2. 运行预检

```bash
make boot_host_preflight
```

请特别留意输出中的以下项目：

- `SIP:`
- `allow-research-guests:`
- `current kern.bootargs:`
- `Signed Release Binary`
- `Result`

如果 Mac 安全设置已正确配置，应满足以下条件：

- SIP 为 `disabled`。
- Research Guests 为 `enabled`。
- 启动参数包含 `amfi_get_out_of_my_way=1`。
- `Signed Release Binary` 测试以退出码 `0` 结束。
- 输出中不应包含 `exit 137`、`signal 9` 或 `nested VM`。

## 四、运行完整的自动化部署

### 1. 在项目根目录运行安装命令

```bash
make setup_machine EXP=1
```

该命令会安装实验性固件变体，其中包括越狱相关功能。

该命令依次执行以下步骤：

1. 安装 Homebrew 依赖。
2. 创建 Python 虚拟环境。
3. 构建所需工具和 `vphone-cli`。
4. 创建一个默认 64 GB 的虚拟磁盘。
5. 下载 iPhone IPSW 和 cloudOS 固件。
6. 合并并修补启动链。
7. 以 DFU 模式启动虚拟机。
8. 获取 SHSH 数据。
9. 恢复虚拟 iPhone。
10. 离线挂载虚拟磁盘并安装 CFW。
11. 执行首次启动初始化。

默认的固件组合是项目提供并测试过的 `iPhone17,3 / iOS 26.1 / 23B85`，以及对应的 cloudOS 来源。

### 2. 部署期间的注意事项

- 不要让 Mac 进入睡眠。
- 不要合上 MacBook 屏幕。
- 不要关闭正在运行安装过程的“终端”窗口。
- 不要手动终止 `vphone-cli`、恢复进程或虚拟机窗口。
- 提示输入 `sudo` 密码时，输入当前 Mac 管理员账户的密码。
- 固件下载和恢复可能耗时很长。只要“终端”仍在持续显示进度，就继续等待。

### 3. 首次启动流程

如果脚本显示类似如下的输出：

```text
press Enter to start VM
```

按一次回车键启动虚拟机。

之后脚本会显示：

```text
Press Enter once the VM is fully booted
```

等待满足以下任一条件：

- 虚拟 iPhone 已进入稳定的系统界面；或
- Regular 变体的控制台显示 `bash-4.4#` 提示符。

确认系统不再持续滚动启动日志后，按回车键。脚本会发送首次启动初始化命令，并等待虚拟机关机。

最终输出应包含：

```text
=== Done ===
Setup completed.
```

## 五、日常启动

部署完成后，每次要启动虚拟 iPhone 时，使用以下命令：

```bash
cd path_to_project/vphone-cli
make boot
```

正常情况下，这会打开虚拟 iPhone 窗口。

退出时，优先使用项目窗口的正常退出方式，或正常关闭虚拟设备。不要在虚拟磁盘写入期间强制终止进程。

## 六、为 vphone 创建 ECDSA 密钥

Dropbear 目前支持 ECDSA。为 `vphone` 创建一个专用的 ECDSA P-521 密钥，这样今后 SSH 登录就无需输入密码。

在宿主 Mac 上运行以下命令：

```bash
ssh-keygen \
  -t ecdsa \
  -b 521 \
  -f ~/.ssh/vphone_ecdsa \
  -N "" \
  -C "vphone-benchmark"
```

查看虚拟机的 IP 地址。

从 iOS 模拟器界面顶部获取 IP 地址，例如：

```text
192.168.64.7
```

然后安装公钥。请将命令中的 IP 地址替换为正确的地址：

```bash
ssh \
  -p 22222 \
  -o PreferredAuthentications=password \
  -o PubkeyAuthentication=no \
  root@192.168.64.7 \
  'PATH=/var/jb/usr/bin:/var/jb/bin:/iosbinpack64/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin; export PATH; umask 077; mkdir -p /var/root/.ssh; cat >> /var/root/.ssh/authorized_keys; chmod 700 /var/root/.ssh; chmod 600 /var/root/.ssh/authorized_keys' \
  < ~/.ssh/vphone_ecdsa.pub
```

根据 `vphone-cli` 项目的配置，`root` 的默认密码是：

```text
alpine
```

验证免密登录：

```bash
ssh \
  -i ~/.ssh/vphone_ecdsa \
  -p 22222 \
  -o IdentitiesOnly=yes \
  -o BatchMode=yes \
  root@192.168.64.7 \
  'echo "public key login OK"'
```

预期输出：

```text
public key login OK
```

如果 Benchmark Bridge 需要使用该密钥，请将私钥路径设置为：

```text
~/.ssh/vphone_ecdsa
```

## 七、iOS 虚拟设备的基本控制

### 1. 通过 SSH 操作

通过 SSH 登录 iPhone：

```bash
ssh -p 22222 root@192.168.64.7
```

EXP 的 Procursus 环境包含 `uiopen`。

打开“设置”：

```bash
uiopen -a Settings
```

按 Bundle ID 打开应用：

```bash
uiopen -b com.apple.Health
uiopen -b com.apple.Maps
```

打开 URL：

```bash
uiopen -u https://www.apple.com.cn
```

列出已注册的应用及其 Bundle ID：

```bash
uicache -l
```

搜索特定应用：

```bash
uicache -l | grep -i safari
```

如果 `/var/jb` 尚未创建，说明 EXP 的首次启动初始化尚未完成。可用以下命令检查：

```bash
ls -ld /var/jb
tail -100 /var/log/vphone_jb_setup.log
```

### 2. 不通过 SSH 操作

在 `vphone-cli` 仓库的根目录运行以下命令：

```bash
cd path_to_project/vphone-cli
```

注入一个 Home 键事件：

```bash
printf '%s\n' \
  '{"t":"key","name":"home","screen":false}' |
  nc -U vm/vphone.sock
```

请求成功时返回：

```json
{"ok":true}
```

这会通过 `vphoned` 注入一个真实的 Home HID 事件。

你可以用同样的方法操作其他硬件按键，包括电源键、音量加和音量减：

```bash
printf '%s\n' '{"t":"key","name":"power","screen":false}' |
  nc -U vm/vphone.sock

printf '%s\n' '{"t":"key","name":"volup","screen":false}' |
  nc -U vm/vphone.sock

printf '%s\n' '{"t":"key","name":"voldown","screen":false}' |
  nc -U vm/vphone.sock
```

#### 截图

```bash
cd /path_to_project/vphone-cli

mkdir -p screenshot

shot_file="$PWD/screenshot/iphone-$(date '+%Y%m%d-%H%M%S').png"

printf '{"t":"screenshot","path":"%s"}\n' "$shot_file" |
  nc -U vm/vphone.sock >/dev/null
```

生成的文件路径类似于：

```text
/path_to_project/vphone-cli/screenshot/iphone-20260721-143052.png
```

#### 点按与滑动

点按某个屏幕坐标：

```bash
printf '%s\n' \
  '{"t":"tap","x":645,"y":1398,"screen":false}' |
  nc -U vm/vphone.sock
```

向上滑动：

```bash
printf '%s\n' \
  '{"t":"swipe","x1":645,"y1":2400,"x2":645,"y2":900,"ms":300,"screen":false}' |
  nc -U vm/vphone.sock
```

默认的屏幕坐标尺寸为 `1290 × 2796`。
