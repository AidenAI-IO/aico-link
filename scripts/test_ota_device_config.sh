#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GENERATOR="$ROOT_DIR/scripts/generate_ota_device_config.sh"

if [ ! -f "$GENERATOR" ]; then
    echo "missing $GENERATOR" >&2
    exit 1
fi

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

cat > "$TMP_DIR/manifest.json" <<'JSON'
{
  "schema_version": 2,
  "channel": "stable",
  "version": "20260523-120000-abcdef0",
  "build_time": "2026-05-23T12:00:00Z",
  "parts": [
    {"name":"boot","asset_a":{"name":"boot_a.img","size":1,"sha256":"boot-a-hash"},"asset_b":{"name":"boot_b.img.tar.gz","size":1,"sha256":"boot-b-archive-hash","image_sha256":"boot-b-image-hash"}},
    {"name":"rootfs","asset":{"name":"rootfs.img.tar.gz","size":1,"sha256":"rootfs-archive-hash","image_sha256":"rootfs-image-hash"}}
  ],
  "signature": {"algorithm":"ed25519","value":"unused"}
}
JSON

bash "$GENERATOR" \
    --manifest "$TMP_DIR/manifest.json" \
    --repo AidenAI-IO/aiden-firmware \
    --channel stable \
    --output "$TMP_DIR/config.json"

jq -e '
  .repo == "AidenAI-IO/aiden-firmware" and
  .channel == "stable" and
  .download_safety_margin_bytes == 16777216 and
  (has("storage_mount_point") | not) and
  (has("storage_device_path") | not) and
  (has("storage_filesystem") | not) and
  (has("reserve_size_bytes") | not) and
  (has("reserve_safety_margin_bytes") | not) and
  .factory_version == "20260523-120000-abcdef0" and
  .factory_build_time == "2026-05-23T12:00:00Z" and
  .factory_partition_hashes.a.boot == "boot-a-hash" and
  .factory_partition_hashes.a.rootfs == "rootfs-image-hash" and
  .factory_partition_hashes.b.boot == "boot-b-image-hash" and
  .factory_partition_hashes.b.rootfs == "rootfs-image-hash"
' "$TMP_DIR/config.json" >/dev/null

for repo in \
    AicoAI-IO \
    AidenAI-IO/aiden-firmware/extra \
    /AidenAI-IO/aiden-firmware \
    AicoAI-IO/ \
    AicoAI-IO//aico-link-firmware; do
    if bash "$GENERATOR" \
        --manifest "$TMP_DIR/manifest.json" \
        --repo "$repo" \
        --channel stable \
        --output "$TMP_DIR/invalid-config.json" >/dev/null 2>&1; then
        echo "accepted invalid repo: $repo" >&2
        exit 1
    fi
done

echo "OTA device config generation tests passed"

for filter in '.schema_version = 1' '.parts += [{"name":"oem"}]' '.parts |= map(select(.name == "boot"))'; do
    jq "$filter" "$TMP_DIR/manifest.json" >"$TMP_DIR/invalid-manifest.json"
    if bash "$GENERATOR" --manifest "$TMP_DIR/invalid-manifest.json" --repo owner/repo --channel stable --output "$TMP_DIR/rejected.json" >/dev/null 2>&1; then
        echo "accepted incompatible factory manifest: $filter" >&2
        exit 1
    fi
done
