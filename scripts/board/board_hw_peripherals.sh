#!/bin/sh
# aico-hw-suite: 50 peripherals - system, Wi-Fi, Bluetooth, USB, storage/TF, NPU, RTC
# board_hw_peripherals.sh - Wi-Fi, Bluetooth, USB gadget, storage and TF card,
# NPU/RGA/MPP, RTC, thermal and system-health checks for an Aico board.
#
# Run ON THE BOARD as root next to board_hw_common.sh,
# board_low_level_common.sh and board_hw_tools.py. See --help.
set +e

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/board_hw_common.sh"
hw_start peripherals "$@"

: "${BLE_SOCKET:=/run/ble_service/ble_service.sock}"
STORAGE_MB=32
STORAGE_DIR="/userdata"
INTERNET=0
WIFI_IFACE=""        # empty: the first wireless interface
EXPECT_SSID=""       # empty: report the network, do not require a name
DNS_HOST="aico.link"
HTTPS_URL=""         # empty: https://$DNS_HOST/
MIN_RSSI=-75
MAX_TEMP_C=80
MIN_WRITE_MBS=5
MIN_READ_MBS=10
RUN_GROUPS=""           # empty: every group in ALL_GROUPS
SKIP_GROUPS=""
IPERF_SERVER=""
DOWNLOAD_URL=""
MIN_WIFI_MBPS=5
BLE_CONNECT=0
BLE_PAIR=0
BLE_SECONDS=90

usage() {
    cat <<'USAGE'
Usage:
  sudo sh board_hw_peripherals.sh [options]

  - system: failed units, kernel errors, thermal zones, CPU frequencies, memory/CMA
  - wifi: interface, driver, association, signal, address, route, gateway loss and
    latency, DNS, NTP; throughput with --iperf-server and/or --download-url
  - bluetooth: hci0, adapter power, ble_service (GATT, advertising); with
    --interactive --ble-connect, a phone connecting to the board
  - usb: HID gadget nodes, UDC binding, USB network (usb0)
  - storage: /userdata space and read/write throughput, TF card detection and
    a write/read-back check (insert a card first; --interactive prompts for it)
  - npu: RKNPU/RGA/MPP device nodes and the VAD and UI-grounding self-tests
  - rtc: /dev/rtc0 ticking and matching the system clock

Options:
  --interactive         Prompt for a TF card when none is inserted
  --only LIST           Comma-separated groups to run, default all
  --skip LIST           Comma-separated groups to skip
  --wifi-iface NAME     Wi-Fi interface, default the first wireless interface
  --expect-ssid SSID    Fail unless associated to this network (default: any)
  --dns-host NAME       Host name the DNS check resolves, default aico.link
  --internet            Also make an HTTPS request through Wi-Fi (network access)
  --https-url URL       Target of --internet, default https://<dns-host>/
  --iperf-server HOST[:PORT]
                        Measure Wi-Fi TCP throughput both ways against an
                        iperf3 server (iperf3 -s on a computer on the same LAN)
  --download-url URL    Measure Wi-Fi download speed with curl from this URL,
                        e.g. a large file served from a LAN computer
  --min-wifi-mbps N     Throughput warning limit, default 5 Mbit/s
  --ble-connect         With --interactive, wait for a phone to connect over BLE
  --ble-pair            With --ble-connect, open the pairing window first so a
                        new phone can bond (adds a bonded device)
  --ble-seconds N       Time allowed for the phone to connect, default 90
  --storage-mb N        Size of the throughput test file, default 32
  --storage-dir DIR     Where the throughput file is written, default /userdata
  --min-rssi DBM        Wi-Fi signal limit, default -75
  --max-temp-c C        Thermal limit, default 80
  --config FILE         Settings file (see board_hw.conf.example)
  -h, --help            Show this help

The storage test writes and deletes one temporary file; nothing else on the
board is changed. The NPU self-tests need the Agent's resident UI-grounding
helper to leave enough CMA memory; a failure there is reported as a warning.
USAGE
}

hw_load_config

while [ "$#" -gt 0 ]; do
    case "$1" in
        --interactive) INTERACTIVE=1; shift ;;
        --only) [ "$#" -ge 2 ] || die "$1 needs a value"; RUN_GROUPS="$(printf '%s' "$2" | tr ',' ' ')"; shift 2 ;;
        --skip) [ "$#" -ge 2 ] || die "$1 needs a value"; SKIP_GROUPS="$(printf '%s' "$2" | tr ',' ' ')"; shift 2 ;;
        --wifi-iface) [ "$#" -ge 2 ] || die "$1 needs a value"; WIFI_IFACE="$2"; shift 2 ;;
        --expect-ssid) [ "$#" -ge 2 ] || die "$1 needs a value"; EXPECT_SSID="$2"; shift 2 ;;
        --dns-host) [ "$#" -ge 2 ] || die "$1 needs a value"; DNS_HOST="$2"; shift 2 ;;
        --internet) INTERNET=1; shift ;;
        --https-url) [ "$#" -ge 2 ] || die "$1 needs a value"; HTTPS_URL="$2"; INTERNET=1; shift 2 ;;
        --iperf-server) [ "$#" -ge 2 ] || die "$1 needs a value"; IPERF_SERVER="$2"; shift 2 ;;
        --download-url) [ "$#" -ge 2 ] || die "$1 needs a value"; DOWNLOAD_URL="$2"; shift 2 ;;
        --min-wifi-mbps) [ "$#" -ge 2 ] || die "$1 needs a value"; MIN_WIFI_MBPS="$2"; shift 2 ;;
        --ble-connect) BLE_CONNECT=1; shift ;;
        --ble-pair) BLE_PAIR=1; shift ;;
        --ble-seconds) [ "$#" -ge 2 ] || die "$1 needs a value"; BLE_SECONDS="$2"; shift 2 ;;
        --storage-mb) [ "$#" -ge 2 ] || die "$1 needs a value"; STORAGE_MB="$2"; shift 2 ;;
        --storage-dir) [ "$#" -ge 2 ] || die "$1 needs a value"; STORAGE_DIR="$2"; shift 2 ;;
        --min-rssi) [ "$#" -ge 2 ] || die "$1 needs a value"; MIN_RSSI="$2"; shift 2 ;;
        --max-temp-c) [ "$#" -ge 2 ] || die "$1 needs a value"; MAX_TEMP_C="$2"; shift 2 ;;
        --artifact-dir) [ "$#" -ge 2 ] || die "$1 needs a value"; AICO_LOW_LEVEL_ARTIFACT_DIR="$2"; shift 2 ;;
        --run-id) [ "$#" -ge 2 ] || die "$1 needs a value"; RUN_ID="$2"; shift 2 ;;
        --config) [ "$#" -ge 2 ] || die "$1 needs a value"; shift 2 ;;  # loaded by hw_start
        -h|--help) usage; exit 0 ;;
        *) die "unknown option: $1" ;;
    esac
done
require_uint "--storage-mb" "$STORAGE_MB"
require_uint "--min-wifi-mbps" "$MIN_WIFI_MBPS"
require_uint "--ble-seconds" "$BLE_SECONDS"
# Check groups run in this order. Adding a group: write group_<name>() below
# and add <name> here.
ALL_GROUPS="system wifi bluetooth usb storage npu rtc"
[ -n "$RUN_GROUPS" ] || RUN_GROUPS="$ALL_GROUPS"
for group in $RUN_GROUPS $SKIP_GROUPS; do
    case " $ALL_GROUPS " in *" $group "*) ;; *) die "unknown group: $group (use: $ALL_GROUPS)" ;; esac
done

# mbps BYTES MILLISECONDS -> integer MB/s
mbps() {
    [ "$2" -gt 0 ] && echo $(( $1 * 1000 / $2 / 1048576 )) || echo 0
}

now_ms() {
    python3 -c 'import time; print(int(time.monotonic()*1000))' 2>/dev/null || echo $(( $(date +%s) * 1000 ))
}

# mbit_ok LABEL MBIT -> pass when at least MIN_WIFI_MBPS, else warn
mbit_ok() {
    if python3 -c 'import sys; sys.exit(0 if float(sys.argv[1]) >= float(sys.argv[2]) else 1)' "$2" "$MIN_WIFI_MBPS" 2>/dev/null; then
        pass "$1 $2 Mbit/s (>= $MIN_WIFI_MBPS)"
    else
        warn "$1 $2 Mbit/s (< $MIN_WIFI_MBPS)"
    fi
}

# Wi-Fi throughput, bound to the Wi-Fi interface so a USB network cannot carry the traffic.
wifi_throughput() {
    wlan_ip="$(ip -4 -o addr show "$WIFI_IFACE" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n 1)"
    if [ -z "$IPERF_SERVER" ] && [ -z "$DOWNLOAD_URL" ]; then
        skip "Wi-Fi throughput (pass --iperf-server HOST or --download-url URL)"
        return 0
    fi
    if [ -n "$IPERF_SERVER" ]; then
        if ! command -v iperf3 >/dev/null 2>&1; then
            skip "iperf3 throughput (iperf3 not installed on the board; use --download-url)"
        elif [ -z "$wlan_ip" ]; then
            fail "iperf3 throughput: $WIFI_IFACE has no address"
        else
            host="${IPERF_SERVER%:*}"
            port="5201"
            [ "$host" != "$IPERF_SERVER" ] && port="${IPERF_SERVER##*:}"
            bind_dev=""
            if iperf3 --help 2>&1 | grep -q -- '--bind-dev'; then
                bind_dev="--bind-dev $WIFI_IFACE"
            else
                warn "iperf3 has no --bind-dev (needs 3.10+); traffic may leave through another interface"
            fi
            for direction in upload download; do
                flag=""
                [ "$direction" = "download" ] && flag="-R"
                # -B only picks the source address; --bind-dev (iperf3 3.10+)
                # also keeps the traffic off another route such as usb0.
                result="$(timeout 30 iperf3 -c "$host" -p "$port" -B "$wlan_ip" $bind_dev -t 5 -J $flag 2>/dev/null)"
                rate="$(printf '%s' "$result" | python3 -c 'import json,sys
d=json.load(sys.stdin); print(round(d["end"]["sum_received"]["bits_per_second"]/1e6,1))' 2>/dev/null)"
                if [ -n "$rate" ]; then
                    mbit_ok "Wi-Fi $direction (iperf3 TCP to $host)" "$rate"
                else
                    fail "Wi-Fi $direction iperf3 to $host:$port failed (server running? firewall?)"
                fi
            done
        fi
    fi
    if [ -n "$DOWNLOAD_URL" ]; then
        if ! command -v curl >/dev/null 2>&1; then
            skip "download throughput (curl missing)"
        else
            out="$(curl -s --interface "$WIFI_IFACE" -o /dev/null -m 60 -w '%{http_code} %{size_download} %{speed_download}' "$DOWNLOAD_URL" 2>/dev/null)"
            code="${out%% *}"
            bytes="$(printf '%s' "$out" | awk '{print $2}')"
            speed="$(printf '%s' "$out" | awk '{printf "%.1f", $3*8/1e6}')"
            if [ "$code" = "200" ] && [ "${bytes%.*}" -gt 1048576 ] 2>/dev/null; then
                mbit_ok "Wi-Fi download $(( ${bytes%.*} / 1048576 )) MiB" "$speed"
            elif [ "$code" = "200" ]; then
                warn "download finished but only ${bytes} bytes; use a file of several MiB for a speed figure ($speed Mbit/s)"
            else
                fail "download from $DOWNLOAD_URL failed (HTTP $code)"
            fi
        fi
    fi
}

# Scratch directories made with mktemp -d (private to the owner), so a file
# planted under a shared or removable mount cannot redirect a root write.
SCRATCH_DIRS=""
scratch_dir() {
    scratch="$(mktemp -d "$1/.aico_hw_test.XXXXXX" 2>/dev/null)" || return 1
    SCRATCH_DIRS="$SCRATCH_DIRS $scratch"
    printf '%s\n' "$scratch"
}
remove_scratch() {
    for scratch in $SCRATCH_DIRS; do rm -rf "$scratch"; done
    SCRATCH_DIRS=""
}
trap 'remove_scratch' EXIT
trap 'remove_scratch; exit 130' INT TERM

# md5_of FILE -> digest, or nonzero status when the file cannot be read.
md5_of() {
    md5_out="$(md5sum "$1" 2>/dev/null)" || return 1
    md5_out="${md5_out%% *}"
    [ -n "$md5_out" ] || return 1
    printf '%s\n' "$md5_out"
}

# kernel_journal ARGS -> this boot's kernel messages; nonzero when unavailable.
kernel_journal() {
    journalctl -b -k --no-pager "$@" 2>/dev/null
}

ble_field() {
    printf '%s' "$1" | python3 -c 'import json,sys
b=json.load(sys.stdin).get("bluetooth",{}); v=b.get(sys.argv[1]); print(str(v).lower() if isinstance(v,bool) else ("" if v is None else v))' "$2" 2>/dev/null
}

warn_if_not_root

group_system() {
    section "system health"
    # A query that fails is missing evidence, not a healthy result.
    if ! units="$(systemctl --failed --no-legend --plain 2>/dev/null)"; then
        warn "cannot list failed systemd units (systemctl failed); unit health not checked"
    elif failed="$(printf '%s\n' "$units" | awk 'NF {print $1}')"; [ -z "$failed" ]; then
        pass "no failed systemd units"
    else
        fail "failed systemd units: $(printf '%s' "$failed" | tr '\n' ' ')"
    fi
    if ! kerr="$(kernel_journal -p err)"; then
        warn "kernel log unavailable (journalctl failed); kernel and regulator errors not checked"
    elif kerr="$(printf '%s\n' "$kerr" | grep -v -- '-- ' | grep -v '^$' | tail -n 8)"; [ -n "$kerr" ]; then
        warn "kernel errors in this boot (latest 8):"
        printf '%s\n' "$kerr" | indent
    else
        pass "no kernel errors in this boot"
    fi
    for zone in /sys/class/thermal/thermal_zone*; do
        [ -r "$zone/temp" ] || continue
        milli="$(cat "$zone/temp" 2>/dev/null)"
        [ -n "$milli" ] || continue
        celsius=$(( milli / 1000 ))
        name="$(cat "$zone/type" 2>/dev/null)"
        [ "$celsius" -le "$MAX_TEMP_C" ] && pass "$name temperature ${celsius} C (<= $MAX_TEMP_C C)" || fail "$name temperature ${celsius} C above $MAX_TEMP_C C"
    done
    for policy in /sys/devices/system/cpu/cpufreq/policy*; do
        [ -d "$policy" ] || continue
        printf '    %s: governor %s, %s..%s kHz, now %s kHz\n' "$(basename "$policy")" \
            "$(cat "$policy/scaling_governor" 2>/dev/null)" "$(cat "$policy/cpuinfo_min_freq" 2>/dev/null)" \
            "$(cat "$policy/cpuinfo_max_freq" 2>/dev/null)" "$(cat "$policy/scaling_cur_freq" 2>/dev/null)"
    done
    if klog="$(kernel_journal)"; then
        vreg="$(printf '%s\n' "$klog" | grep -ciE 'failed to set voltage|cpu.*voltage.*fail|regulator.*fail')"
        [ "${vreg:-0}" -eq 0 ] && pass "no CPU voltage/regulator errors" || fail "$vreg CPU voltage/regulator errors in this boot"
    fi
    grep -E 'MemTotal|MemAvailable|CmaTotal|CmaFree' /proc/meminfo 2>/dev/null | indent
}

group_wifi() {
    section "Wi-Fi"
    if [ -n "$WIFI_IFACE" ] && [ -d "/sys/class/net/$WIFI_IFACE" ]; then
        pass "$WIFI_IFACE present (driver $(basename "$(readlink -f /sys/class/net/$WIFI_IFACE/device/driver 2>/dev/null)" 2>/dev/null))"
        # -o cat drops the "aico kernel:" prefix, which would match 'aic'.
        fw="$(journalctl -b -k -o cat --no-pager 2>/dev/null | grep -iE 'aic(wf|8800|_)|wlan|sdio' | grep -iE 'fw|firmware|ready|fail' | tail -n 3)"
        [ -n "$fw" ] && printf '%s\n' "$fw" | indent
        if command -v wpa_cli >/dev/null 2>&1; then
            st="$(wpa_cli -i "$WIFI_IFACE" status 2>/dev/null)"
            wstate="$(printf '%s\n' "$st" | sed -n 's/^wpa_state=//p')"
            ssid="$(printf '%s\n' "$st" | sed -n 's/^ssid=//p')"
            if [ "$wstate" != "COMPLETED" ]; then
                fail "Wi-Fi not associated (wpa_state=$wstate)"
            elif [ -n "$EXPECT_SSID" ] && [ "$ssid" != "$EXPECT_SSID" ]; then
                fail "associated to '$ssid', expected '$EXPECT_SSID'"
            else
                pass "associated to '$ssid'"
            fi
            poll="$(wpa_cli -i "$WIFI_IFACE" signal_poll 2>/dev/null)"
            rssi="$(printf '%s\n' "$poll" | sed -n 's/^RSSI=//p')"
            if [ -n "$rssi" ]; then
                printf '%s\n' "$(printf '%s' "$poll" | tr '\n' ' ')" | indent
                [ "$rssi" -ge "$MIN_RSSI" ] && pass "signal $rssi dBm (>= $MIN_RSSI)" || warn "weak signal $rssi dBm (< $MIN_RSSI)"
            fi
        else
            skip "association and signal (wpa_cli missing)"
        fi
        addr="$(ip -4 -o addr show "$WIFI_IFACE" 2>/dev/null | awk '{print $4}' | head -n 1)"
        [ -n "$addr" ] && pass "IPv4 address $addr" || fail "no IPv4 address on $WIFI_IFACE"
        gw="$(ip route show default dev "$WIFI_IFACE" 2>/dev/null | awk '{print $3; exit}')"
        if [ -n "$gw" ]; then
            pass "default route via $gw"
            pings="$(ping -c 10 -i 0.3 -W 2 "$gw" 2>/dev/null)"
            loss="$(printf '%s\n' "$pings" | sed -n 's/.* \([0-9]*\)% packet loss.*/\1/p')"
            rtt="$(printf '%s\n' "$pings" | sed -n 's|.*= [0-9.]*/\([0-9.]*\)/[0-9.]*/\([0-9.]*\).*|avg \1 ms, jitter \2 ms|p')"
            [ "${loss:-100}" -le 20 ] && pass "gateway ping loss ${loss}% (${rtt:-no RTT})" || fail "gateway ping loss ${loss:-100}%"
        else
            warn "no default route through $WIFI_IFACE"
        fi
        getent hosts "$DNS_HOST" >/dev/null 2>&1 && pass "DNS resolves $DNS_HOST" || warn "DNS lookup of $DNS_HOST failed"
        sync="$(timedatectl show -p NTPSynchronized --value 2>/dev/null)"
        [ "$sync" = "yes" ] && pass "system clock NTP-synchronized" || warn "NTP not synchronized ($sync)"
        if [ "$INTERNET" = "1" ]; then
            if command -v curl >/dev/null 2>&1; then
                # Any HTTP status proves the TLS connection over Wi-Fi worked.
                code="$(curl -s --interface "$WIFI_IFACE" -o /dev/null -m 15 -w '%{http_code}' "$HTTPS_URL" 2>/dev/null)"
                case "$code" in
                    [1-5][0-9][0-9]) pass "HTTPS reachable through Wi-Fi ($HTTPS_URL, HTTP $code)" ;;
                    *) fail "HTTPS request to $HTTPS_URL failed (no response)" ;;
                esac
            else
                skip "HTTPS check (curl missing)"
            fi
        fi
        wifi_throughput

    else
        fail "no Wi-Fi interface ${WIFI_IFACE:+($WIFI_IFACE) }(SDIO enumeration, firmware or MCU Wi-Fi power)"
    fi
}

group_bluetooth() {
    section "Bluetooth"
    if [ -d /sys/class/bluetooth/hci0 ]; then
        pass "hci0 present"
        if command -v btmgmt >/dev/null 2>&1; then
            btmgmt info 2>/dev/null | head -n 6 | indent
        elif command -v hciconfig >/dev/null 2>&1; then
            hciconfig hci0 2>/dev/null | indent
        fi
    else
        fail "hci0 missing (UART, firmware or BT power)"
    fi
    if [ -S "$BLE_SOCKET" ]; then
        reply="$(python3 "$HW_TOOLS" ble-status --socket "$BLE_SOCKET" 2>&1)"
        if printf '%s' "$reply" | grep -q '"status": "OK"'; then
            printf '%s\n' "$reply" | python3 -c 'import json,sys
b=json.load(sys.stdin).get("bluetooth",{})
for k in ("backend_available","adapter_powered","adapter_address","gatt_registered","advertising","bonded_device_count","device_name"):
    print(f"{k}={b.get(k)}")' 2>/dev/null | indent
            printf '%s' "$reply" | grep -q '"adapter_powered": true' && pass "BLE adapter powered" || fail "BLE adapter not powered"
            printf '%s' "$reply" | grep -q '"gatt_registered": true' && pass "BLE GATT service registered" || warn "BLE GATT service not registered"
            printf '%s' "$reply" | grep -q '"advertising": true' && pass "BLE advertising" || warn "BLE not advertising (normal when a phone is connected)"
        else
            fail "ble_service status failed: $reply"
        fi
    else
        warn "ble_service socket missing ($BLE_SOCKET)"
    fi
    if [ "$BLE_CONNECT" = "1" ] && interactive_or_skip "BLE phone connection"; then
        before="$(python3 "$HW_TOOLS" ble-status --socket "$BLE_SOCKET" 2>/dev/null)"
        bonded_before="$(ble_field "$before" bonded_device_count)"
        name="$(ble_field "$before" device_name)"
        if [ "$BLE_PAIR" = "1" ]; then
            reply="$(python3 "$HW_TOOLS" ble-status --socket "$BLE_SOCKET" --op pairing_start 2>&1)"
            [ "$(ble_field "$reply" pairing_open)" = "true" ] && pass "BLE pairing window opened (deadline $(ble_field "$reply" pairing_deadline))" \
                || fail "BLE pairing_start failed: $reply"
        fi
        if [ "$(ble_field "$before" connected)" = "true" ]; then
            pass "a phone is already connected over BLE: $(ble_field "$before" connected_device_name) ($(ble_field "$before" connected_device_address))"
        else
            printf '\n>>> On the phone, open the Aico App (or a BLE scanner such as nRF Connect) and connect to "%s".\n' "${name:-the board}"
            printf '>>> Waiting up to %s s for the connection...\n' "$BLE_SECONDS"
        fi
        t0="$(date +%s)"
        connected=""
        while [ $(( $(date +%s) - t0 )) -le "$BLE_SECONDS" ]; do
            now="$(python3 "$HW_TOOLS" ble-status --socket "$BLE_SOCKET" 2>/dev/null)"
            if [ "$(ble_field "$now" connected)" = "true" ]; then
                connected="$now"
                [ "$(ble_field "$now" services_resolved)" = "true" ] && break
            fi
            sleep 2
        done
        if [ -n "$connected" ]; then
            pass "phone connected over BLE after ~$(( $(date +%s) - t0 ))s: $(ble_field "$connected" connected_device_name) ($(ble_field "$connected" connected_device_address))"
            [ "$(ble_field "$connected" paired)" = "true" ] && pass "BLE link is paired (encrypted)" || warn "BLE link is not paired"
            [ "$(ble_field "$connected" services_resolved)" = "true" ] && pass "GATT services resolved" || warn "GATT services not resolved yet"
            ancs="$(ble_field "$connected" ancs_subscribed)"
            [ "$ancs" = "true" ] && pass "ANCS notifications subscribed (iPhone)" || printf '    ancs_subscribed=%s (iPhone only)\n' "$ancs"
            if [ "$BLE_PAIR" = "1" ]; then
                bonded_after="$(ble_field "$connected" bonded_device_count)"
                [ "${bonded_after:-0}" -gt "${bonded_before:-0}" ] && pass "new bond stored (${bonded_before:-0} -> $bonded_after)" \
                    || warn "bonded device count unchanged (${bonded_before:-0} -> ${bonded_after:-0}); the phone may have been bonded already"
            fi
        else
            fail "no phone connected over BLE within $BLE_SECONDS s (last error: $(ble_field "$(python3 "$HW_TOOLS" ble-status --socket "$BLE_SOCKET" 2>/dev/null)" last_error))"
        fi
    fi
}

group_usb() {
    section "USB gadget"
    for node in /dev/hidg0 /dev/hidg1 /dev/hidg2; do
        check_exists "HID gadget node" "$node"
    done
    udc="$(cat /sys/kernel/config/usb_gadget/*/UDC 2>/dev/null | grep -v '^$' | head -n 1)"
    [ -n "$udc" ] && pass "gadget bound to UDC $udc" || fail "no gadget bound to a UDC (USB cable to a host?)"
    state="$(cat /sys/class/udc/*/state 2>/dev/null | head -n 1)"
    [ "$state" = "configured" ] && pass "USB device state configured by the host" || warn "USB device state '$state' (host not connected?)"
    if [ -d /sys/class/net/usb0 ]; then
        addr="$(ip -4 -o addr show usb0 2>/dev/null | awk '{print $4}' | head -n 1)"
        oper="$(cat /sys/class/net/usb0/operstate 2>/dev/null)"
        [ -n "$addr" ] && pass "usb0 $addr (link $oper)" || warn "usb0 has no IPv4 address (link $oper)"
    else
        warn "usb0 (USB network) missing"
    fi
}

group_storage() {
    section "storage"
    df -h / "$STORAGE_DIR" 2>/dev/null | indent
    if grep -qs " $STORAGE_DIR .* ro[ ,]" /proc/mounts; then
        fail "$STORAGE_DIR is mounted read-only"
    fi
    free_mb="$(df -Pm "$STORAGE_DIR" 2>/dev/null | awk 'NR==2{print $4}')"
    if [ -n "$free_mb" ] && [ "$free_mb" -gt $((STORAGE_MB * 3)) ]; then
        if ! test_dir="$(scratch_dir "$STORAGE_DIR")"; then
            fail "cannot create a private test directory in $STORAGE_DIR"
            return 0
        fi
        test_file="$test_dir/data"
        bytes=$((STORAGE_MB * 1048576))
        t0="$(now_ms)"
        if dd if=/dev/urandom of="$test_file" bs=1M count="$STORAGE_MB" conv=fsync 2>/dev/null; then
            t1="$(now_ms)"
            sum_written="$(md5_of "$test_file")"
            sync
            { echo 3 > /proc/sys/vm/drop_caches; } 2>/dev/null
            t2="$(now_ms)"
            sum_read="$(md5_of "$test_file")"
            t3="$(now_ms)"
            write_mbs="$(mbps "$bytes" $((t1 - t0)))"
            read_mbs="$(mbps "$bytes" $((t3 - t2)))"
            if [ -z "$sum_written" ] || [ -z "$sum_read" ]; then
                fail "cannot read back the test file on $STORAGE_DIR"
            elif [ "$sum_written" = "$sum_read" ]; then
                pass "$STORAGE_MB MiB written and read back intact"
            else
                fail "read-back checksum mismatch on $STORAGE_DIR"
            fi
            [ "$write_mbs" -ge "$MIN_WRITE_MBS" ] && pass "write ~$write_mbs MB/s (incl. urandom, >= $MIN_WRITE_MBS)" || warn "write ~$write_mbs MB/s (< $MIN_WRITE_MBS)"
            [ "$read_mbs" -ge "$MIN_READ_MBS" ] && pass "read ~$read_mbs MB/s (>= $MIN_READ_MBS)" || warn "read ~$read_mbs MB/s (< $MIN_READ_MBS)"
        else
            fail "cannot write $test_file"
        fi
        remove_scratch
    else
        skip "throughput test: only ${free_mb:-?} MiB free on $STORAGE_DIR"
    fi
    if klog="$(kernel_journal)"; then
        emmc_errors="$(printf '%s\n' "$klog" | grep -ciE 'mmc[0-9].*(error|timeout|crc)|I/O error')"
        [ "${emmc_errors:-0}" -eq 0 ] && pass "no MMC/I-O errors in this boot" || warn "$emmc_errors MMC/I-O error lines in this boot"
    else
        warn "kernel log unavailable (journalctl failed); MMC errors not checked"
    fi

    section "TF card"
    find_sd() {
        for dev in /sys/block/mmcblk*; do
            [ "$(cat "$dev/device/type" 2>/dev/null)" = "SD" ] && { basename "$dev"; return 0; }
        done
        return 1
    }
    sd="$(find_sd)"
    if [ -z "$sd" ] && [ "$INTERACTIVE" = "1" ]; then
        if prompt_enter "Insert a TF (microSD) card. The test only adds and removes one small file."; then
            sleep 3
            sd="$(find_sd)"
        fi
    fi
    if [ -n "$sd" ]; then
        size_gb=$(( $(cat "/sys/block/$sd/size" 2>/dev/null || echo 0) / 2097152 ))
        pass "TF card detected: /dev/$sd (~${size_gb} GB, $(cat "/sys/block/$sd/device/name" 2>/dev/null))"
        mnt="$(grep "^/dev/$sd" /proc/mounts | awk '{print $2}' | head -n 1)"
        if [ -n "$mnt" ]; then
            if ! tf_dir="$(scratch_dir "$mnt")"; then
                fail "cannot write to the TF card at $mnt (read-only or I/O error)"
            elif dd if=/dev/urandom of="$tf_dir/data" bs=1M count=4 conv=fsync 2>/dev/null; then
                a="$(md5_of "$tf_dir/data")"
                sync; { echo 3 > /proc/sys/vm/drop_caches; } 2>/dev/null
                b="$(md5_of "$tf_dir/data")"
                if [ -z "$a" ] || [ -z "$b" ]; then
                    fail "cannot read back the test file on the TF card at $mnt"
                elif [ "$a" = "$b" ]; then
                    pass "TF card write/read-back OK at $mnt"
                else
                    fail "TF card read-back mismatch at $mnt"
                fi
            else
                fail "cannot write to the TF card at $mnt (read-only or I/O error)"
            fi
            remove_scratch
        else
            warn "TF card /dev/$sd is not mounted (storage manager has not accepted it yet); write test skipped"
        fi
    else
        skip "TF card not inserted"
    fi
}

group_npu() {
    section "NPU / RGA / MPP"
    npu_node="$(ls /dev/rknpu* /dev/dri/renderD* 2>/dev/null | head -n 1)"
    [ -n "$npu_node" ] && pass "NPU device node $npu_node" || {
        lsmod 2>/dev/null | grep -qi rknpu && pass "rknpu module loaded" || fail "no RKNPU device node or module"
    }
    [ -e /dev/rga ] && pass "RGA device /dev/rga" || warn "/dev/rga missing"
    [ -e /dev/mpp_service ] && pass "MPP device /dev/mpp_service" || warn "/dev/mpp_service missing"
    [ -r /sys/kernel/debug/rknpu/load ] && printf '    NPU load: %s\n' "$(cat /sys/kernel/debug/rknpu/load 2>/dev/null)"
    vad="$(find_bin rknn_vad)"
    if [ -n "$vad" ] && [ -f /usr/lib/aico/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn ]; then
        run_warn_check "rknn_vad self-test (Silero VAD on the NPU)" timeout 60 "$vad" \
            --model /usr/lib/aico/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn \
            --weights /usr/lib/aico/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
    else
        skip "rknn_vad self-test (binary or model missing)"
    fi
    ug="$(find_bin ui_grounding_helper)"
    model_dir=/usr/lib/aico/models/ui-detr
    if [ -n "$ug" ] && [ -f "$model_dir/model.rknn" ]; then
        run_warn_check "ui_grounding_helper self-test (UI-DETR on the NPU)" timeout 90 "$ug" \
            --runtime "$model_dir/librknnmrt.so" --model "$model_dir/model.rknn" --anchors "$model_dir/RK_anchors.txt" --self-test
    else
        skip "UI-grounding self-test (binary or model missing)"
    fi
}

group_rtc() {
    section "RTC"
    if [ -e /dev/rtc0 ]; then
        a="$(cat /sys/class/rtc/rtc0/since_epoch 2>/dev/null)"
        sleep 2
        b="$(cat /sys/class/rtc/rtc0/since_epoch 2>/dev/null)"
        sys="$(date -u +%s)"
        if [ -n "$a" ] && [ -n "$b" ] && [ $((b - a)) -ge 1 ] && [ $((b - a)) -le 3 ]; then
            pass "RTC ticking ($a -> $b)"
        else
            fail "RTC not ticking (${a:-?} -> ${b:-?})"
        fi
        diff=$(( ${b:-0} - sys ))
        [ "${diff#-}" -le 5 ] && pass "RTC matches the system clock (diff ${diff}s)" || warn "RTC differs from the system clock by ${diff}s"
        printf '    power-off retention: run scripts/board_rtc_test.sh poweroff-pre/poweroff-post from a host\n'
    else
        fail "/dev/rtc0 missing"
    fi
}

if [ -z "$WIFI_IFACE" ]; then
    for path in /sys/class/net/*/wireless; do
        [ -d "$path" ] && { WIFI_IFACE="$(basename "$(dirname "$path")")"; break; }
    done
fi
[ -n "$HTTPS_URL" ] || HTTPS_URL="https://$DNS_HOST/"

for group in $RUN_GROUPS; do
    case " $SKIP_GROUPS " in *" $group "*) continue ;; esac
    "group_$group"
done

hw_finish
