#!/usr/bin/env bash
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

unit="$repo/overlay-debian/etc/systemd/system/aiden-media-modules.service"
grep -Fxq 'StandardOutput=journal+console' "$unit"
grep -Fxq 'StandardError=journal+console' "$unit"

write_topology() {
    local bridge=$1 flags=$2
    # A bridge's bound callback creates entities and links before CIF exposes
    # subdevice nodes. Omit both device node names and HDMI timing information.
    cat >"$fixture/topology" <<EOF
driver          rkcif
model           rkcif-mipi-lvds
- entity 58: rockchip-csi2-dphy0 (2 pads, 1 link)
    pad0: SINK
EOF
    if [ -n "$bridge" ]; then
        cat >>"$fixture/topology" <<EOF
- entity 63: $bridge (1 pad, 1 link)
    type V4L2 subdev subtype Sensor flags 0
    pad0: SOURCE
        -> "rockchip-csi2-dphy0":0 [$flags]
EOF
    fi
}

prepare_case() {
    fixture="$test_root/$1"
    export FIXTURE="$fixture"
    mkdir -p "$fixture"/{bin,ko,sys/module,proc,dev}
    for module in video_rkcif video_rkisp; do
        mkdir -p "$fixture/sys/module/$module/parameters"
        printf 'not-ready\n' >"$fixture/sys/module/$module/parameters/clr_unready_dev"
    done
    for module in rk_dvbm video_rkcif video_rkisp \
        phy-rockchip-csi2-dphy-hw phy-rockchip-csi2-dphy \
        rga3 mpp_vcodec rknpu snd-soc-rv1106 motor rockit; do
        touch "$fixture/ko/$module.ko"
    done
    touch "$fixture/dev/media0" "$fixture/dev/media1"
    cat >"$fixture/bin/insmod" <<'EOF'
#!/bin/sh
set -eu
name=$(basename "$1" .ko | tr '-' '_')
mkdir -p "$FIXTURE/sys/module/$name"
printf '%s\n' "$*" >>"$FIXTURE/loaded"
# Rockit must be initialized after the CIF/ISP cleanup decision.
if [ "$name" = rockit ]; then
    for module in video_rkcif video_rkisp; do
        parameter="$FIXTURE/sys/module/$module/parameters/clr_unready_dev"
        # Failure fixtures use missing or non-regular parameters; reading
        # /dev/full here would never terminate.
        if [ -f "$parameter" ] && [ ! -L "$parameter" ]; then
            cat "$parameter" >>"$FIXTURE/rockit-state"
        else
            printf 'unavailable\n' >>"$FIXTURE/rockit-state"
        fi
    done
fi
EOF
    cat >"$fixture/bin/media-ctl" <<'EOF'
#!/bin/sh
set -eu
[ "$1" = -d ] && [ "$3" = -p ]
case "$2" in
    "$FIXTURE/dev/media0")
        # A linked HDMI entity on an unrelated ISP graph is insufficient.
        printf 'driver          rkisp\n'
        tail -n +2 "$FIXTURE/topology"
        ;;
    "$FIXTURE/dev/media1") cat "$FIXTURE/topology" ;;
    *) exit 1 ;;
esac
if [ -e "$FIXTURE/media-error" ]; then
    echo 'topology unavailable' >&2
    exit 1
fi
EOF
    printf '#!/bin/sh\nexit 0\n' >"$fixture/bin/lsmod"
    printf '#!/bin/sh\nexit 0\n' >"$fixture/bin/udevadm"
    chmod +x "$fixture/bin/"*

    sed \
        -e "s|/usr/lib/aiden/platform/modules|$fixture/ko|g" \
        -e "s|/sys/|$fixture/sys/|g" \
        -e "s|/proc/|$fixture/proc/|g" \
        -e "s|/dev/|$fixture/dev/|g" \
        "$repo/overlay-debian/usr/lib/aiden/aiden-media-modules" >"$fixture/loader"
}

assert_parameters() {
    for module in video_rkcif video_rkisp; do
        grep -qx "$1" "$fixture/sys/module/$module/parameters/clr_unready_dev"
    done
}

run_case() {
    local expected=$1
    PATH="$fixture/bin:$PATH" sh "$fixture/loader" 2>"$fixture/stderr"
    assert_parameters "$expected"
    if [ "$expected" = not-ready ]; then
        grep -Fq '[aiden-media-modules] WARN:' "$fixture/stderr"
        grep -Fq 'CIF/ISP async notifiers remain pending; skipping clr_unready_dev' "$fixture/stderr"
        grep -Fq 'continuing other media/audio modules' "$fixture/stderr"
        grep -Fq 'After the bridge binds, stop aiden-frame.service and rerun' "$fixture/stderr"
    else
        test ! -s "$fixture/stderr"
    fi
    for module in rga3 mpp_vcodec rknpu snd_soc_rv1106 motor rockit; do
        test -d "$fixture/sys/module/$module"
    done
    grep -q 'isp_max_h=0' "$fixture/loaded"
    test "$(grep -cx "$expected" "$fixture/rockit-state")" -eq 2
    test ! -e "$fixture/dev/v4l-subdev0"
    local before
    before=$(wc -l <"$fixture/loaded")
    PATH="$fixture/bin:$PATH" sh "$fixture/loader" 2>"$fixture/stderr"
    test "$(wc -l <"$fixture/loaded")" -eq "$before"
}

for bridge in rk628-csi tc358743; do
    prepare_case "$bridge"
    write_topology "m00_b_$bridge 4-0050" ENABLED
    run_case 1
done

for failure in missing not-writable write-failed; do
    prepare_case "cif-$failure"
    write_topology 'm00_b_rk628-csi 4-0050' ENABLED
    parameter="$fixture/sys/module/video_rkcif/parameters/clr_unready_dev"
    rm "$parameter"
    case "$failure" in
        missing) ;;
        not-writable)
            # Docker mounts sysfs read-only, including for root.
            ln -s /sys/kernel/uevent_seqnum "$parameter"
            test -e "$parameter" && test ! -w "$parameter"
            ;;
        write-failed)
            ln -s /dev/full "$parameter"
            test -w "$parameter"
            ;;
    esac
    PATH="$fixture/bin:$PATH" sh "$fixture/loader" 2>"$fixture/stderr"
    grep -qx not-ready "$fixture/sys/module/video_rkisp/parameters/clr_unready_dev"
    grep -Fq 'WARN: video_rkcif clr_unready_dev is unavailable or its write failed' "$fixture/stderr"
    grep -Fq 'skipping remaining notifier cleanup and continuing other media/audio modules' "$fixture/stderr"
    for module in rga3 mpp_vcodec rknpu snd_soc_rv1106 motor rockit; do
        test -d "$fixture/sys/module/$module"
    done
    test "$(cat "$fixture/rockit-state")" = $'unavailable\nnot-ready'
done

prepare_case absent
write_topology '' ''
run_case not-ready
grep -Fq 'neither RK628 nor TC358743 has an enabled link' "$fixture/stderr"
# A later bound callback can be finalized by rerunning the loader.
before=$(wc -l <"$fixture/loaded")
write_topology 'm00_b_rk628-csi 4-0050' ENABLED
PATH="$fixture/bin:$PATH" sh "$fixture/loader"
assert_parameters 1
test "$(wc -l <"$fixture/loaded")" -eq "$before"

prepare_case unlinked
write_topology 'm00_b_rk628-csi 4-0050' ''
run_case not-ready
grep -Fq 'neither RK628 nor TC358743 has an enabled link' "$fixture/stderr"

prepare_case query-failed
write_topology 'm00_b_rk628-csi 4-0050' ENABLED
touch "$fixture/media-error"
run_case not-ready
grep -Fq 'topology query failed for' "$fixture/stderr"
grep -Fq 'topology unavailable' "$fixture/stderr"
grep -Fq 'HDMI bridge readiness is unknown' "$fixture/stderr"
if grep -Fq 'neither RK628 nor TC358743 has an enabled link' "$fixture/stderr"; then
    echo 'FAIL: query errors must not be reported as a missing bridge' >&2
    exit 1
fi

echo 'PASS: either linked bridge completes CIF/ISP; unavailable bridges or CIF cleanup failures retain async waiting'
