---
sidebar_position: 3
---

# OTA 设备验收流程

在启用生产 OTA 推送之前，应在具有代表性的硬件上完成以下验收测试。建议在每一步记录设备序列号、硬件版本、起始槽位、目标发布版本，以及 `abctl read` 和 `ota status` 的输出。

## 前提条件

- 生产镜像使用生产 Ed25519 公钥构建。
- 本地或自托管的 HTTP(S) 端点包含签名的 `manifest.json` 以及压缩镜像归档：
  `boot_a.img.tar.gz`、`boot_b.img.tar.gz`、`rootfs.img.tar.gz` 和 `update.img.tar.gz`。
- `update.img.tar.gz` 中的 `update.img` 包含专用的空 `ota` 分区，以及 userdata 中位于 `/debian/ota/config.json` 的出厂基线。
- 如果有 UART，建议同时记录 SPL 回滚日志。

`aico-ota-health.service` 只在启动时处理 `/userdata/ota/pending_boot.json`；它不执行网络或
GitHub 更新检查。手动更新必须通过 `ota update` 触发。

## 1. USB 工厂烧录验收

下载本地构建的 `update.img.tar.gz`，解压出 `update.img`，然后使用常规 USB 恢复流程烧录：

```bash
tar -xzf update.img.tar.gz update.img
FLASH_TOOL=pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool
scripts/flash.sh inspect --tool "${FLASH_TOOL}"
sudo scripts/flash.sh flash --tool "${FLASH_TOOL}" \
  --image ./update.img --sha256 "<verified-sha256>" \
  --confirm-erase-all-data
```

仓库根目录下的 `upgrade_tool/upgrade_tool` 是 macOS Mach-O 二进制文件；在 Linux 主机上请使用
上面的 Linux SDK 工具。

设备启动后检查：

```bash
cat /proc/cmdline
findmnt /
ota_device="$(readlink -f /dev/disk/by-partlabel/ota)"
ota_mount_device="$(awk '$2 == "/userdata/ota" && $3 == "ext4" { print $1 }' /proc/mounts)"
userdata_mount_device="$(awk '$2 == "/userdata" { print $1 }' /proc/mounts)"
test -n "$ota_mount_device"
test "$(readlink -f "$ota_mount_device")" = "$ota_device"
test -n "$userdata_mount_device"
test "$(readlink -f "$userdata_mount_device")" != "$ota_device"
df -h /userdata /userdata/ota
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
/usr/lib/aico/ota status
```

预期：

- 出厂启动位于槽位 A。
- `/proc/cmdline` 包含 `aico.slot_suffix=_a` 和 `root=PARTLABEL=rootfs_a`。
- `/` 挂载自 `rootfs_a`（`/dev/mmcblk0p7`）；不存在 `/oem`。
- `/userdata/ota` 是 ext4 挂载，其来源解析为 `/dev/disk/by-partlabel/ota`，并且显示为独立于 `/userdata` 的文件系统。
- 可以从字节偏移 `2048` 处正常解析 `misc` 元数据，槽位 A 为成功状态。
- `/userdata/debian/ota/config.json` 存在，`ota status` 不报告缺少出厂基线。

## 2. 手动切换槽位

切换到非活动槽位：

```bash
/usr/lib/aico/abctl set-active /dev/disk/by-partlabel/misc b --tries 3
sync
reboot
```

重启后检查：

```bash
cat /proc/cmdline
findmnt /
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
```

预期：

- `/proc/cmdline` 包含 `aico.slot_suffix=_b` 和 `root=PARTLABEL=rootfs_b`。
- `/` 挂载自 `rootfs_b`（`/dev/mmcblk0p8`）；不存在 `/oem`。
- 在标记成功之前，槽位 B 仍有剩余尝试次数。

## 3. 标记成功

确认新槽位可用后，提交：

```bash
/usr/lib/aico/abctl mark-successful /dev/disk/by-partlabel/misc b
sync
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
```

预期：槽位 B 为成功状态且尝试次数为 0；之前的槽位仍作为回退槽位保留。

## 4. 回滚试启动

强制试启动到另一个槽位，但不标记成功：

```bash
/usr/lib/aico/abctl set-active /dev/disk/by-partlabel/misc a --tries 1
sync
reboot
```

为阻止健康确认成功，可以在这次有限的测试中 mask `aico-ota-health-marker.service`，或在试启动
期间阻止应用就绪。尝试次数耗尽后再次重启，然后移除 mask。

预期：SPL 返回到上一个成功的槽位。使用 UART、`cat /proc/cmdline` 和 `abctl read` 确认。

## 5. OTA 正常路径

确认配置：

```bash
cat /userdata/debian/ota/config.json
```

配置应指向目标 repo/channel，并在 `factory_partition_hashes.a` 和 `factory_partition_hashes.b` 中包含 `boot`、`rootfs` 哈希。

执行一次 OTA：

```bash
/usr/lib/aico/ota update
```

预期流程：

1. 下载并校验签名清单。
2. 选择非活动槽位的资源。
3. 下载、校验并写入非活动分区。
4. 写入 `/userdata/ota/pending_boot.json`。
5. 切换 `misc` 并重启。
6. 新槽位启动后，写入匹配的 `/userdata/ota/health.ok`。
7. `ota` 标记成功并删除待定文件。

成功后检查：

```bash
/usr/lib/aico/ota status
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
ls -l /userdata/ota/pending_boot.json /userdata/ota/health.ok 2>&1 || true
```

预期：

- `ota status` 显示已提交的版本/构建时间。
- 活动槽位为成功状态且尝试次数为 0。
- `pending_boot.json` 和 `health.ok` 已被清理。

## 6. 失败场景

至少覆盖以下失败场景：

| 场景 | 预期 |
| --- | --- |
| 签名无效或清单无效 | 拒绝更新，不写入分区，不切换槽位 |
| 归档 SHA256 或大小不匹配 | 在写入目标分区之前拒绝更新 |
| 解压后镜像 SHA256 不匹配 | 在流式写入期间拒绝更新；目标槽位保持不可启动，不切换槽位 |
| 降级发布 | 拒绝更新，不切换槽位 |
| 健康标记缺失或不匹配 | 目标槽位不会被标记为成功，尝试次数耗尽后回滚 |
| 非活动 boot 镜像损坏 | SPL 不应卡死，应回退到上一个成功的槽位 |
| 下载中断 | 不切换槽位；网络恢复后可以重试或重新下载 |
| 专用 OTA 分区未挂载 | `ota status`、`ota health` 和 `ota update` 失败，且不会在 userdata 挂载点下创建文件 |
| OTA 分区剩余容量不足以完成下载 | 在请求任何分区资源之前拒绝；非活动槽位保持不变 |

分区写入期间的断电测试应通过可控电源或 HIL 测试台进行；不建议手动随机断电。
