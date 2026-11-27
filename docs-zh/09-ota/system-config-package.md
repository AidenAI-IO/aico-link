# 由业务软件包管理的运行时配置

运行时配置直接包含在 `aico-business` 中；不存在单独的 `aico-system-config` 软件包。每个版本只有
一个 `.deb`，因此程序、资源、配置和维护脚本会一起升级或降级。即使只有几行配置，也可以随业务
软件包通过 APT 发布。是否需要重启和是否需要 OTA 是两个相互独立的判断。

## 一次定义范围，自动包含新文件

`scripts/debian-system/config-package.json` 使用命名空间规则和平台排除项来定义软件包与平台之间的
边界，而不是逐个文件的白名单。`scripts/debian-package/system_config.py` 会根据 `overlay-debian/`
自动生成每个版本实际的文件清单。打包、rootfs 复制排除和镜像审计共享这份清单；发布分类使用相同的
规则。

| 包含在业务软件包中的范围 | 示例 / 用途 |
| --- | --- |
| `/etc/aico/**`、`/etc/aico_*.conf`、`/etc/default/aico-*` | 新功能优先使用专用目录；支持现有的音频、BLE、启动和 swap 配置 |
| `/etc/locale.conf` | 默认 locale；Debian 提供兼容符号链接 `/etc/default/locale → ../locale.conf` |
| `/etc/profile.d/aico-*.sh`、`/etc/sudoers.d/*-aico-*` | root/aico 终端环境，以及通过 sudo 保留代理设置的规则 |
| `/etc/ssh/sshd_config.d/*-aico.conf` | Aico Link SSH 设置 |
| `/etc/systemd/system/aico*.{service,path,timer}`、`aico.target` | 业务和设备辅助服务，受平台排除项约束 |
| `*.service.d/*-aico.conf`、`*.device.d/*-aico-optional.conf` | Aico Link 服务和设备的 drop-in |
| `/etc/systemd/network/{20-wlan0,30-usb0}.network`、`*-aico-*.network` | 网络配置 |
| `/etc/dnsmasq.d/usb0.conf`、`aico*.conf` | USB DHCP 设置 |
| `/etc/systemd/journald.conf.d/*-aico.conf` | 日志设置 |
| `/etc/tmpfiles.d/aico*.conf`、`/etc/udev/rules.d/*-aico-*.rules` | 目录和设备权限规则 |
| `/usr/lib/aico/aico-*` | Aico Link 辅助脚本，受平台排除项约束 |
| `/usr/share/aico/hdmi/*.hex` | 用于持久选择 1080p60/1080p30 的 RK628 运行时 EDID 配置 |

例如，新增 `overlay-debian/etc/aico/camera.conf` 或 `overlay-debian/usr/lib/aico/aico-new-helper`
会自动将其包含进 `.deb`，无需修改清单或约定。新服务应通过 `aico.target` 或现有服务的依赖关系启动。
APT 不会运行 `preset-all`，也不会改变管理员的启用选择。仅有 `[Install] WantedBy=` 条目并不会自动
启用新服务。

平台排除项包括 OTA、分区扩展/槽位、userdata 迁移、机器标识、SSH 身份、用户目录、环境准备、Wi-Fi
驱动和媒体模块的基础脚本和单元。平台库、内核、驱动、挂载单元、APT 源和公钥、OTA 信任根、内核
EDID 默认值以及基础 VQE 配置也仍归平台所有。`/etc/bluetooth/main.conf` 已归 Debian 的 `bluez`
软件包所有，不会被直接接管；厂商配置应优先使用该服务支持的 Aico Link drop-in。文件位于 `/etc`
下并不意味着它可以被接管。

边界规则本身的变更或范围外文件的变更仍属于系统变更，需要 OTA。实际上依赖新库、新驱动、新启动
前提或 ABI 的范围内变更同样需要 `force_ota`。自动路径分类无法证明语义上的兼容性。

## 安装、配置与生效

Debian 13 的规范 locale 路径是 `/etc/locale.conf`。业务软件包只拥有这个普通 conffile，并保留系统
创建的 `/etc/default/locale` 符号链接，避免 dpkg 在已有符号链接上留下 `.dpkg-new` 文件。配置内容、
权限和所有者会在 rootfs 压缩和镜像打包之前检查，并在最终镜像审计中再次检查。

文件以真实的 dpkg 负载安装；钩子不会用 `cp` 覆盖系统文件。软件包中的
`/usr/lib/aico/runtime-config.json` 记录路径、权限、哈希和生效方式：

- 文件归 root 所有：普通配置使用 `0644`，可执行辅助脚本使用 `0755`，sudoers 文件使用 `0440`。
- `/etc` 下的文件注册为 conffile。当上游和本地副本都发生变化时，dpkg 按交互选择处理。无人值守
  安装可以显式使用 `--force-confold`，但这样一些新设置将不会替换本地修改。
- `/userdata` 和 `/run` 下的运行时状态、凭据和用户设置不会被打包。
- 删除源文件后，新软件包将不再包含它；dpkg 可能会保留已废弃的 conffile。要彻底删除或重命名某个
  文件，请在该版本中添加可逆的 `dpkg-maintscript-helper` 迁移，并测试升级和降级。不能把从清单中
  消失当作可以删除管理员配置的许可。

`preinst/prerm` 会记录并停止之前正在运行的业务服务，并在安装前记录实际的配置哈希和权限。
`postinst` 会校验实际的 sudo 配置，应用 Aico Link tmpfiles，执行 daemon-reload，恢复之前正在运行
的业务服务，最后恢复代理重启监听器。之前已停止的业务服务不会被自动启动。校验或启动失败时会保留
恢复记录；修复问题后运行 `sudo dpkg --configure -a` 继续。离线 rootfs 构建会跳过在线的服务停止/
启动操作和重启标记。

规则中的 `live` 表示不需要重启设备：业务配置在业务服务恢复时生效，而终端 profile 和 locale 设置
在下次登录或新终端中生效。其他范围内的文件默认需要稍后重启，包括尚未被审查确认为 live 的新增
配置。APT 不会自动重启 SSH、systemd-networkd、USB 或设备。只有当延迟生效文件的已安装内容或权限
实际发生变化时，才会写入以下标记：

```text
/run/reboot-required
/run/reboot-required.pkgs
/run/aico-business-reboot-required.json   # Specific paths that require a reboot to take effect
```

仅业务代码的升级、仅 live 配置的变更、重新安装相同的配置以及 dpkg 保留本地配置，都不会产生不必要
的重启要求。标记会保留到重启，并且不会被后续安装覆盖。替换脚本文件不会立即切换已在运行的脚本
进程的代码。默认的重启策略可以避免在远程安装期间主动中断设备连接。

## 发布与初次归属转移

主入口每小时检查一次 `main` 并将变更发布到 `dev`。三个通道都支持手动发布；`staging` 和 `prod`
不会自动发布。主入口和 `build-backup.yml` 使用相同的实现。

| 自该通道上一次发布以来的变更 | 产物 | 约定 |
| --- | --- | --- |
| 业务或范围内配置的新增/修改/删除 | 一个 `aico-business` 软件包 | 继承 |
| 平台或边界变更、`force_ota` | OTA 镜像和配套的业务软件包 | 递增 |
| 初次采用配置归属 | OTA 镜像和配套的业务软件包 | 递增一次 |
| 仅文档/测试，或无变更 | 不发布 | 不变 |

初次 OTA 会将这些配置文件从直接的 overlay 复制转为由业务软件包所有，并在平台约定中声明
`runtime_config: 1`。业务清单和发布记录也会记录该版本；为保持兼容，没有该字段的旧发布读作 `0`。
preinst 会拒绝在没有此声明的旧平台上安装新软件包。历史 Release 不会被改写。规划器会自动分配新的
基础约定；之后的范围内配置变更不再使其递增。

发布程序和 APT 索引生成器会校验软件包清单、文件内容/模式、conffile、通道和约定。发布成功后会刷新
GitHub Pages 上的签名仓库。索引仍然没有 `Valid-Until`，每个通道/约定保留最近三个已发布的版本。
业务更新只修改活动 rootfs；切换回旧的 OTA 槽位会恢复该槽位自己的软件包和配置。

```bash
sudo apt update && sudo apt upgrade
dpkg-query -W aico-business
cat /run/aico-business-reboot-required.json  # May not exist if no reboot is required

# Downgrade within the same contract (the example version must still be in the repository)
sudo apt install --allow-downgrades aico-business=0.0.8-1
```

无需构建即可在本地检查实际范围：

```bash
python3 scripts/debian-package/system_config.py inventory /tmp/runtime-config.json
```

Linux 验证：`bash scripts/test_system_config_package.sh` 会在一次性容器中运行真实的 dpkg 升级和
降级、conffile 保留、失败恢复以及生产打包流程。其中的程序是测试替身，并不是可发布的开发板二进制
文件。请使用 `scripts/debian-package/release.sh build` 或三通道工作流构建生产软件包。
