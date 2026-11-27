---
sidebar_position: 5
---

# OTA 专用存储分区

## 目的

OTA 下载、状态和健康标记存放在一个专用的 300 MiB ext4 分区上。它取代了之前在 `/userdata` 上
使用预留文件的设计。普通日志、录音、swap 和 Agent 状态不会占用 OTA 容量，OTA 活动也不会让
`/userdata` 看起来被人为占满。

生产布局在不缩小 userdata 的情况下追加该分区：

```text
...,1792M(rootfs_b),3G(userdata),300M(ota)
```

该分区挂载在 `/userdata/ota`，保留现有的 OTA 运行时路径，同时将它们放在独立的文件系统上。

## 启动与故障关闭（fail closed）行为

Debian 开发板配置定义了该专用分区，systemd 通过 `userdata-ota.mount` 挂载它：

```text
300M(ota)
/dev/mmcblk0p10 -> /userdata/ota (ext4)
```

`aico-slot-resolve.service` 在 `userdata.mount`、`userdata-ota.mount` 之前运行。
`aico-ota-health.service` 在处理健康状态之前，要求 userdata 和 OTA 工作区都已挂载。

Go 升级程序在创建锁、状态或下载文件之前，会独立检查 `/proc/self/mountinfo`。该挂载必须是以
`/dev/disk/by-partlabel/ota` 为根的 ext4 文件系统；同一路径上的 bind mount 或无关文件系统会被
拒绝。如果预期的挂载未生效，`ota update`、`ota health` 和 `ota status` 会失败，而不是写入底层的
userdata 目录。

有意不提供旧的预留文件回退方案，也没有 SD 卡 OTA 缓存路径。产品从未以旧布局出货，因此出厂镜像
可以直接切换到分区设计。

## 容量模型

运行时决策基于文件系统的实际可用空间：

```text
required = remaining signed asset bytes + download safety margin
```

在下载分区资源之前，升级程序会：

1. 只选择非活动槽位需要的资源。
2. 跳过记录的哈希已经匹配的目标分区。
3. 删除过期的缓存条目，同时保留已校验的资源和有效的可续传 `.part` 文件。
4. 减去有效部分下载中已有的字节数。
5. 从专用 OTA 文件系统读取 `statfs(...).Bavail`。
6. 当可用字节数低于要求时，在第一次资源请求之前拒绝更新。

默认的 `download_safety_margin_bytes` 为 16 MiB。它用于覆盖文件系统元数据、目录更新和同步开销；
它不是一个预留文件，设备空闲时也不会减少容量。

Debian 构建还会拒绝不可能完成的目标槽位下载集合。其上限由共享的分区布局推导得出：

```text
300 MiB partition - 30 MiB ext4 unavailable-space allowance - 16 MiB runtime margin
= 254 MiB maximum compressed target-slot download set
```

这 30 MiB 的余量保守地按分区的 10% 为 ext4 元数据和保留块做预算，依据是经过验证的 256 MiB 镜像
通过 `statfs` 暴露约 232.4 MiB 可用空间。运行时的 `statfs` 检查仍是最终依据，因为文件系统元数据、
缓存资源和部分下载因设备而异。

## 出厂镜像流程

固件构建会生成一个空的通用 `ota.img`，并将其包含在 `update.img` 中。只有在设备启动后，运行时
状态和下载内容才会写入该文件系统。

生成签名清单后，Debian 系统配置步骤会将出厂基线写入 `userdata.img` 中的以下位置：

```text
/debian/ota/config.json
```

然后它会重新打包 `update.img`，并根据签名清单审计挂载的 userdata 镜像。OTA 分区本身不是在线 OTA
的目标；在线更新仍只写入非活动的 `boot` 和 `rootfs` 分区。

## 保留的现有保护措施

- 过期下载缓存清理。
- 针对网络中断的可续传 `.part` 下载。
- 发生 ENOSPC 后删除 `.part`，因为在同一个已满文件系统位置上重试不可能成功。
- 下载归档的大小和 SHA256 校验。
- 压缩镜像解压后的镜像哈希校验。
- 解压后镜像大小与目标分区的比对检查。
- 针对并发 OTA 命令的单一 OTA 更新锁。
- 现有的 StorageMonitor 清理以及面向用户的 `/userdata` 存储通知。

不再有预留空间的分配、释放、恢复、SD 路由或 StorageManager 迁移协调。

## StorageMonitor 与 SD 迁移

StorageMonitor 对 `/userdata` 进行采样。由于 `/userdata/ota` 是独立挂载的文件系统，OTA 下载不会
减少 `/userdata` 报告的 `statfs` 可用空间。它现有的清理阶段和通知仍会保护 Agent 日志、音频归档和
会话归档，应当保留。

SD 卡 StorageManager 继续挂载存储卡，并根据现有的 eMMC 水位线迁移符合条件的音频数据。它既不承载
OTA 下载，也不与 OTA 共享容量或锁，因此 SD 卡的插入、拔出、格式化和迁移都不会改变 OTA 容量。

## 诊断

```bash
mount | grep ' /userdata/ota '
df -h /userdata /userdata/ota
du -h /userdata/ota/downloads/* /userdata/ota/downloads/*.part 2>/dev/null
/usr/lib/aico/ota status
tail -n 100 /var/log/ota/ota.log
```

预期的挂载来源：

```text
/dev/disk/by-partlabel/ota on /userdata/ota type ext4
```

## 验收标准

- 完整镜像构建会生成 `ota.img` 并将其打包进 `update.img`。
- `/userdata/debian/ota/config.json` 来自经过审计的出厂 `userdata.img`；`ota.img` 初始为空。
- 缺少专用挂载时，OTA 命令直接拒绝执行。
- 容量不足时在任何分区资源请求之前拒绝。
- 有效的部分下载会减少剩余所需字节数。
- 网络失败时保留可续传的部分文件；ENOSPC 时删除它们。
- StorageMonitor 和 SD 迁移行为保持独立于 OTA。
