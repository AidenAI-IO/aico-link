#!/bin/sh
# wifi6_test.sh - WiFi 6 (802.11ax / HE) verification and throughput test for
# the Aico board (AIC8800D80).
#
# Runs the full WiFi 6 check used during bring-up:
#   1. HE switch state      - AICO_WIFI_HE in /etc/aico_boot.conf + driver he_on
#   2. Link verification    - associated, 5 GHz, negotiated HE-MCS rate, signal
#   3. Latency / packet loss - ping gateway (pure WiFi) and an external host
#   4. Throughput           - multi-stream HTTP download from a LAN peer
#
# The AIC8800 HE link can degrade a minute or so after association on some APs
# (associates and pings, but bulk IPv4 throughput collapses). Use --reassociate
# to start from a fresh association window right before measuring throughput.
#
# Designed to run ON THE BOARD, as root (iw / ping / wpa_cli need privileges):
#   scp scripts/wifi6_test.sh aico@192.168.42.1:/tmp/
#   ssh aico@192.168.42.1 'sudo sh /tmp/wifi6_test.sh --reassociate \
#       --url http://192.168.50.37:8899/big.bin'
#
# Throughput needs a LAN peer serving a large file over HTTP, e.g. on that peer:
#   dd if=/dev/zero of=/tmp/big.bin bs=1M count=500
#   python3 -m http.server 8899 --bind 0.0.0.0
set -u

IFACE="${WIFI_IFACE:-wlan0}"
CONF="${AICO_BOOT_CONF:-/etc/aico_boot.conf}"
URL=""
STREAMS=4
SECONDS=15
PING_EXT="${PING_EXT:-223.5.5.5}"
PING_COUNT="${PING_COUNT:-20}"
DO_REASSOC=0
DO_THROUGHPUT=1

IW=/usr/sbin/iw
WPA_CLI=/usr/sbin/wpa_cli
command -v "$IW" >/dev/null 2>&1 || IW=iw
command -v "$WPA_CLI" >/dev/null 2>&1 || WPA_CLI=wpa_cli

usage() {
    cat <<'USAGE'
Usage: wifi6_test.sh [options]

Options:
  --url URL          large-file HTTP URL on a LAN peer, for the download test
  --streams N        parallel download streams (default 4)
  --seconds N        max seconds per download stream (default 15)
  --reassociate      wpa_cli reassociate first, to reset the HE degradation window
  --ping-ext HOST    external latency target (default 223.5.5.5)
  --ping-count N     ping packets per target (default 20)
  --no-throughput    only check HE link state and latency (no download)
  --iface IFACE      wireless interface (default wlan0)
  -h, --help         show this help

Run as root. A high negotiated HE-MCS rate with low measured throughput points to
the AIC8800 HE/IPv4 degradation - re-run with --reassociate, or disable HE
(AICO_WIFI_HE=0) to compare against WiFi 5 (VHT).
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --url)         [ "$#" -ge 2 ] || { echo "--url needs an argument" >&2; exit 2; }; URL="$2"; shift 2 ;;
        --streams)     [ "$#" -ge 2 ] || { echo "--streams needs an argument" >&2; exit 2; }; STREAMS="$2"; shift 2 ;;
        --seconds)     [ "$#" -ge 2 ] || { echo "--seconds needs an argument" >&2; exit 2; }; SECONDS="$2"; shift 2 ;;
        --reassociate) DO_REASSOC=1; shift ;;
        --ping-ext)    [ "$#" -ge 2 ] || { echo "--ping-ext needs an argument" >&2; exit 2; }; PING_EXT="$2"; shift 2 ;;
        --ping-count)  [ "$#" -ge 2 ] || { echo "--ping-count needs an argument" >&2; exit 2; }; PING_COUNT="$2"; shift 2 ;;
        --no-throughput) DO_THROUGHPUT=0; shift ;;
        --iface)       [ "$#" -ge 2 ] || { echo "--iface needs an argument" >&2; exit 2; }; IFACE="$2"; shift 2 ;;
        -h|--help)     usage; exit 0 ;;
        *)             echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

section() { printf '\n== %s ==\n' "$*"; }

# ---- 1. HE switch state ----------------------------------------------------
section "WiFi 6 (HE) switch"
he_conf="$(grep -E '^AICO_WIFI_HE=' "$CONF" 2>/dev/null | cut -d= -f2)"
he_drv="$(cat /sys/module/aic8800_fdrv/parameters/he_on 2>/dev/null)"
echo "  config $CONF: AICO_WIFI_HE=${he_conf:-?}"
echo "  driver he_on=${he_drv:-?}  (Y = WiFi 6 active)"
if [ "$he_drv" = "Y" ]; then
    echo "  => WiFi 6 (HE) is ENABLED"
else
    echo "  => WiFi 6 (HE) is OFF; enable with AICO_WIFI_HE=1 and reload the driver"
fi

# ---- optional fresh reassociation ------------------------------------------
GW="$(ip route 2>/dev/null | awk '/^default/ {print $3; exit}')"
if [ "$DO_REASSOC" = "1" ]; then
    section "reassociate (fresh window)"
    "$WPA_CLI" -i "$IFACE" reassociate >/dev/null 2>&1 && echo "  reassociate requested" || echo "  reassociate failed"
    i=0
    while [ "$i" -lt 15 ]; do
        [ -n "$GW" ] && ping -c1 -W1 "$GW" >/dev/null 2>&1 && break
        i=$((i + 1)); sleep 1
    done
    echo "  gateway reachable: $([ -n "$GW" ] && ping -c1 -W1 "$GW" >/dev/null 2>&1 && echo yes || echo no)"
fi

# ---- 2. link verification --------------------------------------------------
section "link ($IFACE)"
link="$("$IW" dev "$IFACE" link 2>/dev/null)"
if [ -z "$link" ] || ! printf '%s' "$link" | grep -qi connected; then
    echo "  not associated"
else
    printf '%s\n' "$link" | grep -iE 'SSID|freq|signal|bitrate' | sed 's/^[[:space:]]*/  /'
    freq="$(printf '%s' "$link" | awk '/freq:/ {print $2}')"
    case "$freq" in 5*) echo "  band: 5 GHz" ;; 2*) echo "  band: 2.4 GHz" ;; esac
    if printf '%s' "$link" | grep -qi 'HE-MCS'; then
        echo "  => negotiated as WiFi 6 (HE-MCS)"
    elif printf '%s' "$link" | grep -qi 'VHT-MCS'; then
        echo "  => negotiated as WiFi 5 (VHT-MCS), not HE"
    fi
fi

# ---- 3. latency / packet loss ----------------------------------------------
section "latency / loss"
ping_report() {
    _label="$1"; _host="$2"
    [ -n "$_host" ] || { echo "  $_label: no target"; return; }
    printf '  %s (%s):\n' "$_label" "$_host"
    ping -c "$PING_COUNT" -i 0.2 -W 2 "$_host" 2>&1 \
        | grep -E 'packet loss|rtt|round-trip' | sed 's/^/    /' \
        || echo "    ping failed (need root?)"
}
ping_report "gateway (pure WiFi)" "$GW"
ping_report "external (WAN)" "$PING_EXT"

# ---- 4. throughput ---------------------------------------------------------
if [ "$DO_THROUGHPUT" = "1" ]; then
    section "throughput (download, $STREAMS streams)"
    if [ -z "$URL" ]; then
        echo "  no --url given; skipping (pass a LAN peer's HTTP file URL)"
    elif ! command -v curl >/dev/null 2>&1; then
        echo "  curl not found; cannot run download test"
    else
        echo "  source: $URL"
        tmp="$(mktemp -d "${TMPDIR:-/tmp}/wifi6.XXXXXX")"
        i=0
        while [ "$i" -lt "$STREAMS" ]; do
            curl -s --noproxy '*' -o /dev/null --max-time "$SECONDS" \
                -w '%{speed_download}\n' "$URL" > "$tmp/s$i" 2>/dev/null &
            i=$((i + 1))
        done
        wait
        cat "$tmp"/s* 2>/dev/null | awk '{s+=$1} END {
            printf "  aggregate: %.1f MB/s = %.0f Mbit/s (%d streams)\n", s/1048576, s*8/1000000, NR
        }'
        rm -rf "$tmp"
    fi
fi

section "done"
