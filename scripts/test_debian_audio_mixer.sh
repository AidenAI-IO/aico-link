#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
readonly HELPER=${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-audio-mixer
readonly UNIT=${REPO_ROOT}/overlay-debian/etc/systemd/system/aico-audio-mixer.service
TEST_ROOT=$(mktemp -d)
readonly TEST_ROOT
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "Debian audio mixer test failure: $*" >&2
    exit 1
}

# A missing ExecStart executable must fail the required unit, not skip it.
if grep -Eq '^Condition.*=' "${UNIT}"; then
    fail "the required mixer unit may silently skip initialization"
fi
grep -Fqx 'ExecStart=/usr/lib/aico/aico-audio-mixer' "${UNIT}"
sh -n "${HELPER}"

mkdir -p "${TEST_ROOT}/bin"
cat >"${TEST_ROOT}/bin/amixer" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"${AICO_MIXER_TEST_LOG}"
# The unrelated default card is already usable; the codec is a different card.
if [ "${1:-}" = controls ]; then
    echo "numid=1,iface=MIXER,name='Unrelated Playback Volume'"
    exit 0
fi
[ "${1:-}" = -c ] && [ "${2:-}" = rv1106acodec ] || exit 1

case "${3:-}" in
    sget)
        if [ "$4" = 'ADC MIC Left Gain' ]; then
            attempt=$(cat "${AICO_MIXER_TEST_ATTEMPTS}")
            printf '%s\n' "$((attempt + 1))" >"${AICO_MIXER_TEST_ATTEMPTS}"
        fi
        attempt=$(cat "${AICO_MIXER_TEST_ATTEMPTS}")
        case "${AICO_MIXER_TEST_MODE}" in
            absent-card) exit 1 ;;
            delayed)
                [ "$attempt" -ge 2 ] || exit 1
                case "$4" in
                    'ADC ALC Right'|'ADC ALC Right Volume')
                        [ "$attempt" -ge 3 ] || exit 1 ;;
                esac
                ;;
            missing-control)
                case "$4" in
                    'ADC ALC Right'|'ADC ALC Right Volume') exit 1 ;;
                esac
                ;;
        esac
        name=$4
        ;;
    -q)
        [ "$4" = set ] || exit 1
        if [ "${AICO_MIXER_TEST_MODE}" = write-failure ] &&
            [ "$5" = 'ADC MIC Left Gain' ]; then
            exit 1
        fi
        name=$5
        ;;
    *) exit 1 ;;
esac

if [ "${AICO_MIXER_TEST_MODE}" = volume ]; then
    case "$name" in
        'ADC ALC Left'|'ADC ALC Right') exit 1 ;;
    esac
fi
case "$name" in
    'ADC MIC Left Gain'|'ADC MIC Right Gain'|'ADC MICBIAS Voltage'|'ADC Mode'|\
    'ADC ALC Left'|'ADC ALC Right'|'ADC ALC Left Volume'|'ADC ALC Right Volume')
        exit 0 ;;
    *) exit 1 ;;
esac
EOF
cat >"${TEST_ROOT}/bin/sleep" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"${AICO_MIXER_TEST_SLEEPS}"
EOF
printf '#!/bin/sh\nexit 0\n' >"${TEST_ROOT}/bin/udevadm"
chmod 0755 "${TEST_ROOT}/bin/"*
sh -n "${TEST_ROOT}/bin/amixer"

export PATH="${TEST_ROOT}/bin:${PATH}"
export AICO_AMIXER_BIN="${TEST_ROOT}/bin/amixer"
export AICO_MIXER_TEST_LOG="${TEST_ROOT}/amixer.log"
export AICO_MIXER_TEST_ATTEMPTS="${TEST_ROOT}/attempts"
export AICO_MIXER_TEST_SLEEPS="${TEST_ROOT}/sleeps"

run_mixer() {
    export AICO_MIXER_TEST_MODE=$1
    : >"${AICO_MIXER_TEST_LOG}"
    : >"${AICO_MIXER_TEST_SLEEPS}"
    printf '0\n' >"${AICO_MIXER_TEST_ATTEMPTS}"
    "${HELPER}" >"${TEST_ROOT}/stdout" 2>"${TEST_ROOT}/stderr"
}

check_writes() {
    local alc_suffix=$1
    cat >"${TEST_ROOT}/expected-writes" <<EOF
-c rv1106acodec -q set ADC MIC Left Gain 3
-c rv1106acodec -q set ADC MIC Right Gain 3
-c rv1106acodec -q set ADC MICBIAS Voltage VREFx0_975
-c rv1106acodec -q set ADC Mode SingadcL
-c rv1106acodec -q set ADC ALC Left${alc_suffix} 26
-c rv1106acodec -q set ADC ALC Right${alc_suffix} 26
EOF
    # The Volume variant first attempts the unsuffixed aliases.
    grep -F -- '-q set' "${AICO_MIXER_TEST_LOG}" |
        if [ -n "${alc_suffix}" ]; then
            grep -vE 'ADC ALC (Left|Right) 26$'
        else
            cat
        fi >"${TEST_ROOT}/actual-writes"
    diff -u "${TEST_ROOT}/expected-writes" "${TEST_ROOT}/actual-writes"
}

run_mixer ready
check_writes ''
[ ! -s "${AICO_MIXER_TEST_SLEEPS}" ] || fail "ready controls caused a retry"

run_mixer delayed
check_writes ''
[ "$(cat "${AICO_MIXER_TEST_ATTEMPTS}")" -eq 3 ] || fail "delayed controls were not retried"
[ "$(wc -l <"${AICO_MIXER_TEST_SLEEPS}")" -eq 2 ] || fail "unexpected retry count"
# All controls must be readable before the first write.
first_write=$(grep -n -m1 -- '-q set' "${AICO_MIXER_TEST_LOG}" | cut -d: -f1)
last_probe=$(grep -n 'sget' "${AICO_MIXER_TEST_LOG}" | tail -1 | cut -d: -f1)
[ "${first_write}" -gt "${last_probe}" ] || fail "wrote controls before readiness"

run_mixer volume
check_writes ' Volume'

for mode in absent-card missing-control; do
    if run_mixer "${mode}"; then
        fail "initialization succeeded with ${mode}"
    fi
    [ "$(cat "${AICO_MIXER_TEST_ATTEMPTS}")" -eq 40 ] || fail "readiness retries were not bounded"
    if grep -Fq -- '-q set' "${AICO_MIXER_TEST_LOG}"; then
        fail "wrote controls without a complete codec mixer"
    fi
    grep -Fq 'required controls on rv1106acodec did not become ready' "${TEST_ROOT}/stderr"
done

if run_mixer write-failure; then
    fail "initialization succeeded after a failed control write"
fi
grep -Fq 'failed to set ADC MIC Left Gain to 3' "${TEST_ROOT}/stderr"
echo 'PASS: codec selection, control readiness, ALC aliases and initialization failures'
