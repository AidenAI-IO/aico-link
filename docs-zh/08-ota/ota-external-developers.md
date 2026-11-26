---
sidebar_position: 7
---

# 面向外部开发者的 OTA

外部分发方使用与官方发布相同的 Debian 构建和镜像审计。只要提供签名清单以及匹配的受信任 Ed25519
公钥，设备就可以从任意 HTTP(S) 源安装更新。

## 构建自定义发布

生成签名密钥对，并将私钥保存在仓库之外：

```bash
openssl genpkey -algorithm ed25519 -out ota_private_key.pem
openssl pkey -in ota_private_key.pem -pubout -out ota_public_key.pem
scripts/validate_ota_pubkey.sh ota_public_key.pem
```

构建完整的 Debian 固件和签名 OTA 资源集合：

```bash
VERSION="v1.0.0-custom"
CHANNEL="custom"
BASE_URL="https://firmware.example.com/aico/$VERSION"

OTA_PRIVATE_KEY_PATH="$PWD/ota_private_key.pem" \
OTA_PUBLIC_KEY_PATH="$PWD/ota_public_key.pem" \
AGENT_CONFIG_PATH="$PWD/agent.toml" \
OTA_CHANNEL="$CHANNEL" \
OTA_BUILD_VERSION="$VERSION" \
OTA_BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
OTA_BASE_URL="$BASE_URL" \
./debian_build.sh
```

`OTA_BASE_URL` 为直接静态托管固定资源 URL。对于受管的 GitHub dev/staging/prod 发布，请使用
[通道发布工作流](channel-release.md)，它会根据发布计划同时设置仓库和不可变的资源 URL。

可发布的目录包含：

```text
output/debian/image/
├── boot_a.img.tar.gz
├── boot_b.img.tar.gz
├── manifest.json
├── rootfs.img.tar.gz
└── update.img.tar.gz
```

该目录还包含可直接烧录的本地 `update.img` 及其校验和。发布白名单有意只发布压缩镜像，而不发布这份
额外的本地副本。

## 托管资源

### 自定义分发方的 GitHub Releases

官方受管通道使用通道工作流。独立的分发方可以手动发布自定义标签，并通过显式的清单 URL 更新：

```bash
TAG="$VERSION"
REPO="YOUR_USERNAME/aico-link-firmware"

gh release create "$TAG" \
  --title "Custom Firmware $TAG" \
  output/debian/image/boot_a.img.tar.gz \
  output/debian/image/boot_b.img.tar.gz \
  output/debian/image/rootfs.img.tar.gz \
  output/debian/image/update.img.tar.gz \
  output/debian/image/manifest.json
```

### 静态 Web 服务器或 CDN

构建时将 `OTA_BASE_URL` 设为最终的目录 URL，然后原样上传，不要重命名或重新压缩任何文件：

```bash
rsync -avz \
  output/debian/image/*.img.tar.gz \
  output/debian/image/manifest.json \
  user@firmware.example.com:/var/www/firmware/aico/$VERSION/
```

本地测试时，可以直接提供同一个目录：

```bash
cd output/debian/image
python3 -m http.server 8000
```

## 信任与更新

通过可信渠道将公钥复制到设备：

```bash
DEVICE_IP="192.168.42.1"
scp ota_public_key.pem "root@$DEVICE_IP:/userdata/ota/custom_pubkey.pem"
```

使用直接的清单 URL 运行更新：

```bash
ota update \
  --manifest-url "https://firmware.example.com/aico/v1.0.0-custom/manifest.json" \
  --public-key /userdata/ota/custom_pubkey.pem
```

添加 `--dry-run` 可下载并校验所有资源，而不写入分区或切换槽位。

## 持久化设备配置

Debian 出厂配置位于 `/userdata/debian/ota/config.json`。要将设备固定到自定义源，请添加
`manifest_url` 和 `public_key_path`，同时保留生成的出厂版本、构建时间、安全余量和分区哈希：

```json
{
  "manifest_url": "https://firmware.example.com/aico/latest/manifest.json",
  "public_key_path": "/userdata/ota/custom_pubkey.pem"
}
```

不要用只包含这两个字段的内容替换整个生成的文件。降级检查和非活动槽位校验都需要出厂基线。

## 清单规则

生成的清单是签名的规范化 JSON。每个 `parts` 条目命名为 `boot` 或 `rootfs`，并包含与槽位无关的
`asset`，或槽位专用的 `asset_a` 和 `asset_b`。

发布的资源是 `.img.tar.gz` 文件。对于每个资源：

- `size` 和 `sha256` 描述下载的归档；
- `image_sha256` 描述解压后写入分区的镜像；
- 提供了 `OTA_BASE_URL` 时会包含 `url`；
- `boot` 仍为槽位专用，因为其 DTB 带有槽位专用的 bootargs。

如果目标槽位的下载集合超出由专用 OTA 分区推导出的容量，构建会拒绝。

## 通道与版本策略

通道名称必须匹配 `[A-Za-z0-9._-]`。受管的 dev/staging/prod 设备会强制要求签名中的通道与其配置
一致，即使使用直接清单 URL 也是如此。请使用配置了自定义通道/源的测试设备运行这些自定义示例。签名
密钥仍是信任边界；通道名称不能替代签名校验。

请使用单调递增的构建时间和可追溯的版本，例如 UTC 时间戳加 Git 提交。OTA 会拒绝更旧的构建时间，
以及构建时间相同但版本冲突的发布。

## 安全要求

- 永远不要提交签名私钥或生产 `agent.toml`。
- 生产分发使用 HTTPS。
- 通过另一个可信渠道校验公钥指纹。
- `manifest.json` 签名后，永远不要修改、重命名或重新压缩资源。
- 将不同的分发主体视为不同的签名密钥。

## 故障排查

检查 Debian 启动时的健康单元和日志：

```bash
systemctl status aico-ota-health.service
journalctl -u aico-ota-health.service
tail -f /var/log/ota/ota.log
```

常用检查：

```bash
ota status
ota update --manifest-url URL --public-key KEY --dry-run
ota verify-manifest /path/to/manifest.json --public-key /path/to/pubkey.pem
```

常见失败：

- `manifest signature verification failed`：密钥错误、清单被修改，或私钥与公钥不匹配。
- `invalid channel`：通道名称包含不支持的字符。
- `missing required release asset`：托管的文件名与签名清单不一致，或 GitHub Release 没有包含全部
  六个白名单资源。
- 归档或镜像校验和不匹配：签名后有资源被修改。
