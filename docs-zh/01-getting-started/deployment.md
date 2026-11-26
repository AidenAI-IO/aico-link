---
sidebar_position: 6
---

# 部署到设备

## 完整的 Debian 固件

生产部署路径是一个完整的 Debian 镜像。使用以下命令构建经过审计的应用程序包、Debian rootfs、BSP、
A/B 镜像以及已签名的本地 OTA 元数据：

```bash
./debian_build.sh
```

可直接烧录的镜像是：

```text
output/debian/image/update.img
```

在 Linux 上，使用下面带保护机制的辅助脚本烧录。它会校验镜像摘要，要求设备处于 Loader/Maskrom 模式，
并要求你显式确认会覆盖 userdata 的破坏性操作：

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

仓库根目录下的 `upgrade_tool/upgrade_tool` 是 macOS Mach-O 二进制文件；在 Linux 上请使用内置
`pico-sdk/` 目录中的工具。在 macOS 上，使用以下命令烧录本地构建的镜像：

```bash
./upgrade_tool/upgrade_tool uf ./output/debian/image/update.img
```

镜像会安装以下主要运行时目录：
```text
/usr/lib/aico/                                  # Audited production applications
/usr/lib/aico/platform/lib/                                  # Vendor and application libraries
/usr/lib/aico/models/                                # VAD model and weights
/usr/share/aico/                          # Config Web, skills, audio, and EDID assets
/usr/lib/aico/                                # Debian rootfs service helpers
/etc/systemd/system/aico-*.service            # Product systemd units
/userdata/agent/agent.toml                     # External build-time Agent configuration
/userdata/debian/wifi/wpa_supplicant-wlan0.conf
/userdata/debian/ota/config.json               # Debian OTA factory configuration
```

Aico Link 的 Debian 设备树删除了从 `rv1106-evb.dtsi` 继承的厂商 `restart-poweroff` 节点。
编译后的生产 DTB 和两个打包的启动镜像都会针对该节点进行审计，因此 Linux 关机不会通过厂商的回退路径故意重启。
这需要重新构建并安装 BSP 启动镜像；只更新业务软件包不会改变已安装的设备树。

常规的 `sudo shutdown now`、`sudo poweroff` 和 `sudo systemctl poweroff` 会使用最终的 MCU 关机钩子。
在服务停止、swap 被禁用、持久化文件系统被卸载或设为只读之后，该辅助脚本会以 SHUTTING_DOWN 回应 MCU 的
状态探测，并发送 SHUTDOWN_READY。随后 MCU 切断电源。重启、halt 以及服务/软件包重启都不会执行这一握手。
如果 MCU 不可用，钩子会在十秒后超时，Linux 继续进入关机 halt；此时无法确认是否已物理断电。

`overlay-debian/` 负责 Debian 平台的附加内容。rootfs 阶段会安装 `aico-business`、BSP 库和模块，
以及 OTA 信任密钥。业务模型和通知音来自 `assets/business/`。两个 rootfs 槽位都从同一镜像启动；
不会生成 OEM 文件系统。

## 开发用二进制更新

无需重新构建固件即可构建并审计应用程序：

```bash
scripts/debian-apps/build-apps.sh all
```

生产二进制文件位于 `output/debian-apps/apps/bin/` 下。若要在设备上临时测试，
请先停止对应的 systemd 单元，再替换其二进制文件：

```bash
ssh root@<device-ip> 'systemctl stop aico-frame.service'
scp output/debian-apps/apps/bin/frame_service root@<device-ip>:/usr/lib/aico/frame_service
ssh root@<device-ip> 'chmod 0755 /usr/lib/aico/frame_service && systemctl start aico-frame.service'
```

`audio_service`、`ble_service` 或 `agent` 也使用同样的方式。Agent 二进制文件还提供 `config-web` 子命令。
直接复制二进制文件只会修改当前活动的 rootfs 槽位，并且可能使 OTA 诊断所期望的出厂哈希失效，
因此只应在短周期开发中使用。要进行可复现的部署，请重新构建并烧录完整镜像。

`frame_service_cli` 和 `audio_service_cli` 包含在业务软件包中，用于 OTA 自检。其他诊断工具（例如
`example_*`）仍属于 apps 构建产物。只把有限测试所需的工具复制到 `/userdata` 下的某个目录，
测试完成后将其删除。

不要通过把单个文件复制到 `/etc` 的方式部署服务定义。rootfs 辅助脚本和 systemd 单元与 Debian rootfs
镜像一起进行版本管理，必须一起变更。

## 服务关系

`aico.target` 将产品服务归为一组。重要的启动顺序通过 systemd 依赖表达，而不是依靠文件名顺序：

1. 槽位解析和 rootfs 扩容暴露当前活动的 rootfs、userdata 和 OTA 分区。
2. 完成 userdata 迁移、机器标识、平台库注册以及严格的环境生成。
3. 媒体、Wi-Fi、蓝牙、USB gadget、DHCP 和时间服务准备硬件和网络。
4. Frame、音频、BLE、Agent、Config Web、adb host 和 WLAN 恢复服务独立启动。
   MCU 服务独占 `/dev/ttyS5`，提供硬件 API 和唤醒长轮询，并协调由 MCU 请求的 Linux 关机。
5. OTA 健康汇总器在提交待定槽位之前检查必需的本地服务。

使用以下命令查看依赖图：

```bash
systemctl list-dependencies aico.target
systemctl --failed
```

## 常用服务命令

```bash
systemctl status aico-frame.service --no-pager
systemctl restart aico-frame.service
systemctl status aico-audio.service --no-pager
systemctl restart aico-audio.service
systemctl status aico-ble.service --no-pager
systemctl status aico-agent.service --no-pager
systemctl restart aico-agent.service
systemctl restart aico-config-web.service
systemctl status aico-usb-gadget.service --no-pager
systemctl status aico-mcu-event.service --no-pager
sudo journalctl -u aico-mcu-event.service -f
```

## SSH 会话与关机通知

精简 rootfs 显式安装了 `libpam-systemd`。在 `UsePAM yes` 下，新的 SSH 连接属于 `user-1000.slice`
下的 `session-*.scope` 单元。这些 scope 会在关机期间被停止。Debian 的 `ssh.service` 保留了
`KillMode=process`，因此重启 SSH 监听器不会断开现有连接。在已有开发板上安装该软件包只影响新的登录；
测试前请重新连接。不需要重启 SSH 服务。

仅靠会话注册并不能在这块开发板上恢复关机广播。Debian armhf 的 systemd 257 以 `-UTMP` 构建，
而 OpenSSH 会在分配 PTY 之前打开 PAM 会话。因此得到的 logind 会话最初没有 `TTY`。
`/etc/ssh/sshrc` 会在分配之后运行 `aico-ssh-session-tty`，通过 logind 的 `SetTTY` API 注册该终端。
它以登录用户身份运行，在不强行挤掉其他控制者的情况下短暂获取会话控制权，然后退出。
不需要守护进程或额外的 Python 软件包。没有 PTY 的连接会被跳过；注册失败不会阻止登录。该钩子保留了
X11 cookie 设置。使用自定义 `~/.ssh/rc` 的用户必须在其中调用 `/usr/lib/aico/aico-ssh-session-tty`，
因为 OpenSSH 会使用用户钩子而不是系统钩子。

在一次全新的交互式 SSH 登录中验证：

```bash
cat /proc/$$/cgroup
loginctl show-session "$XDG_SESSION_ID" -p Scope -p TTY
```

预期看到 `session-*.scope` cgroup 和 `TTY=pts/...`。测试前请打开第二个 SSH 终端。
在第二个终端中运行 `sudo shutdown -k +1`，在第一个终端中观察广播，然后在第二个终端中用
`sudo shutdown -c` 取消。logind 会把发起关机请求的终端排除在广播之外。保持两个连接都打开：
在五分钟内安排关机会暂时阻止新的登录。在 systemd 257 上，`shutdown -k now` 不会走同一条定时警告路径。
除非使用 `--no-wall`，常规的 `shutdown` 和 `systemctl poweroff` 都支持 wall 广播。

如需进行真实的关机测试，在第二个终端中使用 `sudo shutdown now`，并在第一个终端中观察通知和 SSH 关闭。
完整的关机时序仍需要在 SSH 退出后通过串口控制台观察；会话清理并不能证明 USB 拆除、swapoff
或文件系统卸载能及时完成。`user@1000.service` 保持启用（在测试的开发板上约占 3 MB）。

## 关键配置文件

| 文件 | 说明 |
| --- | --- |
| `/etc/aico_boot.conf` | 由 systemd 单元加载的产品功能开关 |
| `/etc/aico_frame_service.conf` | Frame 服务的启动和采集设置 |
| `/etc/aico_audio_service.conf` | 音频服务的套接字和音量状态设置 |
| `/etc/aico_ble_service.conf` | BLE 套接字、名称、事件容量和配对窗口 |
| `/userdata/agent/agent.toml` | Agent 运行时配置 |
| `/userdata/system/env` | 持久化的设备环境变量来源 |
| `/run/aico/system.env` | 经过校验、自动生成并供服务使用的环境变量 |
| `/userdata/debian/wifi/wpa_supplicant-wlan0.conf` | 由 Config Web 管理的 Wi-Fi 配置 |
| `/userdata/system/wifi-proxies.json` | 按 SSID 设置的代理模式和可选的上游 URL |
| `/userdata/debian/ota/config.json` | Debian OTA 仓库和出厂基线 |

## 日志位置

| 服务 | 日志 |
| --- | --- |
| Frame 服务 | `/var/log/frame_service/frame_service.log` |
| 音频服务 | `/var/log/audio_service/audio_service.log` |
| BLE 服务 | `/var/log/ble_service/ble_service.log` |
| adb host 启动 | `/var/log/adb/adb-startup.log` |
| OTA 健康检查 | `/var/log/ota/ota.log` |
| OTA 恢复 | `/var/log/ota/ota-recovery.log` |
| Config Web OTA 更新 | `/userdata/ota/config_web_ota_update.log` |
| Agent | `/userdata/agent/log/agent.log` |
| TTYD 终端 | `/var/log/ttyd/ttyd.log` |
| 蓝牙挂载 | `/var/log/aico-hciattach.log` |
| bluetoothd | `/var/log/bluetoothd/bluetoothd.log` |
| Wi-Fi 代理 | `/var/log/wifi_proxy/wifi_proxy.log` |
| Wi-Fi 驱动 | `/var/log/wifi_driver/wifi_driver.log` |
| wpa_supplicant (wlan0) | `/var/log/wpa_supplicant/wlan0.log` |
| WLAN guard | `/var/log/wlan_guard/wlan_guard.log` |
| 启动时间线 | `/var/log/aico_boot_timeline.log` |

Config Web 的 **Export logs** 操作（`GET /api/logs/support`）会把最新的诊断数据打包为 `aico-logs.tar.gz`。
除 Agent、Langfuse episode 和最新的 LLM HTTP 日志外，它还包括上面的服务日志、蓝牙辅助日志
（`/var/log/aico-hciattach.log` 和 `/var/log/bluetoothd/bluetoothd.log`）、OTA 恢复/TTYD/启动时间线日志，
以及以 `dmesg.log` 保存的当前内核环形缓冲区。服务日志和 dmesg 只保留最近的 1 MiB（现有的 LLM HTTP
导出最多保留 4 MiB）；不可用的文件会以文本占位符的形式包含在内，因此某个服务日志缺失或权限受限不会中止导出。

使用 `journalctl -u <unit>` 查看 systemd 生命周期和辅助脚本的失败。有意持久化的服务 stdout/stderr
仍保存在上述文件中。

`aico-mcu-event.service` 单元现在运行 `mcu_service`，即 `/dev/ttyS5`（115200 8N1）上的生产 UART 对端。
它提供电池/定时器/LED 操作和基于游标的唤醒长轮询，并协调由 MCU 请求的 Linux 关机。
应用约定、关机时序约束以及烧录后的验证命令参见 [MCU 服务](../03-services/mcu-service.md)。
在使用其他 UART 占用者之前，请先停止该单元。这块开发板上的 journald 是易失的；
如果需要保留记录，请在重启前导出日志。

如果 Wi-Fi 启动失败，尽量在不重启的情况下收集以下信息：

```bash
systemctl status aico-wifi-driver.service wpa_supplicant@wlan0.service \
    systemd-networkd.service aico-wlan-guard.service --no-pager
cat /var/log/wifi_driver/wifi_driver.log
cat /var/log/wpa_supplicant/wlan0.log
cat /var/log/wlan_guard/wlan_guard.log
wpa_cli -i wlan0 status
networkctl status wlan0 --no-pager
```

Wi-Fi 日志有意只记录配置元数据和状态变化，而不记录 `wpa_supplicant-wlan0.conf` 的内容，
因此 PSK 不会被复制到诊断文件中。

`frame_service` 独占 `/dev/video0`；在直接进行摄像头诊断之前，请先停止 `aico-frame.service`。
Agent 的截图工具依赖 Frame 服务，而语音和音量工具依赖音频服务。
