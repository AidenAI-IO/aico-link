---
sidebar_position: 8
---

# OTA Distribution Quick Examples

All examples use the Debian production entrypoint. It builds and audits the
complete firmware, signs `manifest.json`, and writes local artifacts to
`output/debian/image/`. The current distribution path is a local or
self-hosted HTTP(S) endpoint; the build does not publish to GitHub.

## Example 1: Local Development

Serve the generated files from a host that the device can reach:

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

cd output/debian/image
python3 -m http.server 8000 --bind 0.0.0.0
```

On the device, verify downloads and signatures without switching slots:

```bash
ota update \
  --manifest-url "http://192.168.1.100:8000/manifest.json" \
  --public-key /userdata/ota/dev_pubkey.pem \
  --dry-run
```

## Example 2: Self-Hosted HTTP(S)

Build with the final static directory URL embedded in the signed manifest,
then copy the exact archives and manifest without renaming or recompressing:

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

## Example 3: Custom GitHub Release

Official dev/staging/prod releases use the [manual channel workflow](channel-release.md).
This example publishes to a separate custom repository and uses a test device
configured for that custom source.

```bash
TAG="v1.0.0-custom"
REPO="YOUR_USERNAME/aico-link-firmware"

OTA_PRIVATE_KEY_PATH="$PWD/ota_private_key.pem" \
OTA_PUBLIC_KEY_PATH="$PWD/ota_public_key.pem" \
AGENT_CONFIG_PATH="$PWD/agent.toml" \
OTA_REPO="$REPO" \
OTA_CHANNEL=custom \
OTA_BUILD_VERSION="$TAG" \
OTA_BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
./debian_build.sh

gh release create "$TAG" \
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

## Security Notes

1. Keep the private key and `agent.toml` outside the repository.
2. Use HTTPS outside an isolated development network.
3. Distribute and verify the public key through a trusted channel.
4. Publish the exact manifest and archives produced by the same build; changing
   any asset after signing makes verification fail.
