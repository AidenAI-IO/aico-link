#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_CATALOG="$SCRIPT_DIR/rootfs_cli_tools.catalog"
CATALOG_LIB="$SCRIPT_DIR/rootfs_cli_tool_catalog.sh"

# shellcheck source=/dev/null
source "$CATALOG_LIB"

usage() {
  cat >&2 <<'USAGE'
Usage:
  clean_rootfs_overlay_staging.sh [--catalog FILE] [--managed-state FILE]
                                  --dest-overlay DIR

Remove stale generated files from the pico-sdk rootfs overlay staging tree.
USAGE
}

log() {
  printf '%s\n' "$*" >&2
}

die() {
  log "clean_rootfs_overlay_staging.sh: $*"
  exit 1
}

catalog_path="$DEFAULT_CATALOG"
managed_state=""
dest_overlay=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --catalog)
      catalog_path="${2:-}"
      shift 2
      ;;
    --dest-overlay)
      dest_overlay="${2:-}"
      shift 2
      ;;
    --managed-state)
      managed_state="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      die "unknown argument: $1"
      ;;
  esac
done

[ -n "$catalog_path" ] || die "missing --catalog value"
[ -n "$dest_overlay" ] || die "missing --dest-overlay"
[ -d "$dest_overlay" ] || die "missing destination rootfs overlay: $dest_overlay"
catalog_names="$(rootfs_cli_catalog_names "$catalog_path")" || exit 1

remove_stale_path() {
  local relative_path="$1"
  local path="$dest_overlay/$relative_path"

  case "$relative_path" in
    ""|/*|*../*) die "invalid stale rootfs overlay path: $relative_path" ;;
  esac

  if [ -e "$path" ] || [ -L "$path" ]; then
    rm -rf -- "$path"
    log "Removed stale rootfs overlay path: $relative_path"
  fi
}

# Aico Link platform share assets are now part of rootfs and must be preserved.

# Rootfs CLI tools are generated for each image build. Remove stale binaries
# from both the previous successful staging run and the current catalog before
# staging the freshly verified bundle.
if [ -n "$managed_state" ] && [ -f "$managed_state" ]; then
  while IFS= read -r name; do
    case "$name" in
      "") continue ;;
      [!A-Za-z0-9]*|*[!A-Za-z0-9._+-]*) die "invalid managed rootfs CLI tool name: $name" ;;
    esac
    remove_stale_path "usr/bin/$name"
  done < "$managed_state"
fi
while IFS= read -r name; do
  remove_stale_path "usr/bin/$name"
done <<< "$catalog_names"
