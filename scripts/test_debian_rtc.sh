#!/usr/bin/env bash
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly HELPER="${REPO_ROOT}/overlay-debian/usr/lib/aico/aico-rtc"
readonly TEST_ROOT=$(mktemp -d)
trap 'rm -rf "${TEST_ROOT}"' EXIT
readonly REAL_DATE=$(command -v date)
export REAL_DATE
mkdir -p "${TEST_ROOT}/bin"

fail() { echo "RTC test failure: $*" >&2; exit 1; }
epoch() { "${REAL_DATE}" -u -d "$1" +%s; }

# Stub only clock I/O; GNU date parses the real RTC output and baseline.
# The test can never set either the host's RTC or its system clock.
cat >"${TEST_ROOT}/bin/date" <<'EOF'
#!/bin/sh
set -eu
if [ "$*" = '-u +%s' ]; then
    [ "${SYSTEM_READ_FAILURE:-0}" = 0 ] || exit 1
    cat "${RTC_TEST_CASE}/system"
    exit 0
fi
case "$*" in
    '-u --set=@'*)
        printf '%s\n' "$*" >>"${RTC_TEST_CASE}/sets"
        [ "${SYSTEM_SET_FAILURE:-0}" = 0 ] || exit 1
        printf '%s\n' "${2#--set=@}" >"${RTC_TEST_CASE}/system"
        exit 0
        ;;
esac
exec "${REAL_DATE}" "$@"
EOF
cat >"${TEST_ROOT}/bin/hwclock" <<'EOF'
#!/bin/sh
set -eu
[ "${TZ}" = UTC0 ] && [ "${LC_ALL}" = C ] || exit 99
case "$*" in
    '--show --utc --noadjfile')
        cat "${RTC_TEST_CASE}/rtc"
        [ "${RTC_READ_FAILURE:-0}" = 0 ]
        ;;
    '--systohc --utc --noadjfile')
        [ "${RTC_WRITE_FAILURE:-0}" = 0 ] || exit 1
        cat "${RTC_TEST_CASE}/system" >>"${RTC_TEST_CASE}/writes"
        ;;
    *) echo "Unexpected hwclock arguments: $*" >&2; exit 99 ;;
esac
EOF
chmod +x "${TEST_ROOT}/bin/"*

run_case() {
    local name=$1 rtc=$2 system=$3 baseline=$4 expected_system=$5 sets=$6 writes=$7
    shift 7
    local case_dir="${TEST_ROOT}/${name}"
    mkdir -p "${case_dir}"
    printf '%s\n' "${rtc}" >"${case_dir}/rtc"
    epoch "${system}" >"${case_dir}/system"
    : >"${case_dir}/sets"
    : >"${case_dir}/writes"
    local status=0
    env PATH="${TEST_ROOT}/bin:${PATH}" RTC_TEST_CASE="${case_dir}" \
        RTC_DEFAULT_DATE="${baseline}" TZ=Asia/Shanghai LC_ALL=C \
        "$@" sh "${HELPER}" >"${case_dir}/stdout" 2>"${case_dir}/stderr" || status=$?
    [ "${status}" -eq "${EXPECTED_STATUS:-0}" ] || fail "${name}: exit ${status}"
    [ "$(cat "${case_dir}/system")" = "$(epoch "${expected_system}")" ] \
        || fail "${name}: system clock moved incorrectly"
    [ "$(wc -l <"${case_dir}/sets")" -eq "${sets}" ] \
        || fail "${name}: unexpected number of system clock sets"
    [ "$(wc -l <"${case_dir}/writes")" -eq "${writes}" ] \
        || fail "${name}: unexpected RTC write"
    if [ "${writes}" -gt 0 ]; then
        [ "$(cat "${case_dir}/writes")" = "$(epoch "${expected_system}")" ] \
            || fail "${name}: wrong RTC value"
    fi
}

baseline='2026-06-09'
run_case reset_2021 '2021-01-01 19:16:38.000000+00:00' '2026-04-13 UTC' "$baseline" "$baseline UTC" 1 1
run_case reset_epoch '1970-01-01 00:00:00+00:00' '1970-01-01 UTC' "$baseline" "$baseline UTC" 1 1
run_case before_epoch '1969-12-31 23:59:59+00:00' '1970-01-01 UTC' "$baseline" "$baseline UTC" 1 1
run_case old_2026 '2026-06-08 23:59:59.999999+00:00' '2026-06-08 UTC' "$baseline" "$baseline UTC" 1 1
run_case synced_system '2021-01-01 00:00:00+00:00' '2026-09-28 13:14:15 UTC' "$baseline" '2026-09-28 13:14:15 UTC' 0 1
run_case system_at_baseline '2021-01-01 00:00:00+00:00' "$baseline UTC" "$baseline" "$baseline UTC" 0 1
run_case rtc_at_baseline '2026-06-09 00:00:00+00:00' '2026-04-13 UTC' "$baseline" '2026-04-13 UTC' 0 0
run_case rtc_after_baseline '2026-09-28 12:19:00+00:00' '2026-04-13 UTC' "$baseline" '2026-04-13 UTC' 0 0
run_case future_rtc '2030-01-01 00:00:00+00:00' '2026-09-28 UTC' "$baseline" '2026-09-28 UTC' 0 0
run_case local_date_prior '2026-06-08 20:00:00-04:00' '2026-04-13 UTC' "$baseline" '2026-04-13 UTC' 0 0
run_case local_date_same '2026-06-09 00:00:00+08:00' '2026-04-13 UTC' "$baseline" "$baseline UTC" 1 1
run_case custom_baseline '2026-09-01 00:00:00+00:00' '2026-09-01 UTC' '2026-09-28' '2026-09-28 UTC' 1 1
run_case default_baseline '2021-01-01 00:00:00+00:00' '2026-04-13 UTC' '' '2026-06-09 UTC' 1 1
run_case malformed_rtc 'not a date' '2026-04-13 UTC' "$baseline" '2026-04-13 UTC' 0 0
run_case malformed_seconds '2026-09-28 12:19:69+00:00' '2026-04-13 UTC' "$baseline" '2026-04-13 UTC' 0 0
run_case empty_rtc '' '2026-04-13 UTC' "$baseline" '2026-04-13 UTC' 0 0
run_case unreadable_rtc '2021-01-01 00:00:00+00:00' '2026-04-13 UTC' "$baseline" '2026-04-13 UTC' 0 0 RTC_READ_FAILURE=1
for name in malformed_rtc malformed_seconds empty_rtc unreadable_rtc; do
    grep -q 'Unable to .*RTC' "${TEST_ROOT}/${name}/stderr" || fail "${name}: missing diagnostic"
done
EXPECTED_STATUS=1 run_case invalid_baseline '2021-01-01 00:00:00+00:00' '2026-04-13 UTC' '2026-02-30' '2026-04-13 UTC' 0 0
EXPECTED_STATUS=1 run_case malformed_baseline '2021-01-01 00:00:00+00:00' '2026-04-13 UTC' 'tomorrow' '2026-04-13 UTC' 0 0
EXPECTED_STATUS=1 run_case system_read_failure '2021-01-01 00:00:00+00:00' '2026-04-13 UTC' "$baseline" '2026-04-13 UTC' 0 0 SYSTEM_READ_FAILURE=1
EXPECTED_STATUS=1 run_case system_set_failure '2021-01-01 00:00:00+00:00' '2026-04-13 UTC' "$baseline" '2026-04-13 UTC' 1 0 SYSTEM_SET_FAILURE=1
EXPECTED_STATUS=1 run_case rtc_write_failure '2021-01-01 00:00:00+00:00' '2026-09-28 UTC' "$baseline" '2026-09-28 UTC' 0 0 RTC_WRITE_FAILURE=1

# Rerunning after a successful repair must not write the RTC again.
run_case already_repaired '2026-06-09 00:00:00+00:00' '2026-06-09 UTC' "$baseline" '2026-06-09 UTC' 0 0
printf 'PASS: RTC baseline, UTC boundaries, newer clocks, invalid reads and write failures\n'
