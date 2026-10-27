#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
STAGE_SCRIPT="$ROOT_DIR/scripts/stage_rootfs_cli_tools.sh"
CLEAN_SCRIPT="$ROOT_DIR/scripts/clean_rootfs_overlay_staging.sh"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

source_dir="$tmpdir/source"
dest_overlay="$tmpdir/dest-overlay"
preserve_overlay="$tmpdir/preserve-overlay"
catalog="$tmpdir/tools.catalog"
managed_state="$tmpdir/managed-tools.list"
mkdir -p "$source_dir" "$dest_overlay/etc"
mkdir -p "$preserve_overlay"
printf 'config\n' > "$dest_overlay/etc/aico_boot.conf"

cat > "$catalog" <<'EOF'
# name|version|kind|source|target|source_sha256|artifact_path|strip_policy
fq|v0.17.0|go|github.com/wader/fq@v0.17.0|linux/arm/v7|-|-|preserve
yq|v4.53.3|go|github.com/mikefarah/yq/v4@v4.53.3|linux/arm/v7|-|-|preserve
rg|15.2.0|tar_gz|https://example.com/rg.tar.gz|armv7-unknown-linux-musleabihf|aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa|rg/rg|preserve
fx|v1.0.0|go|example.com/fx@v1.0.0|linux/arm/v7|-|-|normal
EOF

make_fake_arm_elf() {
    output="$1"
    # Minimal ELF32 little-endian ARM header. The staging seam only needs to
    # distinguish target binaries from accidentally supplied host executables.
    printf '\177ELF\001\001\001\000\000\000\000\000\000\000\000\000\002\000\050\000\001\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\064\000\000\000\000\000\000\000\000\000\000\000\064\000\000\000\000\000\000\000' > "$output"
    chmod 755 "$output"
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

for tool in fq yq rg fx; do
    make_fake_arm_elf "$source_dir/$tool"
    printf '%s  %s\n' "$(sha256_of "$source_dir/$tool")" "$tool" >> "$source_dir/manifest.sha256"
done

if [ ! -x "$STAGE_SCRIPT" ]; then
    echo "missing executable rootfs CLI staging script: $STAGE_SCRIPT" >&2
    exit 1
fi

"$STAGE_SCRIPT" --catalog "$catalog" --managed-state "$managed_state" --source-dir "$source_dir" --dest-overlay "$dest_overlay"

for tool in fq yq rg fx; do
    staged="$dest_overlay/usr/bin/$tool"
    if [ ! -x "$staged" ]; then
        echo "staging must install executable /usr/bin/$tool" >&2
        exit 1
    fi
    if [ "$(sha256_of "$staged")" != "$(sha256_of "$source_dir/$tool")" ]; then
        echo "staging must preserve $tool content" >&2
        exit 1
    fi
done
if [ "$(paste -sd ' ' "$managed_state")" != "fq yq rg fx" ]; then
    echo "staging must persist the complete managed tool list after a successful install" >&2
    exit 1
fi

"$STAGE_SCRIPT" --catalog "$catalog" --policy preserve --source-dir "$source_dir" --dest-overlay "$preserve_overlay"
for tool in fq yq rg; do
    if [ ! -x "$preserve_overlay/usr/bin/$tool" ]; then
        echo "preserve staging must install protected tool: $tool" >&2
        exit 1
    fi
done
if [ -e "$preserve_overlay/usr/bin/fx" ]; then
    echo "preserve staging must not reinstall normal-strip tools" >&2
    exit 1
fi

printf 'corrupt\n' >> "$source_dir/fq"
if "$STAGE_SCRIPT" --catalog "$catalog" --source-dir "$source_dir" --dest-overlay "$dest_overlay" 2>"$tmpdir/checksum.err"; then
    echo "staging must reject a checksum mismatch" >&2
    exit 1
fi
if ! grep -q 'checksum verification failed' "$tmpdir/checksum.err"; then
    echo "checksum mismatch must have an actionable error" >&2
    exit 1
fi

make_fake_arm_elf "$source_dir/fq"
printf '#!/bin/sh\n' > "$source_dir/yq"
chmod 755 "$source_dir/yq"
: > "$source_dir/manifest.sha256"
for tool in fq yq rg fx; do
    printf '%s  %s\n' "$(sha256_of "$source_dir/$tool")" "$tool" >> "$source_dir/manifest.sha256"
done
if "$STAGE_SCRIPT" --catalog "$catalog" --source-dir "$source_dir" --dest-overlay "$dest_overlay" 2>"$tmpdir/arch.err"; then
    echo "staging must reject a non-ARM executable" >&2
    exit 1
fi
if ! grep -q 'not an ARM32 ELF executable' "$tmpdir/arch.err"; then
    echo "architecture mismatch must have an actionable error" >&2
    exit 1
fi

"$CLEAN_SCRIPT" --catalog "$catalog" --managed-state "$managed_state" --dest-overlay "$dest_overlay"
for tool in fq yq rg fx; do
    if [ -e "$dest_overlay/usr/bin/$tool" ]; then
        echo "cleanup must remove stale rootfs tool: $tool" >&2
        exit 1
    fi
done
if [ "$(cat "$dest_overlay/etc/aico_boot.conf")" != "config" ]; then
    echo "cleanup must preserve unrelated rootfs staging" >&2
    exit 1
fi

echo "rootfs CLI staging tests passed"
