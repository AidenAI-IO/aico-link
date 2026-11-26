---
sidebar_position: 3
---

# ttyd 浏览器终端

Debian 固件镜像集成了 ttyd，作为开发板端维护用的浏览器终端。通过 USB 访问的地址是
`http://192.168.42.1:3000/webtty/`；在 LAN 访问开启时，可以从局域网通过
`http://<board-ip>:8080/webtty/` 访问（参见 [LAN 访问](./device-management-api.md#lan-访问)）。

## Debian 集成

生产构建会下载固定版本的 ttyd 1.7.3 armhf 静态二进制文件，校验其 SHA-256 摘要，
并将其安装到业务软件包中，无需额外引入 Node.js 运行时。构建命令：

```bash
./debian_build.sh
```

## 启动

ttyd 由 systemd 管理：

```bash
systemctl start aico-ttyd.service
systemctl status aico-ttyd.service --no-pager
systemctl restart aico-ttyd.service
```

该服务读取 `/etc/aico_boot.conf`。设置 `ENABLE_TTYD=0` 可禁止启动。
已有的 `ENABLE_WETTY`、`WETTY_*` 和 `WETTY_COMMAND` 覆盖项仍作为迁移兼容回退被接受。

默认运行参数：

| 参数 | 默认值 |
| --- | --- |
| 监听接口 | `lo`（固定，不可配置） |
| 端口 | `3000` |
| 基础路径 | `/webtty/` |
| 命令 | `/usr/lib/aico/aico-ttyd-login` |
| 日志 | `/var/log/ttyd/ttyd.log` |

## 访问

ttyd 只监听回环地址，因此永远无法直接从网络访问。有两个前端会转发到它：

- Config Web 在绑定到 `usb0` 的 socket 上提供 `http://192.168.42.1:3000/webtty/`
  （`--usb-terminal-port=3000`），因此只有 USB 客户端可以使用。配置网页中的 `Terminal`
  链接会通过 USB 打开该地址。设置了设备密码时，没有会话的浏览器会先被静默配对。
- Agent 在 8080 端口代理 `/webtty/`。局域网客户端只有在 LAN 访问开启时才能访问，
  且在设置了设备密码时需要先登录（参见 [登录与配对](./device-management-api.md#登录与配对)）。

Docker 沙箱自行运行 ttyd，并在其对外发布的 Agent Web 端口上代理 `/webtty/`。

ttyd 守护进程以非特权用户 `aico` 运行。其默认登录辅助程序会提示输入本地账户名，
然后使用 `su --login` 验证该账户的密码。`aico` 的密码就是在 Agent 设置中设置的设备密码；
在设置之前，该账户处于锁定状态，任何人都无法登录。
只有在 LAN 访问开启且已设置设备密码时，SSH 才对局域网开放。这种方式避免了 Debian `login`/PTY
交互导致 ttyd 一直等待输入却不显示提示符的问题。自定义的 `TTYD_COMMAND` 仍以 `aico` 身份运行。

`aico` 和 `root` 的登录 shell（SSH、ttyd 或 `sudo -i`）都会从 `/run/wifi_proxy/proxy-env`
加载当前网络代理。支持 HTTP、HTTPS 和 SOCKS 的工具会使用 `127.0.0.1:18080` 上的回环中继；
该中继会为当前 Wi-Fi 网络选择配置好的上游代理。大写和小写的代理变量都会导出。
生成的文件对两个账户都可读，且不包含上游凭据；`/userdata/system/wifi-proxies.json` 和
`/run/aico/system.env` 仍保持私有。

`sudo` 通过 `/etc/sudoers.d/20-aico-proxy` 只保留代理变量，因此 `sudo apt update`
与登录 shell 走相同的路由。仍然需要身份验证。安装此修复后，已打开的终端可以执行
`. /etc/profile.d/aico-env.sh` 重新加载；新打开的登录终端会自动加载。
SSH 单次命令不会加载登录配置文件；如果需要相同环境，请使用
`ssh luckfox 'bash -lc "COMMAND"'`。

## 移动浏览器默认设置

ttyd 1.7.3 不包含移动端虚拟键盘工具栏，但可以通过其 `--client-option` 机制调整内置的
xterm.js 客户端。Aico Link 固件针对小尺寸触摸屏和低端移动浏览器应用了以下默认值：

| 选项 | 默认值 | 用途 |
| --- | --- | --- |
| `rendererType` | `canvas` | 避免在移动浏览器上依赖 WebGL 上下文 |
| `fontSize` | `24` | 保持文字清晰可读，并避免 iOS 输入时自动缩放 |
| `scrollback` | `500` | 限制浏览器端终端缓冲区大小 |
| `cursorStyle` | `bar` | 让输入时的插入点更容易跟踪 |
| `disableResizeOverlay` | `true` | 避免移动浏览器界面栏调整视口大小时出现临时浮层 |
| `max-clients` | `2` | 在内存受限的开发板上限制并发 shell 数量 |

在 `/etc/aico_boot.conf` 中设置对应的 `TTYD_*` 变量即可调整这些值。旧的 `WETTY_*`
名称仍为迁移而保留支持。较新 ttyd 版本文档中提到的移动端工具栏和视口元数据，
在固定的 1.7.3 客户端中不可用。
