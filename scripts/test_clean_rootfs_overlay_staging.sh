#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
CLEAN_SCRIPT="$ROOT_DIR/scripts/clean_rootfs_overlay_staging.sh"

if [ ! -x "$CLEAN_SCRIPT" ]; then
    echo "missing executable rootfs overlay cleanup script: $CLEAN_SCRIPT" >&2
    exit 1
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

dest_overlay="$tmpdir/dest-overlay"
catalog="$tmpdir/tools.catalog"
managed_state="$tmpdir/managed-tools.list"
mkdir -p "$dest_overlay/etc"
mkdir -p "$dest_overlay/usr/share/aico/skills/planner"
mkdir -p "$dest_overlay/usr/share/aico/skills/device-operator"
mkdir -p "$dest_overlay/usr/share/aico"

printf 'config\n' > "$dest_overlay/etc/aico_boot.conf"
printf 'actions\n' > "$dest_overlay/usr/share/aico/quick_actions.json"
printf 'planner\n' > "$dest_overlay/usr/share/aico/skills/planner/SKILL.md"
printf 'operator\n' > "$dest_overlay/usr/share/aico/skills/device-operator/SKILL.md"
mkdir -p "$dest_overlay/usr/bin"
printf 'tool\n' > "$dest_overlay/usr/bin/fx"
printf 'retired\n' > "$dest_overlay/usr/bin/retired-tool"
printf 'retired-tool\n' > "$managed_state"

cat > "$catalog" <<'EOF'
fx|v1.0.0|go|example.com/fx@v1.0.0|linux/arm/v7|-|-|normal
EOF

"$CLEAN_SCRIPT" --catalog "$catalog" --managed-state "$managed_state" --dest-overlay "$dest_overlay"

if [ "$(cat "$dest_overlay/usr/share/aico/quick_actions.json")" != actions ]; then
    echo "cleanup script must preserve Aico Link rootfs assets" >&2
    exit 1
fi

if [ "$(cat "$dest_overlay/etc/aico_boot.conf")" != "config" ]; then
    echo "cleanup script must preserve rootfs etc staging" >&2
    exit 1
fi
if [ -e "$dest_overlay/usr/bin/fx" ]; then
    echo "cleanup script must remove every tool declared by the catalog" >&2
    exit 1
fi
if [ -e "$dest_overlay/usr/bin/retired-tool" ]; then
    echo "cleanup script must remove tools recorded by the previous managed state" >&2
    exit 1
fi

if "$CLEAN_SCRIPT" --catalog "$catalog" --managed-state "$managed_state" --dest-overlay "$tmpdir/missing" 2>"$tmpdir/missing.err"; then
    echo "cleanup script must fail for a missing destination overlay" >&2
    exit 1
fi
if ! grep -q 'missing destination rootfs overlay' "$tmpdir/missing.err"; then
    echo "cleanup script must explain missing destination overlay errors" >&2
    exit 1
fi

echo "rootfs overlay cleanup tests passed"
