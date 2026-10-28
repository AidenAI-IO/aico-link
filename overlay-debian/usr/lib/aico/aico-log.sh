#!/bin/sh
# Shared logger for first-party init and watchdog scripts.
# Output: <UTC> [LEVEL][service][component] event [message="..."]

aico_log_escape() {
    awk '
        BEGIN { ORS = "" }
        {
            if (NR > 1) printf "\\n"
            for (i = 1; i <= length($0); i++) {
                c = substr($0, i, 1)
                if (c == "\\") printf "\\\\"
                else if (c == "\"") printf "\\\""
                else if (c == "\r") printf "\\r"
                else if (c == "\t") printf "\\t"
                else printf "%s", c
            }
        }
    '
}

aico_log_identifier() {
    normalized="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' |
        sed 's/[^a-z0-9]/_/g; s/__*/_/g; s/^_//; s/_$//')"
    [ -n "$normalized" ] && printf '%s' "$normalized" || printf 'unknown'
}

aico_log() {
    level="$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]')"
    case "$level" in
        DEBUG|INFO|WARN|ERROR) ;;
        *) level=INFO ;;
    esac
    service="$(aico_log_identifier "$2")"
    component="$(aico_log_identifier "$3")"
    event="$(aico_log_identifier "$4")"
    shift 4

    timestamp="$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)"
    [ -n "$timestamp" ] || timestamp=1970-01-01T00:00:00Z
    printf '%s [%s][%s][%s] %s' \
        "$timestamp" "$level" "$service" "$component" "$event"
    if [ "$#" -gt 0 ]; then
        escaped="$(printf '%s' "$*" | aico_log_escape)"
        printf ' message="%s"' "$escaped"
    fi
    printf '\n'
}

aico_log_to_file() {
    log_path="$1"
    shift
    aico_log "$@" >> "$log_path"
}
