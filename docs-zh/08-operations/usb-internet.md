---
sidebar_position: 8
---

# 通过 USB 上网

当开发板 Wi-Fi 不可用时，通过 USB 连接的 Mac 或 Android 手机可以为 Agent、HTTP/HTTPS API、
WebSocket 和下载提供互联网访问。该功能使用现有的 ECM 链路和开发板的本地代理；它不会改变
USB HID、DHCP 或开发板地址 `192.168.42.1`。

## 启用与使用

通过 USB 连接打开 **Config Web → Advanced Settings → USB Internet**。该开关**默认关闭**，
包括其设置文件不存在时。Mac 和 Android 共用同一个开关。开关状态会立即保存，重启后仍然保留。
打开后，标题下方会显示“Waiting for Internet sharing”，直到有主机注册会话。各平台的操作说明
显示在开关行下方，并会自动换行以适配手机屏幕。

- **Mac：** 在本固件仓库的检出目录中运行 `scripts/usb-internet-macos.sh start`。该辅助脚本
  在后台运行并跟随开发板上的开关状态，它自己从不打开开关。线缆断开或开发板重启后会自动重连。
- **Android：** 安装包含 `UsbInternetRelay` 的 App 版本，连接开发板，然后打开 Aico App。
  App 现有的 `connectedDevice` 前台服务会读取开发板开关，并自动启动/停止原生中继。如需在后台
  使用，请保持该服务运行。通知会提示手机何时正在共享网络。已验证的 Wi-Fi 优先于蜂窝数据；
  通过蜂窝网络共享会消耗手机的流量。

关闭开发板开关会立即撤销会话，并关闭已建立的代理连接。主机会在下一次状态检查时（通常在 5 秒
内）移除其监听器，之后恢复原有的 Wi-Fi/系统代理策略。再次打开开关需要一个新的活动会话。

该设置只能通过绑定在 USB 上的 Config Web 监听器或开发板本地进程修改。已认证的 LAN 设置页面
可以读取但不能修改该设置。保存失败时会恢复为上一次确认的开关状态。

### Mac 辅助脚本

要求：Python 3.8+、OpenSSH 7.6+、已信任开发板的 SSH 主机密钥，并且能以 `aico` 身份使用密钥
登录。辅助脚本既不需要 Mac 管理员权限，也不需要开发板 sudo。经授权的设置和时钟操作由开发板
服务自行完成。

```bash
scripts/usb-internet-macos.sh start
scripts/usb-internet-macos.sh status
scripts/usb-internet-macos.sh check
scripts/usb-internet-macos.sh stop
```

可选的第二个参数是 SSH config 别名或 `user@host`。`start` 会启动一个主机端 worker，即使开发板
开关当前处于关闭状态。`status` 报告 worker 状态；`check` 通过一次真实的 HTTPS 请求验证活动
会话。`stop` 停止主机端 worker，但不改变开发板开关。worker 状态和简短的状态转换日志位于
`~/.cache/aico-usb-internet/` 下，其中不包含代理密码。

```text
Agent / managed command
  -> board 127.0.0.1:18080 (existing local HTTP/SOCKS proxy)
  -> board 127.0.0.1:18081 (SSH reverse dynamic listener)
  -> USB ECM / SSH
  -> Mac DNS + TCP connection
  -> Internet
```

辅助脚本只在共享开启时创建反向监听器。本地的 `ssh -D` 会把 SOCKS 监听器建在这条链路错误的
一端。主机休眠时服务暂停；唤醒或重新插线会触发重试。该辅助脚本不会被安装为登录项，Mac 重启后
需要重新启动。SSH 打开的是普通的主机套接字，因此仅对浏览器生效的 HTTP 代理设置不一定能为辅助
脚本提供可用的上游。

### Android 中继

手机仍然是 USB **主机**，开发板仍然是 ECM/HID USB **设备**。不要启用 Android 系统的 USB 网络
共享：它通常的 USB 角色与这种拓扑正好相反。

```text
Agent / managed command
  -> board 127.0.0.1:18080
  -> phone's DHCP-assigned ECM address:18081
  -> authenticated native SOCKS5 CONNECT relay
  -> DNS and socket explicitly bound to phone Internet Network
  -> Wi-Fi / cellular / validated VPN
```

监听器只绑定到手机的 USB IPv4 地址，并且只接受开发板对端 `192.168.42.1`。每个会话使用由
用户名 `aico` 和随机密码组成的 SOCKS 凭据，仅保存在内存中。开发板根据控制请求的 USB 对端地址
推导代理地址，而不会采用请求中提供的主机 URL。App 不假设手机的 DHCP 地址是固定的。

现有的 React Native 进程可以继续绑定在开发板网络上。每次中继查找都使用
`Network.getAllByName`，每个出站套接字在连接到解析出的数字地址之前都会调用
`Network.bindSocket`。App 从不为互联网请求切换进程级绑定。它优先使用已验证的 Wi-Fi，其次是
蜂窝网络；如果存在 VPN 网络，则要求该 VPN 已验证，而不会悄悄使用底层的物理网络。

原生 worker 将中继限制为最多 32 个并发隧道，并使用有上限的缓冲区。SOCKS5 CONNECT 接受域名、
IPv4 和 IPv6 目标；拒绝 BIND 和 UDP ASSOCIATE。半关闭可确保上传完成后仍能收到响应。USB 断开、
上游变化、服务关闭以及开发板开关关闭都会关闭套接字。上游变化会中断现有请求，是否重试由应用的
重试规则决定。OEM 后台限制、VPN 锁定以及 ECM 驱动可用性仍需在实际目标手机上测试。

## 会话与配置约定

代理进程负责管理开关和租约。Config Web 通过仅 root 可访问的 Unix 套接字
`/run/wifi_proxy/usb-internet/control.sock` 转发经批准的请求。开关保存在
`/userdata/system/usb-internet.json`。会话、上游凭据和活动连接仅保存在内存中，代理进程重启后
即消失。该开关目前只保存在设备本地，不包含在网络备份中。如果 USB 设置或控制套接字无法初始化，
代理会记录失败，并在 USB 共享不可用的情况下继续提供现有的 Wi-Fi/系统代理策略。修复日志中报告
的路径并重启代理即可重试。

| 端点 | 行为 |
| --- | --- |
| `GET /api/network/usb-internet` | 返回 `enabled`、`active`、可选的 `host`、`expires_in_seconds` 和 `via_usb`；从不返回凭据或会话 token |
| `PUT /api/network/usb-internet` | 保存 `{"enabled":true}` 或 `{"enabled":false}`；仅限 USB/本地且同源的请求 |
| `PUT /api/network/usb-internet/session` | 注册/续期 `session_id`、`host`（`mac` 或 `android`）、Android `password`，以及可选的 `unix_time_ms` |
| `DELETE /api/network/usb-internet/session` | 由所属对端释放匹配的 `session_id` |

会话 ID 和 Android 密码由 32–64 个小写十六进制字符组成。主机每 5 秒续期一次。开发板使用单调
时钟，在最后一次成功续期 30 秒后使租约过期；一个每秒运行的回收器还会在过期后关闭空闲或正在
传输的套接字。同一时间只有一个主机持有租约；竞争的持有方会收到 HTTP 409。开关关闭时注册会
返回 HTTP 403。

Mac 会话注册仅限本地，通常通过 SSH 在开发板上执行 curl 完成。其上游固定为
`127.0.0.1:18081`。Android 注册需要真实的 USB 入口，并使用对端 IP 加固定端口 18081。远程
客户端无法注册环回地址或任意互联网代理。

成功的会话可以在开发板时钟与主机相差超过两分钟时为开发板引导时钟。主机时间戳必须介于 2024 年
到 2100 年之间。时钟同步只在完成开关启用和归属校验之后，通过受信任的 USB/本地路径进行。对于
启动时没有有效 RTC 或 NTP 的开发板，这一步是必需的。TLS 校验保持开启；修改墙上时间不会延长
租约。

USB 会话仅在活动期间覆盖已保存的 SSID 策略。本地地址和 USB 子网会绕过中继。生成的客户端环境
使用现有的本地 HTTP 代理，因此 DNS 名称会通过 CONNECT/SOCKS 传给主机解析。客户端不会获得主机
凭据。会话续期不会重启 Agent。进入或离开不同的代理策略时，生成的代理环境发生变化，可能会让
现有的 Agent 重启监视器触发一次重启。

## 覆盖范围与验证

这是**支持代理的 TCP 连接**，而不是完整的 IP 网关。它不提供任意 UDP、QUIC、ICMP/ping、系统
DNS 或 NTP。忽略代理变量或使用自有排除规则的应用可能会绕过它。对于 shell 工具，必要时请显式
应用托管环境：

```bash
ssh aico@192.168.42.1 \
  '/usr/lib/aico/aico-managed-env-run curl -fI https://example.com'
```

`make check` 包含 USB Internet 管理器/代理/API 回归测试和 Web 开关测试。它们覆盖默认关闭的
持久化、入口限制、凭据脱敏、会话归属/过期、关闭开关及待处理连接的撤销、HTTP 转发、回退恢复、
保存失败以及过时的 UI 响应。配套 App 包含针对认证、转发、半关闭、对端拒绝和关闭流程的 JVM
套接字测试，运行方式：

```bash
cd android
./gradlew :app:testDebugUnitTest --tests link.aico.app.UsbSocksServerTest
```

手机验收还必须测试熄屏、后台服务存活、Wi-Fi/蜂窝切换、VPN 策略、仅 IPv6 的蜂窝网络/NAT64、
重新插线后 DHCP 地址变化，以及一次真实的 Agent API/流式请求。JVM 测试和 APK 构建并不能证明
硬件兼容性。

## 完整 IP 替代方案

Mac 管理员也可以为 ECM 子网配置转发和 PF NAT，并使用稳定的主机 USB 地址、经由该地址的开发板
默认路由以及显式 DNS。这是一套独立的配置，不由此开关启用。请保留现有的 PF/VPN 规则并记录回滚
状态。macOS 互联网共享会自行管理 DHCP/子网，因此除非有意重新配置拓扑，否则它会与当前开发板的
DHCP 服务器冲突。

所检查的开发板内核禁用了 `CONFIG_TUN` 和 `CONFIG_USB_NET_DRIVERS`。基于 TUN 的完整 IP Android
方案或反转 USB 角色都需要额外的内核/固件工作。已实现的代理路径不需要这两者。
