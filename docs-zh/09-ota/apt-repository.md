# GitHub APT 仓库

业务软件包仓库：`https://aidenai-io.github.io/aico-link/apt`。
GitHub Pages 托管签名的 `InRelease`、`Release.gpg`、软件包索引和 `.deb` 文件。
软件包只来自三个通道中已公开的 Release；未公开的 Actions 构建产物永远不会加入仓库。

该仓库只包含 Aico 软件包。如果某个旧约定的基础 OTA 发布中记录了
`aiden-business_<version>-1_armhf.deb` 资源，却没有对应的 `aico-business` 资源，那么该约定
以及所有使用该基础的业务发布都会被排除。这些约定不会获得 APT suite 或软件包池；请改用当前的
Aico 基础镜像。在执行该排除之前，仍会校验完整的已发布历史。受支持的 Aico 约定中缺失软件包、
校验和或清单无效仍然是错误；仅包含旧约定的历史也不能用一个空仓库替换站点。现有受支持的 Aico
约定保留其正常的版本保留策略。

## 升级开发板

带有此功能的新基础镜像会根据 `/usr/lib/aico/platform/contract.json` 自动选择仓库。例如，约定
为 `1` 的 dev 使用 `dev-c1`；staging/prod 各自使用其约定对应的仓库。

```bash
sudo apt update && sudo apt upgrade
dpkg-query -W aico-business
sudo /usr/lib/aico/ota --config /userdata/debian/ota/config.json self-check
```

APT/dpkg 会自动停止并恢复之前正在运行的业务服务。用户配置和 userdata 会被保留。业务和实时
配置升级不需要重启设备；网络/SSH/USB 配置的变更会提示稍后重启，而不会主动中断连接。范围和
标记参见 [由业务软件包管理的运行时配置](system-config-package.md)。新约定的软件包不会作为旧约定
的候选版本出现。设备只有在安装新的 OTA 之后，才会切换到新基础约定对应的仓库。软件包的 preinst
仍会检查通道、约定、基础发布标签和系统指纹。

固件页面会在基础固件版本旁边显示已安装的业务发布版本（例如
`dev-v0.0.77 + aico-business-v0.0.78`）。它从 dpkg 读取已配置的软件包，因此仅业务升级会改变
显示的版本，而不会改变基础固件版本。

Config Web 的“Check and install updates”操作（`POST /api/ota/updates`）会启动
`aico-hybrid-update.service`，按以下顺序执行：

1. 针对当前通道的 GitHub Release 运行 `ota check`，校验签名清单、版本和构建时间。只有在有新
   固件可用时才运行 `ota update`。检查本身不会下载镜像、写入分区或改变 OTA 事务状态。
2. 切换槽位并重启进入新固件后，等待现有的 OTA 健康服务。只有当请求的版本和构建时间都处于
   `committed` 时才继续；发生回滚或健康确认失败时，任务结束且不升级业务软件包。
3. 当没有新固件可用，或新固件已确认后，只刷新来自 `aico-business.sources` 的签名索引。模拟执行
   `apt-get --only-upgrade install aico-business`，仅当该软件包有更新时才安装。只针对该软件包
   可以避免通过通用的 `apt-get upgrade` 升级其他已安装软件；会改变其他软件包或删除任何软件包
   的计划会被拒绝。平台仓库 pin、APT 签名校验和软件包约定检查仍然生效。

更新服务独立于 Config Web 运行，因此在业务软件包升级期间停止并重启门户不会中断任务。它的标记
保存在 `/userdata/ota/hybrid-update.pending`，使其能在 OTA 重启后继续执行；`GET /api/ota/status`
会持续提供日志和进度。业务软件包升级成功后不会主动重启设备。运行时配置所需的后续重启仍通过
`/run/aico-business-reboot-required.json` 提示。APT 刷新或安装失败会报告为失败，而不是“已是
最新”。worker 将每次 OTA 元数据检查、APT 索引刷新和 APT 模拟限制在 600 秒以内。APT 索引刷新
最多尝试三次，间隔分别为 15 秒和 30 秒，以便 OTA 重启后 Wi-Fi 关联、DHCP 或软件包源有时间
恢复可用。重试耗尽或元数据超时会释放更新锁并清除请求标记。固件烧录和软件包安装不受此元数据
超时限制。软件包安装被中断后，请先修复 dpkg 状态再手动重试。

`/etc/apt/preferences.d/aico-business` 为匹配仓库中的业务软件包设置优先级 990，为 Debian 仓库
软件包设置优先级 1，低于已安装软件包的优先级 100。因此常规的 `apt upgrade` 只会升级业务软件包。
Debian 系统软件包通过 OTA 更新，避免在基础约定不变的情况下系统库发生变化。仍支持显式安装尚未
安装的 Debian 工具。如果你添加了第三方系统仓库，请单独为其配置 pin。有意修改系统库的维护者可以
显式选择版本或调整 pin，然后通过新的 OTA 建立基础约定。

`apt upgrade` 不会重新安装已安装的版本。要用同版本的已发布软件包替换构建出的软件包：

```bash
sudo apt install --reinstall aico-business
```

## 现有受管设备的初始设置

从经过审查的源码检出中复制公钥和配置工具。不要直接执行来自未经验证的网络响应的脚本。

```bash
scp overlay-debian/usr/share/keyrings/aico-archive-keyring.asc \
    overlay-debian/usr/lib/aico/aico-apt-source aico@<device-ip>:/tmp/
ssh -t aico@<device-ip> 'sudo python3 /tmp/aico-apt-source --public-key /tmp/aico-archive-keyring.asc'
ssh -t aico@<device-ip> 'sudo apt update && sudo apt upgrade'
```

公钥指纹：`D40CD0F29C3A65449439857F53CFFB430DBD167D`。
只信任仓库的 `Signed-By` 公钥；不要使用 `apt-key` 或 `trusted=yes`。
该工具会拒绝没有通道或约定不完整的旧镜像。这些设备必须先用其通道的受管基础镜像完整重新烧录。
仓库配置、公钥和系统软件包 pin 属于平台，通过 OTA 下发，不由业务软件包修改。

## 发布与维护

初始设置：

1. 在 GitHub 仓库中，于 Settings → Pages → Source 下选择 **GitHub Actions**。
2. 将与已提交公钥对应的 ASCII armored OpenPGP 签名私钥保存到仓库 Actions secret
   `APT_SIGNING_PRIVATE_KEY` 中。请使用专用的 APT 密钥；不要复用 OTA Ed25519 PEM 密钥。
3. 推送并合并此工作流，然后手动运行 **Aico Link APT Repository**，根据已发布版本构建索引。合并
   之前，你也可以在 **Debian Build (backup / self-hosted-02)** 中选择功能分支，启用 `apt_only`
   并保持 `dry_run=false`，从而只刷新 APT 索引，忽略构建和新发布参数。

主发布和备用发布在成功公开 GitHub Release 之后都会调用此工作流。它不会重新构建业务软件包、分配
版本或改变发布比较基线。签名 secret 会在发布之前检查。APT 刷新失败不会撤销已经公开的 Release；
修复问题后单独重新运行 APT 工作流即可。

索引不设置 `Valid-Until`，因此不需要定时刷新；它只在发布后或手动触发时更新。APT 仍会校验索引
签名和软件包校验和，三个通道的新版本仍需手动发布。Pages 的 `github-pages` Environment 必须允许
运行发布工作流的分支。

每个通道的当前约定保留最近三个已发布的软件包。已被取代的约定只保留最新的一个软件包，因为混合
更新会在读取 APT 之前先安装 OTA（并将设备迁移到当前约定）。如果软件包会超出 900 MiB 的 Pages
预算，会从最旧的已取代约定 suite 开始删除，直到站点符合预算；当前约定永远不会被裁剪。索引使用
APT by-hash 下载；软件包路径由约定和发布标签固定。如果旧缓存引用了已被清理的软件包，请重新运行
`apt update`。只有当仅当前约定就超出预算，或构建后的站点仍然超出预算时，部署才会停止并保留旧
站点。

来源记录、SHA256、软件包架构/版本以及内嵌的约定都必须通过校验，然后整个站点才会被签名并原子
部署。私钥只放在签名步骤的临时 GnuPG 目录中；清理后只上传公开的站点内容。

在 Linux 上本地验证：

```bash
python3 scripts/test_apt_repository.py
GNUPGHOME=/path/to/private-gnupg python3 scripts/apt/repository.py \
  --repo AidenAI-IO/aico-link --output output/apt/site
```

测试会运行真实的 `apt update` 和模拟的 `apt upgrade`，覆盖约定隔离、系统软件包 pin、错误的公钥、
被篡改的索引、无过期时间的签名元数据以及发布软件包校验失败。请安全备份签名私钥。轮换公钥时，
先通过 OTA 向设备下发同时信任新旧两个密钥的过渡配置，然后再切换仓库签名密钥。
