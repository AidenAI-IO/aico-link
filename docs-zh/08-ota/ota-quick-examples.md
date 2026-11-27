---
sidebar_position: 8
---

# OTA 分发快速示例

所有示例都使用 Debian 生产入口。它会构建并审计完整固件，为 `manifest.json` 签名，并将本地产物
写入 `output/debian/image/`。当前的分发路径是本地或自托管的 HTTP(S) 端点；构建不会发布到
GitHub。

请使用未配置为托管通道（`dev`、`staging` 或 `prod`）的测试设备。托管设备只接受为其自身通道签名、
版本号为 `<channel>-v<semver>` 格式的清单，即使通过 `--manifest-url` 也是如此，因此会以
`signed manifest does not belong to configured channel` 拒绝这些示例。参见
[面向外部开发者的 OTA](ota-external-developers.md)。

## 示例 1：本地开发

在设备可以访问的主机上提供生成的文件。服务器在子 shell 中后台运行，因此后续步骤仍在仓库根目录下执行：

```bash
VERSION="dev-$(date +%Y%m%d-%H%M%S)"
BASE_URL="http://192.168.1.100:8000"

openssl genpkey -algorithm ed25519 -out ota_private_key.pem
openssl pkey -in ota_private_key.pem -pubout -out ota_public_key.pem

OTA_PRIVATE_KEY_PATH="$PWD/ota_private_key.pem" \
OTA_PUBLIC_KEY_PATH="$PWD/ota_public_key.pem" \
AGENT_CONFIG_PATH="$PWD/agent.toml" \
OTA_CHANNEL=dev \
OTA_BUILD_VERSION="$VERSION" \
OTA_BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
OTA_BASE_URL="$BASE_URL" \
./debian_build.sh

(cd output/debian/image && python3 -m http.server 8000 --bind 0.0.0.0) &
SERVER_PID=$!
```

将公钥复制到设备：

```bash
DEVICE_IP="192.168.42.1"
scp ota_public_key.pem "root@$DEVICE_IP:/userdata/ota/dev_pubkey.pem"
```

在设备上验证下载和签名，但不切换槽位：

```bash
ota update \
  --manifest-url "http://192.168.1.100:8000/manifest.json" \
  --public-key /userdata/ota/dev_pubkey.pem \
  --dry-run
```

在 dry run 完成之前保持服务器运行，完成后在主机上用 `kill "$SERVER_PID"` 停止它。

## 示例 2：自托管 HTTP(S)

构建时将最终的静态目录 URL 嵌入签名清单，然后原样复制归档和清单，不要重命名或重新压缩：

```bash
VERSION="v1.0.0"
BASE_URL="https://firmware.example.com/aico/$VERSION"

OTA_PRIVATE_KEY_PATH="$PWD/ota_private_key.pem" \
OTA_PUBLIC_KEY_PATH="$PWD/ota_public_key.pem" \
AGENT_CONFIG_PATH="$PWD/agent.toml" \
OTA_CHANNEL=internal \
OTA_BUILD_VERSION="$VERSION" \
OTA_BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
OTA_BASE_URL="$BASE_URL" \
./debian_build.sh

rsync -avz \
  output/debian/image/*.img.tar.gz \
  output/debian/image/manifest.json \
  user@firmware.example.com:/var/www/firmware/aico/$VERSION/

ota update \
  --manifest-url "$BASE_URL/manifest.json" \
  --public-key /userdata/ota/company_pubkey.pem
```

## 示例 3：自定义 GitHub Release

官方的 dev/staging/prod 发布使用 [通道发布工作流](channel-release.md)。本示例发布到一个单独的
自定义仓库，并使用为该自定义源配置的测试设备。

```bash
TAG="v1.0.0-custom"
REPO="YOUR_USERNAME/aico-link-firmware"

OTA_PRIVATE_KEY_PATH="$PWD/ota_private_key.pem" \
OTA_PUBLIC_KEY_PATH="$PWD/ota_public_key.pem" \
AGENT_CONFIG_PATH="$PWD/agent.toml" \
OTA_REPO="$REPO" \
OTA_BASE_URL="https://github.com/$REPO/releases/download/$TAG" \
OTA_CHANNEL=custom \
OTA_BUILD_VERSION="$TAG" \
OTA_BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
./debian_build.sh

gh release create "$TAG" \
  --repo "$REPO" \
  --title "Custom Firmware $TAG" \
  output/debian/image/boot_a.img.tar.gz \
  output/debian/image/boot_b.img.tar.gz \
  output/debian/image/rootfs.img.tar.gz \
  output/debian/image/update.img.tar.gz \
  output/debian/image/manifest.json

ota update \
  --manifest-url "https://github.com/$REPO/releases/download/$TAG/manifest.json" \
  --public-key /userdata/ota/custom_pubkey.pem
```

## 安全提示

1. 将私钥和 `agent.toml` 保存在仓库之外。
2. 在隔离的开发网络之外使用 HTTPS。
3. 通过可信渠道分发和验证公钥。
4. 发布同一次构建生成的原始清单和归档；签名后修改任何资源都会导致校验失败。
