---
sidebar_position: 4
---

# Debian 业务软件包

Debian 构建流水线使用 `aico-business` 作为基线业务软件包。应用构建阶段首先构建并审计 armhf
业务程序。`scripts/debian-package/build.sh` 在 Linux 构建主机上的 Debian 容器中调用 `dpkg-deb`。
单独打包时输出到 `output/debian-package/`；完整镜像构建会生成
`output/debian-system/aico-business.deb`。系统构建阶段在 debootstrap 生成的 rootfs 中运行
`dpkg -i`，然后创建 `rootfs.ext4` 和 A/B 镜像。

软件包内容使用标准 Debian 路径：

```text
/usr/lib/aico/                 # Agent, frame/audio/BLE, OTA CLI, VAD helper, and runtime helper scripts
/usr/lib/aico/models/          # RV1106 audio and UI grounding models
/usr/share/aico/config-web/    # Config Web static assets
/usr/share/aico/skills/        # bundled skills
/usr/share/aico/audio/         # Business audio assets
/usr/share/aico/ekho-data/     # Mandarin voice and mixed-language alphabet assets
/usr/share/doc/aico-business/ekho/ # Ekho/voice licenses, provenance, upstream source, and patch
/etc/aico/ and others         # In-scope runtime configuration, registered as conffiles
```

基础 rootfs 提供驱动、内核模块、Rockchip 运行时库、EDID、平台排除列表中列出的基础集成脚本以及
OTA 信任根。OEM 分区和 `/oem` 目录已被移除，且没有兼容路径；运行旧版本的设备需要完整重新烧录。
`/userdata/agent`、`/userdata/system` 和用户 Skill 不包含在软件包中。

运行时配置直接包含在同一个业务软件包中。其范围、生效时机和初次归属转移参见
[由业务软件包管理的运行时配置](system-config-package.md)。

软件包的 `release-manifest.json` 记录业务版本、架构、平台约定范围、配置 schema 和
`business_epoch`。三通道发布软件包中的 `preinst` 会检查当前的平台约定、通道和基础标识。完整的
升级策略（包括软件包签名和磁盘空间检查）仍需要一个单独的业务升级编排器；下载的软件包目前通过
apt 安装。新布局从约定 `1` 开始，软件包要求为 `[1, 2)`。基础在
`/usr/lib/aico/platform/contract.json` 中声明其约定，该文件不归业务软件包所有。旧布局无法通过
槽位 OTA 跨越这一变更，必须重新烧录。新布局之后的平台更新使用完整的 boot/rootfs OTA。

构建命令（在 Linux 主机上运行）：

```bash
scripts/debian-apps/build-apps.sh all
scripts/debian-system/build.sh builder
DEBIAN_PACKAGE_OUTPUT_DIR=output/debian-system \
  scripts/debian-package/build.sh
scripts/debian-system/build.sh bsp
OTA_PUBLIC_KEY_PATH=keys/ota_pubkey.pem scripts/debian-system/build.sh rootfs
```

完整镜像命令会自动生成并安装该软件包：

```bash
./debian_build.sh
```

macOS 开发机不直接运行 Debian/armhf 构建。请将仓库同步到 Linux 构建主机（例如 `ssh luhaodev`），
运行上述命令，然后把 `output/debian-system` 产物同步回开发机。

新的平台路径与软件包归属：

| 内容 | 路径 | 所有者 |
| --- | --- | --- |
| 业务程序和自检 CLI | `/usr/lib/aico/` | aico-business |
| Ekho 可执行文件、普通话数据以及源码/许可证记录 | `/usr/lib/aico/ekho`、`/usr/share/aico/ekho-data/`、`/usr/share/doc/aico-business/ekho/` | aico-business |
| 业务模型和通知音频 | `/usr/lib/aico/models/`、`/usr/share/aico/audio/voice-notifications/` | aico-business |
| 运行时配置和 Aico Link 服务辅助脚本 | `config-package.json` 定义的命名空间（不含平台项） | aico-business |
| RGA 和 VQE 共享库 | `/usr/lib/aico/platform/lib/` | 基础 rootfs |
| 模块和 Wi-Fi/MCU 固件 | `/usr/lib/aico/platform/modules/` | 基础 rootfs |
| EDID 和 VQE 配置 | `/usr/share/aico/edid/`、`/usr/share/aico/audio/config_aivqe.json` | 基础 rootfs |
| OTA Ed25519 公钥 | `/usr/share/keyrings/aico-ota.pem` | 基础 rootfs |

rootfs A/B 各 1792 MiB，位于分区节点 p7/p8；userdata 为 p9，ota 为 p10。原 OEM 槽位合计的
512 MiB 平均分给了两个 rootfs 槽位。userdata 仍为 3 GiB，OTA 仍为 300 MiB。新清单使用
schema 2，必须完整包含 boot 和 rootfs。发布资源为 `boot_a.img.tar.gz`、`boot_b.img.tar.gz`、
`rootfs.img.tar.gz`、`update.img.tar.gz` 和 `manifest.json`；`.deb` 也可以单独发布。

修改平台文件或受信任的公钥后，请重新生成 rootfs。镜像组装会将公钥输入与 rootfs 构建记录比较，
拒绝复用不匹配的旧 rootfs。构建顺序为：应用 → BSP → 软件包/rootfs → 镜像 → 清单/配置 → 审计。
A/B 写入、校验、个性化、健康确认以及失败时的回滚仍基于槽位进行。

rootfs 还会记录 boot/env 输入的校验和；重新构建 BSP 后，旧的 rootfs 不能直接用于组装。每个
rootfs 阶段都会重新打包当前的应用，以免复用带有旧路径的缓存业务软件包。

## 版本管理

`scripts/debian-package/version.sh` 是默认业务版本的唯一来源：业务版本 `0.0.1`，Debian 修订号
`2`，完整的本地软件包版本 `0.0.1-2`。可以在命令行中通过 `AICO_BUSINESS_VERSION` 和
`AICO_BUSINESS_REVISION` 覆盖。对于公开的三通道发布，发布计划会自动分配版本和基础约定；参见
[三通道发布](channel-release.md)。

没有发布计划时，平台约定默认为 `1`。每次发布系统 OTA 都会分配新约定；后续业务软件包继承其通道
最新的基础。OTA 清单仍使用 schema 2。

## 独立构建与 GitHub Releases

Linux amd64 构建主机需要 Docker、Git、curl、Python 3.11+ 和 dpkg-deb。工作树必须干净，并且固件
修订版本（包括 vendored SDK 源码）必须与应用构建记录一致。

```bash
scripts/debian-package/release.sh build
```

该命令会准备固定版本的 Go 工具链，构建 OpenCV 和业务程序，审计应用 ELF 文件，并生成 `.deb`。它
使用单独的轻量打包容器；不编译 BSP、不构建 rootfs，也不需要 OTA 私钥或 Agent 凭据。Go/OpenCV
缓存环境变量与 `debian_build.sh` 相同。如果当前提交的应用已经构建并审计过，可以使用
`release.sh stage` 只重新打包。来自其他提交或带有未提交改动的产物会被拒绝。

产物目录 `output/debian-package/release/` 包含：

```text
aico-business_0.0.1-2_armhf.deb
release-manifest.json
build-metadata.json
RELEASE-NOTES.md
SHA256SUMS
```

**Debian Business Package (artifacts only)** 工作流只构建产物。公开发布使用
**Aico Link Channel Release**，全部手动触发，由计划决定发布业务软件包还是完整 OTA。旧的
`scripts/debian-package/release.sh publish` 入口已被禁用。发布操作、自动变更比较、失败后重试以及
所需的仓库设置参见 [三通道发布](channel-release.md)。

发布还会更新 [GitHub APT 仓库](apt-repository.md)。设备使用 `Signed-By` 校验签名索引和软件包
校验和，然后通过 `apt update && apt upgrade` 升级业务软件包。手动下载的 `SHA256SUMS` 文件只检查
传输完整性，不等同于软件包签名。

## 在现有设备上安装

Aico 命名过渡要求在安装重命名后的业务软件包之前，先完成一次配套的完整 boot 和 rootfs OTA。软件包
的安装前检查会拒绝之前的平台布局。请先完成平台 OTA，以保留 `/userdata` 及其迁移后的配置；参见
[产品名称与技术标识符](../02-architecture/naming.md)。

只在具有新布局和兼容约定的设备上安装。先将 `contract.json` 与软件包清单比较，并保留之前的
`.deb` 和用户配置备份。

```bash
cd /path/to/downloaded-release
sha256sum -c SHA256SUMS
cat /usr/lib/aico/platform/contract.json
sudo apt install ./aico-business_0.0.1-2_armhf.deb
sudo /usr/lib/aico/ota --config /userdata/debian/ota/config.json self-check
dpkg-query -W aico-business
```

Ekho 会随应用自动构建。软件包带有 Flite `slt` 美式英语女声补丁和普通话数据；它声明了 Flite
（`libflite1`，包括其 SLT 音色）、libsndfile、Sonic、mpg123 和 C++ 运行时依赖。在创建镜像时，
rootfs 软件包列表会在 `dpkg -i` 之前安装这些依赖；升级现有设备时由 `apt install` 解析它们。打包
程序在复制之前会校验暂存的 Ekho 内容的校验和。无需手动安装 `/userdata/ekho`，也无需在设备上编译。
安装后以服务用户身份检查语音合成：

```bash
sudo -u aico /usr/lib/aico/ekho -v Mandarin -s 20 --english-speed -10 -a 0 \
  --samplerate 22050 -o /tmp/ekho-smoke.wav '你好。Hello from Aico.'
```

这会生成一个 WAV 文件但不播放。Agent 设置和播放行为参见
[本地 Ekho 语音](../05-agent/audio-features.md#本地-ekho默认)。检查完成后请删除该测试 WAV。

从修订号 2 开始，`preinst/prerm/postinst/postrm` 维护脚本会通过 apt/dpkg 自动停止和启动业务服务。
升级和重新安装时会记录正在运行的服务，先停止代理重启监听器及其任务，然后停止 Agent、Config Web、
Wi-Fi 代理、frame、audio、BLE 和 ttyd。解包和配置成功后，它们会执行 daemon-reload，恢复之前正在
运行的服务，最后恢复监听器。启用状态保持不变；之前已停止的服务不会被自动启动，而手动启动但未
启用的服务会被恢复。常规的 systemd 依赖关系仍然生效。

从旧软件包首次升级时，由新软件包的 preinst 负责停止服务。要用相同版本进行演练：

```bash
sudo apt install --reinstall ./aico-business_0.0.1-2_armhf.deb
```

在 rootfs 构建的 chroot 中、设置了 `SYSTEMD_OFFLINE=1`、`DPKG_ROOT` 非空或没有 systemd 时，会跳过
服务操作，并且遵循 `policy-rc.d`。首次安装时未运行的业务服务不会被自动启动；镜像中安装的 systemd
单元会在首次启动时启动它们。卸载软件包会停止服务。conffile 按 dpkg 规则保留，辅助脚本会被删除，
用户数据不会被删除。

服务快照保存在 `/var/lib/aico-business/service-transition/`。dpkg 中止回调会尝试恢复之前的服务。
如果恢复失败，快照会被保留，维护脚本返回失败。修复原因后运行 `sudo dpkg --configure -a` 重试。
在修复问题之前不要删除快照。维护脚本不会自动回滚软件包文件、迁移配置或运行硬件健康检查。安装
成功后仍需运行上面的自检；如果失败，请从备份恢复旧软件包。自检会把 Agent HTTP 不可用记录为警告，
但要求 Config Web 可以访问，以便仍能通过恢复门户修复损坏的 Agent 配置。对于受管软件包，preinst
会自动检查约定。

从早期测试软件包 `5.2.1-2` 重新编号为 `0.0.1-2` 属于显式降级。自动化安装时还需使用
`apt install -y --allow-downgrades ./aico-business_0.0.1-2_armhf.deb`。业务软件包只修改活动
rootfs；槽位 B 不会同步升级，固件 OTA 的出厂版本记录也不会更新。业务软件包管理 `/etc/locale.conf`
中的 `LANG=C.UTF-8`；Debian 的 `/etc/default/locale` 符号链接指向它，在下次登录时生效。平台约定
文件仍归基础所有。初次配置归属转移需要新的 OTA 基线；手动编辑约定文件是不够的。
