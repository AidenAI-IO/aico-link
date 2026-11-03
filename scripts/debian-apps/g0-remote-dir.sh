#!/usr/bin/env bash
# Shared G0 remote install-root helpers for prepare-board-g0.sh and run-board-g0.sh.
# Must be sourced; do not execute directly.

readonly AICO_G0_REMOTE_DIR_DEFAULT=/home/aico/debian-apps-g0
readonly AICO_G0_BUNDLE_METADATA_PATH=debian-apps-g0/bundle-metadata.txt

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

# read_g0_bundle_install_root ARCHIVE prints the install_root encoded at bundle time.
read_g0_bundle_install_root() {
    local archive=$1
    local metadata install_root
    if [ ! -f "${archive}" ]; then
        echo "G0 bundle archive is missing: ${archive}" >&2
        return 1
    fi
    metadata=$(tar -xOf "${archive}" "${AICO_G0_BUNDLE_METADATA_PATH}") || {
        echo "G0 bundle is missing ${AICO_G0_BUNDLE_METADATA_PATH}" >&2
        return 1
    }
    install_root=$(printf '%s\n' "${metadata}" | awk -F= '/^install_root=/{print $2; exit}')
    if [ -z "${install_root}" ]; then
        echo "G0 bundle-metadata.txt is missing install_root" >&2
        return 1
    fi
    printf '%s\n' "${install_root}"
}

# require_g0_bundle_remote_dir ARCHIVE EXPECTED fails when the encoded root differs.
require_g0_bundle_remote_dir() {
    local archive=$1
    local expected=$2
    local recorded
    recorded=$(read_g0_bundle_install_root "${archive}") || return 1
    if [ "${recorded}" != "${expected}" ]; then
        echo "G0 bundle install_root (${recorded}) does not match AICO_G0_REMOTE_DIR (${expected})" >&2
        return 1
    fi
}
