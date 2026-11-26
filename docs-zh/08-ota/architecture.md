---
sidebar_position: 1
---

# OTA 架构与运行时

OTA 由三层完成：`debian_build.sh` 编排 Debian 应用/系统镜像和签名产物，厂商 SDK 提供 BSP 和镜像打包内部实现，设备端 `ota` 在手动触发时完成下载、写入和切换。一次性的 systemd 健康处理在启动后提交健康的槽位。手动触发的 [通道发布工作流](channel-release.md) 自动完成分类、约定、构建和发布。

## 分区布局

生产镜像使用 A/B 布局：

```text
32K(env),512K@32K(idblock),256K(uboot),4M(misc),32M(boot_a),32M(boot_b),1792M(rootfs_a),1792M(rootfs_b),3G(userdata),300M(ota)
```

| 分区 | A/B | OTA 行为 |
| --- | --- | --- |
| `env` | 否 | 不通过 OTA 更新；仅限工厂烧录或 USB 恢复 |
| `idblock` | 否 | 不通过 OTA 更新；避免变砖风险 |
| `uboot` | 否 | 不通过 OTA 更新；旧 bootloader 需要完整烧录更新 |
| `misc` | 否 | 存储 A/B 元数据，OTA 只修改槽位状态 |
| `boot` | 是 | 写入非活动的 `boot_a` 或 `boot_b` |
| `rootfs` | 是 | 写入非活动的 `rootfs_a` 或 `rootfs_b` |
| `userdata` | 否 | 升级时保留，存储非 OTA 的持久数据 |
| `ota` | 否 | 专用的 OTA 配置、状态、健康标记和下载缓存；仅工厂烧录 |

## 启动流程

1. ROM 加载 SPL。
2. SPL 从 `misc` 分区字节偏移 `2048` 处读取 Android AVB A/B 元数据。
3. SPL 选择优先级最高且可启动的槽位，加载 `boot_a` 或 `boot_b`。
4. 槽位专用的 FIT boot 镜像提供 `root=PARTLABEL=rootfs_a|rootfs_b` 和 `aico.slot_suffix=_a|_b`。
5. Linux 挂载对应的 `rootfs_*`。
6. `aico-slot-resolve.service` 为活动槽位解析稳定的分区路径。
7. `userdata.mount` 和 `userdata-ota.mount` 挂载持久数据和专用 OTA 工作区。平台文件和业务二进制文件已位于活动 rootfs 中。
8. `aico-ota-health-marker.service` 汇总必需的应用健康状态和 Config Web 恢复门户，然后
   `aico-ota-health.service` 处理一次待定的 OTA 状态。在这一确认过程中，Agent 启动是可选的。
   OTA 命令通过 `health_timeout_seconds`（默认 5 分钟）控制健康标记的等待时间。健康服务有一个
   10 分钟的外层启动超时，用于处理可中断的卡顿。如果把健康等待时间增加到超出这一预算，也要在
   systemd drop-in 中增大 `TimeoutStartSec`，为元数据提交留出时间。

## 升级流程

1. `ota` 读取 `/userdata/debian/ota/config.json` 和 `/usr/share/keyrings/aico-ota.pem`。
2. 获取配置的 `manifest_url`，或从仓库的发布列表中为配置的 dev/staging/prod 通道找出最高的
   OTA 版本。仅业务的发布会被跳过。较旧的出厂配置保留 `releases/latest` 回退；自定义静态源使用
   显式的清单 URL。
3. 下载 `manifest.json`，移除 `signature.value`，并校验其规范化 JSON 的 Ed25519 签名。受管通道还必须与签名中的通道和版本前缀一致，使用直接清单 URL 时也是如此。
4. 拒绝 `build_time` 更旧，或构建时间相同但版本不同的降级。
5. 选择非活动槽位，并从清单中解析对应槽位的资源。
6. 清理过期的下载缓存，并在扣除已校验的缓存和可续传的部分下载后计算剩余字节数。
7. 从专用 OTA 文件系统读取实际可用字节数，要求能够容纳剩余下载内容加上配置的安全余量。
8. 下载镜像，并在触碰任何分区之前校验归档大小和签名中的 SHA256。
9. 将非活动的目标槽位标记为不可启动，然后将每个解压后的镜像流式写入对应分区，同时计算并检查 `image_sha256`；哈希通过后执行 fsync，任何不匹配都会拒绝本次更新且不激活该槽位。
10. 从 eMMC 回读写入的镜像字节并校验相同的镜像 SHA256，然后在需要时对 rootfs 进行个性化处理。
11. 删除旧的 `health.ok`，写入 `/userdata/ota/pending_boot.json`。
12. 修改 `misc`，将目标槽位设为活动的试启动槽位，默认尝试次数为 3。
13. 重启进入目标槽位。

因此，常规更新在写入过程中只解压一次压缩镜像，而不是为写入前的镜像校验和归档预扫描分别解压。完整的 eMMC 回读仍作为独立的写入完整性检查。dry-run 模式仍会解压镜像并计算哈希，但不写入。

## 健康确认与回滚

新槽位启动后，Go 守护进程在运行时初始化完成后调用 OTA 健康写入逻辑。只有满足以下所有条件时才会写入 `/userdata/ota/health.ok`：

- `pending_boot.json` 存在。
- 当前 `aico.slot_suffix` 等于待定的目标槽位。
- 当前 rootfs 槽位等于待定的目标槽位。
- 健康标记写入的内容包括来自待定记录的版本、构建时间、nonce 以及当前启动 ID。

当 `ota` 看到匹配的标记时：

1. 先将当前受 CRC 保护的 `misc` 元数据持久保存到 `/userdata/ota/ab-commit.pending`，然后将槽位
   标记为成功、同步 `misc`，并回读校验元数据。
2. 在 `/userdata/ota/state.json` 中更新已提交的版本/构建时间以及各槽位的分区哈希。
3. 持久删除 `pending_boot.json`，然后撤销元数据备份。

健康处理、早期启动恢复和手动回滚在使用 `misc` 之前，都会先恢复被中断的健康提交。有效的元数据
会被保留并同步；CRC 无效的元数据会从备份中恢复。如果已提交的状态已经发布，恢复过程会完成待定
标记的清理。这样，在写入、同步或状态发布期间被中断的过程可以重试。恢复在 Linux 中运行；它不会为
SPL 增加冗余元数据支持，也不能保证在 Linux 启动之前断电时的槽位选择。systemd 超时也无法终止卡在
不可中断内核 I/O 中的进程。

如果健康窗口超时，`ota health` 会主动重启，让 SPL 消耗尝试次数。当尝试次数耗尽且目标槽位未成功时，SPL 回退到上一个成功的槽位。当 `ota health` 在旧槽位中观察到回滚时，会清理待定状态并将状态阶段标记为 `rolled-back`。

## 清单约定

必须使用清单 schema 2；schema 1 和 OEM 部分会被拒绝。清单中的 `parts[].name` 只能是 `boot` 或 `rootfs`。每个部分使用以下资源形式之一：

- `asset`：与槽位无关的 `{name,size,sha256}`，仅适用于两侧字节完全相同的镜像。
- `asset_a` 和 `asset_b`：槽位专用的 `{name,size,sha256}`。

对于 `.img.tar.gz` 资源，`size` 和 `sha256` 描述的是下载的归档。必需的 `image_sha256` 字段描述解压后的 `.img`；OTA 状态和 `requires_partitions` 比较的是这个解压后镜像的哈希。

生产 Debian OTA 必须原子地同时包含 boot 和 rootfs。`boot` 必须使用 `asset_a` 和 `asset_b`，因为 boot 镜像包含槽位专用的 DTB bootargs。`rootfs` 可以使用槽位专用资源，或在确认两侧字节完全相同时使用与槽位无关的资源。

## 出厂基线

发布的 `update.img` 必须在 userdata 中包含位于 `/userdata/debian/ota/config.json` 的出厂配置。
Debian 构建根据签名清单生成该配置，通过系统镜像构建器安装它，并在最终的挂载镜像审计之前重新
打包 `update.img`。

`config.json` 至少必须包含：

- `factory_version` - 出厂烧录版本号，用于降级保护和选择性更新校验
- `factory_build_time` - 出厂烧录构建时间
- `factory_partition_hashes.a.boot|rootfs` - 槽位 A 各分区的 SHA256
- `factory_partition_hashes.b.boot|rootfs` - 槽位 B 各分区的 SHA256

可选配置字段：

- `manifest_url` - 直接指定清单 URL（当前的本地/自托管路径）
- `public_key_path` - 覆盖默认公钥路径（默认 `/usr/share/keyrings/aico-ota.pem`）
- `github_token_path` - GitHub token 文件路径（私有仓库必需）
- `download_safety_margin_bytes` - 在剩余下载之外保留的可用字节数（默认 16 MiB）

专用存储的标识不可配置：生产 OTA 始终要求 `/dev/disk/by-partlabel/ota` 以 ext4 文件系统、以 `/`
为根挂载在 `/userdata/ota` 上。测试代码可以注入合成的挂载信息，而无需暴露设备端的配置绕过手段。

Debian 构建根据同一份布局约定推导目标槽位的下载上限。当前的 300 MiB 分区为 ext4 元数据和保留块
保守预留 30 MiB，再加上 16 MiB 的运行时安全余量，得到 254 MiB 的最大压缩下载集合。

出厂基线必须区分槽位，因为 `boot_a.img` 和 `boot_b.img` 的哈希不同。缺少基线时 OTA 初始化必须失败；不应猜测当前分区的版本。

注意：`generate_ota_device_config.sh` 生成的 `config.json` 还包含 `repo` 和 `channel` 字段，但这些字段仅供人阅读，OTA 代码不会读取它们。实际的通道校验来自清单本身。

## 私有仓库 Token

公开的 GitHub 发布不需要设备 token。私有仓库可以将只读 token 放在：

```text
/userdata/ota/gh_token
```

当 token 存在时，`ota` 会在 GitHub Release 元数据、清单和镜像下载请求中添加 bearer token。OTA 签名密钥和 GitHub token 是两个相互独立的凭据。
