#!/bin/sh
# disk_speedtest.sh - sequential read/write speed test for board storage.
#
# Measures write (dd conv=fdatasync) and cache-dropped read throughput on each
# writable mount. It writes a temporary file on the mount and never writes raw
# block devices, so card/eMMC contents are untouched. By default it tests the
# onboard eMMC (/userdata) and the external TF/SD card (/mnt/sdcard).
#
# Designed to run ON THE BOARD, as root (read dropping needs privileges):
#   scp scripts/disk_speedtest.sh aiden@192.168.42.1:/tmp/
#   ssh aiden@192.168.42.1 'sudo sh /tmp/disk_speedtest.sh'
#
# Note: a card wired/configured for a 1-bit SD bus caps near ~3 MB/s regardless
# of the card; check `/sys/kernel/debug/mmcN/ios` (bus width) if speeds surprise.
# This reports sequential throughput only; random IOPS needs fio.
set -u

SIZE_MB="${SIZE_MB:-128}"
# Space-separated "label:mountpoint[:device]" entries; device is for display.
TARGETS="${TARGETS:-eMMC:/userdata:/dev/mmcblk0 TF-card:/mnt/sdcard:/dev/mmcblk1}"

usage() {
    cat <<'USAGE'
Usage: disk_speedtest.sh [options]

Options:
  --size MB         test file size in MiB (default: 128)
  --targets LIST    space-separated "label:mountpoint[:device]" entries
                    (default: eMMC:/userdata TF-card:/mnt/sdcard)
  -h, --help        show this help

Run as root. Writes a temp file on each mount (never raw devices) and removes
it afterwards. Reports sequential write (fdatasync) and cache-dropped read.
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --size)    [ "$#" -ge 2 ] || { echo "--size needs an argument" >&2; exit 2; }; SIZE_MB="$2"; shift 2 ;;
        --targets) [ "$#" -ge 2 ] || { echo "--targets needs an argument" >&2; exit 2; }; TARGETS="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *)         echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

case "$SIZE_MB" in ''|*[!0-9]*) echo "--size must be numeric" >&2; exit 2 ;; esac

drop_caches() { sync; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true; }

# parse dd stderr summary line "... N bytes ... copied, T s, R MB/s"
rate_of() { awk -F, '/copied|bytes/ {gsub(/^ +/,"",$NF); print $NF}'; }

for t in $TARGETS; do
    label=$(echo "$t" | cut -d: -f1)
    mnt=$(echo "$t" | cut -d: -f2)
    dev=$(echo "$t" | cut -d: -f3)
    echo "== $label ($mnt${dev:+, $dev}) =="
    if [ ! -d "$mnt" ]; then echo "  mount $mnt not present; skip"; continue; fi
    if ! touch "$mnt/.spd_probe" 2>/dev/null; then echo "  $mnt not writable; skip"; continue; fi
    rm -f "$mnt/.spd_probe"
    avail_kb=$(df -k "$mnt" | awk 'NR==2{print $4}')
    need_kb=$((SIZE_MB * 1024 + 51200))
    if [ "${avail_kb:-0}" -lt "$need_kb" ]; then
        echo "  not enough free space on $mnt (${avail_kb}KB avail, need ${need_kb}KB); skip"
        continue
    fi
    F="$mnt/.disk_speedtest.bin"
    printf '  write: '
    dd if=/dev/zero of="$F" bs=1M count="$SIZE_MB" conv=fdatasync 2>&1 | rate_of
    drop_caches
    printf '  read:  '
    dd if="$F" of=/dev/null bs=1M 2>&1 | rate_of
    rm -f "$F"
done
echo "done"
