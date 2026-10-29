#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
HELPER="$ROOT_DIR/overlay-debian/usr/lib/aico/aico-log.sh"

sh -n "$HELPER"
. "$HELPER"

message='status=1 detail="first line"
second line'
LINE_FILE="$(mktemp "${TMPDIR:-/tmp}/aico-logger-format.XXXXXX")"
trap 'rm -f "$LINE_FILE"' 0 HUP INT TERM
aico_log WARN Agent 'Process Supervisor' 'Process Exited' "$message" > "$LINE_FILE"

grep -Eq \
    '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z \[WARN\]\[agent\]\[process_supervisor\] process_exited message=".*"$' \
    "$LINE_FILE"

[ "$(wc -l < "$LINE_FILE" | tr -d ' ')" = "1" ]
grep -Fq 'detail=\"first line\"\nsecond line' "$LINE_FILE"

echo "logger format checks passed"
