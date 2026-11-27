---
sidebar_position: 3
---

# 启动服务与运行时布局

Debian 使用 systemd 作为 PID 1。产品服务由 `aico.target` 归组；启动顺序、重启策略和失败传播在单元关系中声明，
而不是依靠文件名顺序。

## 核心单元

| 单元 | 用途 |
| --- | --- |
| `aico-slot-resolve.service` | 根据当前活动槽位解析 A/B 分区设备 |
| `userdata.mount`、`userdata-ota.mount` | 挂载产品数据分区 |
| `aico-rootfs-grow.service` | 将首次启动的 ext4 文件系统扩展到其分区大小 |
| `aico-userdata-migrate.service` | 验证并迁移持久化的 Debian userdata |
| `aico-machine-id.service` | 配置稳定的机器标识 |
| `aico-platform-ldconfig.service` | 从当前活动的 rootfs 槽位注册平台库 |
| `aico-environment.service` | 生成严格的运行时环境变量 |
| `aico-media-modules.service` | 加载媒体模块并准备视频设备访问 |
| `aico-audio-mixer.service` | 在音频采集之前初始化 RV1106 麦克风控制项 |
| `aico-wifi-driver.service` | 加载 AIC8800 Wi-Fi 和蓝牙固件 |
| `aico-bluetooth-attach.service` | 挂载 AIC8800 UART 传输 |
| `aico-usb-gadget.service` | 创建键盘、指针、Consumer Control 和 ECM 功能 |
| `aico-usb-dnsmasq.service` | 仅在 `usb0` 上提供 DHCP |
| `aico-usb-ecm-watchdog.service` | 恢复已确认停滞的 ECM 会话 |
| `aico-wlan-guard.service` | 执行有次数上限的 Wi-Fi 恢复 |
| `aico-frame.service` | 运行 HDMI frame 服务 |
| `aico-audio.service` | 运行音频采集和播放 |
| `aico-ble.service` | 运行 BLE 唤醒和 ANCS 集成 |
| `aico-mcu-event.service` | 运行 MCU UART、唤醒事件、硬件查询和关机协调 |
| `aico-wifi-proxy.service` | 运行固定的回环代理，并为每个 Wi-Fi 选择上游 |
| `aico-agent.service` | 运行 Go Agent |
| `aico-config-web.service` | 提供本地配置门户 |
| `aico-ttyd.service` | 在 USB 网络上提供 ttyd 浏览器终端 |
| `aico-ota-health-marker.service` | 汇总必需的本地健康状态 |
| `aico-ota-health.service` | 处理待定的 A/B OTA 状态 |

使用 systemd 查看当前的依赖图和失败项：

```bash
systemctl list-dependencies aico.target
systemctl --failed
```

Rockchip 的启动参数没有传递 `rw`，也没有对应的 fstab 条目，因此 `/` 以只读方式启动。
`aico-rootfs-grow.service` 会在每次启动时，在扩展 rootfs 以及挂载 userdata 和 OTA 工作区之前运行
`mount -o remount,rw /`，这就是它被排在 `aico-platform-ldconfig`、`systemd-timesyncd` 和媒体服务之前的原因。
如果这些单元以 `Read-only file system` 失败，请检查 `aico-rootfs-grow.service` 是否已运行。

在某个 rootfs 槽位首次启动时，该辅助脚本会扩展已挂载的 rootfs，并将 userdata 和 OTA ext4 的块数与其分区容量进行比较。
已经占满分区的数据文件系统会被跳过。只有确实需要扩展且未挂载的数据文件系统，才会在 `resize2fs` 之前用
`e2fsck -f -p` 检查；fsck 状态为 0 和 1 时允许扩展，其他状态会中止准备流程，且不写入完成标记。
已挂载的文件系统使用在线扩展，不进行离线 fsck。数据挂载要求准备流程成功，因此失败的检查无法通过之后针对已挂载文件系统的重试来绕过。
成功的准备在本次启动期间保持有效。该扩展服务会拒绝手动的停止和重启请求，防止这些请求通过 `Requires=` 依赖停止正在使用的数据挂载。
修正准备失败的原因之后，使用 `systemctl start aico-rootfs-grow.service` 重试。

ext4 回归测试套件使用镜像文件和模拟的挂载发现。其中的已挂载用例验证 userdata 和 OTA 都不会进行离线 fsck；
实际的在线扩展仍需要在开发板上对已挂载的文件系统进行验证。

当 OTA 替换某个 rootfs 槽位、同时保留之前使用过的 userdata 时，这一点很重要：即使未挂载的文件系统是干净的且已占满分区，
`resize2fs` 也可能要求先进行 fsck。此类失败会取消依赖它的服务启动任务。在较旧的固件上，数据挂载仍可能启动，
日志保留定时器之后也可能重试准备流程，但不会重新启动被取消的设备服务。请同时检查启动日志和服务组，
因为这样的重试可能会让 `systemctl --failed` 为空，而设备服务却处于非活动状态：

```bash
journalctl -b -u aico-rootfs-grow.service
systemctl status aico.target aico-agent.service aico-usb-gadget.service ssh.service
```

## USB HID 与 ECM

`aico-usb-gadget.service` 暴露一个复合 gadget（`1d6b:0104`），包含键盘、指针、Consumer Control 和一个 ECM 网络接口。
以下两个不变式保证它能够可靠地枚举：

- **HID 报告描述符使用 POSIX 八进制转义。** Debian 上的 `/bin/sh` 是 `dash`，它不解释 `\xNN`；
  否则描述符会被写成 ASCII 文本，主机会记录 `unknown main item tag`、`item fetching failed` 或
  `hid-generic ... error -22`。声明的长度分别为 45（键盘）、58（指针）和 47（Consumer Control）字节。
- **`usb0` 由 systemd-networkd 负责，并采用无载波配置。** `30-usb0.network` 单元设置了 `RequiredForOnline=no`
  和 `ConfigureWithoutCarrier=yes`，因此静态地址 `192.168.42.1/24` 不会在 USB 链路完成枚举之前被移除。
  gadget、ECM 看门狗和等待辅助脚本不得以会丢弃刚设置地址的方式调用 `networkctl reconfigure`。

Agent 通过可配置的 `AICO_USB_COMPOSITE_REFRESH_COMMAND` 刷新复合 gadget；Debian 的默认值是
`/usr/lib/aico/aico-usb-ecm-watchdog` 辅助脚本。

单独的 `error -71`（`device not accepting address`、`unable to enumerate USB
device`）是更底层的控制传输失败。
请检查线缆、电源和主机端口；描述符修复无法解释这一问题。

## Frame 服务

配置：`/etc/aico_frame_service.conf`

该单元启动 `/usr/lib/aico/aico-frame-start`，它会选择 HDMI 桥接芯片，应用 EDID 和触发策略，从
`/userdata/agent/agent.toml` 读取 `[advanced_settings.hardware.frame_service].keep_streamon`，
然后执行 `/usr/lib/aico/frame_service`。

```bash
systemctl start aico-frame.service
systemctl stop aico-frame.service
systemctl restart aico-frame.service
systemctl status aico-frame.service --no-pager
```

RK628 输入默认为 1080p60。使用 `sudo aico-hdmi-mode 1080p30` 切换到兼容模式，使用 `sudo aico-hdmi-mode 1080p60`
切换回来，使用 `aico-hdmi-mode status` 读取已保存的选择。该命令会重启采集，并把选择保存在 userdata 中，重启后依然有效。参见
[RK628 输入模式](../03-hardware/rk628f-adaptation.md#持久化输入模式)。


HDMI 未就绪时，该服务会无限期重试。其持久化输出为 `/var/log/frame_service/frame_service.log`。

## 音频服务

配置：`/etc/aico_audio_service.conf`

`aico-audio.service` 在前台执行 `/usr/lib/aico/audio_service`，并由 systemd 负责重启和停止行为。

在采集开始之前，`aico-audio-mixer.service` 会等待 `rv1106acodec` ALSA 声卡上的所有麦克风控制项就绪，
并恢复麦克风增益、偏置、ADC 模式和 ALC 设置。它按 ID 选择声卡，与默认声卡和启动时的枚举顺序无关。
控制项缺失、mixer 辅助程序缺失或控制项写入失败都会阻止音频服务启动；详情请查看
`journalctl -u aico-audio-mixer.service`。

```bash
systemctl restart aico-audio.service
systemctl status aico-audio.service --no-pager
```

## Agent

Agent 单元执行：

```bash
/usr/lib/aico/agent -dir /userdata/agent -addr 0.0.0.0:8080
```

启动前，`aico-python-prepare` 会验证并准备持久化的 Python userbase。该单元加载 `/run/aico/system.env`，
然后将 pip 环境固定为 `/userdata/agent/python`。

```text
/userdata/agent/
├── agent.toml
├── python/
├── skills/
├── log/
└── memory/
```

Agent 输出持久化保存在 `/userdata/agent/log/agent.log`。

## Wi-Fi 连接与恢复

`aico-wifi-driver.service` 以 `he_on=${AICO_WIFI_HE:-1}` 加载 `aic8800_fdrv.ko`。开发板上的 Wi-Fi 模块是
AIC8800D80（SDIO `c8a1:0082`，功能 2 为 `c8a1:0182`）：一款双频、单流的 802.11ax 芯片，在 2.4 GHz 和 5 GHz
上都支持 HE，最高 80 MHz（HE-MCS 0-11）。镜像默认启用 Wi-Fi 6 HE。固件目录沿用 SDK 中的名称 `aic8800dc_fw`，
其中也包含 AIC8800D80 固件（`fmacfw_8800d80_u02.bin`）。

早期镜像默认使用 `he_on=0`。提交
[`2b08d9a9`](https://github.com/AidenAI-IO/aico-link/commit/2b08d9a945846252c3e0d9760187663176cfff57)
（2026-06-11，#177）在上一代硬件设计出现 HE 模式下 IPv4 停滞后加入了该覆盖，#682 又把它带入了 Debian 加载器。
当前的 AIC8800D80 设计不需要这一变通方案。

2026-10-27，一块以 `he_on=1` 加载的 AIC8800D80 开发板报告了 `HE supp 1`，在 `iw phy` 中两个频段都声明了 HE 能力，
并以最高 `HE-MCS 11`
（600 Mbit/s TX）关联到一个 5 GHz AP（信道 64，80 MHz）。在四分钟内，480 次网关 ICMP 探测和 240 次互联网 ICMP 探测
均无丢包，连续八次 16 MB 的 HTTPS 下载全部成功。

如果遇到不兼容的 AP，在 `/etc/aico_boot.conf` 中设置 `AICO_WIFI_HE=0` 即可回退到 HT/VHT。
该参数在模块加载时生效，因此需要重启才能应用；仅重启驱动服务不会重新加载已加载的模块。

Aico Link 镜像设置了 `AICO_WIFI_LOW_POWER=1`。匹配的 AIC8800D80 驱动会收到 `lp_on=1`，并在固件启动后、
station 接口打开时请求固件低功耗级别 1。常规的数据包收发保持启用；这不会启用厂商的挂起数据包过滤，
不会切断模块电源，也不会把 Wi-Fi 配置为系统唤醒源。常规的 `ps_on` 参数独立地保持启用。
此设置需要同时更新驱动模块和加载器。修改 `AICO_WIFI_LOW_POWER` 后请重启；将其设为 `0` 可以取消自动请求。
在当前保留的 BSP 绑定下，不支持仅重新加载 `aic8800_fdrv`，这样做可能导致 Wi-Fi 不可用。

如需在 RV1106 上临时对比，可以通过串口使用厂商命令：

```bash
# Request low-power level 1, keeping packet filtering disabled.
iw dev wlan0 vendor send 0x001a11 0x1018 \
    0x08 0x00 0x06 0x00 0x00 0x00 0x00 0x00

# Request level 0 again, keeping packet filtering disabled.
iw dev wlan0 vendor send 0x001a11 0x1018 \
    0x08 0x00 0x06 0x00 0x01 0x00 0x00 0x00
```

负载是一个小端序的 netlink 属性：长度 8、类型 6，以及一个 32 位的延迟模式值。值 0 请求低功耗运行；值 1 请求级别 0。
该命令需要一个已打开的 station 接口。其返回码报告的是提交错误；固件的单独确认请查看内核日志。
`/sys/module/aic8800_fdrv/parameters/lp_on` 报告的是启动策略，而不是实际的睡眠驻留情况或之后的运行时覆盖。
请单独测量开发板功耗，并确保两次读数的流量、服务和挂起状态相同。此 Wi-Fi 控制不会启用蓝牙单独的厂商低功耗模式。

`aico-wlan-guard.service` 每 10 秒检查一次 Wi-Fi 网关。ICMP 响应或新的 ARP 应答都会被接受，因此过滤 ping 的 AP
不会触发反复断开。连续五次检查失败后，它会重新关联，必要时再循环重启接口并重启 supplicant。DHCP 仍由
systemd-networkd 负责。guard 对 supplicant 使用 `Wants=`，因此重启 supplicant 不会终止它自己的恢复流程。

恢复最多尝试三次，直到连续六次健康检查成功后才恢复次数预算。预算耗尽后，guard 会继续监控，但不再断开接口。
`/etc/aico_boot.conf` 中的 `WLAN_GUARD_MAX_RECOVERIES` 和 `WLAN_GUARD_HEALTHY_THRESHOLD` 可以覆盖这些默认值；
修改后请重启 guard。重启 guard 也会重置其预算。不使用 `iw ... set power_save off` 作为修复手段：
在内置驱动的 `rwnx_cfg80211_set_power_mgmt` 实现中，该操作不起任何作用。

驱动、supplicant 和 guard 还会保留有大小上限的持久化诊断信息：

| 组件 | 路径 | 内容 |
| --- | --- | --- |
| 驱动加载器 | `/var/log/wifi_driver/wifi_driver.log` | 模块路径、加载结果、参数以及 `wlan0` 的创建；超过 2,000 行时裁剪为最新的 1,000 行 |
| Supplicant | `/var/log/wpa_supplicant/wlan0.log` | 配置解析、关联、认证和重启错误；超过 2,000 行时裁剪为最新的 1,000 行 |
| 恢复 guard | `/var/log/wlan_guard/wlan_guard.log` | 链路状态变化、网关失败、恢复尝试以及预算耗尽 |

这些文件是对 journald 的补充，而不是替代。驱动和 supplicant 日志会在服务启动时以及每个 guard 监控周期内检查一次，
超过 2,000 行后裁剪为最新的 1,000 行；guard 日志在超过 2,000 行后限制为最新的 1,000 行。
不会写入任何 Wi-Fi 配置内容或 PSK。这样既避免了增加一个持续运行的诊断服务，又能跨重启保留证据。

排查无线连接问题时，请使用 USB 连接：

```bash
ssh aico@192.168.42.1
sudo journalctl -u aico-wlan-guard -u wpa_supplicant@wlan0 --since '-10 min'
sudo ip neigh show dev wlan0
sudo ping -c 10 -W 1 -I wlan0 <wifi-gateway>
sudo arping -c 3 -w 4 -I wlan0 <wifi-gateway>
```

通过从局域网客户端反复建立 SSH 连接（包括空闲一段时间之后）来验证无线通路；仅凭 DHCP 成功和 `wpa_state=COMPLETED`
并不能证明单播流量正常。

如果链路保持关联但单播流量停滞，请重启后与 `AICO_WIFI_HE=0` 的情况进行对比。上一代硬件设计曾在 2.4 GHz AP 上于 HE 模式下
复现这一故障；在那里禁用 HE 恢复了网关流量。

### 漫游

`wpa_supplicant@wlan0` 会在 `/userdata/debian/wifi/wpa_supplicant-wlan0.conf` 中已保存的网络之后，
通过 `-I` 加载镜像自带的策略 `/etc/aico/wifi-roaming.conf`。该策略启用了 `bgscan="simple:30:-70:3600"`：

- 低于 -70 dBm 时，supplicant 在连接状态下每 30 秒扫描一次，当同一 SSID 的另一个 AP 信号明显更强时切换过去。
  连续 121 次短间隔扫描（约一小时持续弱信号）之后，`bgscan simple` 会退避到长间隔。
  之后再出现低于阈值的事件仍会立即触发一次扫描，并且每次长间隔扫描都会恢复一次短间隔扫描的机会。
- 在 -70 dBm 及以上时，每小时扫描一次。
- AIC8800 驱动会报告信号阈值越界（`NL80211_CMD_NOTIFY_CQM`），因此扫描间隔会跟随实时信号变化。

网络失败或超出范围时，仍会照常重新连接到剩余已保存网络中最好的那个。该文件会覆盖用户配置中写入的全局 `bgscan`；
如需更改策略，请编辑该文件并重启 `wpa_supplicant@wlan0`。该文件缺失时 `wpa_supplicant` 不会启动。

2026-10-29，一块开发板在双 AP mesh 网络（5 GHz，信道 64）上以 -30 dBm 的测试阈值运行该策略。
被移到较弱的 AP（-38 到 -46 dBm）后，它收到了 CQM 低信号事件，进行了扫描，并在无需用户操作的情况下回到了 -21 dBm 的 AP。

AIC8800 固件无法做到无缝切换。一次手动的 `wpa_cli roam` 用时约 0.7 秒，100 次网关 ping 中丢失了两次。
其他切换报告了本地断开（`reason=0`），有时随后出现 `ASSOC-REJECT status_code=1`，并在载波丢失 0.5 到 14 秒后于
较强的 AP 上恢复。因此 `20-wlan0.network` 设置了 `IgnoreCarrierLoss=3s`，使 systemd-networkd 在短暂中断期间保留
DHCP 地址，而不是释放后重新进行 DHCP。更长的中断仍会释放租约并重新获取。

如需观察漫游决策，可以临时提高 supplicant 的日志级别：

```bash
sudo wpa_cli -i wlan0 log_level DEBUG
sudo grep -E 'RSSI (low|high)|bgscan simple|within-ESS|Trying to associate' \
    /var/log/wpa_supplicant/wlan0.log
sudo wpa_cli -i wlan0 log_level INFO
```

## Wi-Fi 代理

`aico-wifi-proxy.service` 在 Agent 之前于 `127.0.0.1:18080` 上启动 `agent wifi-proxy`。
受管理的服务和登录 shell 默认使用该回环地址。设置 `AICO_WIFI_PROXY_ENABLED=0` 后，在受影响的服务或 shell 重启之后，
会恢复为直接使用原始环境变量中的代理。该代理每两秒重新加载一次 `/userdata/system/wifi-proxies.json` 和当前 `wlan0` 的 SSID。
每个已保存的网络可以使用 `/userdata/system/env` 中的系统上游、直接连接，或使用其自己的 HTTP、HTTPS 或 SOCKS5 代理。
切换 Wi-Fi 不需要重启 Agent，除非生成的代理环境变量发生变化，包括所选代理协议或该网络的 `NO_PROXY` 值发生变化。

## OTA

`aico-ota-health-marker.service` 会等待必需的本地服务和 Config Web 恢复门户就绪，然后写入一个与事务绑定的健康结果。
Agent 在恢复期间允许失败；它的失败不会阻塞门户，也不会把 OTA 槽位标记为不健康。随后 `aico-ota-health.service` 执行：

```bash
/usr/lib/aico/ota --config /userdata/debian/ota/config.json health
```

待定的 OTA 验证始终会检查 Config Web 是否处于活动状态并能通过 HTTP 响应。未设置或禁用 `ENABLE_CONFIG_WEB`
不会跳过这些检查；没有恢复门户，槽位就无法被标记为健康。

持久化的 OTA 分区挂载在 `/userdata/ota/`，包含状态、下载内容、待定启动数据和健康标记。不可变的出厂仓库和分区基线
单独保存在 `/userdata/debian/ota/config.json`。

完整的状态机参见 [OTA 概览](../09-ota/README.md)。

## Config Web

systemd 单元执行：

```bash
/usr/lib/aico/agent config-web --bind=0.0.0.0 --port=80 --config=/userdata/agent/agent.toml --wifi-config=/userdata/debian/wifi/wpa_supplicant-wlan0.conf --wifi-interface=wlan0 --wifi-backend=systemd-networkd --system-env=/userdata/system/env --web-root=/usr/share/aico/config-web
```

Config Web 使用 Debian 控制辅助脚本重启 Agent 和 frame 服务。它从不调用 SysV 服务脚本。

## 系统环境变量

`/userdata/system/env` 是由用户管理的持久化来源。`aico-environment.service` 会验证它并写入 `/run/aico/system.env`。
产品单元通过 `EnvironmentFile=` 使用这个生成的文件；无效的外部设置无法替换对服务至关重要的固定值，例如受管理的 Python userbase。
持久化来源中的代理变量定义了 Wi-Fi 代理所使用的系统/默认上游；受管理的程序收到的是其稳定的回环代理地址。
登录 shell 通过 `/etc/profile.d/aico-env.sh` 加载同一份经过验证的运行时环境变量。

## 开发与调试

- 只有在停止其所属单元之后才能替换二进制文件。
- 使用 `journalctl -u <unit>` 查看单元和辅助脚本的失败。
- 使用各服务专属的持久化日志查看应用输出。
- 直接打开 `/dev/video0` 之前，先停止 `aico-frame.service`。
- rootfs 辅助脚本或单元文件发生变化时，重新构建完整镜像。
