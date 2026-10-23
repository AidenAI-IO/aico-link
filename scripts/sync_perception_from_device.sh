#!/bin/bash
# Sync perception_v1.json + screenshots/ from a device back to the repo.
#
# Usage:
#   scripts/sync_perception_from_device.sh <device_ip>
#
# Example:
#   scripts/sync_perception_from_device.sh 192.168.31.107
#
# What it does:
#   1. Diffs the device's /userdata/agent/benchmark/suites/perception/ vs.
#      this repo's benchmark/suites/perception/ — shows what would change.
#   2. Asks for confirmation.
#   3. scp's perception_v1.json + screenshots/ from device to repo.
#   4. Stages the diff in git (does NOT commit — review first).

set -euo pipefail

if [ $# -lt 1 ]; then
    echo "Usage: $0 <device_ip>" >&2
    echo "Example: $0 192.168.31.107" >&2
    exit 1
fi

DEVICE="$1"
DEVICE_USER="${DEVICE_USER:-root}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOCAL_DIR="$REPO_ROOT/benchmark/suites/perception"
DEVICE_DIR="/userdata/agent/benchmark/suites/perception"

if [ ! -d "$LOCAL_DIR" ]; then
    echo "Error: $LOCAL_DIR not found. Are you in the aico-link-firmware repo?" >&2
    exit 1
fi

SSH_OPTS=(-o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
SSH="ssh ${SSH_OPTS[*]} ${DEVICE_USER}@${DEVICE}"

echo "→ Connecting to ${DEVICE_USER}@${DEVICE} ..."
if ! $SSH "test -d ${DEVICE_DIR}" 2>/dev/null; then
    echo "Error: ${DEVICE_DIR} not found on device" >&2
    exit 1
fi

echo "→ Comparing device vs. local..."
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

scp -r "${SSH_OPTS[@]}" "${DEVICE_USER}@${DEVICE}:${DEVICE_DIR}/" "$TMP_DIR/perception/" >/dev/null

if diff -qr "$LOCAL_DIR" "$TMP_DIR/perception" >/dev/null 2>&1; then
    echo "✓ Already in sync. Nothing to do."
    exit 0
fi

echo ""
echo "Changes to apply:"
echo "──────────────────────────────────────────────────────"
diff -qr "$LOCAL_DIR" "$TMP_DIR/perception" || true
echo "──────────────────────────────────────────────────────"
echo ""
read -r -p "Apply these changes? [y/N] " ans
case "$ans" in
    [yY]|[yY][eE][sS]) ;;
    *) echo "Aborted."; exit 0;;
esac

# rsync would be cleaner but devices may not have it; cp -R works.
rm -rf "$LOCAL_DIR"
cp -R "$TMP_DIR/perception" "$LOCAL_DIR"

echo ""
echo "✓ Synced to $LOCAL_DIR"
echo ""
echo "Staged in git (review and commit):"
git -C "$REPO_ROOT" add "$LOCAL_DIR"
git -C "$REPO_ROOT" status --short -- "benchmark/suites/perception"
echo ""
echo "Next: git -C $REPO_ROOT commit -m \"feat(benchmark): add perception tasks from device\""
