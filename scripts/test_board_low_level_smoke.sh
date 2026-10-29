#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"

check_help_line() {
    expected="$1"
    actual="$2"
    printf '%s\n' "$actual" | grep -F "$expected" >/dev/null
}

for script in \
    board_low_level_common.sh \
    board_low_level_system.sh \
    board_low_level_hid.sh \
    board_low_level_hdmi.sh \
    board_low_level_audio.sh \
    board_low_level_logs.sh \
    board_low_level_smoke.sh
do
    sh -n "$SCRIPT_DIR/$script"
done

smoke_help="$("$SCRIPT_DIR/board_low_level_smoke.sh" --help)"
check_help_line "board_low_level_system.sh" "$smoke_help"
check_help_line "board_low_level_hid.sh" "$smoke_help"
check_help_line "board_low_level_hdmi.sh" "$smoke_help"
check_help_line "board_low_level_audio.sh" "$smoke_help"
check_help_line "board_low_level_logs.sh" "$smoke_help"
check_help_line "does not use Agent, Config Web, SSH, or HTTP APIs" "$smoke_help"

for script in \
    board_low_level_smoke.sh \
    board_low_level_hid.sh \
    board_low_level_hdmi.sh \
    board_low_level_audio.sh \
    board_low_level_logs.sh \
    board_low_level_system.sh
do
    if grep -E 'curl|https?://|/api/|AICO_AGENT|AICO_CONFIG_WEB|/var/log/agent|agent\.log|ssh |scp ' "$SCRIPT_DIR/$script" >/dev/null; then
        echo "unexpected network dependency in $script" >&2
        exit 1
    fi
done

echo "board low level smoke scripts are local-only"
