---
sidebar_position: 2
---

# OTA 密钥管理

生产 OTA 清单使用 Ed25519 签名。设备通过 `/usr/share/keyrings/aico-ota.pem` 校验 `manifest.json`，在写入之前校验下载的归档，并且只有在流式镜像哈希和 eMMC 回读检查都通过后才激活非活动槽位。

## 密钥生成

离线生成生产用的 Ed25519 私钥和公钥：

```bash
openssl genpkey -algorithm ed25519 -out ota_ed25519_private_key.pem
openssl pkey -in ota_ed25519_private_key.pem -pubout -out ota_pubkey.pem
```

私钥 `ota_ed25519_private_key.pem` 不得提交到仓库。请将其视为发布签名基础设施。

校验公钥格式：

```bash
scripts/validate_ota_pubkey.sh ota_pubkey.pem
```

该脚本只接受 Ed25519 公钥。RSA、ECDSA、格式错误的密钥或缺失的文件都会被拒绝。

## 公钥部署

生产固件构建必须提供匹配的密钥对和外部 Agent 配置。将它们传给 Debian 构建入口：

```bash
OTA_PRIVATE_KEY_PATH=/path/to/ota_ed25519_private_key.pem \
OTA_PUBLIC_KEY_PATH=/path/to/ota_pubkey.pem \
AGENT_CONFIG_PATH=/path/to/agent.toml \
./debian_build.sh
```

构建会校验两个密钥是否匹配，并将公钥打包到：

```text
/usr/share/keyrings/aico-ota.pem
```

### 已提交的信任锚

`keys/ota_pubkey.pem` 保存的是自动化构建所用签名密钥的公钥部分。公钥并不是机密：相同的字节已经
存在于每台设备的 `/usr/share/keyrings/aico-ota.pem` 以及每个已发布的镜像中。提交它可以让任何构建
都生成能够接受已发布更新的镜像。

不要在其中保留提及 dev、test 或 placeholder 的注释行。Debian rootfs 阶段会拒绝用带有此类注释的
密钥构建生产镜像。

要确认已提交的密钥与已发布镜像实际信任的密钥一致，可以从已发布的 `rootfs.img` 中提取它并比较
指纹（Linux）：

```bash
tar -xzf rootfs.img.tar.gz rootfs.img
debugfs -R "dump /usr/share/keyrings/aico-ota.pem published_pubkey.pem" rootfs.img
openssl pkey -pubin -in published_pubkey.pem -outform DER | sha256sum
openssl pkey -pubin -in keys/ota_pubkey.pem -outform DER | sha256sum
```

### 信任一个你无法代为签名的签名方

`/usr/share/keyrings/aico-ota.pem` 中的密钥决定设备接受谁的清单。私钥决定构建生成的清单由谁签名。
这是两个独立的角色，而校验清单只需要公钥部分，因此镜像可以接受在别处签名的更新，而该签名方的
私钥无需离开其签名环境。

Debian 构建按以下顺序确定信任锚：

1. `OTA_TRUST_PUBLIC_KEY_PATH`（如已设置）。
2. `keys/ota_pubkey.pem`，即上面已提交的签名方。
3. `OTA_PUBLIC_KEY_PATH`，此时镜像只信任它自己。

因此，不带额外参数的本地构建生成的镜像已经能接受已发布的更新，同时仍用本地密钥为自己的
`manifest.json` 签名。该本地清单无法用这个镜像校验通过，但这没有害处：设备上没有任何东西会读取
它。只要两个密钥不同，构建就会打印一条同时列出两个密钥的警告，因此这种情况绝不会悄无声息地发生。

签名环境必须把信任锚固定为自己的密钥，而不是继承已提交的那个，否则在密钥轮换时，发出的镜像会
信任旧的签名方，而清单却来自新的签名方：

```bash
OTA_PRIVATE_KEY_PATH=/path/to/ota_ed25519_private_key.pem \
OTA_PUBLIC_KEY_PATH=/path/to/ota_pubkey.pem \
OTA_TRUST_PUBLIC_KEY_PATH=/path/to/ota_pubkey.pem \
AGENT_CONFIG_PATH=/path/to/agent.toml \
./debian_build.sh
```

已经部署在现场的设备，可以通过将 `/usr/share/keyrings/aico-ota.pem` 替换为另一个签名方的公钥，
改为信任该签名方。这是下文恢复路径的手动形式，需要逐台设备操作。

## 本地签名

完整的 Debian 构建会生成并签名本地清单：

```bash
OTA_PRIVATE_KEY_PATH=ota_ed25519_private_key.pem \
OTA_PUBLIC_KEY_PATH=ota_pubkey.pem \
AGENT_CONFIG_PATH=/path/to/agent.toml \
OTA_BUILD_VERSION=20260521-120000-abcdef0 \
OTA_CHANNEL=stable \
OTA_BUILD_TIME=2026-05-21T12:00:00Z \
./debian_build.sh
```

构建会在 `output/debian/image` 中留下 `manifest.json` 和对应的 `.img.tar.gz` 资源。发布自动化
不在当前范围内；请原样发布这些文件，不要修改或重新压缩。

## 密钥轮换

V1 设备信任 `/usr/share/keyrings/aico-ota.pem`。在设备群接受新公钥之前，不要切换生产签名私钥，否则旧设备会拒绝用新私钥签名的清单。

安全的轮换流程：

1. 离线生成新的 Ed25519 密钥对。
2. 暂时在受控的签名环境中保留旧私钥。
3. 构建一个包含新 `/usr/share/keyrings/aico-ota.pem` 的过渡 OTA。
4. 用旧私钥签名并发布该过渡版本。
5. 通过 `ota status`、发布遥测或现场检查，确认目标设备已启动并标记成功。
6. 确认设备群信任新公钥后，将签名环境切换到新私钥。
7. 用新私钥签名后续发布。

USB 或工厂方式的密钥轮换是另一条路径：

1. 构建包含新公钥的完整镜像。
2. 通过 USB 恢复或工厂流程烧录。
3. 确认设备使用新公钥后，切换对应通道的签名密钥。

## 私钥泄露处理

如果 OTA 私钥可能已泄露：

1. 禁止访问所有包含该泄露密钥的签名环境。
2. 删除或隔离可能已用该泄露密钥签名的不可信发布和资源。
3. 离线生成新的 Ed25519 密钥对。
4. 构建包含新公钥的恢复镜像。
5. 对无法安全信任 OTA 的设备，优先使用 USB 或受控的物理恢复。
6. 如果必须使用 OTA，请发布经过人工审计的过渡版本，并使用 `ota status` 和 `abctl read` 监控。
7. 如果仓库访问 token 也可能已泄露，请单独轮换 `/userdata/ota/gh_token`。该 token 只用于私有仓库的发布下载，不参与清单签名；公开发布不需要配置 token。更多运行时路径参见 [architecture.md](architecture.md#私有仓库-token)。
