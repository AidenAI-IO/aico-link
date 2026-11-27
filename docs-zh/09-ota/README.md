---
sidebar_label: 概览
sidebar_position: 0
---

# OTA 概览

本项目的生产 OTA 使用 A/B 分区、签名清单和启动健康确认机制。运行期间，设备只写入非活动槽位。重启后，Rockchip SPL 选择新槽位；如果新系统未在健康窗口内确认成功，SPL 会自动回滚到上一个成功的槽位。

此版本移除了 OEM 分区和 `/oem`，仅支持完整镜像烧录，不提供从旧布局迁移的 OTA。新布局使用
boot/rootfs A/B 槽位，每个 rootfs 槽位 1792 MiB，OTA 清单 schema 版本为 2。

## 范围

- 目标硬件：Luckfox Pico Zero / RV1106 + eMMC。
- 分发方式：`main` 有变更时由每小时定时构建自动发布的 dev GitHub Releases、手动触发的
  dev/staging/prod GitHub Releases、本地构建产物，或手动托管的 HTTP(S) 目录。通道发布会根据源码变更选择发布业务软件包或完整的签名 OTA；参见
  [通道发布](channel-release.md)。
- 升级方式：设备端 `/usr/lib/aico/ota` 获取清单、校验签名、验证 SHA256、写入非活动槽位、切换 `misc` 并重启。
- 回滚方式：Rockchip SPL A/B 元数据控制启动尝试次数；只有在应用健康确认之后才标记为成功。

## 文档索引

- [OTA 架构与运行时](architecture.md)
- [健康检查、数据兼容性与 A/B 回滚](health-check-rollback-design.md)
- [OTA 密钥管理](key-management.md)
- [设备验收流程](device-acceptance.md)
- [A/B 与 `abctl` 验证](verification.md)
- [OTA 专用存储分区](no-space-plan.md)
- [GitHub 代理配置](ota-github-proxy.md)
- [外部开发者指南](ota-external-developers.md)
- [分发快速示例](ota-quick-examples.md)
- [发布通道策略](ota-release-channels.md)
- [手动 dev / staging / prod 发布](channel-release.md)
- [Debian 业务软件包](debian-package.md)

## 核心约束

- OTA 不更新 `env`、`idblock` 或 `uboot`；它们只能通过工厂烧录或 USB 恢复更新。
- OTA 只写入非活动槽位的 `boot_*` 和 `rootfs_*`。
- 专用的 300 MiB `ota` 分区挂载在 `/userdata/ota`，用于存储 OTA 状态、下载缓存和健康标记。出厂配置仍保存在持久化 userdata 文件系统中的 `/userdata/debian/ota/config.json`。
- `boot_a.img` 和 `boot_b.img` 包含不同的槽位 bootargs；清单必须使用对应槽位的 boot 资源。
- 当缺少出厂基线，或清单签名/哈希校验失败时，设备必须按故障关闭（fail closed）原则拒绝继续。
- 除非 `/userdata/ota` 是以 `/dev/disk/by-partlabel/ota` 为根的 ext4 挂载，否则 OTA 命令会直接拒绝执行（fail closed）；并且要求实际可用空间足以容纳剩余下载内容再加 16 MiB 余量。对于当前的 300 MiB 分区，`debian_build.sh` 还会将目标槽位的下载集合上限设为 254 MiB。

## 常用命令

```bash
# View OTA status
/usr/lib/aico/ota status

# Check and perform OTA update immediately
/usr/lib/aico/ota update

# View A/B metadata
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc

# View current slot and rootfs
cat /proc/cmdline
findmnt /
```

`check-now` 仍作为兼容别名保留；新的脚本和文档应使用 `update`。

`ota check` 只检查签名的发布元数据，并以 JSON 输出 `available`、`version`、`build_time` 和
`current_version`。它不会安装固件。Config Web 在触发固件安装之前会使用这一检查，随后进行限定
范围的 `aico-business` APT 更新。参见 [APT 更新](apt-repository.md)。

## 相关源码

| 路径 | 说明 |
| --- | --- |
| `src/agent/cmd/ota` | OTA CLI 入口，包括手动更新和健康处理 |
| `src/agent/cmd/abctl` | A/B 元数据诊断工具 |
| `src/agent/internal/ota` | 清单、下载、状态机、槽位、健康等 OTA 核心逻辑 |
| `overlay-debian/etc/systemd/system/aico-slot-resolve.service` | 在本地挂载之前解析当前活动的 A/B 槽位 |
| `overlay-debian/etc/systemd/system/aico-ota-health.service` | 启动时处理待定的 OTA 健康状态 |
| `scripts/generate_ota_manifest.sh` | 生成签名的 OTA 清单 |
| `scripts/generate_ota_device_config.sh` | 根据清单生成出厂配置 |
| `scripts/ota_partition_layout.sh` | 读取 Debian 系统 OTA 分区大小并推导发布容量 |
| `scripts/debian-system/container-install-ota-config.sh` | 安装出厂 OTA 配置并重新打包 `update.img` |
| `pico-sdk/project/scripts/mk-ab-misc.py` | 生成出厂 `misc.img` A/B 元数据 |
