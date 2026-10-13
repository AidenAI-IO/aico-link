#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
readonly HELPER=${REPO_ROOT}/overlay-debian/usr/lib/aiden/aiden-rootfs-grow
TEST_ROOT=$(mktemp -d)
readonly TEST_ROOT
cleanup() {
    local status=$?
    if [ "${status}" -ne 0 ] && [ -f "${TEST_ROOT}/output" ]; then
        cat "${TEST_ROOT}/output" >&2
    fi
    rm -rf "${TEST_ROOT}"
}
trap cleanup EXIT

fail() {
    echo "Debian rootfs growth test failure: $*" >&2
    exit 1
}

export GROW_TEST_ROOT=${TEST_ROOT}
GROW_REAL_DUMPE2FS=$(command -v dumpe2fs)
GROW_REAL_RESIZE2FS=$(command -v resize2fs)
GROW_REAL_E2FSCK=$(command -v e2fsck)
GROW_REAL_READLINK=$(command -v readlink)
export GROW_REAL_DUMPE2FS GROW_REAL_RESIZE2FS GROW_REAL_E2FSCK GROW_REAL_READLINK
mkdir -p "${TEST_ROOT}/bin"

# Use real ext4 images and e2fsprogs. Only the board's device discovery, mount
# operations and block-device size query are replaced; no host mounts are used.
cat >"${TEST_ROOT}/device-map" <<'EOF'
image_for_device() {
    case "$1" in
        /dev/mmcblk0p8) printf '%s/rootfs.img\n' "${GROW_TEST_ROOT}" ;;
        /dev/disk/by-partlabel/userdata) printf '%s/userdata.img\n' "${GROW_TEST_ROOT}" ;;
        /dev/disk/by-partlabel/ota) printf '%s/ota.img\n' "${GROW_TEST_ROOT}" ;;
        *) echo "Unexpected test device: $1" >&2; exit 1 ;;
    esac
}
EOF
cat >"${TEST_ROOT}/bin/readlink" <<'EOF'
#!/bin/sh
set -eu
case "$2" in
    /sys/dev/block/179:8|/dev/disk/by-partlabel/rootfs_b) echo /dev/mmcblk0p8 ;;
    *) exec "${GROW_REAL_READLINK}" "$@" ;;
esac
EOF
cat >"${TEST_ROOT}/bin/findmnt" <<'EOF'
#!/bin/sh
set -eu
case "$*" in
    '-nr -o MAJ:MIN --target /') echo 179:8 ;;
    '-rn -S /dev/disk/by-partlabel/userdata')
        [ "${GROW_TEST_MOUNTED:-}" = userdata ] || exit 1
        echo /userdata
        ;;
    '-rn -S /dev/disk/by-partlabel/ota')
        [ "${GROW_TEST_MOUNTED:-}" = ota ] || exit 1
        echo /userdata/ota
        ;;
    *) echo "Unexpected findmnt arguments: $*" >&2; exit 1 ;;
esac
EOF
cat >"${TEST_ROOT}/bin/mount" <<'EOF'
#!/bin/sh
set -eu
[ "$*" = '-o remount,rw /' ]
printf 'mount %s\n' "$*" >>"${GROW_TEST_ROOT}/calls"
EOF
cat >"${TEST_ROOT}/bin/blockdev" <<'EOF'
#!/bin/sh
set -eu
. "${GROW_TEST_ROOT}/device-map"
[ "$1" = --getsize64 ]
stat -c %s "$(image_for_device "$2")"
EOF
cat >"${TEST_ROOT}/bin/dumpe2fs" <<'EOF'
#!/bin/sh
set -eu
. "${GROW_TEST_ROOT}/device-map"
[ "$1" = -h ]
exec "${GROW_REAL_DUMPE2FS}" -h "$(image_for_device "$2")"
EOF
cat >"${TEST_ROOT}/bin/resize2fs" <<'EOF'
#!/bin/sh
set -eu
. "${GROW_TEST_ROOT}/device-map"
printf 'resize2fs %s\n' "$*" >>"${GROW_TEST_ROOT}/calls"
if [ "${GROW_TEST_RESIZE_FAIL:-}" = "$1" ]; then exit 1; fi
exec "${GROW_REAL_RESIZE2FS}" "$(image_for_device "$1")"
EOF
cat >"${TEST_ROOT}/bin/e2fsck" <<'EOF'
#!/bin/sh
set -eu
. "${GROW_TEST_ROOT}/device-map"
printf 'e2fsck %s\n' "$*" >>"${GROW_TEST_ROOT}/calls"
[ "$1" = -f ] && [ "$2" = -p ]
# A repair must never run against a filesystem reported as mounted.
if [ -n "${GROW_TEST_MOUNTED:-}" ] && [ "$3" = "/dev/disk/by-partlabel/${GROW_TEST_MOUNTED}" ]; then
    echo "Attempted offline fsck of mounted ${GROW_TEST_MOUNTED}" >&2
    exit 99
fi
if [ -n "${GROW_TEST_FSCK_STATUS:-}" ]; then exit "${GROW_TEST_FSCK_STATUS}"; fi
status=0
"${GROW_REAL_E2FSCK}" -f -p "$(image_for_device "$3")" || status=$?
if [ "${status}" -le 1 ] && [ "${GROW_TEST_CORRECTED:-0}" = 1 ]; then status=1; fi
exit "${status}"
EOF
chmod +x "${TEST_ROOT}/bin/"*
export PATH="${TEST_ROOT}/bin:${PATH}"
export AIDEN_SLOT_ENV=${TEST_ROOT}/slot.env
export AIDEN_ROOTFS_GROW_MARKER=${TEST_ROOT}/rootfs-grown
printf 'AIDEN_SLOT_SUFFIX=_b\n' >"${AIDEN_SLOT_ENV}"

make_image() {
    local name=$1 initial_mb=$2 partition_mb=$3 block_size=$4
    truncate -s "${initial_mb}M" "${TEST_ROOT}/${name}.img"
    mke2fs -q -F -t ext4 -b "${block_size}" "${TEST_ROOT}/${name}.img"
    truncate -s "${partition_mb}M" "${TEST_ROOT}/${name}.img"
}

prepare_case() {
    rm -f "${AIDEN_ROOTFS_GROW_MARKER}" "${TEST_ROOT}/rootfs-grown.tmp."*
    : >"${TEST_ROOT}/calls"
    unset GROW_TEST_MOUNTED GROW_TEST_FSCK_STATUS GROW_TEST_CORRECTED GROW_TEST_RESIZE_FAIL
    make_image rootfs 16 32 4096
    make_image userdata 32 32 4096
    make_image ota 16 16 1024
}

mark_previously_mounted() {
    # resize2fs requires fsck when last mount is newer than last check, including
    # for a clean filesystem whose geometry already matches its partition.
    local image=$1 mount_time geometry recorded_mount_time
    mount_time=$(( $(date +%s) + 60 ))
    # The @ prefix makes debugfs interpret the value as Unix epoch seconds.
    if ! debugfs -w -R "set_super_value mtime @${mount_time}" "${image}" \
        >"${TEST_ROOT}/debugfs-output" 2>&1; then
        cat "${TEST_ROOT}/debugfs-output" >&2
        fail "could not set the last-mount timestamp for ${image}"
    fi
    # debugfs can report command errors with status 0, so check the stored value.
    geometry=$(LC_ALL=C TZ=UTC "${GROW_REAL_DUMPE2FS}" -h "${image}" 2>/dev/null)
    recorded_mount_time=$(printf '%s\n' "${geometry}" | sed -n 's/^Last mount time:[[:space:]]*//p')
    [ "$(LC_ALL=C TZ=UTC date -d "${recorded_mount_time}" +%s)" -eq "${mount_time}" ] \
        || fail "last-mount timestamp was not written to ${image}"
}

assert_full() {
    local image=$1 geometry blocks block_size
    geometry=$("${GROW_REAL_DUMPE2FS}" -h "${image}" 2>/dev/null)
    blocks=$(printf '%s\n' "${geometry}" | awk '/^Block count:/ { print $3 }')
    block_size=$(printf '%s\n' "${geometry}" | awk '/^Block size:/ { print $3 }')
    [ "$((blocks * block_size))" -eq "$(stat -c %s "${image}")" ] \
        || fail "filesystem does not fill ${image}"
}

run_helper() {
    sh "${HELPER}" >"${TEST_ROOT}/output" 2>&1
}

prepare_case
make_image userdata 3072 3072 4096
mark_previously_mounted "${TEST_ROOT}/userdata.img"
mark_previously_mounted "${TEST_ROOT}/ota.img"
legacy_status=0
"${GROW_REAL_RESIZE2FS}" "${TEST_ROOT}/userdata.img" >"${TEST_ROOT}/legacy-output" 2>&1 || legacy_status=$?
[ "${legacy_status}" -eq 1 ] || fail "previously used userdata did not reproduce the original failure"
grep -q "Please run 'e2fsck -f" "${TEST_ROOT}/legacy-output"
run_helper
assert_full "${TEST_ROOT}/rootfs.img"
[ -f "${AIDEN_ROOTFS_GROW_MARKER}" ] || fail "rootfs OTA with preserved userdata did not complete"
grep -qx 'slot=_b' "${AIDEN_ROOTFS_GROW_MARKER}"
[ "$(grep -c '^resize2fs ' "${TEST_ROOT}/calls")" -eq 1 ] \
    || fail "already-full data filesystems were resized"
if grep -q '^e2fsck ' "${TEST_ROOT}/calls"; then fail "already-full data filesystems were checked"; fi

# The completion marker skips growth, while every boot still remounts root rw.
: >"${TEST_ROOT}/calls"
run_helper
grep -qx 'mount -o remount,rw /' "${TEST_ROOT}/calls"
[ "$(wc -l <"${TEST_ROOT}/calls")" -eq 1 ] || fail "a completed slot repeated growth"

prepare_case
make_image userdata 16 32 4096
make_image ota 16 32 1024
run_helper
assert_full "${TEST_ROOT}/userdata.img"
assert_full "${TEST_ROOT}/ota.img"
[ "$(grep -c '^e2fsck ' "${TEST_ROOT}/calls")" -eq 2 ] || fail "offline growth did not check both data filesystems"
[ -f "${AIDEN_ROOTFS_GROW_MARKER}" ] || fail "fresh data growth did not complete"

prepare_case
make_image userdata 16 32 4096
mark_previously_mounted "${TEST_ROOT}/userdata.img"
export GROW_TEST_CORRECTED=1
run_helper
assert_full "${TEST_ROOT}/userdata.img"
[ -f "${AIDEN_ROOTFS_GROW_MARKER}" ] || fail "corrected fsck status prevented growth"

# These cases verify the no-offline-fsck branch for devices reported as mounted.
# resize2fs still operates on image files here; real online growth needs a mount.
for mounted_device in userdata ota; do
    prepare_case
    case "${mounted_device}" in
        userdata) make_image userdata 16 32 4096 ;;
        ota) make_image ota 16 32 1024 ;;
    esac
    export GROW_TEST_MOUNTED=${mounted_device}
    run_helper
    assert_full "${TEST_ROOT}/${mounted_device}.img"
    if grep -q '^e2fsck ' "${TEST_ROOT}/calls"; then fail "mounted ${mounted_device} was checked offline"; fi
done

for fsck_status in 2 4 8; do
    prepare_case
    make_image userdata 16 32 4096
    export GROW_TEST_FSCK_STATUS=${fsck_status}
    status=0
    run_helper || status=$?
    [ "${status}" -eq "${fsck_status}" ] || fail "fsck status ${fsck_status} was not propagated"
    [ ! -e "${AIDEN_ROOTFS_GROW_MARKER}" ] || fail "failed fsck wrote a completion marker"
    if grep -q '^resize2fs /dev/disk/' "${TEST_ROOT}/calls"; then fail "data was resized after failed fsck"; fi
done

prepare_case
make_image userdata 16 32 4096
export GROW_TEST_RESIZE_FAIL=/dev/disk/by-partlabel/userdata
if run_helper; then fail "failed resize was accepted"; fi
[ ! -e "${AIDEN_ROOTFS_GROW_MARKER}" ] || fail "failed resize wrote a completion marker"

prepare_case
truncate -s 16M "${TEST_ROOT}/userdata.img"
if run_helper; then fail "a filesystem larger than its partition was accepted"; fi
grep -q 'larger than its partition' "${TEST_ROOT}/output"
[ ! -e "${AIDEN_ROOTFS_GROW_MARKER}" ] || fail "invalid geometry wrote a completion marker"
if grep -q '^e2fsck ' "${TEST_ROOT}/calls"; then fail "invalid geometry was repaired"; fi

prepare_case
truncate -s 0 "${TEST_ROOT}/userdata.img"
truncate -s 32M "${TEST_ROOT}/userdata.img"
if run_helper; then fail "unreadable ext4 metadata was accepted"; fi
grep -q 'Cannot read ext4 geometry' "${TEST_ROOT}/output"
[ ! -e "${AIDEN_ROOTFS_GROW_MARKER}" ] || fail "unreadable ext4 metadata wrote a completion marker"

echo "Debian rootfs growth tests passed"
