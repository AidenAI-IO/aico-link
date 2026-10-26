#!/bin/sh
# wifi_speedtest.sh - WiFi link quality and throughput test for the Aiden board.
#
# Reports, for the wireless interface:
#   - link info (band/frequency, signal, negotiated PHY rate) via iw
#   - latency and packet loss via ping (gateway + an external host)
#   - throughput via iperf3 multi-stream download (-R) and upload
#
# Designed to run ON THE BOARD, as root (ping and iw need raw/net-admin caps):
#   scp scripts/wifi_speedtest.sh aiden@192.168.42.1:/tmp/
#   ssh aiden@192.168.42.1 'sudo sh /tmp/wifi_speedtest.sh'
#
# iperf3 must be installed on the board (apt-get install iperf3). The default
# public server is a best-effort internet endpoint; throughput over a long
# internet path reflects the broadband/WAN link, not the WiFi PHY. For a pure
# WiFi measurement, point --server at an iperf3 host on the same LAN.
set -eu

IFACE="${WIFI_IFACE:-wlan0}"
SERVER="${IPERF_SERVER:-ash.speedtest.clouvider.net}"
PORT="${IPERF_PORT:-5200}"
STREAMS="${IPERF_STREAMS:-8}"
DURATION="${IPERF_TIME:-8}"
PING_HOST="${PING_HOST:-}"          # empty = auto-detect default gateway
PING_EXT="${PING_EXT:-223.5.5.5}"   # external host for WAN latency/loss
PING_COUNT="${PING_COUNT:-30}"
DO_IPERF=1

usage() {
    cat <<'USAGE'
Usage: wifi_speedtest.sh [options]

Options:
  --iface IFACE      wireless interface            (default: wlan0)
  --server HOST      iperf3 server                 (default: ash.speedtest.clouvider.net)
  --port PORT        iperf3 server port            (default: 5200)
  --streams N        parallel iperf3 streams       (default: 8)
  --time N           iperf3 test duration seconds  (default: 8)
  --ping-host HOST   LAN latency target            (default: default gateway)
  --ping-ext HOST    WAN latency target            (default: 223.5.5.5)
  --ping-count N     ping packets per target       (default: 30)
  --no-iperf         skip throughput test (link + ping only)
  -h, --help         show this help

Run as root: ICMP ping and `iw` need elevated privileges on this board.
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --iface)      [ "$#" -ge 2 ] || { echo "--iface needs an argument" >&2; exit 2; }; IFACE="$2"; shift 2 ;;
        --server)     [ "$#" -ge 2 ] || { echo "--server needs an argument" >&2; exit 2; }; SERVER="$2"; shift 2 ;;
        --port)       [ "$#" -ge 2 ] || { echo "--port needs an argument" >&2; exit 2; }; PORT="$2"; shift 2 ;;
        --streams)    [ "$#" -ge 2 ] || { echo "--streams needs an argument" >&2; exit 2; }; STREAMS="$2"; shift 2 ;;
        --time)       [ "$#" -ge 2 ] || { echo "--time needs an argument" >&2; exit 2; }; DURATION="$2"; shift 2 ;;
        --ping-host)  [ "$#" -ge 2 ] || { echo "--ping-host needs an argument" >&2; exit 2; }; PING_HOST="$2"; shift 2 ;;
        --ping-ext)   [ "$#" -ge 2 ] || { echo "--ping-ext needs an argument" >&2; exit 2; }; PING_EXT="$2"; shift 2 ;;
        --ping-count) [ "$#" -ge 2 ] || { echo "--ping-count needs an argument" >&2; exit 2; }; PING_COUNT="$2"; shift 2 ;;
        --no-iperf)   DO_IPERF=0; shift ;;
        -h|--help)    usage; exit 0 ;;
        *)            echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

section() { printf '\n== %s ==\n' "$*"; }

# ---- Link info -------------------------------------------------------------
section "link ($IFACE)"
if command -v iw >/dev/null 2>&1; then
    iw dev "$IFACE" link 2>&1 | grep -iE 'SSID|freq|signal|bitrate' || echo "  (not associated?)"
else
    echo "  iw not found; skipping link info"
fi

# ---- Latency / packet loss -------------------------------------------------
GW="$PING_HOST"
if [ -z "$GW" ]; then
    GW="$(ip route 2>/dev/null | awk '/^default/ {print $3; exit}')"
fi

ping_report() {
    _label="$1"; _host="$2"
    [ -n "$_host" ] || { echo "  $_label: no target"; return; }
    printf '  %s (%s):\n' "$_label" "$_host"
    ping -c "$PING_COUNT" -i 0.2 -W 2 "$_host" 2>&1 \
        | grep -E 'packet loss|rtt|round-trip' | sed 's/^/    /' \
        || echo "    ping failed (need root?)"
}

section "latency / loss"
ping_report "gateway (LAN, pure WiFi)" "$GW"
ping_report "external (WAN)" "$PING_EXT"

# ---- Throughput ------------------------------------------------------------
if [ "$DO_IPERF" = "1" ]; then
    section "throughput (iperf3 $SERVER:$PORT, $STREAMS streams, ${DURATION}s)"
    if ! command -v iperf3 >/dev/null 2>&1; then
        echo "  iperf3 not installed (apt-get install iperf3); skipping"
    else
        run_iperf() {
            # $1 = extra iperf3 flags; prints the aggregate receiver rate or FAILED
            _out="$(iperf3 -c "$SERVER" -p "$PORT" -P "$STREAMS" -t "$DURATION" $1 2>&1)"
            _rate="$(printf '%s\n' "$_out" | awk '/SUM/ && /receiver/ {print $6, $7}' | tail -1)"
            if [ -n "$_rate" ]; then
                printf '%s\n' "$_rate"
            else
                printf 'FAILED (%s)\n' "$(printf '%s' "$_out" | grep -iE 'busy|error|unable' | head -1 | sed 's/^iperf3: //')"
            fi
        }
        printf '  download (-R): '
        run_iperf "-R"
        sleep 3   # let the public server release the previous session
        printf '  upload:        '
        run_iperf ""
    fi
fi

section "done"
