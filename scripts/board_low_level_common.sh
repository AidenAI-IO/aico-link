#!/bin/sh

if [ -n "${AICO_LOW_LEVEL_COMMON_LOADED:-}" ]; then
    return 0 2>/dev/null || exit 0
fi
AICO_LOW_LEVEL_COMMON_LOADED=1

default_run_id() {
    date -u +%Y%m%dT%H%M%SZ 2>/dev/null || date +%Y%m%dT%H%M%S
}

: "${RUN_ID:=$(default_run_id)}"
: "${AICO_LOW_LEVEL_ARTIFACT_DIR:=/tmp}"
: "${FAILS:=0}"
: "${WARNS:=0}"

die() {
    echo "error: $*" >&2
    exit 2
}

require_uint() {
    case "${2:-}" in
        ''|*[!0-9]*)
            die "$1 must be numeric"
            ;;
    esac
}

section() {
    printf '\n== %s ==\n' "$*"
}

pass() {
    printf '[PASS] %s\n' "$*"
}

warn() {
    WARNS=$((WARNS + 1))
    printf '[WARN] %s\n' "$*"
}

fail() {
    FAILS=$((FAILS + 1))
    printf '[FAIL] %s\n' "$*"
}

skip() {
    printf '[SKIP] %s\n' "$*"
}

indent() {
    sed 's/^/    /'
}

run_check() {
    aico_check_name="$1"
    shift
    aico_check_tmp="$(mktemp "${TMPDIR:-/tmp}/aico_low_level_check.XXXXXX")" || {
        fail "$aico_check_name: cannot create temporary log"
        return 1
    }
    "$@" >"$aico_check_tmp" 2>&1
    aico_check_rc=$?
    if [ "$aico_check_rc" -eq 0 ]; then
        pass "$aico_check_name"
    else
        fail "$aico_check_name (rc=$aico_check_rc)"
    fi
    [ -s "$aico_check_tmp" ] && indent < "$aico_check_tmp"
    rm -f "$aico_check_tmp"
    return "$aico_check_rc"
}

run_warn_check() {
    aico_check_name="$1"
    shift
    aico_check_tmp="$(mktemp "${TMPDIR:-/tmp}/aico_low_level_warn.XXXXXX")" || {
        fail "$aico_check_name: cannot create temporary log"
        return 1
    }
    "$@" >"$aico_check_tmp" 2>&1
    aico_check_rc=$?
    if [ "$aico_check_rc" -eq 0 ]; then
        pass "$aico_check_name"
    else
        warn "$aico_check_name (rc=$aico_check_rc)"
    fi
    [ -s "$aico_check_tmp" ] && indent < "$aico_check_tmp"
    rm -f "$aico_check_tmp"
    return 0
}

find_bin() {
    aico_bin_name="$1"
    for aico_bin_path in "/usr/lib/aico/$aico_bin_name" "/usr/local/bin/$aico_bin_name" "/usr/bin/$aico_bin_name" "/bin/$aico_bin_name"; do
        if [ -x "$aico_bin_path" ]; then
            printf '%s\n' "$aico_bin_path"
            return 0
        fi
    done
    command -v "$aico_bin_name" 2>/dev/null
}

check_exists() {
    aico_check_name="$1"
    aico_check_path="$2"
    if [ -e "$aico_check_path" ]; then
        pass "$aico_check_name exists: $aico_check_path"
        ls -l "$aico_check_path" 2>/dev/null | indent
        return 0
    fi
    fail "$aico_check_name missing: $aico_check_path"
    return 1
}

check_socket() {
    aico_check_name="$1"
    aico_check_path="$2"
    if [ -S "$aico_check_path" ]; then
        pass "$aico_check_name socket exists: $aico_check_path"
        ls -l "$aico_check_path" 2>/dev/null | indent
        return 0
    fi
    fail "$aico_check_name socket missing: $aico_check_path"
    return 1
}

service_check() {
    aico_service_name="$1"
    aico_service_units="$2"
    aico_service_init="$3"

    if command -v systemctl >/dev/null 2>&1; then
        for aico_service_unit in $aico_service_units; do
            aico_load_state="$(systemctl show -p LoadState --value "$aico_service_unit" 2>/dev/null)"
            if [ -n "$aico_load_state" ] && [ "$aico_load_state" != "not-found" ]; then
                run_check "$aico_service_name systemd unit active: $aico_service_unit" systemctl is-active "$aico_service_unit"
                systemctl --no-pager --lines=8 status "$aico_service_unit" 2>/dev/null | indent
                return 0
            fi
        done
    fi

    if [ -x "$aico_service_init" ]; then
        run_check "$aico_service_name init status: $aico_service_init" "$aico_service_init" status
        return 0
    fi

    warn "$aico_service_name service status path not found"
    return 0
}

ensure_artifact_dir() {
    mkdir -p "$AICO_LOW_LEVEL_ARTIFACT_DIR" 2>/dev/null || die "cannot create artifact dir: $AICO_LOW_LEVEL_ARTIFACT_DIR"
}

artifact_path() {
    ensure_artifact_dir
    printf '%s/aico_low_level_%s_%s\n' "$AICO_LOW_LEVEL_ARTIFACT_DIR" "$RUN_ID" "$1"
}

print_artifact() {
    printf '  %-12s %s\n' "$1:" "$2"
}

print_summary() {
    section "summary"
    printf 'warnings=%s failures=%s\n' "$WARNS" "$FAILS"
    [ "$FAILS" -eq 0 ]
}
