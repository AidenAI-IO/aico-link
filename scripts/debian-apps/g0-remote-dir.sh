#!/usr/bin/env bash
# Shared G0 remote install-root helpers for prepare-board-g0.sh and run-board-g0.sh.
# Must be sourced; do not execute directly.

readonly AICO_G0_REMOTE_DIR_DEFAULT=/home/aico/debian-apps-g0

# resolve_g0_remote_dir prints the validated AICO_G0_REMOTE_DIR (or default).
# Allowed values match run-board-g0.sh: /home/<user>/debian-apps-g0 or /tmp/debian-apps-g0.
resolve_g0_remote_dir() {
    local remote_dir=${AICO_G0_REMOTE_DIR:-${AICO_G0_REMOTE_DIR_DEFAULT}}
    local remote_user=
    if [[ "${remote_dir}" =~ ^/home/([A-Za-z0-9._-]+)/debian-apps-g0$ ]]; then
        remote_user=${BASH_REMATCH[1]}
        if [ "${remote_user}" = . ] || [ "${remote_user}" = .. ]; then
            echo "AICO_G0_REMOTE_DIR contains an unsafe home component" >&2
            return 1
        fi
    elif [ "${remote_dir}" != /tmp/debian-apps-g0 ]; then
        echo "AICO_G0_REMOTE_DIR is outside the approved non-system locations" >&2
        return 1
    fi
    printf '%s\n' "${remote_dir}"
}
