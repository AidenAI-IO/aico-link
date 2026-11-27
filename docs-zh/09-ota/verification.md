---
sidebar_position: 4
---

# A/B 与 `abctl` 验证

`abctl` 是用于 OTA A/B 元数据的诊断和工厂测试工具。它可以操作普通文件，也可以操作设备上的 `/dev/disk/by-partlabel/misc`。

## 元数据约定

- A/B 元数据位于 `misc` 分区字节偏移 `2048` 处。
- 数据结构为 32 字节，兼容 Android AVB A/B 布局版本 `1.0`。
- CRC32 使用大端 IEEE，覆盖元数据字节 `0..27`。
- 保留字节保持保留。应用的 OTA 状态保存在挂载于 `/userdata/ota` 的专用分区上，不写入 `misc` 保留字节。
- 出厂 `misc.img` 初始状态：槽位 A 优先级 15，已成功；槽位 B 禁用。

## 主机镜像检查

使用普通文件进行安全测试：

```bash
build/bin/abctl init /tmp/misc.img --size 4M
build/bin/abctl read /tmp/misc.img
```

预期输出包括槽位 A 已成功、槽位 B 优先级 0、`last_boot=A`。

手动切换状态：

```bash
build/bin/abctl set-active /tmp/misc.img b --tries 3
build/bin/abctl read /tmp/misc.img
build/bin/abctl mark-successful /tmp/misc.img b
build/bin/abctl read /tmp/misc.img
```

显式写入测试状态：

```bash
build/bin/abctl write /tmp/misc.img \
  --a-priority 14 --a-tries 0 --a-successful 1 \
  --b-priority 15 --b-tries 3 --b-successful 0
build/bin/abctl read /tmp/misc.img
```

`abctl write` 会拒绝非法状态，例如已成功的槽位却有非零的尝试次数。

## 设备检查

在设备上读取真实的 `misc`：

```bash
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
```

手动切换槽位：

```bash
/usr/lib/aico/abctl set-active /dev/disk/by-partlabel/misc b --tries 3
sync
reboot
```

启动后确认活动槽位：

```bash
cat /proc/cmdline
findmnt /
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc
```

确认设备健康后，提交槽位：

```bash
/usr/lib/aico/abctl mark-successful /dev/disk/by-partlabel/misc b
sync
```

## 回滚测试

设置一次试启动，且不标记为成功：

```bash
/usr/lib/aico/abctl set-active /dev/disk/by-partlabel/misc b --tries 1
sync
reboot
```

不要提交 B。持续重启，直到 SPL 用尽尝试次数并回到上一个成功的槽位。使用 `cat /proc/cmdline`、`findmnt /` 和 `abctl read` 确认。

## 预期诊断信号

- `/proc/cmdline` 包含 `aico.slot_suffix=_a` 或 `_b`。
- `/proc/cmdline` 中的 `root=PARTLABEL=rootfs_a|rootfs_b` 与槽位后缀一致。
- `/` 使用 `rootfs_a` 或 `rootfs_b`，与槽位后缀一致。不存在 `/oem`。
- `abctl read` 可以解析 AVB A/B 元数据，且没有 CRC 或布局错误。
- `ota status` 可以显示 OTA 状态、活动槽位、待定启动和原始 A/B 数据。
